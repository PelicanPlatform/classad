package db

import "testing"

// BenchmarkJoinBuildSide measures what exchanging the sides is worth when no index
// separates the two reads: both orientations read the same records, and what differs is
// which side is HELD.
//
// The reads are a wash: the same records are touched whichever way round. What is not a
// wash, and what this measures, is the hash -- hashRows, B/op, and the time it takes to
// build 40000 entries rather than 64 and probe them rather than be probed. Inserting costs
// more than looking up, so the orientation that holds less is also the faster one here.
//
// Arms alternate, and each asserts it took the path its name claims: two arms that silently
// ran the same plan report a clean 1.00x and mean nothing.
func BenchmarkJoinBuildSide(b *testing.B) {
	cat := buildSideFixture(b, 64, 40000, false)
	spec := bsSpec("small", "big") // small written first: the hash would hold the big side

	run := func(b *testing.B, disabled bool) {
		forceNoBuildSideSwap = disabled
		defer func() { forceNoBuildSideSwap = false }()
		var swaps, held int
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var st JoinStats
			seq, err := cat.Join(spec, JoinLimits{}, &st)
			if err != nil {
				b.Fatal(err)
			}
			for range seq {
			}
			if st.BuildSideSwapped {
				swaps++
			}
			held = st.HashRows
		}
		b.StopTimer()
		switch {
		case disabled && swaps > 0:
			b.Fatalf("the disabled arm exchanged the sides %d times", swaps)
		case !disabled && swaps != b.N:
			b.Fatalf("the chosen arm exchanged the sides %d of %d times", swaps, b.N)
		}
		b.ReportMetric(float64(held), "hashRows")
	}

	for _, r := range []string{"r0", "r1"} {
		arms := []string{"chosen", "as-written"}
		if r == "r1" {
			arms[0], arms[1] = arms[1], arms[0]
		}
		for _, arm := range arms {
			b.Run(r+"/"+arm, func(b *testing.B) { run(b, arm == "as-written") })
		}
	}
}
