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
	"strings"
)

// explainSide reports how the store would read one join side, as `.explain` would.
func (cat *Catalog) explainSide(side JoinSide) (QueryExplain, error) {
	// A side with no constraint of its own reads the whole table, which is what `true`
	// plans as. The empty string is not a constraint and does not parse, and the cost
	// model never asks about one -- it answers the whole-table size without coming here.
	// The join's EXPLAIN does ask, because "reads everything" is an access path a reader
	// wants described rather than a hole in the report.
	constraint := side.Constraint
	if strings.TrimSpace(constraint) == "" {
		constraint = "true"
	}
	if d, ok := cat.Table(side.Table); ok {
		return d.Explain(constraint)
	}
	if a, ok := cat.ArchiveTable(side.Table); ok {
		return a.Explain(constraint)
	}
	return QueryExplain{}, fmt.Errorf("no table named %q", side.Table)
}

// constrainedRows is how many records reading side visits, given that it carries a
// constraint of its own. why is non-empty when the read could not be priced, and is then
// phrased for a reader of the join's EXPLAIN.
//
// total is the whole table, which bounds the answer: the explain estimates from index
// statistics that count superseded versions, so on a table with churn its figure can exceed
// the records that are actually there.
func (cat *Catalog) constrainedRows(side JoinSide, total int) (rows int, why string) {
	ex, err := cat.explainSide(side)
	if err != nil {
		return 0, fmt.Sprintf("%s's own constraint could not be planned (%v), so the scan a "+
			"membership constraint would replace cannot be priced", side.Alias, err)
	}
	if ex.Plan == "" {
		// No plan was reported, so there is nothing to price. Declining is the whole
		// point: a number invented here would be decided with, and the decision would
		// read as an informed one in everything it reports.
		return 0, fmt.Sprintf("%s's own constraint produced no plan, so the scan a "+
			"membership constraint would replace cannot be sized", side.Alias)
	}
	return min(int(ex.EstVisited), total), ""
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
func (cat *Catalog) sideRows(side JoinSide, total int) (rows int, why string) {
	if strings.TrimSpace(side.Constraint) == "" {
		return total, ""
	}
	if forceUnpricedSides {
		return 0, "the hashed side carries its own constraint, which the row count does not describe"
	}
	return cat.constrainedRows(side, total)
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
