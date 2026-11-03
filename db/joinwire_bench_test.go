package db

import (
	"fmt"
	"testing"
)

// BenchmarkJoinRowForm compares the two row forms on the shape the join is wanted for, both
// in one process with the arms alternating round to round -- a Go benchmark's first arm warms
// the page cache and the allocator for the second, and a fixed order hands that advantage to
// whichever runs last.
//
// The ads arm is what a caller that wants objects pays. The wire arm is what a caller that
// relays pays, and it is the one a joined row crossing a wire should be built from: it never
// renders text and the consumer never parses any.
func BenchmarkJoinRowFormWire(b *testing.B) {
	const jobs = 2000
	cat := memEvolveFixture(b, jobs)
	spec := memEvolveSpec()
	want := jobs * memEpochs

	ads := func(b *testing.B) {
		b.ReportAllocs()
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
			if n != want {
				b.Fatalf("got %d rows, want %d", n, want)
			}
		}
	}
	wireRowsArm := func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var st JoinStats
			seq, err := cat.JoinWire(spec, JoinLimits{}, false, &st)
			if err != nil {
				b.Fatal(err)
			}
			n, bytes := 0, 0
			for row := range seq {
				bytes += len(row)
				n++
			}
			if st.Err != nil {
				b.Fatal(st.Err)
			}
			if n != want || bytes == 0 {
				b.Fatalf("got %d rows / %d bytes, want %d rows", n, bytes, want)
			}
		}
	}

	for round := 0; round < 4; round++ {
		arms := []struct {
			name string
			fn   func(*testing.B)
		}{{"as-ads", ads}, {"as-wire-rows", wireRowsArm}}
		if round%2 == 1 {
			arms[0], arms[1] = arms[1], arms[0]
		}
		for _, a := range arms {
			b.Run(fmt.Sprintf("r%d/%s", round, a.name), a.fn)
		}
	}
}
