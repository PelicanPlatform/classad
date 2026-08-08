package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// aggPair builds a client/server over a persistent db seeded with n job-shaped ads. Values are
// chosen so the columnar and scanning paths have something to disagree about: a large integer
// (where float formatting would show), a real column, and values that escape their slot width.
func aggPair(t *testing.T, n int) (*Client, *db.DB, func()) {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)

	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		qdate := 1700000000 + i // large enough that float notation would be visible
		// Huge is chosen so the column's integer SUM exceeds 2^53: rendering that from a float64
		// accumulator would round, and disagree with the reference's int64 sum.
		if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", i), fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nRequestMemory = %d\nQDate = %d\nWallClock = %d.5\nHuge = %d",
			i/10, i%10, ((i%16)+1)*512, qdate, i, 4000000000000+int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return c, d, func() { c.Close(); s.Close(); d.Close() }
}

// aggText runs one aggregate through the RPC and returns its single cell.
func aggText(t *testing.T, c *Client, fn db.AggFunc, attr, constraint string) string {
	t.Helper()
	rows, err := c.Aggregate(context.Background(), constraint, nil, []db.AggSpec{{Func: fn, Arg: attr}})
	if err != nil {
		t.Fatalf("%v(%s) where %q: %v", fn, attr, constraint, err)
	}
	if len(rows) != 1 || len(rows[0].Values) != 1 {
		t.Fatalf("%v(%s): rows = %+v, want one value", fn, attr, rows)
	}
	return rows[0].Values[0]
}

// TestColumnarAggregateMatchesScan is the test this feature lives or dies by: with the
// accelerator on, every aggregate must return the EXACT SAME TEXT as with it off. A numeric
// difference would be a bug; a formatting difference (1.7e+09 for 1700000015) would be just as
// wrong and far easier to ship unnoticed.
func TestColumnarAggregateMatchesScan(t *testing.T) {
	c, d, cleanup := aggPair(t, 3000)
	defer cleanup()

	cases := []struct {
		fn         db.AggFunc
		attr       string
		constraint string
	}{
		{db.AggMax, "ProcId", "true"},
		{db.AggMin, "ProcId", "true"},
		{db.AggMax, "QDate", "true"}, // large ints: float notation would show here
		{db.AggMin, "QDate", "true"},
		{db.AggMax, "WallClock", "true"}, // a real column
		{db.AggMin, "WallClock", "true"},
		{db.AggCount, "ProcId", "true"},
		{db.AggMax, "RequestMemory", "RequestMemory < 4096"}, // same-field predicate
		{db.AggMin, "RequestMemory", "RequestMemory >= 4096"},
		{db.AggMax, "RequestMemory", "RequestMemory > 99999999"}, // matches nothing
		{db.AggSum, "ProcId", "true"},
		{db.AggSum, "Huge", "true"},      // integer sum past 2^53: float rounding would show
		{db.AggSum, "WallClock", "true"}, // a real column: promotes to a real
		{db.AggAvg, "ProcId", "true"},
		{db.AggAvg, "WallClock", "true"},
		{db.AggSum, "RequestMemory", "RequestMemory > 99999999"}, // empty SUM
		{db.AggAvg, "RequestMemory", "RequestMemory > 99999999"}, // empty AVG
	}

	// Scanning answers first: schema-scan is not enabled yet, so these cannot take the fast path.
	if d.SchemaScanInfo().Enabled {
		t.Fatal("fixture already has the accelerator enabled; the baseline would not be the scan")
	}
	want := make([]string, len(cases))
	for i, tc := range cases {
		want[i] = aggText(t, c, tc.fn, tc.attr, tc.constraint)
	}

	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to sample in this fixture")
	}
	if !d.SchemaScanInfo().Enabled {
		t.Fatal("accelerator did not enable")
	}
	for i, tc := range cases {
		// Prove the fast path actually serves this case. Without it, a case the columnar path
		// quietly declined would compare the scan against itself and pass for the wrong reason --
		// the same trap that made an early version of BenchmarkAggregateMax measure nothing.
		if _, ok := db.ColumnarAggregate(func(attr string) (db.NumStats, bool) {
			return d.NumStats(tc.constraint, attr)
		}, nil, []db.AggSpec{{Func: tc.fn, Arg: tc.attr}}); !ok {
			t.Errorf("%v(%s) where %q: the columnar path declines it, so the comparison below is "+
				"scan-vs-scan and proves nothing", tc.fn, tc.attr, tc.constraint)
			continue
		}
		got := aggText(t, c, tc.fn, tc.attr, tc.constraint)
		if got != want[i] {
			t.Errorf("%v(%s) where %q: accelerated %q, scanned %q",
				tc.fn, tc.attr, tc.constraint, got, want[i])
		}
	}
}

// TestColumnarAggregateDeclinesFilter guards the one case where answering from the columnar pass
// would be actively wrong rather than just unavailable: a per-aggregate FILTER the pass knows
// nothing about. The answer must be the filtered one, not the unfiltered total.
func TestColumnarAggregateDeclinesFilter(t *testing.T) {
	c, d, cleanup := aggPair(t, 3000)
	defer cleanup()
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to sample")
	}
	ctx := context.Background()

	rows, err := c.Aggregate(ctx, "true", nil, []db.AggSpec{
		{Func: db.AggMax, Arg: "ProcId", Filter: "ProcId < 5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Values) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	if got := rows[0].Values[0]; got != "4" {
		t.Errorf("MAX(ProcId) FILTER (ProcId < 5) = %q, want 4 -- the unfiltered answer is 9, so a "+
			"columnar answer that ignored the filter would show up as 9", got)
	}
}

// TestColumnarAggregateGroupedStillScans pins that grouping is out of scope: the fast path serves
// one ungrouped aggregate, and a GROUP BY must keep producing its groups.
func TestColumnarAggregateGroupedStillScans(t *testing.T) {
	c, d, cleanup := aggPair(t, 1000)
	defer cleanup()
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to sample")
	}
	rows, err := c.Aggregate(context.Background(), "true",
		[]string{"ProcId"}, []db.AggSpec{{Func: db.AggMax, Arg: "RequestMemory"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 10 { // ProcId 0..9
		t.Errorf("grouped MAX returned %d rows, want 10", len(rows))
	}
}
