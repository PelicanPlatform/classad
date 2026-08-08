package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// BenchmarkAggregateMax measures an unconstrained MAX through the actual server paths: the
// wire-native projected scan (what it did before) against the columnar column pass. This is the
// honest comparison -- the scan baseline here is QueryProject, not a ClassAd-per-record decode.
func BenchmarkAggregateMax(b *testing.B) {
	const n = 50000
	// A small segment size so the ads SEAL: only sealed segments carry a columnar block, and the
	// default 8 MiB segment would swallow this fixture whole, leaving the "columnar" run measuring
	// the row fallback instead.
	d, err := db.OpenConfig(db.Config{Dir: b.TempDir(), SegmentSize: 1 << 18})
	if err != nil {
		b.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()

	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", i), fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nRequestMemory = %d\nQDate = %d",
			i/10, i%10000, ((i%16)+1)*512, 1700000000+i)); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		b.Fatal(err)
	}

	run := func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rows, err := c.Aggregate(ctx, "true", nil, []db.AggSpec{{Func: db.AggMax, Arg: "ProcId"}})
			if err != nil {
				b.Fatal(err)
			}
			if len(rows) != 1 {
				b.Fatalf("rows = %d", len(rows))
			}
		}
	}
	b.Run("projected_scan", run)

	// Drive read demand on ProcId so the schema's hot tier includes it -- that is what a
	// maintenance pass would do for a queried column, and a hot column reads with no decode
	// while a cold one decompresses its column group.
	for i := 0; i < 25; i++ {
		if _, err := c.Aggregate(ctx, "ProcId >= 0", nil, []db.AggSpec{{Func: db.AggCount, Arg: "*"}}); err != nil {
			b.Fatal(err)
		}
	}
	if !d.EnableSchemaScan(4000, 8) {
		b.Skip("no sealed segments to sample")
	}
	info := d.SchemaScanInfo()
	if info.CoveredSegments == 0 {
		b.Fatalf("no segment carries a columnar block (%d sealed): the run below would measure the "+
			"row fallback, not the accelerator", info.SealedSegments)
	}
	hot := false
	for _, f := range info.HotFields {
		if f == "ProcId" {
			hot = true
		}
	}
	b.Logf("accelerator: %d fields, %d/%d segments covered, ProcId hot: %v",
		info.SchemaFields, info.CoveredSegments, info.SealedSegments, hot)
	if ns, ok := d.NumStats("true", "ProcId"); !ok {
		b.Fatal("columnar aggregate declines ProcId; the benchmark below would just re-measure the scan")
	} else {
		b.Logf("columnar MAX(ProcId) = %v over N=%d", ns.Max, ns.N)
	}
	b.Run("columnar", run)
}
