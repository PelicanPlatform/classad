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
	// Fast path: an unconstrained COUNT(*) with no grouping is the collection's live row
	// count, which it tracks in O(shards) via Len() -- no scan of every ad. This is the common
	// `SELECT COUNT(*) FROM <table>`. Only when the collection has no hidden structural
	// (parent-only) ads, which a match-all scan excludes but Len() counts (a chained job_queue
	// has them; a flat table like Startd does not). Mirrors ArchiveTable.Aggregate.
	if len(groupCols) == 0 && len(aggs) == 1 && aggs[0].Func == AggCount && aggs[0].Arg == "*" &&
		aggs[0].Filter == "" && IsMatchAll(constraint) && !db.Chained() {
		return []AggRow{{Values: []string{strconv.Itoa(db.Len())}}}, nil
	}

	// Fast path: a constrained COUNT(*) WHERE <numeric comparison on one int schema field> runs
	// as the adschema columnar scan when the table has schema-scan enabled and the predicate is
	// eligible. CountConstraint returns ok=false otherwise (schema-scan off, or a predicate the
	// columnar path can't handle), and we fall through to the projected scan.
	if len(groupCols) == 0 && len(aggs) == 1 && aggs[0].Func == AggCount && aggs[0].Arg == "*" &&
		aggs[0].Filter == "" && !IsMatchAll(constraint) {
		if n, ok := db.CountConstraint(constraint); ok {
			return []AggRow{{Values: []string{strconv.Itoa(n)}}}, nil
		}
	}

	// Fast path: a single MIN/MAX/COUNT(attr) over a numeric column, read from the columnar
	// blocks rather than by decoding every record. Only counting was routed here before, so an
	// unconstrained MAX over a large table scanned row-wise. Declines unless the table has
	// schema-scan enabled and the aggregate is in scope (see ColumnarAggregate).
	if rows, ok := ColumnarAggregate(func(attr string) (NumStats, bool) {
		return db.NumStats(constraint, attr)
	}, groupCols, aggs); ok {
		return rows, nil
	}

	// Fast path: a GROUP BY one column -- numeric or string, raw or bucketed -- with COUNT(*) and/or
	// MIN/MAX/SUM/AVG/COUNT(attr), answered from the columnar blocks. Every fast path above is gated on
	// there being NO grouping, so `select JobStatus, count(*) from jobs group by JobStatus` -- a shape a
	// dashboard asks constantly -- decoded every matching record even on a table carrying the
	// accelerator. Archive tables already route this through GroupedFromColumns; this gives mutable
	// tables the same path and the same row-shaping rather than a second implementation. A bucketed
	// group column is what makes a time series ("per day") take this path rather than the scan below.
	// Declines (and falls through) for more than one group column, an aggregate over a non-numeric
	// column, a per-aggregate FILTER, COUNT(DISTINCT), or a chained table -- the DB.GroupStats*
	// wrappers refuse a chained table because the columnar pass reads a record's own columns and
	// knows nothing about the parent it would have inherited the group or aggregate attribute from.
	if rows, ok := GroupedFromColumns(db, constraint, groupCols, aggs); ok {
		return rows, nil
	}

	// Project only the attributes the aggregation reads (group columns + the non-"*" aggregate
	// arguments), so the scan reads them wire-native instead of fully decoding every matching ad.
	attrs, groupCol, aggCol := AggProjection(groupCols, aggs)

	seq, err := db.QueryProjectStats(constraint, attrs, stats)
	if err != nil {
		return nil, err
	}
	return AggregateValues(seq, attrs, groupCols, aggs, groupCol, aggCol, stop)
}
