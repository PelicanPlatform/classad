package collections

import (
	"fmt"
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// skewedMemoryAds inserts a skewed Memory distribution mirroring a real StartD pool: most
// nodes are small, a handful are huge (768 GiB) which stretch max far past the bulk. Returns
// the true fraction with Memory > 4096. lowN nodes have Memory in [512,4096]; highN have
// Memory in (4096, 32768] plus `huge` outliers at 786432.
func putSkewedMemory(c *Collection, lowN, highN, huge int) float64 {
	id := 0
	put := func(mem int) {
		c.Put([]byte(fmt.Sprintf("k%d", id)), mustAdMem(mem))
		id++
	}
	for i := 0; i < lowN; i++ {
		put(512 + i%3585) // 512..4096, all <= 4096
	}
	for i := 0; i < highN; i++ {
		put(4097 + i%28671) // 4097..32768, all > 4096
	}
	for i := 0; i < huge; i++ {
		put(786432) // 768 GiB outliers: stretch max, break the uniform assumption
	}
	total := lowN + highN + huge
	return float64(highN+huge) / float64(total)
}

func mustAdMem(mem int) *classad.ClassAd {
	ad, err := classad.Parse(fmt.Sprintf(`[ Memory = %d ]`, mem))
	if err != nil {
		panic(err)
	}
	return ad
}

// selForOp returns the ExplainQuery selectivity estimate for a single-probe query, requiring
// the probe to be index-usable with a selectivity.
func selForOp(t *testing.T, c *Collection, query string) float64 {
	t.Helper()
	ex := c.ExplainQuery(mustQuery(t, query))
	if len(ex.Probes) != 1 || !ex.Probes[0].HasSelectivity {
		t.Fatalf("%q: expected one probe with selectivity, got %+v", query, ex.Probes)
	}
	return ex.Probes[0].Selectivity
}

// TestHistogramSkewedRangeEstimate is the regression for the wrong-stats bug: with a skewed
// Memory distribution, the old uniform-[min,max] model estimates `Memory > 4096` at ~99.5%,
// while the true fraction is ~42%. The equi-depth histogram must estimate close to the truth.
func TestHistogramSkewedRangeEstimate(t *testing.T) {
	// The selectivity estimator reads sealed-segment indexes, so persist + Reindex to seal.
	c, err := Open(Options{Dir: t.TempDir(), ValueAttrs: []string{"Memory"}, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	trueHigh := putSkewedMemory(c, 7957, 5872, 10) // ~0.425 of records have Memory > 4096
	c.Reindex()

	// What the OLD uniform model would say, for the record: 1 - (4096-min)/(max-min).
	naiveHigh := 1 - (4096.0-512)/(786432.0-512)
	if naiveHigh < 0.95 {
		t.Fatalf("test data not skewed enough: naive estimate %.3f", naiveHigh)
	}

	gotHigh := selForOp(t, c, "Memory > 4096")
	gotLow := selForOp(t, c, "Memory <= 4096")

	if math.Abs(gotHigh-trueHigh) > 0.06 {
		t.Errorf("Memory > 4096 selectivity = %.3f, want ~%.3f (true); naive model would say %.3f", gotHigh, trueHigh, naiveHigh)
	}
	if math.Abs(gotLow-(1-trueHigh)) > 0.06 {
		t.Errorf("Memory <= 4096 selectivity = %.3f, want ~%.3f", gotLow, 1-trueHigh)
	}
	// The whole point: the histogram is nowhere near the broken uniform estimate.
	if gotHigh > 0.7 {
		t.Errorf("Memory > 4096 selectivity %.3f is still near the broken uniform estimate %.3f", gotHigh, naiveHigh)
	}
}

// TestHistogramSurvivesReopen verifies the histogram is serialized into the sealed-segment
// sidecar (v9) and reconstructed on reopen, so the skew-aware estimate persists.
func TestHistogramSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, ValueAttrs: []string{"Memory"}, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	trueHigh := putSkewedMemory(c, 4000, 3000, 8)
	c.Reindex() // seal + write v9 sidecars
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := Open(Options{Dir: dir, ValueAttrs: []string{"Memory"}, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got := selForOp(t, d, "Memory > 4096")
	if math.Abs(got-trueHigh) > 0.08 {
		t.Errorf("after reopen, Memory > 4096 selectivity = %.3f, want ~%.3f (histogram lost across the sidecar?)", got, trueHigh)
	}
}

// TestValHistogramSerializationRoundTrip round-trips a segStats histogram through the sidecar
// stats block encoder/decoder and checks the CDF is preserved at several thresholds.
func TestValHistogramSerializationRoundTrip(t *testing.T) {
	keys := make([]float64, 0, 500)
	counts := make([]uint64, 0, 500)
	var total uint64
	for v := 0; v < 500; v++ {
		keys = append(keys, float64(v))
		c := uint64(1 + v%7) // varied counts
		counts = append(counts, c)
		total += c
	}
	s := &segStats{hasRange: true, covered: total, hist: buildValHistogram(keys, counts, total)}
	if s.hist == nil {
		t.Fatal("nil histogram")
	}
	b := appendSegStats(nil, s)
	got := readSegStats(b, 0)
	if got.hist == nil {
		t.Fatal("histogram not decoded")
	}
	for _, tv := range []float64{-10, 0, 50, 123, 250, 499, 1000} {
		a := s.hist.cdf(tv)
		bb := got.hist.cdf(tv)
		if math.Abs(a-bb) > 1e-9 {
			t.Errorf("cdf(%.0f): before %.6f, after %.6f", tv, a, bb)
		}
	}
}

// TestValHistogramCDFProperties verifies the CDF is well-formed: exact at every bucket top
// (cum/total), 0 below the low edge, 1 at/above the max, and monotonic non-decreasing.
func TestValHistogramCDFProperties(t *testing.T) {
	// Heavy per-value counts (each >= the bucket target) so every value forms its own bucket
	// and the bucket tops are exact cumulative fractions.
	keys := []float64{10, 20, 30}
	counts := []uint64{200, 900, 8900} // total 10000
	h := buildValHistogram(keys, counts, 10000)
	if h == nil {
		t.Fatal("nil histogram")
	}
	// Exact at each bucket top above the low edge (frac reaches 1 at the boundary). The first
	// boundary coincides with the low edge, where cdf is 0 by convention (see cdf's comment).
	for i, b := range h.bound {
		if b == h.lo {
			if got := h.cdf(b); got != 0 {
				t.Errorf("cdf(low edge %.0f) = %.4f, want 0", b, got)
			}
			continue
		}
		want := float64(h.cum[i]) / 10000
		if got := h.cdf(b); math.Abs(got-want) > 1e-9 {
			t.Errorf("cdf(bound %.0f) = %.4f, want %.4f", b, got, want)
		}
	}
	if got := h.cdf(5); got != 0 {
		t.Errorf("cdf below low edge = %.4f, want 0", got)
	}
	if got := h.cdf(1000); got != 1 {
		t.Errorf("cdf above max = %.4f, want 1", got)
	}
	// Monotonic non-decreasing across the range.
	prev := -1.0
	for x := 0.0; x <= 40; x++ {
		v := h.cdf(x)
		if v < prev-1e-12 || v < 0 || v > 1 {
			t.Fatalf("cdf not monotone/clamped at %.0f: %.4f (prev %.4f)", x, v, prev)
		}
		prev = v
	}
	// estRange ops are complementary and clamped.
	if lo, hi := h.estRange("<=", 20), h.estRange(">", 20); math.Abs(lo+hi-1) > 1e-9 {
		t.Errorf("<=20 (%.4f) + >20 (%.4f) != 1", lo, hi)
	}
}
