package collections

import (
	"fmt"
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestConjunctSelectivityProperties pins the invariants the damped combiner must hold,
// independently of how well it happens to estimate any particular corpus.
func TestConjunctSelectivityProperties(t *testing.T) {
	t.Parallel()
	cases := [][]float64{
		{}, {0.5}, {1, 1, 1}, {0, 0.5}, {0.5, 0},
		{0.9, 0.9, 0.9}, {0.001, 0.9}, {0.3, 0.3, 0.3, 0.3, 0.3},
		{0.25, 0.5, 0.75}, {0.999, 0.999, 0.999, 0.999, 0.999, 0.999, 0.999, 0.999},
	}
	for _, sel := range cases {
		name := fmt.Sprint(sel)
		got := conjunctSelectivity(append([]float64(nil), sel...))
		if got < 0 || got > 1 {
			t.Errorf("%s: selectivity %v out of [0,1]", name, got)
		}
		if len(sel) == 0 {
			continue
		}
		// A conjunction cannot match more than its most selective conjunct does.
		lo := math.Inf(1)
		prod := 1.0
		for _, s := range sel {
			lo = math.Min(lo, s)
			prod *= s
		}
		if got > lo+1e-12 {
			t.Errorf("%s: %v exceeds the most selective conjunct %v", name, got, lo)
		}
		// Damping only ever raises the independence product; it never claims MORE
		// pruning than independence would.
		if got < prod-1e-12 {
			t.Errorf("%s: %v is below the independence product %v", name, got, prod)
		}
		// Order must not matter (the combiner sorts).
		rev := make([]float64, len(sel))
		for i := range sel {
			rev[i] = sel[len(sel)-1-i]
		}
		if r := conjunctSelectivity(rev); math.Abs(r-got) > 1e-12 {
			t.Errorf("%s: order-dependent (%v vs %v)", name, got, r)
		}
	}

	// Monotone: adding a conjunct never raises the estimate.
	base := []float64{0.4, 0.6}
	with := []float64{0.4, 0.6, 0.7}
	if a, b := conjunctSelectivity(append([]float64(nil), base...)),
		conjunctSelectivity(append([]float64(nil), with...)); b > a+1e-12 {
		t.Errorf("adding a conjunct raised the estimate: %v -> %v", a, b)
	}
	// An impossible conjunct still yields zero.
	if got := conjunctSelectivity([]float64{0, 0.9, 0.9}); got != 0 {
		t.Errorf("a zero-selectivity conjunct must zero the conjunction, got %v", got)
	}
	// The most selective conjunct counts in full: with one very selective probe the
	// estimate stays near it rather than collapsing further.
	if got := conjunctSelectivity([]float64{0.001, 0.9, 0.9}); got > 0.001 || got < 0.0005 {
		t.Errorf("dominant conjunct not preserved: %v", got)
	}
}

// TestConjunctSelectivityOnCorrelatedAds is the reason the damping exists: the ads this
// store holds have strongly correlated attributes -- a machine's Memory, Cpus and Disk all
// scale with its size -- so multiplying per-probe selectivities compounds an error once per
// conjunct. The damped combiner must land materially closer to the truth.
func TestConjunctSelectivityOnCorrelatedAds(t *testing.T) {
	t.Parallel()
	// Six machine shapes; every attribute is a function of the shape, so the three
	// conjuncts below select the same slots rather than independent thirds of the pool.
	shapes := [][3]int{
		{2048, 1, 20000}, {4096, 2, 40000}, {8192, 4, 80000},
		{16384, 8, 160000}, {65536, 32, 640000}, {262144, 128, 2560000},
	}
	const n = 6000
	c := New(Options{Shards: 4, ValueAttrs: []string{"Memory", "Cpus", "Disk"}})
	for i := 0; i < n; i++ {
		s := shapes[i%len(shapes)]
		if err := c.Put([]byte(fmt.Sprintf("s%d", i)), mustAd(t, fmt.Sprintf(
			`[ Id=%d; Memory=%d; Cpus=%d; Disk=%d ]`, i, s[0], s[1], s[2]))); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	for _, qs := range []string{
		`Memory >= 16384 && Cpus >= 8`,
		`Memory >= 16384 && Cpus >= 8 && Disk >= 160000`,
		`Memory >= 4096 && Cpus >= 2 && Disk >= 40000`,
	} {
		q := mustQuery(t, qs)
		actual := 0
		for range c.Query(q) {
			actual++
		}
		want := float64(actual) / n

		sel := probeSelectivities(t, c, q, n)
		if len(sel) < 2 {
			t.Fatalf("%s: expected several usable probes, got %d", qs, len(sel))
		}
		product := 1.0
		for _, s := range sel {
			product *= s
		}
		damped := conjunctSelectivity(append([]float64(nil), sel...))

		if math.Abs(damped-want) >= math.Abs(product-want) {
			t.Errorf("%s: damped %.4f is no closer to the actual %.4f than the product %.4f",
				qs, damped, want, product)
		}
		t.Logf("%s: actual %.3f, product %.4f, damped %.4f", qs, want, product, damped)
	}
}

// TestConjunctSelectivityKeepsIndependentGate is the regression guard for the trade the
// damping makes: it RAISES estimates, and the pushdown gate refuses to push down above
// maxPushdownFrac. On uncorrelated attributes that must not start refusing pushdowns that
// the independence product would have allowed.
func TestConjunctSelectivityKeepsIndependentGate(t *testing.T) {
	t.Parallel()
	// Three attributes cycling on coprime periods: as close to independent as a
	// deterministic corpus gets.
	const n = 6000
	c := New(Options{Shards: 4, ValueAttrs: []string{"A", "B", "D"}})
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("s%d", i)), mustAd(t, fmt.Sprintf(
			`[ Id=%d; A=%d; B=%d; D=%d ]`, i, i%7, (i/7)%11, (i/77)%13))); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	for _, qs := range []string{
		`A >= 4 && B >= 6`,
		`A >= 4 && B >= 6 && D >= 7`,
		`A >= 1 && B >= 1 && D >= 1`,
	} {
		q := mustQuery(t, qs)
		sel := probeSelectivities(t, c, q, n)
		product := 1.0
		for _, s := range sel {
			product *= s
		}
		damped := conjunctSelectivity(append([]float64(nil), sel...))
		if damped > maxPushdownFrac && product <= maxPushdownFrac {
			t.Errorf("%s: damping pushed the estimate over the gate (%.4f > %.2f) where the "+
				"product (%.4f) would have pushed down", qs, damped, maxPushdownFrac, product)
		}
	}
}

// probeSelectivities returns each usable probe's estimated selectivity for q, as a fraction
// of total.
func probeSelectivities(t *testing.T, c *Collection, q *vm.Query, total int) []float64 {
	t.Helper()
	var sel []float64
	for _, up := range c.planIndex(q.Probes()) {
		cand, covered := c.estimateCandidates(up)
		if !covered {
			t.Fatal("probe has no selectivity estimate")
		}
		sel = append(sel, math.Min(1, cand/float64(total)))
	}
	return sel
}
