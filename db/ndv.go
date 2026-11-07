package db

import "github.com/PelicanPlatform/classad/collections"

// Distinct-value counts (NDV) at the catalog level -- the statistic a join's cost model
// needs and the only one it cannot derive from the sizes and selectivities it already has.
//
// Everything here is a read of index metadata already resident: no record is decoded and no
// posting is paged. That is the requirement, not an optimization. A planner runs per query,
// and a statistic that costs a scan to obtain would be paid for on every query to save work
// on some of them. If a future statistic needs a scan, the answer is to maintain it on the
// write path, not to compute it here.

// Accuracy classes, re-exported so a planner can branch on them without importing
// collections.
const (
	NDVExact     = collections.NDVExact
	NDVBound     = collections.NDVBound
	NDVEstimated = collections.NDVEstimated
)

// Error directions, re-exported alongside the classes.
const (
	NDVErrNone   = collections.NDVErrNone
	NDVErrLow    = collections.NDVErrLow
	NDVErrHigh   = collections.NDVErrHigh
	NDVErrEither = collections.NDVErrEither
)

// AttrNDV reports how many distinct values attr takes in this table, from the per-segment
// index summaries alone. ok is false when nothing indexes attr, which a caller must treat
// as "unknown" rather than as a small number.
func (db *DB) AttrNDV(attr string) (NDVEstimate, bool) { return db.c.AttrNDV(attr) }

// AttrNDV reports how many distinct values attr takes in this archive. See DB.AttrNDV.
func (t *ArchiveTable) AttrNDV(attr string) (NDVEstimate, bool) { return t.a.AttrNDV(attr) }

// AttrNDV reports one table's distinct-value count for attr, over either kind of table, so
// a join planner can ask about both sides the same way. ok is false when there is no such
// table or nothing indexes the attribute in it -- deliberately the same answer, because
// both mean the planner has no statistic and neither is a cardinality.
func (cat *Catalog) AttrNDV(table, attr string) (NDVEstimate, bool) {
	if d, ok := cat.Table(table); ok {
		return d.AttrNDV(attr)
	}
	if a, ok := cat.ArchiveTable(table); ok {
		return a.AttrNDV(attr)
	}
	return NDVEstimate{}, false
}
