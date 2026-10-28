package collections

import (
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Per-aggregate FILTER on the grouped columnar path.
//
// SQL's `COUNT(*) FILTER (WHERE JobStatus == 2)` restricts ONE aggregate to the rows of its group
// that satisfy a condition, so a single pass answers several differently-conditioned questions:
//
//	SELECT Owner, COUNT(*), COUNT(*) FILTER (WHERE JobStatus == 2) ... GROUP BY Owner
//
// Without it that is one query per condition. The grouped columnar path used to decline the whole
// request the moment any aggregate carried one, which sent a query the columns could nearly answer
// back to the per-record scan -- the same cliff the bucketed and multi-column forms each had before
// they were served, and the same two orders of magnitude.
//
// A filter narrows an AGGREGATE and never the GROUP. A group whose every row fails every filter
// still appears, with COUNT 0 and the other functions undefined, which is what SQL specifies and
// what the scanning reference computes. That is the whole reason a filter cannot be folded into the
// WHERE clause and has to be carried separately down here: the WHERE decides which rows exist, and
// the filter decides only which of them a given aggregate sees.
//
// The mechanism is one more narrowing plane per distinct filter per block, built by exactly the
// machinery the WHERE clause is built by (numPredsOnFields, then narrowByField). Reusing it is not
// only less code: the correctness argument for answering a filter from the columns IS the argument
// for answering the WHERE clause from them, so there is no second notion of "which records match an
// expression" that could drift from the first.
//
// Two differences from the WHERE planes are load-bearing:
//
//   - A filter never prunes a BLOCK or a SEGMENT. Its zone maps may prove that no record in a block
//     passes it, and that is used -- but only to zero that filter's plane, never to skip the block,
//     because the block's records still form groups and still feed every OTHER aggregate.
//   - A filter is compiled without the CurrentTime folding numPredsCT does. A folded predicate holds
//     only in a window proven free of CurrentTime, and the WHERE path can afford that bet because it
//     re-runs the whole query on an unproven window; a filter would have to unwind one plane out of
//     several. A filter reading CurrentTime therefore declines the query, which is the behaviour
//     anything this path cannot serve already has.
//
// What declines: any filter numPredsOnFields will not take -- a non-native expression, a disjunction,
// a comparison against an attribute rather than a literal, a string comparison, an attribute the
// collection-wide schema does not carry as a numeric field. The caller then scans, which is where it
// was before.

// groupFilters is a grouped query's compiled per-aggregate FILTERs, plus the per-block and
// per-record state the scan threads through them.
//
// Filters are DEDUPLICATED by text: `COUNT(*) FILTER (WHERE s == 2)` beside `AVG(x) FILTER (WHERE
// s == 2)` narrows one plane, not two. Two aggregates over the same column under DIFFERENT filters
// are different columns here, because each needs its own accumulator; the caller is the one that
// decides that, by handing a repeated attribute (see GroupStatsConstraintAggs).
type groupFilters struct {
	// One entry per distinct filter, in order of first appearance.
	preds   [][]fieldPred
	lookups [][]func(wire.Ad) ([]byte, bool) // the row fallback's attribute readers, per filter

	// colOf[j] is aggregate column j's filter index, or -1 when it is unfiltered. countOf[k] is
	// requested filtered record count k's filter index, which is never -1: an unfiltered count is
	// the group's own Count and is not asked for here.
	colOf   []int
	countOf []int

	// Per-block narrowing planes, one per filter, reused from block to block the way the WHERE
	// clause's single candSet is.
	cand []candSet
	// Per-segment field indices, one list per filter. Filled by resolveSeg.
	idxs [][]int
	// pass[f] is whether the record being accumulated right now passed filter f. One bool per
	// filter per record, written in place, so a filtered query allocates nothing per record.
	pass []bool
}

// compileGroupFilters compiles the filters a grouped query carries, or ok=false when any of them is
// a shape the columns cannot answer. colFilters is aligned with the aggregate columns ("" where the
// column is unfiltered); countFilters are the filters of aggregates that read no column at all, the
// `COUNT(*) FILTER (WHERE ...)` shape.
//
// It returns nil, true for a query with no filters at all, which is the shape every caller had
// before this existed: nil threads through every path below as "no filters" without a branch per
// record beyond the nil test the compiler hoists.
func (c *Collection) compileGroupFilters(s *adSchema, colFilters, countFilters []string) (*groupFilters, bool) {
	any := false
	for _, f := range colFilters {
		if f != "" {
			any = true
			break
		}
	}
	if !any && len(countFilters) == 0 {
		return nil, true
	}
	gf := &groupFilters{
		colOf:   make([]int, len(colFilters)),
		countOf: make([]int, len(countFilters)),
	}
	at := map[string]int{}
	intern := func(filter string) (int, bool) {
		if i, ok := at[filter]; ok {
			return i, true
		}
		q, err := vm.Parse(filter)
		if err != nil {
			return 0, false
		}
		// Deliberately numPredsOnFields and not numPredsCT: see the CurrentTime note above.
		preds, ok := c.numPredsOnFields(q, s)
		if !ok || len(preds) == 0 {
			// No predicates would mean a filter that constrains nothing, which this has no
			// shape for -- and which a caller should not have sent.
			return 0, false
		}
		i := len(gf.preds)
		at[filter] = i
		gf.preds = append(gf.preds, preds)
		lk := make([]func(wire.Ad) ([]byte, bool), len(preds))
		for j, p := range preds {
			lk[j] = c.attrLookup(p.fieldID)
		}
		gf.lookups = append(gf.lookups, lk)
		return i, true
	}
	for j, f := range colFilters {
		if f == "" {
			gf.colOf[j] = -1
			continue
		}
		i, ok := intern(f)
		if !ok {
			return nil, false
		}
		gf.colOf[j] = i
	}
	for k, f := range countFilters {
		if f == "" {
			return nil, false // an unfiltered count is the group's Count; nothing to compile
		}
		i, ok := intern(f)
		if !ok {
			return nil, false
		}
		gf.countOf[k] = i
	}
	gf.cand = make([]candSet, len(gf.preds))
	gf.idxs = make([][]int, len(gf.preds))
	gf.pass = make([]bool, len(gf.preds))
	return gf, true
}

// n is how many distinct filters the query carries, which sizes every groupAcc's row counts.
func (gf *groupFilters) n() int {
	if gf == nil {
		return 0
	}
	return len(gf.preds)
}

// resolveSeg maps every filter's predicated fields into this SEGMENT's schema. ok=false when one is
// absent, which sends the whole segment to the row fallback for the same reason a missing group
// column does: a filter that cannot be evaluated here would silently pass or fail every record.
func (gf *groupFilters) resolveSeg(cs *colSegment) bool {
	if gf == nil {
		return true
	}
	for i, preds := range gf.preds {
		idxs, ok := resolveFields(cs, preds)
		if !ok {
			return false
		}
		gf.idxs[i] = idxs
	}
	return true
}

// narrowBlock builds one plane per filter over the block's n records: the records that pass that
// filter, whether or not they also satisfy the query's WHERE clause (the scan only ever consults a
// plane for a record that already survived the WHERE).
//
// ok=false when a column will not narrow, which gives the query back to the scan exactly as the
// WHERE clause's own narrowing does.
func (gf *groupFilters) narrowBlock(blk *columnarBlock, bc *blockCache, sc *predScratch) bool {
	if gf == nil {
		return true
	}
	for i, preds := range gf.preds {
		cand := &gf.cand[i]
		// A filter's zone maps can prove no record in the block passes it. That empties the
		// PLANE and must never skip the block: its records still form groups and still feed the
		// unfiltered aggregates.
		if !blockMayMatch(blk, gf.idxs[i], preds) {
			cand.reset(blk.n)
			continue
		}
		cand.setAll(blk.n)
		for j := range preds {
			if cand.live == 0 {
				break
			}
			if !narrowByField(blk, gf.idxs[i][j], preds[j], bc, cand, sc) {
				return false
			}
		}
	}
	return true
}

// mark records which filters record k of the current block passed, for the accumulators to consult.
func (gf *groupFilters) mark(k int) {
	if gf == nil {
		return
	}
	for i := range gf.pass {
		gf.pass[i] = gf.cand[i].test(k)
	}
}

// markAd is mark for the row fallbacks, which have no columns to narrow and evaluate each filter's
// predicates against the record itself -- the same per-record form bruteGroupStats already uses for
// the WHERE clause, so the two agree on what a predicate means by construction.
//
// ok=false when a predicated attribute holds an expression, which only evaluation can resolve; the
// query then goes back to the scan, as it does when the WHERE clause meets one.
func (gf *groupFilters) markAd(ad wire.Ad) bool {
	if gf == nil {
		return true
	}
	for i, preds := range gf.preds {
		gf.pass[i] = true
		for j := range preds {
			node, found := gf.lookups[i][j](ad)
			if !found {
				gf.pass[i] = false
				break
			}
			if isExprNode(node) {
				return false
			}
			nv, isNum := nodeColVal(node)
			if !isNum || !preds[j].eval(nv.f) {
				gf.pass[i] = false
				break
			}
		}
	}
	return true
}

// keepsCol reports whether the record just marked feeds aggregate column j.
func (gf *groupFilters) keepsCol(j int) bool {
	if gf == nil {
		return true
	}
	f := gf.colOf[j]
	return f < 0 || gf.pass[f]
}

// countInto folds the record just marked into g's per-filter record counts, which is what a filtered
// COUNT(*) is counted in. It is called once per record that landed in a group -- never for one that
// fell in no bucket, which leaves the result entirely.
func (gf *groupFilters) countInto(g *groupAcc) {
	if gf == nil {
		return
	}
	for i, ok := range gf.pass {
		if ok {
			g.rows[i]++
		}
	}
}

// setAll fills the set with every record of a block of n, the starting point for a narrowing pass
// that is not also a visibility pass. The WHERE clause's own pass starts from setVisible instead,
// because it is the one deciding which records exist; a filter is consulted only for records that
// pass has already kept.
func (c *candSet) setAll(n int) {
	c.reset(n)
	if n == 0 {
		return
	}
	for i := range c.w {
		c.w[i] = ^uint64(0)
	}
	// Bits past n would resurrect records that are not there. candSet.and relies on the tail being
	// clear on both sides, so it is cleared here rather than tolerated.
	if r := n & 63; r != 0 {
		c.w[len(c.w)-1] = 1<<uint(r) - 1
	}
	c.live = n
}
