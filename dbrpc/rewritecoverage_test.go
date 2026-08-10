package dbrpc

import (
	"context"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// TestRewriteThenAggregateCoverage reproduces an operator's observation: after `.rewrite` on a
// history table a numeric aggregate is slow, and after `.schema rebuild` it is fast again.
//
// A rewrite re-encodes every record into fresh segments. Any columnar block built against the old
// records describes bytes that no longer exist, so the rewrite has to either rebuild the blocks or
// drop them. This reports coverage and timing at each step to say which happens.
func TestRewriteThenAggregateCoverage(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); cat.Close() }()
	ctx := context.Background()

	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{SegmentSize: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12000; i++ {
		if err := c.ArchiveAppend(ctx, "history", wideHistoryAd(i)); err != nil {
			t.Fatal(err)
		}
	}
	a, ok := cat.ArchiveTable("history")
	if !ok {
		t.Fatal("archive missing")
	}

	maxProcId := func() (string, time.Duration) {
		start := time.Now()
		rows, err := c.ArchiveAggregate(ctx, "history", "true", nil,
			[]db.AggSpec{{Func: db.AggMax, Arg: "ProcId"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || len(rows[0].Values) != 1 {
			t.Fatalf("rows = %+v", rows)
		}
		return rows[0].Values[0], time.Since(start)
	}
	report := func(stage string) {
		info := a.SchemaScanInfo()
		val, d := maxProcId()
		t.Logf("%-22s enabled=%-5v coverage=%d/%d  MAX=%s in %v",
			stage, info.Enabled, info.CoveredSegments, info.SealedSegments, val, d.Round(time.Millisecond))
	}

	report("before enable")
	if !a.BuildAndEnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to sample")
	}
	report("after enable")

	want, _ := maxProcId()

	// The operator's sequence.
	a.Rewrite()
	report("after .rewrite")
	afterRewrite := a.SchemaScanInfo()

	if !a.ReschemaScan(4000, 8) {
		t.Fatal("ReschemaScan failed")
	}
	report("after .schema rebuild")
	afterRebuild := a.SchemaScanInfo()

	// Correctness first: whatever the coverage, the answer must not change.
	if got, _ := maxProcId(); got != want {
		t.Errorf("MAX changed across rewrite/rebuild: %s -> %s", want, got)
	}

	// The finding: does a rewrite leave the accelerator covering the archive?
	if afterRewrite.CoveredSegments < afterRewrite.SealedSegments {
		t.Errorf("REPRODUCED: after .rewrite only %d/%d sealed segments carry a block, so the "+
			"aggregate falls back to reading records; .schema rebuild restores %d/%d",
			afterRewrite.CoveredSegments, afterRewrite.SealedSegments,
			afterRebuild.CoveredSegments, afterRebuild.SealedSegments)
	}
}
