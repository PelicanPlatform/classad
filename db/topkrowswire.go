package db

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// TOP-K ROWS PER GROUP, KEEPING THE ROWS IN WIRE FORM.
//
// TopKRowsPerGroup is the reference: it projects to classad.Values and ranks those. That is the
// right shape for a caller already holding values, and the wrong one for the caller this exists
// for -- a server about to put the rows on a connection. There the values are built only to be
// re-encoded, and the far end decodes them again, which is the cost opJoinWire measured and
// removed on the join (350,978 allocations rendering, 536,132 parsing, together about twice what
// the join itself cost).
//
// So this rung ranks from the WIRE BYTES and keeps them: the order value is read out of its node
// without building a value, the group key is assembled into one reused buffer, and a kept row is
// the scan's own bytes copied once. The ROWS -- the wide part, and the part that would be
// rendered and reparsed -- are never decoded at all.
//
// THE GROUP KEY IS THE REFERENCE'S RELATION, NOT THE NODE BYTES. Keying on the raw nodes was the
// obvious thing and it is wrong: GroupValueText puts the integer 2 and the real 2.0 in ONE group
// (see groupListText, which says so), and their nodes differ. So the key is built with
// AppendGroupValueText, the same rendering the reference groups by, appended in place. A string
// column -- the common one -- goes straight from the node's bytes to the quoted form without a Go
// string in between, so the common case still allocates nothing per record.
//
// AN EXPRESSION IS EVALUATED, because the reference's projection hands it an evaluated value.
// A job ad's QDate may be stored as an expression over its siblings, and a rung that read the
// node and gave up would silently drop exactly those rows. The scan is therefore the
// reference-chasing one, so a row carries what its expressions refer to and can evaluate
// self-contained; a row is decoded only when one of ITS group or order nodes is an expression.
//
// WHAT IT COSTS TO KEEP BYTES. ScanRawWire yields a row aliasing a buffer it reuses, so a row the
// keep retains must be copied -- bounded by K per group, which is the whole point: the reference
// and this one both copy what they keep, and neither copies what they do not.
//
// THE ROWS CARRY MORE THAN Attrs. The reference cuts each row back to the caller's attributes,
// because it holds a value slice and can. A wire row cannot be narrowed without reassembling it,
// which is the work this is avoiding, so a row here carries the group columns and the order
// attribute as well when the caller did not ask for them. That is the projection contract
// (AGENTS.md: a server may return more than the projection asked for, and callers tolerate it),
// and a caller wanting exactly its own attributes trims at the edge where it renders.

// TopKRowsWireGroup is one group's answer with its rows left in wire form.
//
// Values names the group exactly as TopKRowsGroup.Values does. Rows are self-contained wire ads,
// best first, each owned by the caller.
type TopKRowsWireGroup struct {
	Values []classad.Value
	Rows   [][]byte
}

// topKWireKeep is topKRowKeep over wire rows. The eviction rule is topKWorse, shared with the
// reference so the two rungs cannot disagree about which row is better.
//
// An evicted slot's buffer is reused for the row replacing it: a group at capacity in a long
// scan would otherwise allocate once per improvement, and improvements are common when the scan
// order correlates with the order attribute -- which for a job table ordered by QDate it does.
type topKWireKeep struct {
	// vals holds every kept row's ordering values end to end, stride terms long, exactly as
	// topKRowKeep does.
	vals  []NumValue
	rows  [][]byte
	k     int
	terms []TopKOrderTerm
}

func (t *topKWireKeep) stride() int { return len(t.terms) }

func (t *topKWireKeep) at(i int) []NumValue {
	n := t.stride()
	return t.vals[i*n : (i+1)*n]
}

func (t *topKWireKeep) count() int { return len(t.vals) / t.stride() }

func (t *topKWireKeep) offer(vals []NumValue, row []byte) {
	for _, v := range vals {
		if v.Kind == NumReal && math.IsNaN(v.F) {
			return // never evicted, so it would hold a slot against every real row
		}
	}
	if t.count() < t.k {
		t.vals = append(t.vals, vals...)
		t.rows = append(t.rows, append([]byte(nil), row...))
		return
	}
	wi := t.worstIdx()
	if topKWorseRow(t.at(wi), vals, t.terms) {
		copy(t.at(wi), vals)
		t.rows[wi] = append(t.rows[wi][:0], row...)
	}
}

func (t *topKWireKeep) worstIdx() int {
	wi := 0
	for i := 1; i < t.count(); i++ {
		if topKWorseRow(t.at(i), t.at(wi), t.terms) {
			wi = i
		}
	}
	return wi
}

// ordered returns the kept rows best first, by the same comparison and the same stable sort the
// reference uses, so two rows tied on the order attribute come back in the same relative order
// from both rungs.
func (t *topKWireKeep) ordered() [][]byte {
	idx := make([]int, len(t.rows))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return topKWorseRow(t.at(idx[b]), t.at(idx[a]), t.terms)
	})
	out := make([][]byte, len(idx))
	for i, j := range idx {
		out[i] = t.rows[j]
	}
	return out
}

// numValueOfNode reads a node's numeric value without building a classad.Value.
//
// It is numValueOf's wire-side twin and must agree with it: a row this rejects is a row the
// reference drops, and a difference between them is a row that ranks on one rung and vanishes on
// the other. wire.NumberValue accepts the same int/real/bool set, through the same unary-minus
// folding the evaluator applies.
func numValueOfNode(node []byte) (NumValue, bool) {
	if lit, ok := wire.NumberValue(node); ok {
		switch lit.Kind {
		case wire.LitInt:
			return NumValue{F: float64(lit.Int), I: lit.Int, Kind: NumInt}, true
		case wire.LitReal:
			return NumValue{F: lit.Real, I: int64(lit.Real), Kind: NumReal}, true
		}
		return NumValue{}, false
	}
	// A BOOLEAN RANKS. numValueOf takes one (false below true), so the reference ranks a
	// boolean order column and this must too -- but wire.NumberValue reads only ints and
	// reals, so asking it alone drops every such row, and drops them as a GROUP rather than
	// as an error. The string test first keeps LiteralValue off the one node kind whose
	// decoding would allocate.
	if _, isStr := wire.StringLiteralValue(node); isStr {
		return NumValue{}, false
	}
	if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitBool {
		if lit.Bool {
			return NumValue{F: 1, I: 1, Kind: NumBool}, true
		}
		return NumValue{F: 0, I: 0, Kind: NumBool}, true
	}
	return NumValue{}, false
}

// TopKRowsPerGroupWire is TopKRowsPerGroup with the rows left in wire form. See the note above
// for what that changes: nothing about which rows are chosen or their order, and two things about
// what comes back -- the rows are bytes, and they carry the group and order attributes whether or
// not the caller's Attrs named them.
func (db *DB) TopKRowsPerGroupWire(constraint string, spec TopKRowsSpec) ([]TopKRowsWireGroup, error) {
	return db.topKRowsPerGroupWire(constraint, spec, nil)
}

// TopKRowsPerGroupWireStats is TopKRowsPerGroupWire filling stats (may be nil) with the scan's work.
func (db *DB) TopKRowsPerGroupWireStats(constraint string, spec TopKRowsSpec, stats *collections.ScanStats) ([]TopKRowsWireGroup, error) {
	return db.topKRowsPerGroupWire(constraint, spec, stats)
}

func (db *DB) topKRowsPerGroupWire(constraint string, spec TopKRowsSpec, stats *collections.ScanStats) ([]TopKRowsWireGroup, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	// A bucketed group column needs the value, not the node, to round -- and rounding is
	// exactly the step that makes two different nodes one group, which byte keying cannot do.
	// Refuse rather than group wrongly; the reference serves this case.
	for _, g := range spec.GroupCols {
		if g.BucketWidth > 0 {
			return nil, fmt.Errorf("classad-db: top-k wire rows cannot bucket a group column (%s); use TopKRowsPerGroup", g.Attr)
		}
	}

	// The projection is the caller's attributes plus the group columns and the order attribute,
	// which have to be IN the row because that is where this rung reads them from.
	attrs := append([]string(nil), spec.Attrs...)
	have := map[string]bool{}
	for _, a := range attrs {
		have[strings.ToLower(a)] = true
	}
	need := func(a string) {
		if !have[strings.ToLower(a)] {
			attrs = append(attrs, a)
			have[strings.ToLower(a)] = true
		}
	}
	for _, g := range spec.GroupCols {
		need(g.Attr)
	}
	for _, t := range spec.OrderBy {
		need(t.Attr)
	}

	seq, err := db.QueryRawWireRefsStats(constraint, attrs, false, stats)
	if err != nil {
		return nil, err
	}

	type groupEntry struct {
		values []classad.Value
		keep   *topKWireKeep
	}
	at := map[string]int{}
	var groups []*groupEntry
	var key []byte
	var nodes [][]byte
	if n := len(spec.GroupCols); n > 0 {
		nodes = make([][]byte, n)
	}
	ovals := make([]NumValue, len(spec.OrderBy)) // reused: the keep copies what it keeps
	for row := range seq {
		r := wireRow{ad: wire.Ad(row)}
		ranked := true
		for i, t := range spec.OrderBy {
			onode, found := r.ad.LookupByName(t.Attr)
			if !found {
				ranked = false
				break
			}
			ov, ok := numValueOfNode(onode)
			if !ok {
				// Not a literal number. Either it is an expression over the row's
				// siblings -- which the reference evaluates, so this must too -- or it
				// is genuinely unrankable, which numValueOf will say in the same breath.
				if wire.IsLiteralNode(onode) {
					ranked = false // a literal, and not a number: nothing to rank it by
					break
				}
				if ov, ok = numValueOf(r.eval(t.Attr)); !ok {
					ranked = false
					break
				}
			}
			ovals[i] = ov
		}
		if !ranked {
			continue
		}
		// Length-delimited so no concatenation of one group's values can spell another's:
		// without it the pair ("a","bc") and the pair ("ab","c") share a key.
		key = key[:0]
		for i, g := range spec.GroupCols {
			// An ABSENT group attribute is undefined, and undefined is a group -- which is
			// what the reference does, because its projection yields undefined for a missing
			// attribute and undefined renders like any other group value. Dropping the row
			// instead loses a whole group, silently, and only for rows nothing names.
			node, found := r.ad.LookupByName(g.Attr)
			if !found {
				node = nil
			}
			nodes[i] = node
			mark := len(key)
			key = append(key, 0, 0, 0, 0)
			key = r.appendGroupText(key, node, g.Attr)
			putLen32(key[mark:], len(key)-mark-4)
		}
		i, seen := at[string(key)] // keyed on the bytes: the compiler elides the copy
		if !seen {
			i = len(groups)
			at[string(key)] = i
			// Once per GROUP, not per record: the values that NAME the group, built the way
			// the reference builds them so the two answers are comparable cell for cell.
			values := make([]classad.Value, len(spec.GroupCols))
			for j, node := range nodes {
				values[j] = r.value(node, spec.GroupCols[j].Attr)
			}
			groups = append(groups, &groupEntry{
				values: values,
				keep:   &topKWireKeep{k: spec.K, terms: spec.OrderBy},
			})
		}
		groups[i].keep.offer(ovals, row)
	}

	out := make([]TopKRowsWireGroup, len(groups))
	for i, g := range groups {
		out[i] = TopKRowsWireGroup{Values: g.values, Rows: g.keep.ordered()}
	}
	return out, nil
}

func putLen32(dst []byte, n int) {
	l := uint32(n)
	dst[0], dst[1], dst[2], dst[3] = byte(l), byte(l>>8), byte(l>>16), byte(l>>24)
}

// wireRow reads one scanned row's attributes, decoding the row ONLY if something in it has to be
// evaluated -- and then at most once, however many of its attributes are expressions.
type wireRow struct {
	ad      wire.Ad
	decoded *classad.ClassAd
	failed  bool
}

func (r *wireRow) eval(name string) classad.Value {
	if r.decoded == nil && !r.failed {
		a, err := wire.DecodeInline(r.ad)
		if err != nil {
			r.failed = true
		} else {
			r.decoded = classad.FromAST(a)
		}
	}
	if r.decoded == nil {
		return classad.NewUndefinedValue()
	}
	return r.decoded.EvaluateAttr(name)
}

// value is the node's value: the literal it holds, or what it evaluates to.
func (r *wireRow) value(node []byte, name string) classad.Value {
	if node == nil {
		return classad.NewUndefinedValue() // absent, which is a value and a group
	}
	if lit, ok := wire.LiteralValue(node); ok {
		return litValue(lit)
	}
	return r.eval(name)
}

// appendGroupText appends the node's group rendering, which must be the one GroupValueText would
// produce for the same value -- that relation is what decides which rows share a group.
//
// A STRING goes straight from the node's bytes to the quoted form. That is the hot case (a group
// column is usually an owner, a site, a status) and it is the one where going through a value
// would allocate: wire.LiteralValue copies the bytes into a Go string, which the key would then
// copy again. ast.AppendQuoteStringBytes writes the same text Value.String() does, which is what
// GroupValueText returns for a string.
func (r *wireRow) appendGroupText(dst, node []byte, name string) []byte {
	if node == nil {
		return AppendGroupValueText(dst, classad.NewUndefinedValue())
	}
	if b, ok := wire.StringLiteralValue(node); ok {
		return ast.AppendQuoteStringBytes(dst, b)
	}
	return AppendGroupValueText(dst, r.value(node, name))
}
