package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// Per-block zone pruning. A block whose column range cannot satisfy a predicate is skipped whole --
// no visibility walk, no column read, no cold-tail decompression.
//
// Pruning is a PERFORMANCE property enforced by a CORRECTNESS one: skipping a block that does hold a
// match silently undercounts. So every test here compares against the same query with pruning
// disabled, and the escape case is called out separately because it is the one that looks safe and
// is not.

// zoneFixture builds records whose ClusterId increases monotonically (so blocks are clustered by it)
// while RequestMemory cycles (so blocks are not clustered by that). Optionally one record carries a
// too-wide ClusterId, which ESCAPES: its value is not in the column, so the column range does not
// describe it.
func zoneFixture(tb testing.TB, n int, wideAt int) *Collection {
	tb.Helper()
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	for i := 0; i < n; i++ {
		cluster := fmt.Sprintf("%d", i) // monotonic: clusters blocks
		if wideAt >= 0 && i == wideAt {
			cluster = fmt.Sprintf("%d", 1<<40) // too wide for the fitted slot -> escapes
		}
		src := fmt.Sprintf("ClusterId = %s\nProcId = %d\nRequestMemory = %d\nRequestCpus = %d\nOwner = \"u%d\"",
			cluster, i%10, 1024+(i%32)*512, 1+i%8, i%32)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(tb, src)); err != nil {
			tb.Fatal(err)
		}
	}
	for _, e := range []string{"ClusterId >= 0", "RequestMemory >= 0", "RequestCpus >= 0"} {
		q, err := vm.Parse(e)
		if err != nil {
			tb.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			for range c.Query(q) {
			}
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		tb.Skip("no sealed segments")
	}
	return c
}

// stripZones removes every block's zones, disabling pruning, and reports how many it cleared.
func stripZones(c *Collection) int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil {
				continue
			}
			if cs := seg.colblk.Load(); cs != nil {
				for _, blk := range cs.blocks {
					if len(blk.zones) > 0 {
						blk.zones = nil
						n++
					}
				}
			}
		}
	}
	return n
}

// TestZonePruningPreservesAnswers is the correctness gate: pruning must not change any answer.
func TestZonePruningPreservesAnswers(t *testing.T) {
	queries := []string{
		"ClusterId < 100",                         // only the earliest blocks
		"ClusterId > 3900",                        // only the latest
		"ClusterId >= 1000 && ClusterId < 1100",   // a narrow band in the middle
		"ClusterId == 2500",                       // a single value
		"ClusterId > 99999",                       // nothing: every block prunable
		"RequestMemory > 4096",                    // unclustered: nothing prunable
		"ClusterId < 500 && RequestMemory > 4096", // one clustered, one not
	}
	// With pruning.
	c := zoneFixture(t, 4000, -1)
	defer c.Close()
	withPruning := map[string]int{}
	for _, expr := range queries {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		got, served := c.CountQuery(q)
		if !served {
			t.Fatalf("%s: declined", expr)
		}
		withPruning[expr] = got
	}
	// Same store, pruning disabled.
	if n := stripZones(c); n == 0 {
		t.Fatal("no block carried zones; pruning was never active and this test proves nothing")
	}
	for _, expr := range queries {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		got, served := c.CountQuery(q)
		if !served {
			t.Fatalf("%s: declined without zones", expr)
		}
		row := 0
		for range c.Query(q) {
			row++
		}
		if got != row {
			t.Errorf("%s: unpruned columnar %d != row %d", expr, got, row)
		}
		if withPruning[expr] != got {
			t.Errorf("%s: PRUNING CHANGED THE ANSWER: %d with zones, %d without",
				expr, withPruning[expr], got)
		}
		t.Logf("%-42s %d (identical with and without pruning)", expr, got)
	}
}

// TestZonePruningRespectsEscapedValues is the case that looks safe and is not. A value too wide for
// its slot lives in the cold tail, so the column's [min,max] does not describe it -- pruning on that
// range would drop a record that matches.
func TestZonePruningRespectsEscapedValues(t *testing.T) {
	const n = 4000
	// The wide record sits early, so its block's COLUMN range is roughly [0,128) while the record
	// itself holds 2^40. A query for the wide value must still find it.
	c := zoneFixture(t, n, 5)
	defer c.Close()

	q, err := vm.Parse(fmt.Sprintf("ClusterId == %d", int64(1)<<40))
	if err != nil {
		t.Fatal(err)
	}
	row := 0
	for range c.Query(q) {
		row++
	}
	if row != 1 {
		t.Fatalf("row path found %d records with the wide ClusterId, want 1: the fixture is wrong", row)
	}
	got, served := c.CountQuery(q)
	if !served {
		t.Skip("columnar path declined")
	}
	if got != 1 {
		t.Errorf("columnar count %d, want 1: a block was pruned on a column range that does not "+
			"describe its escaped value", got)
	}
	// And the block holding it must be marked inexact, which is what makes that safe.
	found := false
	for _, sh := range c.shards {
		sh.mu.RLock()
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil {
				continue
			}
			cs := seg.colblk.Load()
			if cs == nil {
				continue
			}
			for _, blk := range cs.blocks {
				for _, z := range blk.zones {
					if z.escaped {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Error("no block zone is marked escaped; nothing would stop pruning from dropping the match")
	}
}

// TestZonesSurviveReopen covers persistence: zones are a performance property, so losing them on
// reload would be silent.
func TestZonesSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4000; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("ClusterId = %d\nProcId = %d\nRequestMemory = %d",
				i, i%10, 1024+(i%32)*512))); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments")
	}
	q, err := vm.Parse("ClusterId >= 1000 && ClusterId < 1100")
	if err != nil {
		t.Fatal(err)
	}
	before, served := c.CountQuery(q)
	if !served {
		t.Fatal("declined before reopen")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	zoned := 0
	for _, sh := range c2.shards {
		sh.mu.RLock()
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil {
				continue
			}
			if cs := seg.colblk.Load(); cs != nil {
				for _, blk := range cs.blocks {
					if len(blk.zones) > 0 {
						zoned++
					}
				}
			}
		}
	}
	if zoned == 0 {
		t.Error("no reloaded block carries zones; block pruning is silently off after a reopen")
	}
	after, served := c2.CountQuery(q)
	if !served {
		t.Skip("declined after reopen (accelerator not adopted)")
	}
	if after != before {
		t.Errorf("count changed across reopen: %d -> %d", before, after)
	}
	t.Logf("%d reloaded block(s) carry zones; count %d preserved", zoned, after)
}

// BenchmarkZonePruning measures it where it can help (a clustered attribute) and where it cannot
// (an unclustered one), because pruning that only helps clustered data is a different claim from
// pruning that always helps.
func BenchmarkZonePruning(b *testing.B) {
	for _, tc := range []struct{ name, expr string }{
		{"clustered/narrowBand", "ClusterId >= 1000 && ClusterId < 1100"},
		{"clustered/noMatch", "ClusterId > 99999999"},
		{"unclustered", "RequestMemory > 4096"},
	} {
		c := zoneFixture(b, 60000, -1)
		q, err := vm.Parse(tc.expr)
		if err != nil {
			b.Fatal(err)
		}
		if _, ok := c.CountQuery(q); !ok {
			b.Fatalf("%s declined", tc.expr)
		}
		b.Run(tc.name+"/pruned", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c.CountQuery(q)
			}
		})
		stripZones(c)
		b.Run(tc.name+"/unpruned", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c.CountQuery(q)
			}
		})
		c.Close()
	}
}

// BenchmarkZonePruningEndToEnd is the whole stack against the path it replaces: a selective query on
// a clustered attribute, columnar with block pruning versus the ordinary row scan.
func BenchmarkZonePruningEndToEnd(b *testing.B) {
	c := zoneFixture(b, 60000, -1)
	defer c.Close()
	q, err := vm.Parse("ClusterId >= 1000 && ClusterId < 1100")
	if err != nil {
		b.Fatal(err)
	}
	got, ok := c.CountQuery(q)
	if !ok {
		b.Fatal("declined")
	}
	row := 0
	for range c.Query(q) {
		row++
	}
	if got != row {
		b.Fatalf("columnar %d != row %d", got, row)
	}
	b.Run("columnarPruned", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.CountQuery(q)
		}
	})
	b.Run("rowScan", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			n := 0
			for range c.Query(q) {
				n++
			}
		}
	})
}
