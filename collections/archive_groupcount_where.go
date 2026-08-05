package collections

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Constrained index-resident GROUP BY counts.
//
// The unconstrained form (archive_groupcount.go) can attribute a whole segment's postings
// because every record in the segment is in the answer. With a WHERE clause that is only
// true for segments whose records ALL satisfy the constraint, so the question becomes: can
// we prove, from a segment's zone map alone, that its every record matches?
//
// The tempting shortcut is to reuse the probes a query decomposes into (the same ones
// zonePrune uses) and declare a segment fully matching when every probe is satisfied over
// its zone bounds. That is unsound. Probes are a conservative UNDER-approximation -- the
// conjuncts that must hold -- so "every probe is implied" does not mean "the constraint is
// implied". A disjunctive or negated constraint has probes that say much less than the
// expression does, and attributing a segment on that basis would over-count.
//
// So this path does not reason about probes at all. It re-reads the constraint's syntax and
// accepts only a shape it can characterize exactly: a conjunction of comparisons between a
// zone-mapped attribute and a numeric literal. For that shape the expression IS the
// conjunction of its conditions, so zone bounds that imply every condition imply the whole
// constraint. Every other shape -- a disjunction, a negation, a function call, a comparison
// between two attributes -- is refused, and the caller scans.

// rangeCond is one verified `attr op literal` condition from a constraint.
type rangeCond struct {
	attr string
	op   string
	val  float64
}

// parseConjunctiveRanges returns the conditions a constraint is exactly equivalent to, or
// ok=false if it is not a pure conjunction of numeric comparisons on plain attribute
// references. Being exact is the whole point: a caller may treat the returned conditions as
// a complete restatement of the constraint.
func parseConjunctiveRanges(constraint string) ([]rangeCond, bool) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, false
	}
	var out []rangeCond
	var walk func(e ast.Expr) bool
	walk = func(e ast.Expr) bool {
		switch n := e.(type) {
		case *ast.ParenExpr:
			return walk(n.Inner)
		case *ast.BinaryOp:
			if n.Op == "&&" {
				return walk(n.Left) && walk(n.Right)
			}
			c, ok := asRangeCond(n)
			if !ok {
				return false
			}
			out = append(out, c)
			return true
		default:
			return false
		}
	}
	if !walk(q.Expr()) || len(out) == 0 {
		return nil, false
	}
	return out, true
}

// asRangeCond matches `attr op number` (or the mirrored `number op attr`) for an ordering
// or equality operator, normalizing to attr-on-the-left form.
func asRangeCond(n *ast.BinaryOp) (rangeCond, bool) {
	switch n.Op {
	case "<", "<=", ">", ">=", "==":
	default:
		return rangeCond{}, false // includes !=, =?=, =!=, and every non-comparison
	}
	if a, ok := plainAttr(n.Left); ok {
		if v, ok := numericLiteral(n.Right); ok {
			return rangeCond{attr: a, op: n.Op, val: v}, true
		}
		return rangeCond{}, false
	}
	if a, ok := plainAttr(n.Right); ok {
		if v, ok := numericLiteral(n.Left); ok {
			return rangeCond{attr: a, op: mirrorOp(n.Op), val: v}, true
		}
	}
	return rangeCond{}, false
}

// plainAttr accepts an unscoped attribute reference. A scoped one (TARGET./PARENT.) does
// not refer to this record, so it is not something a zone map can decide.
func plainAttr(e ast.Expr) (string, bool) {
	if p, ok := e.(*ast.ParenExpr); ok {
		return plainAttr(p.Inner)
	}
	a, ok := e.(*ast.AttributeReference)
	if !ok || a.Scope != ast.NoScope {
		return "", false
	}
	return a.Name, true
}

func numericLiteral(e ast.Expr) (float64, bool) {
	if p, ok := e.(*ast.ParenExpr); ok {
		return numericLiteral(p.Inner)
	}
	switch l := e.(type) {
	case *ast.IntegerLiteral:
		return float64(l.Value), true
	case *ast.RealLiteral:
		return l.Value, true
	}
	return 0, false
}

func mirrorOp(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}
	return op // "=="
}

// alwaysHolds reports whether every value in [z.Min, z.Max] satisfies the condition.
func (c rangeCond) alwaysHolds(z zoneRange) bool {
	switch c.op {
	case ">":
		return z.Min > c.val
	case ">=":
		return z.Min >= c.val
	case "<":
		return z.Max < c.val
	case "<=":
		return z.Max <= c.val
	case "==":
		return z.Min == c.val && z.Max == c.val
	}
	return false
}

// neverHolds reports whether no value in [z.Min, z.Max] satisfies the condition, so the
// segment cannot contribute at all.
func (c rangeCond) neverHolds(z zoneRange) bool {
	switch c.op {
	case ">":
		return z.Max <= c.val
	case ">=":
		return z.Max < c.val
	case "<":
		return z.Min >= c.val
	case "<=":
		return z.Min > c.val
	case "==":
		return c.val < z.Min || c.val > z.Max
	}
	return false
}

func (c rangeCond) holds(v float64) bool {
	switch c.op {
	case ">":
		return v > c.val
	case ">=":
		return v >= c.val
	case "<":
		return v < c.val
	case "<=":
		return v <= c.val
	case "==":
		return v == c.val
	}
	return false
}

// CategoricalGroupCountsWhere is CategoricalGroupCounts restricted to the records matching
// constraint. ok is false unless the constraint is a pure conjunction of numeric
// comparisons against zone-mapped attributes AND the indexes fully account for every record
// (the same completeness bar as the unconstrained form) -- see the file comment for why the
// constraint's syntax is re-checked rather than its probes trusted.
//
// Segments are decided by their zone maps: one whose bounds imply every condition
// contributes its postings wholesale, one that cannot satisfy some condition contributes
// nothing, and only the genuinely straddling ones are read.
func (a *Archive) CategoricalGroupCountsWhere(attr, constraint string) (map[string]int64, bool) {
	return a.c.categoricalGroupCountsWhere(attr, constraint)
}

func (c *Collection) categoricalGroupCountsWhere(attr, constraint string) (map[string]int64, bool) {
	conds, ok := parseConjunctiveRanges(constraint)
	if !ok {
		return nil, false
	}
	spec := c.spec.Load()
	id, ok := c.categoricalAttrID(spec, attr)
	if !ok {
		return nil, false
	}
	// Every condition's attribute must be zone-mapped, or a segment cannot be decided
	// without reading it. Zone maps are keyed by interned id (a different id space from the
	// possibly-inline index spec).
	condIDs := make([]uint32, len(conds))
	for i, cond := range conds {
		zid, ok := c.intern.LookupID(cond.attr)
		if !ok {
			return nil, false
		}
		condIDs[i] = zid
	}

	counts := make(map[string]int64)
	var seen int64 // every record, matching or not -- the completeness check
	var dbuf []byte
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		ok := func() bool {
			defer releaseWindows(wins)
			for _, w := range wins {
				verdict := segmentVerdict(w, conds, condIDs)
				idx := w.seg.readIdx()
				covered := 0
				// The index can supply this segment's counts only when the verdict is
				// uniform over it -- every record in or every record out. A straddling
				// segment has to be read even though its zone maps were perfectly readable.
				if idx != nil && verdict != segStraddle {
					covered = min(int(idx.coveredUpto()), w.used)
					complete := idx.catCanonicalValues(id, func(v string) bool {
						n, ok := idx.catValueCount(id, v)
						if !ok {
							return false
						}
						seen += int64(n)
						if verdict == segAlways {
							counts[v] += int64(n)
						}
						return true
					})
					if !complete {
						return false
					}
				}
				if covered < w.used {
					// Straddling, unindexed, or an uncovered tail: read the records. A
					// segment already proven to hold nothing still has to be counted toward
					// the completeness check, but never contributes.
					n, ok := countAttrWhere(c, w, attr, conds, verdict == segNever, covered, w.used, &dbuf, counts)
					if !ok {
						return false
					}
					seen += n
				}
			}
			return true
		}()
		if !ok {
			return nil, false
		}
	}
	if seen != int64(c.Len()) {
		return nil, false
	}
	return counts, true
}

// segVerdict is what a segment's zone maps prove about the constraint over it.
type segVerdict int

const (
	// segStraddle: the zone bounds prove nothing either way, so the records must be read.
	// This is also the verdict when a condition's attribute has no zone map here.
	segStraddle segVerdict = iota
	// segAlways: every record in the segment satisfies every condition.
	segAlways
	// segNever: no record in the segment can satisfy some condition.
	segNever
)

// segmentVerdict decides a segment against the conditions from its zone maps alone.
//
// The distinction that matters is between "decidable" and "fully matching": a segment whose
// bounds straddle the range is perfectly decidable as *straddling*, and must then be read.
// Treating decidability as sufficient to attribute from the index silently drops every
// straddling segment's records.
func segmentVerdict(w segWindow, conds []rangeCond, condIDs []uint32) segVerdict {
	always := true
	for i, cond := range conds {
		z, ok := w.zones[condIDs[i]]
		if !ok {
			return segStraddle // undecidable without reading records
		}
		if cond.neverHolds(z) {
			return segNever
		}
		if !cond.alwaysHolds(z) {
			always = false
		}
	}
	if always {
		return segAlways
	}
	return segStraddle
}

// countAttrWhere counts the grouping attribute over records in [from, to) that satisfy the
// conditions, returning how many records it examined (matching or not). skip short-circuits
// a segment already proven to match nothing: its records still count toward completeness.
func countAttrWhere(c *Collection, w segWindow, attr string, conds []rangeCond, skip bool,
	from, to int, dbuf *[]byte, counts map[string]int64) (int64, bool) {
	var n int64
	for off := from; off < to; {
		o := uint32(off)
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		off += int(total)
		if isSystemKeyBytes(recKey(w.data, o)) {
			continue
		}
		raw, err := w.codec.Decompress((*dbuf)[:0], recAd(w.data, o))
		if err != nil {
			return 0, false
		}
		*dbuf = raw
		var val string
		gotVal := false
		matches := true
		seenCond := make([]bool, len(conds))
		wire.Ad(raw).ForEachNamed(c.intern, func(name string, node []byte) bool {
			if !gotVal && strings.EqualFold(name, attr) {
				if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitString {
					val, gotVal = lit.Str, true
				} else {
					matches = false
					return false
				}
			}
			for i, cond := range conds {
				if seenCond[i] || !strings.EqualFold(name, cond.attr) {
					continue
				}
				f, ok := literalFloat(node)
				if !ok {
					matches = false
					return false
				}
				seenCond[i] = true
				if !cond.holds(f) {
					matches = false
				}
			}
			return true
		})
		if !gotVal {
			return 0, false // cannot attribute this record to a group
		}
		for i := range conds {
			if !seenCond[i] {
				matches = false // the constraint references an attribute this record lacks
			}
		}
		n++
		if !skip && matches {
			counts[val]++
		}
	}
	return n, true
}
