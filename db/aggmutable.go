package db

import (
	"strconv"

	"github.com/PelicanPlatform/classad/collections"
)

// The mutable-table aggregate ladder.
//
// This used to live in the RPC handler (dbrpc's opAggregate), which meant two consequences. An
// embedded, non-RPC caller of db got no aggregate planning whatsoever -- it could reach the
// projected scan by hand and nothing else, while the archive's identical ladder sat one type away.
// And a mutable aggregate could not report how it was answered, because the stats plumbing is a db
// concern and the fan-out was not here to thread it. One ladder per table kind, both in db, is the
// point of this file; dbrpc now only resolves the table, gates private attributes, and frames rows.

// AggregateColsStats runs a server-side GROUP BY over the table's matches: it applies the
// constraint, groups by the group-column tuple (a column may carry a bucket width, so a numeric
// attribute floors into fixed-width buckets -- the "per day" dimension), and reduces each group
// with COUNT/SUM/AVG/MIN/MAX. With no group columns it returns a single row over the whole match.
// It is the mutable-table counterpart of ArchiveTable.AggregateColsStats and shares its grouping
// and reduce engine (AggregateValues), so the same aggregate over a live table and over a history
// table answer identically rather than through two implementations that could drift.
//
// stats (may be nil) is filled with the scan's work breakdown for EXPLAIN ANALYZE. Only the
// fall-through projected scan -- the slow path taken when the aggregate is not one the columnar
// accelerator covers -- fills it; the Len/count/columnar fast paths do not reassemble records and
// leave stats zero (RecordsVisited == 0), which the caller reads as "served without a per-record
// scan". A nonzero, large RecordsReassembled here is exactly the signature of the slow aggregate.
//
// stop (may be nil) is polled per scanned row; returning true abandons the scan with whatever
// groups have accumulated, so a server can drop a scan whose client has gone away. It is a plain
// callback rather than a context because every other query entry point here takes none, and the
// one caller that cares already holds the connection's context.
func (db *DB) AggregateColsStats(constraint string, groupCols []GroupCol, aggs []AggSpec, stats *collections.ScanStats, stop func() bool) ([]AggRow, error) {
	return runAggLadder(db.aggLadder(stats, stop), AggRequest{Constraint: constraint, GroupCols: groupCols, Aggs: aggs})
}

// aggLadder is the mutable table's ordered list of aggregate strategies, the counterpart of
// ArchiveTable.aggLadder: each rung serves the whole request or declines to the next, and the
// projected scan at the end always answers. Built per call because its last rung closes over the
// caller's stats block and stop callback; the rungs above it read nothing but the request.
//
// It is deliberately NOT the archive's ladder with rungs removed. Three differences are real and
// preserved rather than reconciled:
//
//   - There is no match-none rung. The archive grew one when `where false` was measured costing a
//     full scan of a history table; a mutable table never had one, and giving it one here would
//     change what this ladder does rather than describe it.
//   - Columnar-aggregate precedes grouped-columns, where the archive has them the other way round.
//     The two are mutually exclusive -- one serves exactly zero group columns and the other
//     one or more -- so the order has no effect on any answer, and each ladder keeps the order it
//     was written in rather than being normalised to look like the other.
//   - No rung tags a tier. The archive tags one at every return because its strategies differ by
//     more than two orders of magnitude and a caller asked which ran; nothing asks a mutable
//     table that yet, and tagging here would add a wire-visible statement the server does not
//     make today.
//
// Only the last rung fills stats, and only because it is the only one that scans records; the
// rungs above it reassemble nothing and leave the counts zero, which is what the doc above reads
// as "served without a per-record scan".
func (db *DB) aggLadder(stats *collections.ScanStats, stop func() bool) []AggStrategy {
	return []AggStrategy{
		aggRung{"live-count", func(req AggRequest) ([]AggRow, bool, error) {
			// An unconstrained COUNT(*) with no grouping is the collection's live row count,
			// which it tracks in O(shards) via Len() -- no scan of every ad. This is the common
			// `SELECT COUNT(*) FROM <table>`. Only when the collection has no hidden structural
			// (parent-only) ads, which a match-all scan excludes but Len() counts (a chained
			// job_queue has them; a flat table like Startd does not). Mirrors the archive's index
			// rung, which reads the same shape from the archive's own O(1) tally.
			if !req.isPlainCount() || !IsMatchAll(req.Constraint) || db.Chained() {
				return nil, false, nil
			}
			return []AggRow{{Values: []string{strconv.Itoa(db.Len())}}}, true, nil
		}},
		aggRung{"count-constraint", func(req AggRequest) ([]AggRow, bool, error) {
			// A constrained COUNT(*) WHERE <numeric comparison on one int schema field> runs as
			// the adschema columnar scan when the table has schema-scan enabled and the predicate
			// is eligible. CountConstraint declines otherwise (schema-scan off, or a predicate the
			// columnar path can't handle), and we fall through to the projected scan. Each count
			// rung re-asks IsMatchAll rather than sharing the archive's per-ladder memo, because
			// the code this describes asked it twice here and the two answers are a scan apart.
			if !req.isPlainCount() || IsMatchAll(req.Constraint) {
				return nil, false, nil
			}
			n, ok := db.CountConstraint(req.Constraint)
			if !ok {
				return nil, false, nil
			}
			return []AggRow{{Values: []string{strconv.Itoa(n)}}}, true, nil
		}},
		aggRung{"columnar-aggregate", func(req AggRequest) ([]AggRow, bool, error) {
			// A single MIN/MAX/COUNT(attr) over a numeric column, read from the columnar blocks
			// rather than by decoding every record. Only counting was routed here before, so an
			// unconstrained MAX over a large table scanned row-wise. Declines unless the table has
			// schema-scan enabled and the aggregate is in scope (see ColumnarAggregate).
			rows, ok := ColumnarAggregate(func(attr string) (NumStats, bool) {
				return db.NumStats(req.Constraint, attr)
			}, req.GroupCols, req.Aggs)
			return rows, ok, nil
		}},
		aggRung{"grouped-columns", func(req AggRequest) ([]AggRow, bool, error) {
			// A GROUP BY one or more columns -- each numeric or string, each raw or bucketed --
			// with COUNT(*) and/or MIN/MAX/SUM/AVG/COUNT(attr), answered from the columnar blocks.
			// Every rung above is gated on there being NO grouping, so `select JobStatus, count(*)
			// from jobs group by JobStatus` -- a shape a dashboard asks constantly -- decoded every
			// matching record even on a table carrying the accelerator. Archive tables already
			// route this through GroupedFromColumns; this gives mutable tables the same path and
			// the same row-shaping rather than a second implementation. A bucketed group column is
			// what makes a time series ("per day") take this path rather than the scan below, and a
			// raw column beside a bucketed one is "per owner per day". Declines (and falls through)
			// for more group columns than the packed key holds, an aggregate over a non-numeric
			// column, a per-aggregate FILTER, COUNT(DISTINCT), a predicate not proven free of
			// CurrentTime, or a chained table -- the DB.GroupStats* wrappers refuse a chained table
			// because the columnar pass reads a record's own columns and knows nothing about the
			// parent it would have inherited the group or aggregate attribute from.
			rows, ok := GroupedFromColumns(db, req.Constraint, req.GroupCols, req.Aggs)
			return rows, ok, nil
		}},
		aggRung{"projected-scan", func(req AggRequest) ([]AggRow, bool, error) {
			// The reference, and the rung that must never decline. Project only the attributes the
			// aggregation reads (group columns + the non-"*" aggregate arguments), so the scan
			// reads them wire-native instead of fully decoding every matching ad.
			//
			// It can still answer PARTIALLY, which is the one place the ladder's no-partial rule
			// is honoured by the caller rather than here: when stop fires mid-scan the reduce
			// engine returns whatever groups had accumulated, with no error and no flag, and the
			// RPC layer covers for it by re-checking cancellation before framing anything. Left
			// exactly as it is -- making it decline would leave the ladder with no rung that
			// answers, which is a behaviour change dressed as a description.
			attrs, groupCol, aggCol := AggProjection(req.GroupCols, req.Aggs)
			seq, err := db.QueryProjectStats(req.Constraint, attrs, stats)
			if err != nil {
				return nil, false, err
			}
			rows, err := AggregateValues(seq, attrs, req.GroupCols, req.Aggs, groupCol, aggCol, stop)
			if err != nil {
				return nil, false, err
			}
			return rows, true, nil
		}},
	}
}
