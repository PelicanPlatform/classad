package db

import (
	"testing"

	"github.com/PelicanPlatform/classad/collections"
)

// TestArchiveAggregateColsStats covers AggregateColsStats: it returns exactly what AggregateCols
// does, and fills stats only on the per-record fall-through -- the columnar fast path (a numeric
// schema field) serves the aggregate without reassembly and leaves the counts zero, which is the
// signal EXPLAIN ANALYZE uses to distinguish a fast aggregate from the slow one.
func TestArchiveAggregateColsStats(t *testing.T) {
	fast, _ := buildTopKArchive(t, 4000, true)  // columnarized: ClusterId is a covered numeric field
	slow, _ := buildTopKArchive(t, 4000, false) // not columnarized: every aggregate reassembles

	aggs := []AggSpec{{Func: AggMax, Arg: "ClusterId"}}

	want, err := fast.AggregateCols("true", nil, aggs)
	if err != nil {
		t.Fatalf("AggregateCols: %v", err)
	}
	if len(want) != 1 || len(want[0].Values) != 1 {
		t.Fatalf("want one row/value, got %+v", want)
	}

	// Fast path: MAX(ClusterId) served from the columnar accelerator -- same answer, and NO
	// per-record reassembly (the counts stay zero, so EXPLAIN ANALYZE shows no breakdown).
	var fs collections.ScanStats
	got, err := fast.AggregateColsStats("true", nil, aggs, &fs)
	if err != nil {
		t.Fatalf("AggregateColsStats (columnar): %v", err)
	}
	if got[0].Values[0] != want[0].Values[0] {
		t.Fatalf("columnar MAX = %s, want %s", got[0].Values[0], want[0].Values[0])
	}
	if fs.RecordsReassembled != 0 || fs.RecordsVisited != 0 {
		t.Errorf("columnar fast path reassembled records: %+v (should serve from columns, stats zero)", fs)
	}

	// Fall-through: the same aggregate on a non-columnarized archive reassembles every record it
	// visits -- same answer, but now the breakdown is populated (the slow shape EXPLAIN surfaces).
	var ss collections.ScanStats
	got2, err := slow.AggregateColsStats("true", nil, aggs, &ss)
	if err != nil {
		t.Fatalf("AggregateColsStats (fallback): %v", err)
	}
	if got2[0].Values[0] != want[0].Values[0] {
		t.Fatalf("fallback MAX = %s, want %s", got2[0].Values[0], want[0].Values[0])
	}
	if ss.RecordsVisited == 0 || ss.RecordsReassembled == 0 {
		t.Errorf("fall-through did not record the scan: %+v", ss)
	}
	if ss.RowsMatched != 4000 {
		t.Errorf("RowsMatched = %d, want 4000 (constraint true matches all)", ss.RowsMatched)
	}
}
