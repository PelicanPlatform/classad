package collections

import (
	"fmt"
	"testing"
)

// What zone maps cost a mutable collection, and what they buy it.
//
// The cost is a second pass over a segment's records when it seals, which is paid on the
// write path. The benefit is whole segments skipped on a range query. Both arms write the
// same records with the same value index; only ZoneAttrs differs, so the postings, the
// segment stats and the sealing are identical and what is left is the zone walk.
func BenchmarkZoneMutableWrite(b *testing.B) {
	for _, zones := range []bool{false, true} {
		name := "no-zones"
		if zones {
			name = "zones"
		}
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				c := New(Options{Shards: 1, SegmentSize: 1 << 13, ValueAttrs: []string{"QDate"}})
				if !zones {
					// Clear the zone attributes the constructor installed, leaving the
					// value index in place. Dropping ValueAttrs instead would remove the
					// postings too, and the arms would then differ by more than the zones.
					clearZoneAttrs(c)
				}
				for j := 0; j < 20000; j++ {
					if err := c.Put([]byte(fmt.Sprintf("k%d", j)),
						mustAd(b, fmt.Sprintf(`[ Id=%d; QDate=%d ]`, j, 1000000+j))); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(c.Stats().Segments), "segments")
			}
		})
	}
}

// And what it buys: records a range query does not have to look at.
func BenchmarkZoneMutableRead(b *testing.B) {
	build := func(zones bool) *Collection {
		opts := Options{Shards: 1, SegmentSize: 1 << 13, ValueAttrs: []string{"QDate"}}
		c := New(opts)
		for j := 0; j < 50000; j++ {
			if err := c.Put([]byte(fmt.Sprintf("k%d", j)),
				mustAd(b, fmt.Sprintf(`[ Id=%d; QDate=%d ]`, j, 1000000+j))); err != nil {
				b.Fatal(err)
			}
		}
		c.Reindex()
		if !zones {
			stripSegZones(c)
		}
		return c
	}
	for _, zones := range []bool{false, true} {
		name := "no-zones"
		if zones {
			name = "zones"
		}
		c := build(zones)
		q := mustQuery(b, "QDate > 1049900")
		b.Run(name, func(b *testing.B) {
			n := 0
			for i := 0; i < b.N; i++ {
				n = 0
				for range c.Query(q) {
					n++
				}
			}
			b.ReportMetric(float64(n), "rows")
			b.ReportMetric(float64(c.ExplainQuery(q).ZonePrunedSegments), "pruned")
		})
	}
}

// stripSegZones clears every segment's zone map, which turns pruning off without changing
// anything else about how the collection was built.
func stripSegZones(c *Collection) {
	for _, sh := range c.shards {
		sh.mu.Lock()
		for _, seg := range sh.segs {
			if seg != nil {
				seg.zones = nil
			}
		}
		sh.mu.Unlock()
	}
}

// clearZoneAttrs removes a collection's zone attributes, so segments seal without a zone
// walk while everything else about the collection is unchanged.
func clearZoneAttrs(c *Collection) {
	for _, sh := range c.shards {
		sh.mu.Lock()
		sh.zoneAttrs = nil
		sh.mu.Unlock()
	}
}
