package db

import "testing"

// BenchmarkTupleProbe times a composite-key join narrowed on the tuple against the same join
// narrowed on each attribute separately.
//
// The fixture is deliberately the shape the composite is for: both halves of the key fan
// out, so the conjunction of in-chains admits a cross product several times the size of the
// pair set the FROM side actually holds. A fixture where either attribute takes one value
// would measure nothing -- the cross product is then the pair set, and the two forms do the
// same work.
//
// Arms are built fresh per iteration because the fixture is the dominant cost of a run;
// b.ResetTimer after the build would time the join alone but hide which build the join is
// reading, and the sealed sidecars differ between the two.
func BenchmarkTupleProbe(b *testing.B) {
	for _, arm := range []struct {
		name      string
		composite bool
		tuple     bool
	}{
		// The per-attribute arm asks for that form explicitly rather than relying on the
		// cost gate to decline the tuple one, so the comparison does not change meaning if
		// the gate is ever retuned.
		{"per-attribute", false, false},
		{"tuple-composite", true, true},
	} {
		b.Run(arm.name, func(b *testing.B) {
			cat := sparseTupleFixture(b, 8192, 64, arm.composite)
			var last JoinStats
			run := func(fn func()) { fn() }
			if !arm.tuple {
				run = withoutTupleProbe
			}
			b.ResetTimer()
			for b.Loop() {
				var st JoinStats
				run(func() {
					seq, err := cat.Join(sparseTupleSpec(), JoinLimits{}, &st)
					if err != nil {
						b.Fatal(err)
					}
					for range seq {
					}
				})
				if st.Err != nil {
					b.Fatal(st.Err)
				}
				last = st
			}
			b.StopTimer()
			// Reported so a regression that silently stopped narrowing is visible as more
			// than a timing change.
			b.ReportMetric(float64(last.HashRows), "hashed")
		})
	}
}
