package collections

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
)

// benchRangeCollection builds a single-shard collection with a high-cardinality value
// index -- the shape a history range query hits, where the matching key run holds one
// posting per distinct timestamp rather than a handful of buckets.
func benchRangeCollection(tb testing.TB, n, ndv int) *Collection {
	tb.Helper()
	c := New(Options{Shards: 1, ValueAttrs: []string{"CompletionDate"}})
	for i := 0; i < n; i++ {
		ad := mustAd(tb, fmt.Sprintf(`[ ID=%d; CompletionDate=%d ]`, i, i%ndv))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			tb.Fatal(err)
		}
	}
	c.Reindex()
	return c
}

func benchSegIndex(tb testing.TB, c *Collection) *segIndex {
	tb.Helper()
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if si := seg.idx.Load(); si != nil {
				return si
			}
		}
	}
	tb.Fatal("no built segment index")
	return nil
}

// benchProbes returns the coalesced (band) plan and the un-coalesced one, so a benchmark
// can measure exactly what merging the two bounds buys.
func benchProbes(tb testing.TB, c *Collection, lo, hi float64) (band, split []usableProbe) {
	tb.Helper()
	id, ok := c.intern.LookupID("CompletionDate")
	if !ok {
		tb.Fatal("CompletionDate not interned")
	}
	split = []usableProbe{
		{attrID: id, op: ">", fvals: []float64{lo}},
		{attrID: id, op: "<", fvals: []float64{hi}},
	}
	return coalesceRanges(append([]usableProbe(nil), split...)), split
}

// BenchmarkRangeBandCandidates compares candidate-bitmap construction for a two-sided
// range as one band probe vs. the two half-open probes it replaces. The band scans only
// the keys inside it; the split plan unions everything above lo, unions everything below
// hi, and intersects.
func BenchmarkRangeBandCandidates(b *testing.B) {
	const n, ndv = 200000, 20000
	c := benchRangeCollection(b, n, ndv)
	si := benchSegIndex(b, c)

	// Bands from a wide slice of the key space down to a narrow one.
	for _, w := range []int{ndv / 2, ndv / 10, ndv / 100} {
		lo := float64(ndv/2 - w/2)
		hi := float64(ndv/2 + w/2)
		band, split := benchProbes(b, c, lo, hi)
		b.Run(fmt.Sprintf("keys=%d/band", w), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sinkBM = si.candidateOffsets(band)
			}
		})
		b.Run(fmt.Sprintf("keys=%d/split", w), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sinkBM = si.candidateOffsets(split)
			}
		})
	}
}

// BenchmarkRangeBandCandidatesMmap is BenchmarkRangeBandCandidates on the sealed-segment
// tier, where every posting the scan touches is also a page fault into the sidecar.
func BenchmarkRangeBandCandidatesMmap(b *testing.B) {
	const n, ndv = 200000, 20000
	c := benchRangeCollection(b, n, ndv)
	si := benchSegIndex(b, c)
	path := filepath.Join(b.TempDir(), "seg.idx")
	if err := writeSidecarIndex(path, si); err != nil {
		b.Fatal(err)
	}
	data, closer, err := mapFile(path)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = closer() }()
	mm, err := parseMmapSidecar(data)
	if err != nil {
		b.Fatal(err)
	}

	for _, w := range []int{ndv / 2, ndv / 10, ndv / 100} {
		lo := float64(ndv/2 - w/2)
		hi := float64(ndv/2 + w/2)
		band, split := benchProbes(b, c, lo, hi)
		b.Run(fmt.Sprintf("keys=%d/band", w), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sinkBM = mm.candidateOffsets(band)
			}
		})
		b.Run(fmt.Sprintf("keys=%d/split", w), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sinkBM = mm.candidateOffsets(split)
			}
		})
	}
}

// BenchmarkUnionPostings compares roaring's batched union against the running in-place Or
// it replaced, over the posting-count range a value-index scan actually produces. Postings
// hold RECORD BYTE OFFSETS, so they are spread across the segment's whole address space
// (here 8 MiB, roaring's container size being 64 Ki) rather than packed densely -- which is
// what lets the batch parallelize per container.
func BenchmarkUnionPostings(b *testing.B) {
	const segBytes, recBytes = 8 << 20, 512
	for _, n := range []int{2, 8, 64, 128, 256, 512, 1024, 16384} {
		bms := make([]*roaring.Bitmap, n)
		for i := range bms {
			bm := roaring.New()
			for j := 0; j < 12; j++ { // ~12 records share each key
				bm.Add(uint32((i + j*n) % (segBytes / recBytes) * recBytes))
			}
			bms[i] = bm
		}
		at := func(i int) *roaring.Bitmap { return bms[i] }
		b.Run(fmt.Sprintf("n=%d/batched", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sinkBM = unionPostings(n, at)
			}
		})
		b.Run(fmt.Sprintf("n=%d/running", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				acc := roaring.New()
				for j := 0; j < n; j++ {
					acc.Or(bms[j])
				}
				sinkBM = acc
			}
		})
	}
}

var sinkBM *roaring.Bitmap
