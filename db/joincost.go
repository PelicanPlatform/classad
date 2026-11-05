package db

// Pricing what a join side's read costs, in records.
//
// Every cost the join compares is denominated in records touched: an index lookup is worth
// probeLookupCost of them, and a scan is worth one per record it visits. That only works if
// the number of records a side's read visits is known, and until now it was approximated by
// the table's whole size -- true for a side read whole, and wrong by as much as the
// constraint is selective for a side that carries one.
//
// Rather than guess, the probe decision DECLINED any side with a constraint. That was the
// honest answer to not knowing, and it declined the common shape: a history join almost
// always narrows one side by owner or date. This closes that hole by asking the store what
// the read would do, which is a question the store already answers for EXPLAIN and which
// nothing used to plan with.

import (
	"fmt"
	"math"
	"strings"
)

// explainSide reports how the store would read one join side, as `.explain` would.
func (cat *Catalog) explainSide(side JoinSide) (QueryExplain, error) {
	if d, ok := cat.Table(side.Table); ok {
		return d.Explain(side.Constraint)
	}
	if a, ok := cat.ArchiveTable(side.Table); ok {
		return a.Explain(side.Constraint)
	}
	return QueryExplain{}, fmt.Errorf("no table named %q", side.Table)
}

// readVisits converts an explained plan into the number of records reading it visits.
//
// VISITS, not matches. A read walks what its access path hands it and re-verifies the whole
// predicate on each record, so this counts the records touched -- which is the cost -- and
// overstates the rows that come back by however much of the predicate no index covers.
//
// The quantity is assembled from the plan rather than read off one field, because the field
// that looks like the answer is not it. EstCandidates is what the INDEX hands the scan: it is
// zero for a plan that scans, and on an indexed plan it leaves out the segments no index
// covers, which are walked in full. Taking it at face value would price a full scan at
// nothing, which is the most expensive side a join can have reported as the cheapest.
//
// total and segments are the table's own size and segment count, which the explain does not
// carry for every plan and the caller already holds.
//
// THE OPEN SEGMENT IS NOT COUNTED. A read walks the still-appended tail in full, and the
// tail appears in neither the explain's segment classes nor the table's segment count -- it
// is sealed segments that are classified -- so this undercounts by whatever the tail holds,
// additively and whatever the constraint. The error is bounded by one open segment per
// shard, which is a rounding error on a table big enough for the question to arise, and it
// is an UNDERestimate: it prices the scan low, so it biases every decision made from it
// towards reading the side rather than reaching for an index, which is the direction the
// probe cost model already errs in on purpose.
func readVisits(ex QueryExplain, total, segments int) (records int, ok bool) {
	switch ex.Plan {
	case "empty":
		// The indexed conjuncts contradict each other: nothing is read at all.
		return 0, true

	case "indexed", "indexed-union":
		v := float64(ex.EstCandidates)
		// Segments no index covers are walked whole and are not in EstCandidates. Their
		// share of the records is apportioned by segment count, for want of a per-segment
		// record count; segments of one table hold comparable numbers of records, so the
		// error is in the spread of segment fill rather than in the estimate's order.
		if classed := ex.IndexSegments + ex.IndexSkippedSegments + ex.UnindexedSegments +
			ex.ZonePrunedSegments; ex.UnindexedSegments > 0 && classed > 0 {
			v += float64(total) * float64(ex.UnindexedSegments) / float64(classed)
		}
		return min(int(math.Round(v)), total), true

	case "parallel-scan", "serial-scan":
		// A scan reads the table, less whatever the zone maps rule out before any record is
		// read. The explain counts those segments for a scan but classifies no others, so
		// the caller's segment count is the denominator.
		v := float64(total)
		if ex.ZonePrunedSegments > 0 && segments > 0 {
			v *= 1 - math.Min(1, float64(ex.ZonePrunedSegments)/float64(segments))
		}
		return int(math.Round(v)), true
	}
	// An unrecognized plan string. Reporting a number here would be the planner inventing
	// one, and every caller's next move is to decide something with it.
	return 0, false
}

// constrainedRows is how many records reading side visits, given that it carries a
// constraint of its own. why is non-empty when the read could not be priced, and is then
// phrased for a reader of the join's EXPLAIN.
//
// total and segments describe the whole table, which is what the read costs when the
// constraint turns out to narrow nothing.
func (cat *Catalog) constrainedRows(side JoinSide, total, segments int) (rows int, why string) {
	ex, err := cat.explainSide(side)
	if err != nil {
		return 0, fmt.Sprintf("%s's own constraint could not be planned (%v), so the scan a "+
			"membership constraint would replace cannot be priced", side.Alias, err)
	}
	r, ok := readVisits(ex, total, segments)
	if !ok {
		return 0, fmt.Sprintf("%s's own constraint plans as %q, which this does not know how "+
			"to price, so the scan a membership constraint would replace cannot be sized",
			side.Alias, ex.Plan)
	}
	return r, ""
}

// forceUnpricedSides restores the behaviour this file replaced: a side carrying a constraint
// of its own is declined rather than priced. It exists so a benchmark can measure both
// answers in one process, which is the only way to compare them without comparing two
// machines or two moments.
//
// It is a plain variable rather than a parameter because nothing outside a test may set it,
// and a test that does must not call t.Parallel -- Go runs parallel tests only after the
// serial ones finish, which is what keeps this from needing synchronisation.
var forceUnpricedSides bool

// sideRows is the record count the probe decision prices a side's scan at: the whole table
// when it is read whole, and what its own constraint narrows the read to when it has one.
//
// The constrained case costs an EXPLAIN of the side -- it reads every segment's index stats
// and no records. That is far below the read it describes, and it is paid once per side when
// a join is planned, not per row.
func (cat *Catalog) sideRows(side JoinSide, total, segments int) (rows int, why string) {
	if strings.TrimSpace(side.Constraint) == "" {
		return total, ""
	}
	if forceUnpricedSides {
		return 0, "the hashed side carries its own constraint, which the row count does not describe"
	}
	return cat.constrainedRows(side, total, segments)
}

// andConstraints conjoins two constraint sources, either of which may be empty. cons is
// already parenthesized by its builder; own is parenthesized here because it is the caller's
// text and may be a bare disjunction, which would otherwise bind looser than the &&.
func andConstraints(own, cons string) string {
	if strings.TrimSpace(own) == "" {
		return cons
	}
	if strings.TrimSpace(cons) == "" {
		return own
	}
	return "(" + own + ") && " + cons
}
