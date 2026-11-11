package db

// The outer-driven join: streaming the FROM side in key batches and resolving each batch
// against the inner side's index, instead of reading the inner side whole into one hash.
//
// The hash join and the index-based equi join (db/joinprobe.go) both HOLD a side. That is
// what MaxHashRows bounds, and it is the one place a join refuses rather than running a
// worse plan. This path holds neither side whole: it holds one batch of FROM rows and the
// inner rows that batch's keys actually reach, so the memory a join needs stops being a
// function of how much history exists.
//
// It is the composition of two narrowings, not a choice between them. collections'
// LookupKeys takes the batch's key span and drops every inner segment whose zone map misses
// it -- the segment-level narrowing -- and then walks only the index postings for the batch's
// keys inside the segments that survive. So a batch pays for the inner rows it matches and
// for nothing else.
//
// Batching is what makes this viable rather than an optimisation on top of it. A one-key-at-
// a-time lookup pays the per-lookup floor -- planning, the snapshot, the zone walk over every
// segment -- against a single key's matches; at 64 keys that floor is amortised and the cost
// per row reaches the same rate a sequential scan materialises rows at. Below ~16 keys the
// floor dominates; past ~512 the batch starts widening its own key span and reading segments
// back in. 64 is a knee, not an optimum.

import (
	"fmt"
	"iter"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// joinBatchKeys is how many FROM-side rows one batch covers. A var so a test can move it,
// and so a site whose segments are sized differently has somewhere to change it.
var joinBatchKeys = 64

// outerDrivenMode overrides the cost rule below. Production runs outerDrivenAuto; the other
// two exist so a test can put the same join down both paths, which is the only way to check
// that they answer alike.
type outerDrivenMode int

const (
	outerDrivenAuto  outerDrivenMode = iota // the cost rule decides
	outerDrivenForce                        // take the path wherever it structurally fits
	outerDrivenNever                        // decline always, to measure the join without it
)

var joinOuterDrivenMode = outerDrivenAuto

// Where the two paths cross on SPEED is measured but not yet acted on, because the
// measurement is not yet against the right opponent.
//
// TestOuterDrivenCrossoverAgainstTheHash sweeps the FROM side against the inner one and finds
// a ratio rather than a row count -- 0.28x and 0.27x at ~0.3% of the inner side, 1.09x and
// 1.75x at 20%, unchanged when the inner side grew fourfold. That would argue for taking this
// path on selectivity alone, somewhere near 5%.
//
// It does not, yet. That benchmark reports ProbeKeys of 0 on every row: its baseline is the
// build-side plan -- hash the small FROM side, stream the inner one -- and not the index
// probe (db/joinprobe.go), which is the plan that would actually run against a selective FROM
// side and which narrows the hashed side with the same index this path looks keys up in.
// Beating the weaker of two incumbents is not evidence for replacing the stronger one, so the
// default stays on the memory bounds until the comparison is made against the probe.

// outerDriven is a join the batched path can run, and what it needs to run it.
type outerDriven struct {
	arch    *collections.Archive
	keyAttr string // the inner side's half of the equality, a bare attribute
	rowAttr string // the FROM side's half, likewise
	step    *joinStepPlan
}

// planOuterDriven decides whether a join can be driven from its FROM side.
//
// Every condition here is structural -- the shape of the plan and the inner table's declared
// index set -- so the decision is made once and never revisited mid-stream. A path that could
// discover half way through the FROM side that it cannot serve a row would have to either
// retract rows it has already emitted or answer differently from the hash path, and neither
// is something a join may do.
func (cat *Catalog) planOuterDriven(p *joinPlan, sides []JoinSide) (*outerDriven, bool) {
	if joinOuterDrivenMode == outerDrivenNever {
		return nil, false
	}
	if len(p.steps) != 1 {
		return nil, false // a chain holds one hash per step; this bounds one batch, not n of them
	}
	step := p.steps[0]
	if step.kind == joinRightOuter {
		// The inner side is preserved, so the join owes its unmatched rows. Only a side read
		// whole knows which those are, and this path reads the matching rows alone.
		return nil, false
	}
	if len(step.equiSide) != 1 {
		return nil, false // one equality: a composite key is a batch of tuples, not of values
	}
	keyAttr, rowAttr := step.equiSideAttr[0], step.equiRowAttr[0]
	if keyAttr == "" || rowAttr == "" {
		// Either half is an expression rather than a bare attribute, so the keys a batch
		// would carry are not the values any index holds.
		return nil, false
	}
	side := sides[step.side]
	if constraintOf(side) != "true" {
		// The side's own constraint says which of its rows the caller asked for, and a
		// lookup resolves the key and nothing else. Applying one without the other widens
		// the join to rows the query excluded.
		return nil, false
	}
	t, ok := cat.ArchiveTable(side.Table)
	if !ok {
		return nil, false
	}
	// The key must be served by an index AS THIS PATH WILL ASK FOR IT, which is why the
	// question is put to the planner as the very query a batch runs rather than to the
	// archive's declared index set.
	//
	// The declared set cannot answer it. A string attribute named in ValueAttrs is accepted
	// at create and then reported as indexed by both IndexedAttrs and ZoneAttrs, while every
	// query on it visits each record: a value index serves numbers, and a string belongs in
	// CategoricalAttrs. Gating on the declared set would admit such a key, and this path
	// would then fold a batch of string keys down to no numeric keys at all and answer an
	// empty lookup -- not a slow join, a WRONG one. Explaining the batch query cannot
	// disagree with what the batch does, because it is the same query.
	if ex := t.a.ExplainQuery(collections.BatchKeyQuery(keyAttr, []int64{0})); ex.Plan != "indexed" {
		return nil, false
	}
	// A batch has to be able to hold what it looks up. MaxHashRows bounds rows held, and
	// this path holds a batch's matches rather than a side -- so a limit below the batch
	// size cannot be met by any batch that matches anything, and taking the path would turn
	// a plan the index probe might still have narrowed into a refusal.
	if p.limits.MaxHashRows < joinBatchKeys {
		return nil, false
	}
	// The cost rule: take this path when holding the inner side would cost more memory than
	// one join is allowed to spend.
	//
	// The bound is BYTES, not rows, because rows do not express it (see db/joinmem.go). An
	// earlier form of this rule triggered on MaxHashRows, which is wrong twice over. That
	// count means anything from 400 MB to 7.7 GB depending on how many attributes the ad
	// carries, so it is not a memory budget. And for a wide ad the process runs out of
	// memory well before buildStage reaches the count and returns the error this path was
	// meant to spare it -- so the rescue fired at a threshold the cases needing it never
	// reach.
	//
	// Rows come from index statistics and width from a bounded sample, so choosing reads
	// almost nothing. A side that cannot be priced is left alone: an unknown size is not
	// evidence of a large one, and guessing would take this path on every unsized table.
	if joinOuterDrivenMode != outerDrivenForce {
		rows, sized := cat.sideReadSize(side)
		if !sized {
			return nil, false
		}
		width, known := archiveAdWidth(t)
		if !known {
			return nil, false
		}
		// Either memory bound. Over MaxHashRows the hash refuses outright, so this path is
		// the difference between rows and an error; over MaxHashBytes it would hold more
		// than a join may spend, which is the failure a row count does not catch.
		overRows := rows > p.limits.MaxHashRows
		overBytes := estHashBytes(rows, width) > p.limits.MaxHashBytes

		if !overRows && !overBytes {
			return nil, false
		}
	}
	return &outerDriven{arch: t.a, keyAttr: keyAttr, rowAttr: rowAttr, step: step}, true
}

// constraintOf is a side's constraint with the empty spelling folded into the true one, so a
// test for "asked for everything" is one comparison.
func constraintOf(s JoinSide) string {
	if c := strings.TrimSpace(s.Constraint); c != "" {
		return c
	}
	return "true"
}

// batchKeys is the batch's distinct keys, and whether every one of them is an integer.
//
// Keys are collected as ClassAd values and folded the way the hash's bucket key folds them,
// so that two FROM rows whose keys compare equal produce one lookup key. A row whose key is
// undefined contributes nothing: ClassAd equality propagates undefined, so it matches no
// inner row on either path.
func (od *outerDriven) batchKeys(batch []*joinRow) (ints []int64, reals []float64, allInt bool) {
	seenI := map[int64]bool{}
	seenR := map[float64]bool{}
	allInt = true
	for _, r := range batch {
		v := r.eval(od.rowAttr)
		f, ok := numericKey(v)
		if !ok {
			continue
		}
		if n := int64(f); float64(n) == f {
			if !seenI[n] {
				seenI[n] = true
				ints = append(ints, n)
			}
			continue
		}
		allInt = false
		if !seenR[f] {
			seenR[f] = true
			reals = append(reals, f)
		}
	}
	return ints, reals, allInt
}

// numericKey folds a key value into the one numeric space the hash's bucket key uses, so this
// path and the hash path agree on which keys compare equal. A bool is numeric there (true
// buckets with 1), so it is numeric here.
func numericKey(v classad.Value) (float64, bool) {
	switch {
	case v.IsBool():
		b, err := v.BoolValue()
		if err != nil {
			return 0, false
		}
		if b {
			return 1, true
		}
		return 0, true
	case v.IsInteger():
		n, err := v.IntValue()
		if err != nil {
			return 0, false
		}
		return float64(n), true
	case v.IsReal():
		f, err := v.RealValue()
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// mixedKeyQuery is the constraint a batch stands for when its keys are not all integers:
// `member(attr, {...})` over the values themselves. It is the same shape LookupKeys compiles
// internally, built from an AST rather than text so the values become literals with no
// formatting round trip to disagree about.
func mixedKeyQuery(attr string, ints []int64, reals []float64) *vm.Query {
	elems := make([]ast.Expr, 0, len(ints)+len(reals))
	for _, n := range ints {
		elems = append(elems, &ast.IntegerLiteral{Value: n})
	}
	for _, f := range reals {
		elems = append(elems, &ast.RealLiteral{Value: f})
	}
	return vm.Compile(&ast.FunctionCall{
		Name: "member",
		Args: []ast.Expr{ast.NewAttributeReference(attr, ast.NoScope), &ast.ListLiteral{Elements: elems}},
	})
}

// stage builds the hash for ONE batch: the inner rows the batch's keys reach, bucketed the
// way buildStage buckets a whole side, so the walk above cannot tell the difference.
func (od *outerDriven) stage(p *joinPlan, batch []*joinRow, stats *JoinStats) (*joinStage, int, error) {
	st := &joinStage{step: od.step, hashed: true, buckets: map[string][]*joinRow{}}
	ints, reals, allInt := od.batchKeys(batch)
	keys := len(ints) + len(reals)
	if keys == 0 {
		return st, 0, nil // every key in the batch was undefined: nothing to look up
	}
	var ads iter.Seq[*classad.ClassAd]
	if allInt {
		ads = od.arch.LookupKeys(od.keyAttr, ints)
	} else {
		ads = od.arch.Query(mixedKeyQuery(od.keyAttr, ints, reals))
	}
	held := 0
	for ad := range ads {
		row := decodedRow(ad)
		k, ok := p.sideKey(od.step, row)
		if !ok {
			continue
		}
		st.buckets[k] = append(st.buckets[k], row)
		held++
		// The same bound the hash is held to, applied to what a batch holds. A batch is
		// small by construction, so this fires only on a limit far below the default --
		// but a limit is a limit, and a path that quietly ignored it would hold more than
		// the caller allowed while reporting that it ran.
		if held > p.limits.MaxHashRows {
			return nil, 0, fmt.Errorf("a key batch of the join matched more than %d rows "+
				"from %s; narrow it with a constraint on that side",
				p.limits.MaxHashRows, p.aliases[od.step.side])
		}
	}
	// HashRows is a HIGH-WATER MARK here, not a running total. What this path holds is one
	// batch's worth at a time and the previous batch's rows are dropped before the next is
	// read, so summing them would report memory the join never used at once.
	if held > stats.HashRows {
		stats.HashRows = held
	}
	return st, keys, nil
}

// runJoinOuterDriven streams the FROM side in batches, resolving each batch's keys against the
// inner side and walking that batch's rows through the result.
//
// The walk itself is the ordinary one, so the residual condition, null extension, MaxRows and
// the caller's LIMIT behave exactly as they do on the hash path. What changes is the lifetime
// of the thing being walked: one batch's stage, not the whole side's.
//
// Output order is the FROM side's, as it is on every other path: batches are flushed in the
// order the stream produced them and a batch's rows are walked in the order they arrived.
func runJoinOuterDriven[T any](p *joinPlan, od *outerDriven, scanSeq iter.Seq[*joinRow],
	stats *JoinStats, mk func(wk *joinWalk, p *joinPlan, rows []*joinRow) (T, bool),
	yield func(T) bool) {
	// What the run narrowed, reported in the same shape every other path reports it, so
	// ExplainJoin's description of this step can be checked against what the step did.
	// Attr is known before any batch runs; Keys accumulates as batches resolve, because
	// what this path narrows on is the whole stream's keys, one batch at a time.
	stats.ProbeAttr = od.keyAttr
	stats.ProbeSteps = append(stats.ProbeSteps, JoinProbeStat{
		Alias: p.aliases[od.step.side],
		Attr:  od.keyAttr,
	})
	probeStep := &stats.ProbeSteps[len(stats.ProbeSteps)-1]

	wk := newJoinWalk(p)
	rows := wk.rows
	batch := make([]*joinRow, 0, joinBatchKeys)

	flush := func() (stop bool) {
		if len(batch) == 0 {
			return false
		}
		st, keys, err := od.stage(p, batch, stats)
		if err != nil {
			stats.Err = err
			return true
		}
		stats.OuterDrivenBatches++
		probeStep.Keys += keys
		stats.ProbeKeys += keys
		walk := makeWalk(p, []*joinStage{st}, wk, stats, mk, yield)
		for _, l := range batch {
			stats.ScanRows++
			rows[0] = l
			wk.scanRow()
			if walk(0) {
				return true
			}
			if p.limit > 0 && stats.Rows >= p.limit {
				return true
			}
		}
		batch = batch[:0]
		return false
	}

	for l := range scanSeq {
		batch = append(batch, l)
		if len(batch) < joinBatchKeys {
			continue
		}
		if flush() {
			return
		}
	}
	flush()
}
