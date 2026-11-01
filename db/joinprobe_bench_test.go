package db

import (
	"fmt"
	"testing"
)

// Where does narrowing the hashed side's scan stop paying?
//
// The claim behind an index-based equi join is that a hash join reads one side whole to
// build buckets a selective other side will mostly never probe, and that an index can read
// only the rows whose key is actually wanted. That claim has a limit, and this finds it: the
// row count is held fixed and the key count swept, so the only thing changing is how many
// index lookups the probe costs against the same scan it avoids.
//
// Both arms run the same join over the same data and differ only in whether the probe is
// allowed, and the row counts are compared before anything is timed.
func BenchmarkJoinIndexProbeKeySweep(b *testing.B) {
	const big = 8192
	for _, keys := range []int{16, 32, 64, 128, 256} {
		cat := probeFixture(b, big, keys, true)
		spec := probeSpec(JoinInner)
		segs := 0
		if a, ok := cat.ArchiveTable("hist"); ok {
			segs = a.Stats().Segments
		}

		count := func() (int, *JoinStats) {
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
			return n, &st
		}
		var want, got int
		var fst *JoinStats
		forcingProbe(func() { want, fst = count() })
		withoutProbe(func() { got, _ = count() })
		if got != want {
			b.Fatalf("keys=%d: probe joined %d rows, full scan %d", keys, want, got)
		}
		if fst.ProbeKeys != keys {
			b.Fatalf("keys=%d: forced probe reported %d keys (%q); it did not run",
				keys, fst.ProbeKeys, fst.ProbeWhy)
		}

		// Three arms: the probe forced on at every key count, which is what locates the
		// crossover; the full scan it is compared against; and the gated plan, which shows
		// whether the gate lands on the right side of that crossover.
		for _, arm := range []struct {
			name string
			wrap func(func())
		}{
			{"probe-forced", forcingProbe},
			{"full-scan", withoutProbe},
			{"as-planned", func(f func()) { f() }},
		} {
			b.Run(fmt.Sprintf("keys=%d/segs=%d/%s", keys, segs, arm.name), func(b *testing.B) {
				arm.wrap(func() {
					for i := 0; i < b.N; i++ {
						count()
					}
				})
			})
		}
	}
}
