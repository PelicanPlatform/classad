package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

func countWhere(t *testing.T, c *Collection, expr string) int {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range c.Query(q) {
		n++
	}
	return n
}

// TestIndexSpecSignatureSurvivesReopen covers the staleness half. The configuration id was a
// counter held only in memory, so it restarted at zero: after any add or drop, every sealed
// segment looked stale on the next open and the whole store was re-indexed -- on an archive,
// a full decompress on every restart. Deriving it from the indexed set instead makes it mean
// the same thing across processes.
func TestIndexSpecSignatureSurvivesReopen(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 13, CategoricalAttrs: []string{"Owner"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1200; i++ {
		ad := mustAdOld(t, fmt.Sprintf("Owner = %q\nGroup = %q\nPad = %q",
			fmt.Sprintf("u%d", i%4), fmt.Sprintf("g%d", i%8), strings.Repeat("s", 60)))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	// A configuration change, then a full reindex: nothing should be stale afterwards.
	c.AddIndex([]string{"Group"}, nil)
	c.Reindex()
	staleBefore, sealedBefore := c.StaleIndexSegments()
	if sealedBefore == 0 {
		t.Fatal("no sealed segments to judge")
	}
	if staleBefore != 0 {
		t.Fatalf("%d/%d stale right after a full reindex", staleBefore, sealedBefore)
	}
	c.Close()

	c2, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 13,
		CategoricalAttrs: []string{"Owner", "Group"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	stale, sealed := c2.StaleIndexSegments()
	if sealed == 0 {
		t.Fatal("no sealed segments after reopen")
	}
	if stale != 0 {
		t.Errorf("%d/%d segments stale after reopen with an unchanged configuration -- the "+
			"whole store would be re-indexed on every restart", stale, sealed)
	}
}

// TestSegmentsMayHoldDifferentIndexConfigs is the containment half: segments indexed under
// different configurations coexist, each answering the probes its own index covers and
// scanning for the rest. Every query must return the full, correct count regardless of which
// segments happen to hold which index.
func TestSegmentsMayHoldDifferentIndexConfigs(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 13, CategoricalAttrs: []string{"Owner"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.Close() }()

	const half = 800
	put := func(from, to int) {
		for i := from; i < to; i++ {
			ad := mustAdOld(t, fmt.Sprintf("Owner = %q\nGroup = %q\nPad = %q",
				fmt.Sprintf("u%d", i%4), fmt.Sprintf("g%d", (i/4)%8), strings.Repeat("s", 60)))
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
				t.Fatal(err)
			}
		}
	}
	// First half indexed on Owner only, and sealed under that configuration.
	put(0, half)
	c.Reindex()

	// Now index Group too. The already-sealed segments keep the Owner-only sidecar; the
	// segments written from here carry both. Deliberately no full reindex -- that mixture is
	// the state a bounded backfill horizon would leave behind permanently.
	c.AddIndex([]string{"Group"}, nil)
	put(half, 2*half)

	// Reopen. Note what this does NOT yet prove: recovery re-indexes every segment under the
	// current configuration, so a mixed state cannot survive a restart today and the segments
	// below are all on the new configuration by the time they are queried. Relaxing the
	// recovery gate from "configuration matches" to "this index covers this probe" is what
	// makes a mixed state USABLE; bounding index backfill to recent segments is what will
	// make one persist. Until then this is a regression guard -- results stay complete when
	// segments are indexed, re-indexed, and recovered around a configuration change.
	c.Close()
	c, err = Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 13,
		CategoricalAttrs: []string{"Owner", "Group"}})
	if err != nil {
		t.Fatal(err)
	}

	total := 2 * half
	// Owner is indexed everywhere; Group only in the newer segments and scanned in the older
	// ones. Both must be complete, and so must their conjunction -- Owner (i%4) and Group
	// ((i/4)%8) are chosen to vary independently so that is a real test rather than a
	// vacuous one.
	for _, tc := range []struct {
		expr string
		want int
	}{
		{`Owner == "u1"`, total / 4},
		{`Group == "g3"`, total / 8},
		{`Owner == "u1" && Group == "g3"`, total / 32},
	} {
		if got := countWhere(t, c, tc.expr); got != tc.want {
			t.Errorf("%s: %d rows, want %d -- a segment on an older index configuration "+
				"answered incompletely", tc.expr, got, tc.want)
		}
	}
}
