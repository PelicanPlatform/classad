package collections

import (
	"fmt"
	"strings"
	"testing"
)

// TestIndexBackfillHorizonLeavesOlderSegments is the payoff for identifying index
// configurations by content and using whatever a segment's index covers: with backfill
// bounded, a configuration change reaches only recent segments, older ones keep the index
// they already had, and that mixture SURVIVES a restart -- which is what makes it worth
// having, since recovery used to re-index everything and erase it.
//
// The bound exists because rebuilding decompresses every record it covers, while the index
// it would replace still answers for the attributes it holds. On an archive read newest-first
// with a limit, the oldest segments may never be reached at all.
func TestIndexBackfillHorizonLeavesOlderSegments(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	open := func(cat []string) *Collection {
		t.Helper()
		c, err := Open(Options{
			Dir: dir, Shards: 1, SegmentSize: 1 << 13,
			CategoricalAttrs: cat,
			// Small enough that most segments fall outside it.
			IndexBackfillBytes: 24 << 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	c := open([]string{"Owner"})
	const n = 2400
	for i := 0; i < n; i++ {
		ad := mustAdOld(t, fmt.Sprintf("Owner = %q\nGroup = %q\nPad = %q",
			fmt.Sprintf("u%d", i%4), fmt.Sprintf("g%d", (i/4)%8), strings.Repeat("s", 60)))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	// Add a second index. Only segments inside the horizon should pick it up.
	c.AddIndex([]string{"Group"}, nil)
	c.Reindex()
	c.Close()

	c2 := open([]string{"Owner", "Group"})
	defer c2.Close()

	ownerID := inlineAttrID("owner")
	groupID := inlineAttrID("group")
	oldConfig, newConfig := 0, 0
	sh := c2.shards[0]
	sh.mu.RLock()
	for _, seg := range sh.segs {
		if seg == nil || seg == sh.act {
			continue
		}
		mm := seg.msidx.Load()
		if mm == nil {
			continue
		}
		_, hasOwner := mm.catDir[ownerID]
		_, hasGroup := mm.catDir[groupID]
		switch {
		case hasOwner && hasGroup:
			newConfig++
		case hasOwner:
			oldConfig++
		}
	}
	sh.mu.RUnlock()

	if oldConfig == 0 {
		t.Error("every segment was backfilled: the horizon did not bound the rebuild, or " +
			"recovery re-indexed everything anyway")
	}
	if newConfig == 0 {
		t.Error("no segment carries the new configuration: the horizon bounded too much")
	}
	t.Logf("%d segments on the older configuration, %d on the newer, across a restart",
		oldConfig, newConfig)

	// Whatever the mixture, results must be complete: segments holding Group answer from
	// their index, the rest are scanned.
	for _, tc := range []struct {
		expr string
		want int
	}{
		{`Owner == "u1"`, n / 4},
		{`Group == "g3"`, n / 8},
		{`Owner == "u1" && Group == "g3"`, n / 32},
	} {
		if got := countWhere(t, c2, tc.expr); got != tc.want {
			t.Errorf("%s: %d rows, want %d", tc.expr, got, tc.want)
		}
	}
}
