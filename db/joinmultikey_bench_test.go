package db

import "testing"

// What narrowing a composite-key join is worth.
//
// Both arms are the same join over the same data; one has the probe forced off, which is
// what this join got before a step with several equalities could be narrowed at all. The
// row counts are compared before anything is timed.
func BenchmarkJoinCompositeKey(b *testing.B) {
	cat := multiKeyFixture(b, 8192, 32)
	spec := multiKeySpec()
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
	withoutProbe(func() { want, _ = count() })
	got, _ = count()
	if got != want {
		b.Fatalf("narrowed join returned %d rows, the full scan %d", got, want)
	}

	for _, arm := range []struct {
		name string
		wrap func(func())
	}{
		{"full-scan", withoutProbe},
		{"narrowed", func(f func()) { f() }},
	} {
		b.Run(arm.name, func(b *testing.B) {
			var last *JoinStats
			arm.wrap(func() {
				for i := 0; i < b.N; i++ {
					_, last = count()
				}
			})
			b.ReportMetric(float64(last.HashRows), "hashRows")
			b.ReportMetric(float64(last.ProbeKeys), "probeKeys")
		})
	}
}
