package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// wideHistoryAd is a history ad of realistic WIDTH. A real condor history record carries ~100
// attributes; the earlier benchmarks used 5-6, which understates every cost that scales with the
// record rather than with the one attribute being aggregated -- decompression above all.
func wideHistoryAd(i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\nJobStatus = %d\n",
		i/10, i%10000, i%64, 3+i%2)
	fmt.Fprintf(&b, "RequestMemory = %d\nRequestCpus = %d\nCompletionDate = %d\nQDate = %d\n",
		((i%16)+1)*512, 1+i%8, 1700000000+i, 1699000000+i)
	fmt.Fprintf(&b, "Cmd = \"/home/u%d/analysis/run_%d.sh\"\nIwd = \"/home/u%d/work/job%d\"\n",
		i%64, i%97, i%64, i)
	fmt.Fprintf(&b, "RemoteHost = \"slot%d@node%04d.chtc.wisc.edu\"\nGlobalJobId = \"ap43#%d.%d#%d\"\n",
		1+i%16, i%512, i/10, i%10, 1700000000+i)
	// Filler attributes of the kind history ads actually carry: environment, requirements,
	// resource usage, provisioning detail. Width is the point.
	for k := 0; k < 80; k++ {
		fmt.Fprintf(&b, "Attr%02d = \"value-%d-%d-padding-to-realistic-length\"\n", k, i%23, k)
	}
	return b.String()
}

// archiveMaxFixture builds an archive of exactly n wide history ads under one of three regimes and
// returns a function that runs MAX(ProcId) against it.
//
//	"off"    the accelerator is never enabled -- the projected wire-native scan
//	"full"   enabled AFTER all n are written, so every sealed segment carries a block
//	"stale"  enabled at the halfway point, so the second half sealed blockless
//
// All three hold the SAME n records. An earlier version of this benchmark enabled the accelerator
// and then appended more, so each mode measured a different dataset size -- which made a larger
// dataset look like a slowdown. Equal data is the whole point of the comparison.
func archiveMaxFixture(b *testing.B, n int, regime string) (func(*testing.B), func()) {
	b.Helper()
	cat, err := db.OpenCatalog(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	cleanup := func() { c.Close(); s.Close(); cat.Close() }
	ctx := context.Background()
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{SegmentSize: 1 << 20}); err != nil {
		b.Fatal(err)
	}
	appendRange := func(from, to int) {
		for i := from; i < to; i++ {
			if err := c.ArchiveAppend(ctx, "history", wideHistoryAd(i)); err != nil {
				b.Fatal(err)
			}
		}
	}
	a, ok := cat.ArchiveTable("history")
	if !ok {
		b.Fatal("archive missing")
	}
	switch regime {
	case "off":
		appendRange(0, n)
	case "full":
		appendRange(0, n)
		if !a.BuildAndEnableSchemaScan(4000, 8) {
			b.Skip("no sealed segments to sample")
		}
	case "stale":
		appendRange(0, n/2)
		if !a.BuildAndEnableSchemaScan(4000, 8) {
			b.Skip("no sealed segments to sample")
		}
		appendRange(n/2, n)
	}
	info := a.SchemaScanInfo()
	b.Logf("%s: enabled=%v coverage=%d/%d sealed, %d records",
		regime, info.Enabled, info.CoveredSegments, info.SealedSegments, a.Count())
	return func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rows, err := c.ArchiveAggregate(ctx, "history", "true", nil,
				[]db.AggSpec{{Func: db.AggMax, Arg: "ProcId"}})
			if err != nil {
				b.Fatal(err)
			}
			if len(rows) != 1 {
				b.Fatalf("rows = %d", len(rows))
			}
		}
	}, cleanup
}

// BenchmarkArchiveMaxWide compares the three regimes over identical data on the shape actually
// deployed: an archive (ZSTD with a trained dictionary, large segments) of WIDE history ads.
func BenchmarkArchiveMaxWide(b *testing.B) {
	const n = 16000
	for _, regime := range []string{"off", "full", "stale"} {
		run, cleanup := archiveMaxFixture(b, n, regime)
		b.Run(regime, run)
		cleanup()
	}
}
