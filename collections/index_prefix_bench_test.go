package collections

import (
	"fmt"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// BenchmarkPrefixProbe times `regexp("^<ap>", GlobalJobId)` -- the shape a pool operator
// writes to pull one access point's jobs out of a shared history -- with the attribute
// indexed and without, over the same records. The unindexed arm is what this query cost
// before the probe existed: an anchored pattern was not a recognizable `Attr OP literal`,
// so it yielded no probe at all and every record was decompressed and matched.
//
// The arms differ only in CategoricalAttrs, and the selectivity sweep says what the index
// is worth where it is worth anything: the narrower the prefix, the fewer keys its run
// holds and the fewer postings are read, while the scan pays the same either way.
func BenchmarkPrefixProbe(b *testing.B) {
	const n = 20000
	// GlobalJobId is distinct per record, so "^ap07" names a run of 200 KEYS, not one. Pool
	// takes ten values, so `Pool == "p3"` is the same 10% of the records through ONE key:
	// the control that says whether an unselective prefix loses because it is a prefix (it
	// unions a key's postings per distinct value in the run) or because an indexed read of
	// any shape loses once it stops pruning.
	build := func(b *testing.B, indexed bool) *Collection {
		b.Helper()
		// Append-only: the history shape this query is written against, and the only one the
		// over-read gate covers (overReadGate declines a mutable table, where the candidate
		// walk was never the slow part).
		opts := Options{Dir: b.TempDir(), AppendOnly: true, SegmentSize: 1 << 16}
		if indexed {
			opts.CategoricalAttrs = []string{"GlobalJobId", "Pool"}
		}
		c, err := Open(opts)
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < n; i++ {
			ad := jobAd(i, nil)
			ad.InsertAttrString("GlobalJobId", fmt.Sprintf("ap%02d.%d.0#%d", i%100, i, i))
			ad.InsertAttrString("Pool", fmt.Sprintf("p%d", i%10))
			if err := c.Put([]byte(fmt.Sprintf("k%06d.0", i)), ad); err != nil {
				b.Fatal(err)
			}
		}
		c.Reindex()
		return c
	}
	for _, pat := range []struct {
		expr string
		name string
	}{
		{`regexp("^ap07", GlobalJobId)`, "prefix01pct"},
		{`regexp("^ap0", GlobalJobId)`, "prefix10pct"},
		{`regexp("^ap", GlobalJobId)`, "prefix100pct"}, // the gate declines: both arms scan
		{`Pool == "p3"`, "equality10pct"},              // the one-key control at 10%
	} {
		// Flip which arm runs first on alternate shapes, so a drift over the run cannot be
		// read as one arm being faster.
		arms := []bool{false, true}
		if len(pat.name)%2 == 0 {
			arms = []bool{true, false}
		}
		for _, indexed := range arms {
			label := pat.name + "/scan"
			if indexed {
				label = pat.name + "/indexed"
			}
			b.Run(label, func(b *testing.B) {
				c := build(b, indexed)
				defer c.Close()
				q, err := vm.Parse(pat.expr)
				if err != nil {
					b.Fatal(err)
				}
				var st ScanStats
				rows := 0
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					st = ScanStats{}
					rows = 0
					for range c.QueryRawWireStats(q, []string{"ClusterId"}, false, &st) {
						rows++
					}
				}
				b.StopTimer()
				if rows == 0 {
					b.Fatal("no rows matched: the arm measures nothing")
				}
				b.ReportMetric(float64(st.RecordsVisited), "visited")
				b.ReportMetric(float64(rows), "rows")
			})
		}
	}
}

// BenchmarkCatKeyOrder times putting one attribute's folded keys in order, which is what a
// prefix probe needs and nothing else does. It is built on FIRST USE for that reason: doing
// it at finish instead cost 0.8ms -> 3.3ms to summarize the postings of an attribute with a
// distinct value per record, on the write path (the active segment is re-finished on every
// append batch), for a probe shape most queries never issue. Built lazily, that cost is paid
// once per segment index by the query that asked for it.
func BenchmarkCatKeyOrder(b *testing.B) {
	for _, ndv := range []int{8, 20000} {
		b.Run(fmt.Sprintf("ndv%d", ndv), func(b *testing.B) {
			const n = 20000
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				cp := &catPostings{post: map[string]*roaring.Bitmap{}, exc: roaring.New(), posted: roaring.New()}
				for r := 0; r < n; r++ {
					k := fmt.Sprintf("value%06d", r%ndv)
					bm := cp.post[k]
					if bm == nil {
						bm = roaring.New()
						cp.post[k] = bm
					}
					bm.Add(uint32(r))
					cp.posted.Add(uint32(r))
				}
				b.StartTimer()
				if got := len(cp.sortedKeys()); got != ndv {
					b.Fatalf("ordered %d keys, want %d", got, ndv)
				}
			}
		})
	}
}
