package db

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// Where the two paths actually cross, measured rather than assumed.
//
// The batched path's default engages only where holding the inner side is over budget, and
// the hash keeps everything else. That division was an argument, not a measurement: the
// planner cannot count the FROM side's distinct keys without holding the FROM side, and I
// took the planner's inability to decide cheaply as a reason to leave the incumbent in
// place. Those are different claims. Nothing stops a benchmark deciding offline what the
// planner cannot decide online, and this is that benchmark.
//
// It sweeps the FROM side's size against a fixed inner side, which is the axis the
// comparison turns on: a batched lookup costs per batch of FROM rows, a hash costs once for
// the whole inner side however few FROM rows probe it.
func TestOuterDrivenCrossoverAgainstTheHash(t *testing.T) {
	if testing.Short() {
		t.Skip("times two join paths over several FROM-side sizes")
	}
	for _, inner := range []int{20_000, 80_000} {
		crossoverArm(t, inner)
	}
}

// crossoverArm sweeps the FROM side against one inner size. Run at two inner sizes, it says
// whether the crossover is a RATIO of the two sides or an absolute FROM-side count -- which
// is the difference between a threshold that travels and one tuned to a fixture.
func crossoverArm(t *testing.T, inner int) {
	cat := outerFixture(t, inner, inner, 0, true)
	const rounds = 5

	run := func(mode outerDrivenMode, limit int) (time.Duration, int, int, int) {
		spec := outerSpec(JoinInner)
		spec.Left.Constraint = fmt.Sprintf("ClusterId <= %d", limit)
		var st JoinStats
		var d time.Duration
		rows := 0
		withOuterDrivenMode(mode, func() {
			seq, err := cat.Join(spec, JoinLimits{}, &st)
			if err != nil {
				t.Fatalf("Join: %v", err)
			}
			start := time.Now()
			for range seq {
				rows++
			}
			d = time.Since(start)
		})
		if st.Err != nil {
			t.Fatalf("join stopped early: %v", st.Err)
		}
		return d, rows, st.OuterDrivenBatches, st.ProbeKeys
	}

	type key struct {
		mode outerDrivenMode
		from int
	}
	samples := map[key][]time.Duration{}
	matched := map[key]int{}
	batched := map[key]int{}
	probed := map[key]int{}
	sizes := []int{64, 256, 1024, inner / 20, inner / 5, inner * 4 / 5}
	modes := []outerDrivenMode{outerDrivenNever, outerDrivenForce}

	for r := 0; r < rounds; r++ {
		order := modes
		if r%2 == 1 { // flip the arm order every other round
			order = []outerDrivenMode{outerDrivenForce, outerDrivenNever}
		}
		for _, n := range sizes {
			for _, m := range order {
				d, rows, nb, pk := run(m, n)
				k := key{m, n}
				batched[k] = nb
				probed[k] = pk
				samples[k] = append(samples[k], d)
				if prev, ok := matched[k]; ok && prev != rows {
					t.Fatalf("mode %d from=%d matched %d rows this round and %d last",
						m, n, rows, prev)
				}
				matched[k] = rows
			}
		}
	}

	fmt.Printf("\n=== inner side %d rows, one match per key ===\n", inner)
	fmt.Printf("%8s %10s %8s %9s %12s %12s %10s\n", "FROM", "FROM/inner", "rows", "probeKeys", "hash", "batched", "batched/hash")
	for _, n := range sizes {
		hk, bk := key{outerDrivenNever, n}, key{outerDrivenForce, n}
		// An arm that matched fewer rows did less work, and comparing the times would
		// report that as a win.
		if matched[hk] != matched[bk] {
			t.Fatalf("from=%d: hash matched %d rows, batched %d; the arms are not comparable",
				n, matched[hk], matched[bk])
		}
		if matched[hk] == 0 {
			t.Fatalf("from=%d matched no rows; the arms measure nothing", n)
		}
		// Provenance, not timing. Two arms that both ran the hash produce a tidy 1.00x and
		// say nothing at all, which is how a declined path reports itself as a tie.
		if batched[bk] == 0 {
			t.Fatalf("from=%d: the batched arm ran 0 batches, so it is the hash arm twice", n)
		}
		if batched[hk] != 0 {
			t.Fatalf("from=%d: the hash arm ran %d batches", n, batched[hk])
		}
		h, b := medianDur(samples[hk]), medianDur(samples[bk])
		fmt.Printf("%8d %9.1f%% %8d %9d %12v %12v %9.2fx\n", n,
			100*float64(n)/float64(inner), matched[hk], probed[hk], h, b,
			float64(b.Nanoseconds())/float64(h.Nanoseconds()))
	}
}

func medianDur(ds []time.Duration) time.Duration {
	c := append([]time.Duration(nil), ds...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}
