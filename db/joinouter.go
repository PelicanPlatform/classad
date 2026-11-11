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

// Where the three paths cross on SPEED is measured, and the default still does not act on
// it, for a reason the measurement itself supplies.
//
// TestOuterDrivenAgainstTheIndexProbe runs this path against BOTH incumbents -- the hash and
// the index probe (db/joinprobe.go) -- sweeping fan-out, how many inner rows share a key, at
// a fixed inner side of 20,000 rows over 30 segments:
//
//	pairs/inner   b/hash                b/probe              what the planner picks
//	       0.3%   0.01x 0.05x 0.09x     1.41x 3.73x 4.36x    probe
//	         5%   0.10x 0.15x 0.21x     0.12x 0.34x 0.74x    probe, probe, hash
//	        20%   0.42x 0.47x 0.53x     0.02x 0.07x 0.41x    probe, probe, hash
//	        80%   1.43x 1.47x 1.57x     0.01x 0.04x 0.16x    hash
//
// Three columns per cell, one per fan-out of 1, 8 and 64. They barely move, and that is the
// result: the crossover against the hash is the PAIRS a join produces over the inner side's
// rows -- about 50% on this fixture, and the sweeps below say what that figure depends on --
// and not the ratio of the two sides' row counts. An earlier sweep read
// it as the latter because it held fan-out at 1, where a key count and a pair count are the
// same number.
//
// Against the probe this path wins from 5% up and loses below it. That is the shape of the
// two mechanisms: the probe folds every key into ONE membership constraint, so past the
// store's pushdown estimate (collections, maxPushdownFrac) the index union is abandoned and
// the constraint becomes a predicate over a full scan -- 613 ms against the hash's 24 ms at
// 4,000 keys. Batching at 64 keys never reaches that estimate, so this path is linear in the
// key count where the probe is not. Below 5% the probe narrows to the same rows in one query
// and wins on the per-batch overhead this pays.
//
// So there is a band -- from about 5% of the inner side's rows in pairs to the crossover
// measured below -- where this path beats both. What is missing is not the measurement but the ESTIMATE: engaging on
// it needs the pair count before any row is read, and a pair count is the FROM side's distinct
// keys times the inner side's rows per key, neither of which the plan holds. The FROM side's
// keys are counted by planIndexProbes, which already holds that side to collect them, so the
// number exists -- one step later than the decision is made now. Until that is wired, the
// default stays on the memory bounds, which are structural and need no estimate.
//
// WHAT THAT TABLE DOES NOT TRANSFER, and it is the number most likely to be quoted: ~50% is
// the crossover ON THAT FIXTURE, whose segments are 64 KiB and carry about 670 rows. The
// archive default is 8 MiB. Two sweeps vary one axis each, at fan-out 64, so that what moves
// the ratio can be told from what moves with it.
//
// Table size, at a fixed 2 MiB segment (12,500 to 21,000 rows each, 2 to 19 segments):
//
//	inner rows    5%      20%     80%
//	    25,000   0.06x   0.27x   0.92x
//	   100,000   0.07x   0.27x   0.95x
//	   400,000   0.07x   0.28x   0.95x
//
// Sixteen times the table and the ratio does not move. Pairs over inner rows is the axis; the
// two sides' absolute sizes are not, and neither is a segment count between 2 and 19.
//
// Segment size, at a fixed 400,000-row inner side:
//
//	rows/segment   segments    5%      20%     80%
//	         681        587   0.10x   0.35x   1.24x
//	      21,053         19   0.08x   0.27x   0.95x
//
// This one moves, and only at the top of the range: 0.02x apart at 5% of pairs, 0.26x at 80%.
// The cost is per BATCH and per SEGMENT -- every batch walks the zone map of every segment to
// find the ones its key span can reach -- so it is paid against however few rows a batch keeps,
// and 587 segments pay it 30 times over against 19. It is a segment COUNT effect rather than a
// rows-per-segment one, which is why the first sweep sees none of it at 2 to 19 segments.
//
// So the crossover against the hash is ~80% of the inner side's rows in pairs at production
// segment sizes, not the ~50% this fixture shows, and the margin below it is wide: 0.07x at
// 5% of pairs and 0.27x at 20%, consistently, across every table size measured.
//
// Every ratio above is a measured point. A crossover quoted as a percentage is interpolated
// between two of them, so it is stated as a rough figure and never carried into a comparison.
//
// The last column is worth its own look: it is what a caller really gets, and at fan-out 64
// the planner picks the probe at every ratio, including 80%, where the probe takes 177 ms
// against the hash's 19 ms and this path's 28 ms. The probe's gate prices distinct keys
// against segments and never sees how many rows a key reaches, so fan-out is invisible to it.
// That is the probe's bug, not this path's, and it is filed separately.

// outerDriven is a join the batched path can run, and what it needs to run it.
type outerDriven struct {
	arch    *collections.Archive
	keyAttr string // the inner side's half of the equality, a bare attribute
	rowAttr string // the FROM side's half, likewise
	step    *joinStepPlan
	// where is the inner side's own constraint, parsed, or nil when the side asked for
	// everything. It rides INTO each batch's lookup rather than filtering the lookup's
	// results, so a batch reads only the rows it will keep and the store can drop a
	// record on an index before decoding it.
	where ast.Expr
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
	// The side's own constraint says which of its rows the caller asked for, and a bare key
	// lookup resolves the key and nothing else -- so a batch that dropped it would widen the
	// join to rows the query excluded. The batch carries it instead.
	//
	// A constraint that does not parse declines the path rather than being ignored. The scan
	// paths reject it downstream, where the parse error is reported as a parse error; a path
	// that quietly ran without it would answer a different query from the one asked.
	var where ast.Expr
	if c := constraintOf(side); c != "true" {
		q, perr := vm.Parse(c)
		if perr != nil {
			return nil, false
		}
		if where = q.Expr(); where == nil {
			return nil, false
		}
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
	// With a constraint there is a second question, because what a batch runs is the key AND
	// the constraint, and a constraint can defeat an index the key alone was served by. It is
	// asked separately rather than instead, since each gate catches what the other misses: a
	// key no index serves can still plan well once a constraint narrows it, and a key an index
	// serves can still end in a scan once one is conjoined.
	//
	// The admitted plans are named rather than the refused ones. "indexed" is the batch doing
	// what this path exists to do. "empty" is an artefact of the sentinel key and not an answer
	// about the constraint at all: a constraint on the key attribute itself -- ClusterId > 100
	// -- contradicts the sentinel and folds to nothing, where the same constraint against a
	// batch's real keys plans "indexed". Everything else is declined, "indexed-union" among
	// them: a union over an OR is index-driven and probably fine here, but nothing has measured
	// what a batch costs on one, and an unmeasured plan is not a plan this path should take.
	if where != nil {
		switch ex := t.a.ExplainQuery(collections.BatchKeyQueryWhere(keyAttr, []int64{0}, where)); ex.Plan {
		case "indexed", "empty":
		default:
			return nil, false
		}
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
	return &outerDriven{arch: t.a, keyAttr: keyAttr, rowAttr: rowAttr, step: step, where: where}, true
}

// constraintOf is a side's constraint with the empty spelling folded into the true one, so a
// test for "asked for everything" is one comparison.
func constraintOf(s JoinSide) string {
	if c := strings.TrimSpace(s.Constraint); c != "" {
		return c
	}
	return "true"
}

// batchKeys is the batch's distinct keys, and whether the fast lookup can serve them.
//
// Keys are collected as ClassAd values and folded the way the hash's bucket key folds them
// (appendKeyValue), so that two FROM rows whose keys compare equal produce one lookup key.
// That contract admits FOUR kinds -- bool, integer, real and string -- and this must admit
// exactly the same four. A kind dropped here is not a slower join: the inner rows carrying it
// are never looked up, so an INNER join loses those pairs and a LEFT join null-extends rows
// that had a match. A row whose key is any other kind contributes nothing, which is right
// because appendKeyValue gives it no bucket either, so it matches nothing on either path.
//
// Strings fold to lower case, as appendKeyValue folds them, because ClassAd compares strings
// case-insensitively -- and so does the member() the lookup compiles to, which is what lets
// the folded literal stand for every casing of it.
//
// fast reports that every key is an integer, which is the only case the zone-hoisted
// LookupKeys can serve: its keys are int64, so a real would be truncated into a key that
// matches the wrong rows, and a string has no numeric span to hoist at all.
func (od *outerDriven) batchKeys(batch []*joinRow) (ints []int64, reals []float64, strs []string, fast bool) {
	seenI := map[int64]bool{}
	seenR := map[float64]bool{}
	seenS := map[string]bool{}
	for _, r := range batch {
		v := r.eval(od.rowAttr)
		if v.IsString() {
			str, err := v.StringValue()
			if err != nil {
				continue // claims a kind and will not produce it: no bucket either
			}
			str = strings.ToLower(str)
			if !seenS[str] {
				seenS[str] = true
				strs = append(strs, str)
			}
			continue
		}
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
		if !seenR[f] {
			seenR[f] = true
			reals = append(reals, f)
		}
	}
	return ints, reals, strs, len(reals) == 0 && len(strs) == 0
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

// mixedKeyQuery is the constraint a batch stands for when the fast lookup cannot serve it:
// `member(attr, {...})` over the values themselves. It is the same shape LookupKeys compiles
// internally, built from an AST rather than text so the values become literals with no
// formatting round trip to disagree about.
func mixedKeyQuery(attr string, ints []int64, reals []float64, strs []string, where ast.Expr) *vm.Query {
	elems := make([]ast.Expr, 0, len(ints)+len(reals)+len(strs))
	for _, n := range ints {
		elems = append(elems, &ast.IntegerLiteral{Value: n})
	}
	for _, f := range reals {
		elems = append(elems, &ast.RealLiteral{Value: f})
	}
	for _, str := range strs {
		elems = append(elems, &ast.StringLiteral{Value: str})
	}
	member := &ast.FunctionCall{
		Name: "member",
		Args: []ast.Expr{ast.NewAttributeReference(attr, ast.NoScope), &ast.ListLiteral{Elements: elems}},
	}
	if where == nil {
		return vm.Compile(member)
	}
	return vm.Compile(&ast.BinaryOp{Op: "&&", Left: where, Right: member})
}

// stage builds the hash for ONE batch: the inner rows the batch's keys reach, bucketed the
// way buildStage buckets a whole side, so the walk above cannot tell the difference.
func (od *outerDriven) stage(p *joinPlan, batch []*joinRow, stats *JoinStats) (*joinStage, int, error) {
	st := &joinStage{step: od.step, hashed: true, buckets: map[string][]*joinRow{}}
	ints, reals, strs, fast := od.batchKeys(batch)
	keys := len(ints) + len(reals) + len(strs)
	if keys == 0 {
		return st, 0, nil // every key in the batch was undefined: nothing to look up
	}
	var ads iter.Seq[*classad.ClassAd]
	if fast {
		ads = od.arch.LookupKeysWhere(od.keyAttr, ints, od.where)
	} else {
		ads = od.arch.Query(mixedKeyQuery(od.keyAttr, ints, reals, strs, od.where))
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
