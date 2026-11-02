package db

import (
	"fmt"
	"testing"
)

// BenchmarkJoinRowForm is the A/B for carrying a side's rows as bytes: the same join, both
// ways, in one process.
//
// The arms alternate order round to round, because a Go benchmark's first arm warms the page
// cache and the allocator for the second and a fixed order hands that advantage to whichever
// runs last. Comparing one arm here against a run on another branch instead would compare a
// cold process against a warm one, which has already produced one wrong answer in this
// package.
func BenchmarkJoinRowForm(b *testing.B) {
	const jobs = 2000
	cat := memEvolveFixture(b, jobs)
	spec := memEvolveSpec()

	run := func(b *testing.B, decoded bool) {
		forceDecodedRows = decoded
		defer func() { forceDecodedRows = false }()
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			var st JoinStats
			seq, err := cat.Join(spec, JoinLimits{}, &st)
			if err != nil {
				b.Fatal(err)
			}
			n := 0
			for range seq {
				n++
			}
			if st.Err != nil {
				b.Fatal(st.Err)
			}
			if n != jobs*memEpochs {
				b.Fatalf("got %d rows, want %d", n, jobs*memEpochs)
			}
		}
	}

	for round := 0; round < 6; round++ {
		arms := []struct {
			name    string
			decoded bool
		}{{"decoded-rows", true}, {"wire-rows", false}}
		if round%2 == 1 {
			arms[0], arms[1] = arms[1], arms[0]
		}
		for _, a := range arms {
			b.Run(fmt.Sprintf("r%d/%s", round, a.name), func(b *testing.B) { run(b, a.decoded) })
		}
	}
}
