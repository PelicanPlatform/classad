package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// statsPair serves one in-memory table with query aggregation enabled, returning the
// client, the server (so a test can read the aggregate in process) and a cleanup.
func statsPair(t *testing.T, opts ServeOptions, qopts QueryStatsOptions) (*Client, *Server, func()) {
	t.Helper()
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	s.EnableQueryStats(qopts)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, opts) }()
	c := NewClient(cconn)
	return c, s, func() { c.Close(); s.Close(); d.Close() }
}

// seedJobs writes n ads with a CompletionDate and an Owner.
func seedJobs(t *testing.T, ctx context.Context, c *Client, n int) {
	t.Helper()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ad := fmt.Sprintf("Owner = %q\nCompletionDate = %d\nJobStatus = %d", "u"+string(rune('a'+i%3)), 1700000000+i, i%5)
		if err := tx.NewClassAd(ctx, fmt.Sprintf("j%d", i), ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// waitShapes polls the in-process aggregate until it holds at least n shapes. The
// recording happens on the serving goroutine after the last frame is written, so a
// client that has its rows may still be a few microseconds ahead of it.
func waitShapes(t *testing.T, s *Server, n int) QueryStatsReport {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rep := s.qstats.Load().report("", true)
		if len(rep.Shapes) >= n {
			return rep
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d shape(s) recorded within 2s, want %d", len(rep.Shapes), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitRecorded polls until at least n queries have been aggregated.
func waitRecorded(t *testing.T, s *Server, n int64) QueryStatsReport {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rep := s.qstats.Load().report("", true)
		if rep.Recorded >= n {
			return rep
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d quer(ies) recorded within 2s, want %d", rep.Recorded, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestQueryStatsMergesTimeLiterals is the central claim of shape bucketing: the same
// predicate run with different time literals is ONE shape, not one per timestamp.
// These two queries do wildly different amounts of work -- that is what the exemplars
// are for -- but they are the same query pattern and must aggregate together.
func TestQueryStatsMergesTimeLiterals(t *testing.T) {
	c, s, cleanup := statsPair(t, ServeOptions{Privileged: true}, QueryStatsOptions{})
	defer cleanup()
	ctx := context.Background()
	seedJobs(t, ctx, c, 30)

	for _, cutoff := range []int{1700000005, 1700000010, 1699000000} {
		if _, err := c.Query(ctx, fmt.Sprintf("CompletionDate >= %d", cutoff)); err != nil {
			t.Fatal(err)
		}
	}
	rep := waitRecorded(t, s, 3)
	if len(rep.Shapes) != 1 {
		t.Fatalf("got %d shapes, want 1: %+v", len(rep.Shapes), rep.Shapes)
	}
	sh := rep.Shapes[0]
	if sh.Count != 3 {
		t.Errorf("Count = %d, want 3", sh.Count)
	}
	if !strings.Contains(sh.Shape, "completiondate") || !strings.Contains(sh.Shape, "?") {
		t.Errorf("Shape = %q, want the attribute with its literal dropped", sh.Shape)
	}
	if strings.Contains(sh.Shape, "1700000005") {
		t.Errorf("Shape = %q still carries a literal", sh.Shape)
	}
	// The exemplars are the other half: the bucket says which pattern costs, the
	// exemplars say which parameterization to reproduce.
	if len(sh.Exemplars) == 0 {
		t.Fatal("no exemplars kept")
	}
	for _, ex := range sh.Exemplars {
		if !strings.HasPrefix(ex.Constraint, "CompletionDate >= 1") {
			t.Errorf("exemplar constraint = %q, want the verbatim query text", ex.Constraint)
		}
	}
}

// TestQueryStatsSeparatesDifferentShapes proves the merging above is not just
// everything collapsing into one bucket: genuinely different predicates, a different
// table and a LIMIT each keep their own.
func TestQueryStatsSeparatesDifferentShapes(t *testing.T) {
	c, s, cleanup := statsPair(t, ServeOptions{Privileged: true}, QueryStatsOptions{})
	defer cleanup()
	ctx := context.Background()
	seedJobs(t, ctx, c, 30)

	if _, err := c.Query(ctx, "CompletionDate >= 1700000005"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Query(ctx, `Owner == "ua"`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Query(ctx, "JobStatus == 2 && CompletionDate >= 1700000005"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryLimit(ctx, "CompletionDate >= 1700000005", 2); err != nil {
		t.Fatal(err)
	}
	rep := waitShapes(t, s, 4)
	if len(rep.Shapes) != 4 {
		t.Fatalf("got %d shapes, want 4:", len(rep.Shapes))
	}
	seen := map[string]bool{}
	for _, sh := range rep.Shapes {
		key := sh.Shape
		if sh.Limited {
			key += " [limit]"
		}
		if seen[key] {
			t.Errorf("duplicate shape key %q", key)
		}
		seen[key] = true
		if sh.Count != 1 {
			t.Errorf("shape %q: Count = %d, want 1", key, sh.Count)
		}
	}
	// Same predicate, with and without a LIMIT: different work, different buckets.
	if !seen["(completiondate >= ?)"] || !seen["(completiondate >= ?) [limit]"] {
		t.Errorf("LIMIT is not part of the shape key: %v", seen)
	}
}

// TestQueryStatsRanksByTotalTime fixes the ordering contract: the report leads with the
// shape that consumed the most time in total, not the one with the largest single
// observation. A 40ms query run ten thousand times outranks a one-off 2s query.
func TestQueryStatsRanksByTotalTime(t *testing.T) {
	qs := newQueryStats(QueryStatsOptions{})
	for i := 0; i < 100; i++ {
		qs.record(QueryLog{Op: "Query", Table: "t", Constraint: "A == 1", Duration: 40 * time.Millisecond})
	}
	qs.record(QueryLog{Op: "Query", Table: "t", Constraint: "B == 1", Duration: 2 * time.Second})

	rep := qs.report("", false)
	if len(rep.Shapes) != 2 {
		t.Fatalf("got %d shapes, want 2", len(rep.Shapes))
	}
	if rep.Shapes[0].Shape != "(a == ?)" {
		t.Errorf("leading shape = %q, want the frequent one", rep.Shapes[0].Shape)
	}
	if rep.Shapes[0].Max >= rep.Shapes[1].Max {
		t.Error("the frequent shape should NOT be the one with the largest single observation")
	}
	if got, want := rep.Shapes[0].Mean(), 40*time.Millisecond; got != want {
		t.Errorf("Mean = %v, want %v", got, want)
	}
}

// TestQueryStatsEvictsLowestTotal confirms the map is bounded: a client inventing novel
// shapes evicts the cheapest tracked shape, never the expensive one an operator is
// looking for.
func TestQueryStatsEvictsLowestTotal(t *testing.T) {
	qs := newQueryStats(QueryStatsOptions{MaxShapes: 4})
	qs.record(QueryLog{Op: "Query", Table: "t", Constraint: "Expensive == 1", Duration: time.Second})
	for i := 0; i < 50; i++ {
		qs.record(QueryLog{
			Op: "Query", Table: "t",
			Constraint: fmt.Sprintf("Novel%d == 1", i),
			Duration:   time.Microsecond,
		})
	}
	rep := qs.report("", false)
	if rep.Tracked > 4 {
		t.Fatalf("Tracked = %d, want <= 4", rep.Tracked)
	}
	if rep.Evicted == 0 {
		t.Error("Evicted = 0, want the cap to have dropped shapes")
	}
	if rep.Shapes[0].Shape != "(expensive == ?)" {
		t.Errorf("the expensive shape was evicted; leading shape = %q", rep.Shapes[0].Shape)
	}
}

// TestQueryStatsExemplarsNeedDaemon covers the privacy split: shapes carry no user data
// and are readable at READ level, while exemplars quote the constraint and name the user
// and so require DAEMON.
func TestQueryStatsExemplarsNeedDaemon(t *testing.T) {
	d, derr := db.Open("")
	if derr != nil {
		t.Fatal(derr)
	}
	defer d.Close()
	s := NewServer(d)
	defer s.Close()
	s.EnableQueryStats(QueryStatsOptions{})

	serve := func(opts ServeOptions) *Client {
		cconn, sconn := netPipe()
		go func() { _ = s.ServeConnOpts(sconn, opts) }()
		c := NewClient(cconn)
		t.Cleanup(func() { c.Close() })
		return c
	}
	ctx := context.Background()
	admin := serve(ServeOptions{Privileged: true, Peer: "alice@example"})
	if _, err := admin.Query(ctx, `Owner == "alice"`); err != nil {
		t.Fatal(err)
	}
	waitRecorded(t, s, 1)

	reader := serve(ServeOptions{ReadOnly: true})
	rep, err := reader.QueryStats(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Enabled || len(rep.Shapes) != 1 {
		t.Fatalf("READ peer got Enabled=%v shapes=%d, want the shape", rep.Enabled, len(rep.Shapes))
	}
	if rep.Exemplars || len(rep.Shapes[0].Exemplars) != 0 {
		t.Error("a READ peer was handed exemplars")
	}
	if strings.Contains(rep.Shapes[0].Shape, "alice") {
		t.Errorf("the shape leaked a literal: %q", rep.Shapes[0].Shape)
	}
	if _, rerr := reader.ResetQueryStats(ctx, ""); rerr == nil {
		t.Error("a READ peer was allowed to reset the aggregate")
	}

	drep, err := admin.QueryStats(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !drep.Exemplars || len(drep.Shapes[0].Exemplars) == 0 {
		t.Fatal("a DAEMON peer got no exemplars")
	}
	ex := drep.Shapes[0].Exemplars[0]
	if ex.Constraint != `Owner == "alice"` {
		t.Errorf("exemplar constraint = %q", ex.Constraint)
	}
	if ex.User != "alice@example" {
		t.Errorf("exemplar user = %q, want the connection's peer identity", ex.User)
	}
}

// TestQueryStatsProjectionAndScan covers the two fields that make the report diagnostic
// rather than descriptive: how wide a projection the client asked for (0 = it pulled
// every attribute) and how the scan broke down between pruned and scanned segments.
func TestQueryStatsProjectionAndScan(t *testing.T) {
	c, s, cleanup := statsPair(t, ServeOptions{Privileged: true}, QueryStatsOptions{})
	defer cleanup()
	ctx := context.Background()
	seedJobs(t, ctx, c, 40)

	// A projected read: two attributes requested.
	if _, err := c.QueryRawProjectRefs(ctx, DefaultTable, "JobStatus == 2", []string{"Owner", "JobStatus"}, 0); err != nil {
		t.Fatal(err)
	}
	// An unprojected read of the same predicate: every attribute crosses the wire.
	if _, err := c.Query(ctx, "JobStatus == 2"); err != nil {
		t.Fatal(err)
	}
	rep := waitShapes(t, s, 2)

	var proj, plain *QueryShapeStat
	for i := range rep.Shapes {
		switch rep.Shapes[i].Op {
		case "Query":
			plain = &rep.Shapes[i]
		default:
			proj = &rep.Shapes[i]
		}
	}
	if proj == nil || plain == nil {
		t.Fatalf("expected a projected and a plain shape, got %+v", rep.Shapes)
	}
	if proj.ProjectionMax != 2 {
		t.Errorf("projected shape: ProjectionMax = %d, want 2", proj.ProjectionMax)
	}
	if proj.Unprojected != 0 {
		t.Errorf("projected shape: Unprojected = %d, want 0", proj.Unprojected)
	}
	if plain.Unprojected != 1 {
		t.Errorf("plain shape: Unprojected = %d, want 1 (it asked for every attribute)", plain.Unprojected)
	}
	// The projection op threads ScanStats, so the aggregate knows how much the scan
	// actually touched. The plain query op does not, and reports nothing rather than
	// reporting zero work.
	if proj.ScanSamples != 1 {
		t.Errorf("projected shape: ScanSamples = %d, want 1", proj.ScanSamples)
	}
	if proj.SegmentsScanned+proj.SegmentsPruned == 0 {
		t.Error("projected shape: no segment accounting recorded")
	}
	if plain.ScanSamples != 0 {
		t.Errorf("plain shape: ScanSamples = %d, want 0 (not measured on that path)", plain.ScanSamples)
	}
}

// TestQueryStatsWireScan covers the read path most clients actually use: the wire op has
// no EXPLAIN ANALYZE variant, so unless it collects a breakdown for the aggregate the
// report's scan column is blank exactly where it is needed most.
func TestQueryStatsWireScan(t *testing.T) {
	dir := t.TempDir()
	d, derr := db.Open(dir)
	if derr != nil {
		t.Fatal(derr)
	}
	defer d.Close()
	s := NewServer(d)
	defer s.Close()
	s.EnableQueryStats(QueryStatsOptions{})
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	defer c.Close()

	ctx := context.Background()
	seedJobs(t, ctx, c, 40)
	if err := c.QueryRawWireStream(ctx, DefaultTable, "JobStatus == 2", []string{"Owner"}, 0, false,
		func([]byte) bool { return true }); err != nil {
		t.Fatal(err)
	}
	rep := waitRecorded(t, s, 1)
	var wire *QueryShapeStat
	for i := range rep.Shapes {
		if rep.Shapes[i].Op == "QueryRawWire" {
			wire = &rep.Shapes[i]
		}
	}
	if wire == nil {
		t.Fatalf("no QueryRawWire shape recorded: %+v", rep.Shapes)
	}
	if wire.ScanSamples != 1 {
		t.Errorf("ScanSamples = %d, want 1", wire.ScanSamples)
	}
	if wire.SegmentsScanned+wire.SegmentsPruned == 0 {
		t.Error("no segment accounting recorded on the wire path")
	}
	if wire.ProjectionMax != 1 {
		t.Errorf("ProjectionMax = %d, want 1", wire.ProjectionMax)
	}
}

// TestQueryStatsDisabled confirms a server that is not aggregating answers cleanly
// instead of erroring, and that the caller's own per-query hook still runs.
func TestQueryStatsDisabled(t *testing.T) {
	var logged int
	c, cleanup := serveOptsPair(t, ServeOptions{QueryLog: func(QueryLog) { logged++ }})
	defer cleanup()
	ctx := context.Background()
	if _, err := c.Query(ctx, "true"); err != nil {
		t.Fatal(err)
	}
	rep, err := c.QueryStats(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Enabled {
		t.Error("Enabled = true on a server that never enabled aggregation")
	}
	if len(rep.Shapes) != 0 {
		t.Errorf("got %d shapes from a disabled server", len(rep.Shapes))
	}
}

// TestQueryStatsReset clears the window.
func TestQueryStatsReset(t *testing.T) {
	c, s, cleanup := statsPair(t, ServeOptions{Privileged: true}, QueryStatsOptions{})
	defer cleanup()
	ctx := context.Background()
	if _, err := c.Query(ctx, "A == 1"); err != nil {
		t.Fatal(err)
	}
	waitRecorded(t, s, 1)
	before, err := c.ResetQueryStats(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if before.Recorded != 1 {
		t.Errorf("reset returned Recorded = %d, want the pre-reset count 1", before.Recorded)
	}
	after, err := c.QueryStats(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if after.Recorded != 0 || len(after.Shapes) != 0 {
		t.Errorf("after reset: Recorded = %d, shapes = %d", after.Recorded, len(after.Shapes))
	}
}

// TestQueryStatsMinDuration honours the recording floor.
func TestQueryStatsMinDuration(t *testing.T) {
	qs := newQueryStats(QueryStatsOptions{MinDuration: 100 * time.Millisecond})
	qs.record(QueryLog{Op: "Query", Table: "t", Constraint: "A == 1", Duration: time.Millisecond})
	qs.record(QueryLog{Op: "Query", Table: "t", Constraint: "B == 1", Duration: time.Second})
	rep := qs.report("", false)
	if len(rep.Shapes) != 1 || rep.Shapes[0].Shape != "(b == ?)" {
		t.Fatalf("MinDuration not applied: %+v", rep.Shapes)
	}
}

// TestQueryStatsTableFilter restricts the report to one table.
func TestQueryStatsTableFilter(t *testing.T) {
	qs := newQueryStats(QueryStatsOptions{})
	qs.record(QueryLog{Op: "Query", Table: "jobs", Constraint: "A == 1", Duration: time.Millisecond})
	qs.record(QueryLog{Op: "Query", Table: "history", Constraint: "A == 1", Duration: time.Millisecond})
	if got := len(qs.report("jobs", false).Shapes); got != 1 {
		t.Errorf("table filter returned %d shapes, want 1", got)
	}
	if got := len(qs.report("", false).Shapes); got != 2 {
		t.Errorf("unfiltered report returned %d shapes, want 2", got)
	}
}
