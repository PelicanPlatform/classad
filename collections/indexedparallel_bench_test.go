package collections

import (
	"fmt"
	"testing"
)

// WHAT CHOOSING AN INDEX COSTS IN PARALLELISM.
//
// Query fan-out is gated on there being NO index plan: queryAs requires len(rp.usable)==0
// before it will spread a read across workers, and rawwire_subset.go and rawwire_refs.go
// say the same. So an index does not only decide HOW a read finds its records, it decides
// whether the read may use more than one core: an indexed plan runs on one goroutine
// however many segments it spans, and a reverse (newest-first) scan never fans out at all.
//
// Three arms over the same 200000 records, selectivity swept by a uniform attribute. The
// row counts are compared before anything is timed. Two collections have no index, which
// is the only way to make the planner choose a scan for a predicate an index covers; one
// of those also has fan-out switched off, which is what separates the two effects.
//
//	sel   indexed-serial   scan-serial   scan-parallel
//	 1%       1.4 ms         40.1 ms        36.0 ms
//	 5%       8.4            44.2           28.1
//	10%      16.4            48.2           28.6
//	25%      32.2            55.0           24.7
//	50%      63.1            80.4           31.1
//	90%     116.6           102.4           41.0
//
// Read down the first two columns and the index is doing its job: it beats a serial scan
// everywhere until about 90%, which is roughly where maxPushdownFrac already declines it.
// The pushdown gate is not set wrong for the comparison it makes.
//
// Read across to the third and the index loses from about 25% on, and by 90% it is 2.8x
// slower than the scan it was preferred to. Nothing there is about the index being a bad
// access path. It is that choosing it forfeits the other cores, and the cheaper access
// path on one core loses to the dearer one on six.
//
// So the two candidate fixes are not interchangeable. Declining the index sooner would
// recover the 25-90% band and throw away the index's work to do it; letting an indexed
// read fan out would help across the whole sweep. At 1% the index is 26x faster than any
// scan, so it is the fan-out gate that wants revisiting, not the index.
//
// Note also how little fan-out itself buys here -- 1.4x to 2.2x on twelve cores -- which
// is its own measurement: defaultAutoQueryWorkers is 6 and the returns past a few workers
// are small, so the ceiling on fixing this is nearer 2x than 12x.
func BenchmarkIndexedVsParallelScan(b *testing.B) {
	const n = 200000
	// fill writes n records whose Sel attribute is uniform over [0,100), so a constraint
	// `Sel < p` selects about p percent of them.
	fill := func(c *Collection) {
		for i := 0; i < n; i++ {
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
				mustAd(b, fmt.Sprintf(`[ Id=%d; Sel=%d; Pad=%q ]`,
					i, i%100, "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"))); err != nil {
				b.Fatal(err)
			}
		}
		c.Reindex()
	}
	indexed := New(Options{Shards: 4, SegmentSize: 1 << 16, ValueAttrs: []string{"Sel"}})
	fill(indexed)
	plain := New(Options{Shards: 4, SegmentSize: 1 << 16}) // no index: free to fan out
	fill(plain)
	// A third arm with fan-out switched off, so the two effects can be told apart: the
	// indexed read loses either because an index walk costs more than a scan at this
	// selectivity, or only because it is confined to one core. Which it is decides whether
	// the fix is to decline the index sooner or to let an indexed read fan out.
	serial := New(Options{Shards: 4, SegmentSize: 1 << 16, QueryParallelism: 1})
	fill(serial)

	for _, pct := range []int{1, 5, 10, 25, 50, 90} {
		qs := fmt.Sprintf("Sel < %d", pct)
		q := mustQuery(b, qs)

		exI, exP := indexed.ExplainQuery(q), plain.ExplainQuery(q)
		var ni, np int
		for range indexed.Query(q) {
			ni++
		}
		for range plain.Query(q) {
			np++
		}
		if ni != np {
			b.Fatalf("%s: indexed returned %d rows, unindexed %d", qs, ni, np)
		}
		if exI.Plan != "indexed" {
			b.Fatalf("%s: the indexed collection planned %q, so this measures nothing", qs, exI.Plan)
		}
		if exP.Plan != "parallel-scan" {
			b.Fatalf("%s: the unindexed collection planned %q, want parallel-scan", qs, exP.Plan)
		}

		for _, arm := range []struct {
			name string
			c    *Collection
		}{
			{"indexed-serial", indexed},
			{"scan-serial", serial},
			{"scan-parallel", plain},
		} {
			b.Run(fmt.Sprintf("sel=%02d%%/%s", pct, arm.name), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					cnt := 0
					for range arm.c.Query(q) {
						cnt++
					}
				}
				b.ReportMetric(float64(ni), "rows")
			})
		}
	}
}
