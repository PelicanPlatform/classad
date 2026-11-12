package db

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// outerFixture builds the shape the outer-driven path is for: a large indexed inner table
// and a FROM side streamed against it. `missing` FROM rows name clusters the inner table
// does not hold, so a LEFT join has something to null-extend and an INNER join has
// something to drop -- without them both kinds would agree for the wrong reason.
func outerFixture(t testing.TB, big, small, missing int, index bool) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	// Small segments on purpose: postings are built when a segment seals, so a fixture
	// whose rows all sit in one open segment has the index configured and none built.
	cfg := ArchiveConfig{SegmentSize: 4096}
	if index {
		cfg.ValueAttrs = []string{"ClusterId"}
	}
	hist, err := cat.CreateArchiveTable("hist", cfg)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= big; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("ExitCode", int64(i%7))
		ad.InsertAttrString("Pad", strings.Repeat("x", 64))
		if aerr := hist.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	stride := max(big/small, 1)
	for i := stride; i <= big; i += stride {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttrString("Owner", "alice")
		if aerr := jobs.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	for i := 0; i < missing; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(big+1+i))
		ad.InsertAttrString("Owner", "bob")
		if aerr := jobs.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	return cat
}

func outerSpec(kind JoinKind) JoinSpec {
	return JoinSpec{
		Kind:  kind,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "hist", Alias: "h"},
		Cond:  []string{"j.ClusterId == h.ClusterId"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
}

// withOuterDriven turns the gated path on for one call. The gate is the production default
// (see joinOuterDrivenEnabled), so every test of this path has to set it.
func withOuterDriven(fn func()) { withOuterDrivenMode(outerDrivenForce, fn) }

// withoutOuterDriven measures what the join does with the path declined, which is how a
// test establishes that the hash arm it is comparing against really is the hash arm.
func withoutOuterDriven(fn func()) { withOuterDrivenMode(outerDrivenNever, fn) }

func withOuterDrivenMode(m outerDrivenMode, fn func()) {
	old := joinOuterDrivenMode
	joinOuterDrivenMode = m
	defer func() { joinOuterDrivenMode = old }()
	fn()
}

// withBatchSize runs fn with a different batch size, so the batch boundary can be moved
// across the data rather than tested only where it happens to fall.
func withBatchSize(n int, fn func()) {
	old := joinBatchKeys
	joinBatchKeys = n
	defer func() { joinBatchKeys = old }()
	fn()
}

// drainOrdered collects the joined rows IN ORDER. Order is part of the answer here -- the
// join's output order is the FROM side's, and a path that batches the FROM side is exactly
// the kind that could lose it -- so nothing is sorted.
//
// It does not fail on stats.Err: one test is about a join that is supposed to stop.
func drainOrdered(t *testing.T, cat *Catalog, spec JoinSpec, lim JoinLimits) ([]string, *JoinStats) {
	t.Helper()
	var st JoinStats
	seq, err := cat.Join(spec, lim, &st)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	var out []string
	for row := range seq {
		out = append(out, evalText(row, "j.ClusterId")+"/"+evalText(row, "h.ExitCode"))
	}
	return out, &st
}

// TestOuterDrivenAgreesWithTheHashJoin is the correctness bar: the batched path must return
// exactly what the hash path returns, in the same order, for both join kinds.
func TestOuterDrivenAgreesWithTheHashJoin(t *testing.T) {
	for _, kind := range []JoinKind{JoinInner, JoinLeft} {
		t.Run(fmt.Sprintf("kind%d", kind), func(t *testing.T) {
			cat := outerFixture(t, 2048, 128, 17, true)
			// The baseline is the hash path, and it is asked for explicitly: the
			// default takes this fixture now, because the probe declines on it (145
			// keys reaching 145 records of 2,048) and the floor lifts where there is
			// no probe below it.
			var want []string
			var wst *JoinStats
			withoutOuterDriven(func() { want, wst = drainOrdered(t, cat, outerSpec(kind), JoinLimits{}) })
			if wst.Err != nil {
				t.Fatalf("hash join stopped early: %v", wst.Err)
			}
			if wst.OuterDrivenBatches != 0 {
				t.Fatalf("the baseline ran batched (%d); it is supposed to be the hash path",
					wst.OuterDrivenBatches)
			}
			if len(want) == 0 {
				t.Fatal("the baseline joined no rows, so agreeing with it proves nothing")
			}

			var got []string
			var gst *JoinStats
			withOuterDriven(func() { got, gst = drainOrdered(t, cat, outerSpec(kind), JoinLimits{}) })
			if gst.Err != nil {
				t.Fatalf("outer-driven join stopped early: %v", gst.Err)
			}
			if gst.OuterDrivenBatches == 0 {
				t.Skipf("the outer-driven path declined this plan (kind=%d); "+
					"there is nothing to compare", kind)
			}
			if len(got) != len(want) {
				t.Fatalf("outer-driven returned %d rows, hash returned %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("row %d: outer-driven %q, hash %q (order is part of the answer)",
						i, got[i], want[i])
				}
			}
		})
	}
}

// TestOuterDrivenHoldsOneBatchNotTheTable pins the reason this path exists. The hash path's
// hold is the inner table; this path's is one batch's matches, and the gap between them is
// the whole point.
func TestOuterDrivenHoldsOneBatchNotTheTable(t *testing.T) {
	const big, small = 4096, 256
	cat := outerFixture(t, big, small, 0, true)
	// Explicitly the hash path: the default takes this fixture now (the probe declines on
	// it), and what the hash HOLDS is the comparison this test exists for.
	var wst *JoinStats
	withoutOuterDriven(func() { _, wst = drainOrdered(t, cat, outerSpec(JoinInner), JoinLimits{}) })
	if wst.HashRows < big {
		t.Fatalf("the hash path held %d rows of a %d-row inner table; the fixture is not "+
			"exercising a whole-side hold", wst.HashRows, big)
	}

	var gst *JoinStats
	withOuterDriven(func() { _, gst = drainOrdered(t, cat, outerSpec(JoinInner), JoinLimits{}) })
	if gst.OuterDrivenBatches == 0 {
		t.Fatal("the outer-driven path declined; this test measures what it holds")
	}
	// One batch covers joinBatchKeys FROM rows, and this fixture's inner side holds one row
	// per key, so the high-water hold is bounded by the batch. The bound is generous on
	// purpose: the claim is that the hold is a function of the BATCH and not of the table.
	if gst.HashRows > joinBatchKeys {
		t.Errorf("outer-driven held %d rows at its peak, more than one batch (%d)",
			gst.HashRows, joinBatchKeys)
	}
	if gst.HashRows >= wst.HashRows {
		t.Errorf("outer-driven held %d rows, the hash path %d; it held no less",
			gst.HashRows, wst.HashRows)
	}
}

// TestOuterDrivenRunsAJoinTheHashRefuses is the headline. MaxHashRows is the one place a
// join fails rather than running a worse plan, and a path that holds a batch does not meet
// it at all.
func TestOuterDrivenRunsAJoinTheHashRefuses(t *testing.T) {
	const big, small = 4096, 128
	cat := outerFixture(t, big, small, 0, true)
	lim := JoinLimits{MaxHashRows: big / 2}

	var wst *JoinStats
	withoutOuterDriven(func() { _, wst = drainOrdered(t, cat, outerSpec(JoinInner), lim) })
	if wst.Err == nil {
		t.Fatalf("the hash path completed under MaxHashRows=%d; the limit is not binding "+
			"and this test proves nothing", lim.MaxHashRows)
	}

	var got []string
	var gst *JoinStats
	withOuterDriven(func() { got, gst = drainOrdered(t, cat, outerSpec(JoinInner), lim) })
	if gst.Err != nil {
		t.Fatalf("outer-driven hit the same limit: %v", gst.Err)
	}
	if gst.OuterDrivenBatches == 0 {
		t.Fatal("the outer-driven path declined, so it did not run the refused join")
	}
	if len(got) != small {
		t.Errorf("outer-driven joined %d rows, want %d", len(got), small)
	}
}

// TestOuterDrivenCrossesBatchBoundaries moves the boundary across the data instead of
// testing only where it happens to land. A size that does not divide the FROM side leaves a
// short final batch, which is the case a flush-on-full loop drops.
func TestOuterDrivenCrossesBatchBoundaries(t *testing.T) {
	cat := outerFixture(t, 1024, 97, 11, true)
	want, wst := drainOrdered(t, cat, outerSpec(JoinInner), JoinLimits{})
	if wst.Err != nil {
		t.Fatal(wst.Err)
	}
	for _, n := range []int{1, 2, 7, 64, 1000} {
		t.Run(fmt.Sprintf("batch%d", n), func(t *testing.T) {
			var got []string
			var gst *JoinStats
			withBatchSize(n, func() {
				withOuterDriven(func() { got, gst = drainOrdered(t, cat, outerSpec(JoinInner), JoinLimits{}) })
			})
			if gst.OuterDrivenBatches == 0 {
				t.Fatal("the outer-driven path declined")
			}
			if len(got) != len(want) {
				t.Fatalf("batch=%d returned %d rows, want %d", n, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("batch=%d row %d: %q, want %q", n, i, got[i], want[i])
				}
			}
		})
	}
}

// TestOuterDrivenDeclinesWithoutAValueIndex covers the gate that keeps this path from
// turning into a repeated full scan: with no index on the key, every batch would walk the
// whole inner side.
func TestOuterDrivenDeclinesWithoutAValueIndex(t *testing.T) {
	cat := outerFixture(t, 512, 32, 0, false)
	var gst *JoinStats
	withOuterDriven(func() { _, gst = drainOrdered(t, cat, outerSpec(JoinInner), JoinLimits{}) })
	if gst.OuterDrivenBatches != 0 {
		t.Errorf("the path ran %d batches against a table with no value index on the key",
			gst.OuterDrivenBatches)
	}
}

// stringKeyFixture joins on a STRING attribute that is named in ValueAttrs. The archive
// accepts that, and reports the attribute as indexed through both IndexedAttrs and
// ZoneAttrs, while every query on it visits each record -- a value index serves numbers.
//
// So the declared index set says "indexed" and the truth is "scanned", and a path that
// gated on the declaration would admit this join.
func stringKeyFixture(t testing.TB, big, small int) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateArchiveTable("hist", ArchiveConfig{
		SegmentSize: 4096,
		ValueAttrs:  []string{"ClusterId", "Owner"}, // Owner holds strings
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= big; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("ExitCode", int64(i%7))
		ad.InsertAttrString("Owner", fmt.Sprintf("u%d", i%small))
		ad.InsertAttrString("Pad", strings.Repeat("x", 64))
		if aerr := hist.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	for i := 0; i < small; i++ {
		ad := classad.New()
		ad.InsertAttrString("Owner", fmt.Sprintf("u%d", i))
		if aerr := jobs.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	return cat
}

// TestOuterDrivenDeclinesAStringKeyDeclaredAsAValueAttr is a WRONG-ANSWER regression, not a
// performance one. This path folds a batch's keys into the numeric space the hash's bucket
// key uses; a batch of string keys folds to no keys at all, so the lookup it would run is
// empty and the join would return nothing while reporting that it ran.
func TestOuterDrivenDeclinesAStringKeyDeclaredAsAValueAttr(t *testing.T) {
	const big, small = 2048, 64
	cat := stringKeyFixture(t, big, small)
	spec := JoinSpec{
		Kind:  JoinInner,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "hist", Alias: "h"},
		Cond:  []string{"j.Owner == h.Owner"},
		Equi:  []JoinEqui{{Left: "Owner", Right: "Owner"}},
	}
	drain := func() (int, *JoinStats) {
		var st JoinStats
		seq, err := cat.Join(spec, JoinLimits{}, &st)
		if err != nil {
			t.Fatalf("Join: %v", err)
		}
		n := 0
		for range seq {
			n++
		}
		return n, &st
	}

	want, wst := drain()
	if wst.Err != nil {
		t.Fatal(wst.Err)
	}
	if want != big {
		t.Fatalf("the hash join matched %d rows, want %d; the fixture is not joining", want, big)
	}

	var got int
	var gst *JoinStats
	withOuterDriven(func() { got, gst = drain() })
	if gst.OuterDrivenBatches != 0 {
		t.Errorf("the path ran %d batches on a string key; it cannot serve one",
			gst.OuterDrivenBatches)
	}
	if got != want {
		t.Errorf("with the gate on the join matched %d rows, want %d", got, want)
	}
}

// TestOuterDrivenExplainDescribesTheJoinThatRuns is the bar an execution path has to clear
// before it may decide a plan: the explained plan and the plan that runs must be the same
// plan. A path that reports nothing leaves ExplainJoin describing the join it REPLACED.
func TestOuterDrivenExplainDescribesTheJoinThatRuns(t *testing.T) {
	cat := outerFixture(t, 2048, 128, 0, true)
	spec := outerSpec(JoinInner)

	// With the path off, the explain must not claim it. Off is asked for rather than
	// assumed: the default takes this fixture now, since the probe declines on it.
	var off JoinExplain
	withoutOuterDriven(func() {
		var err error
		if off, err = cat.ExplainJoin(spec, JoinLimits{}); err != nil {
			t.Fatal(err)
		}
	})
	if off.OuterDriven {
		t.Error("the explain claims an outer-driven plan while the path is gated off")
	}

	var ex JoinExplain
	var st *JoinStats
	withOuterDriven(func() {
		var eerr error
		if ex, eerr = cat.ExplainJoin(spec, JoinLimits{}); eerr != nil {
			t.Fatal(eerr)
		}
		_, st = drainOrdered(t, cat, spec, JoinLimits{})
	})
	if st.OuterDrivenBatches == 0 {
		t.Fatal("the join did not run outer-driven, so there is nothing to agree with")
	}
	if !ex.OuterDriven {
		t.Fatal("the join ran outer-driven and the explain did not say so")
	}
	if ex.OuterDrivenBatchKeys != joinBatchKeys {
		t.Errorf("the explain reports batches of %d, the path uses %d",
			ex.OuterDrivenBatchKeys, joinBatchKeys)
	}
	// The same agreement TestExplainJoinAgreesWithTheJoinItDescribes requires of every
	// other path: a step per step, in order, on the same side, and a step the run narrowed
	// must not be one the explain said it could not.
	if len(ex.Steps) != len(st.ProbeSteps) {
		t.Fatalf("the explain has %d steps, the join ran %d", len(ex.Steps), len(st.ProbeSteps))
	}
	for i, s := range ex.Steps {
		ran := st.ProbeSteps[i]
		if s.Alias != ran.Alias {
			t.Errorf("step %d: explain folds %s, the join folded %s", i, s.Alias, ran.Alias)
		}
		if ran.Keys > 0 && s.ProbeAttr == "" {
			t.Errorf("step %s was narrowed on %s, but the explain said it could not be: %q",
				s.Alias, ran.Attr, s.ProbeWhy)
		}
	}
	if st.ProbeSteps[0].Keys == 0 {
		t.Error("the run reported narrowing on no keys at all")
	}
	// The hold is a batch, not a side, and the explain has to say which -- a side-sized
	// EstHashRows here would describe the plan this one replaces.
	if ex.EstHashRows != 0 {
		t.Errorf("EstHashRows = %d; an outer-driven plan holds one batch's matches", ex.EstHashRows)
	}
	if !strings.Contains(ex.HashWhy, "batches") {
		t.Errorf("HashWhy does not say what is held: %q", ex.HashWhy)
	}
}

// TestOuterDrivenExplainIsNotTheProbePathsExplain checks that ExplainJoin describes THIS
// path rather than the one it replaces. The two agree on the attribute whenever both apply,
// so a fixture where both apply cannot tell them apart -- and an explain that had never
// heard of this path would pass such a test.
//
// So the probe is disabled, which makes its gate report the step as un-narrowable. The
// outer-driven path does not depend on that gate, so with it on the step must still be
// reported as narrowed, and on the key the batch resolves.
func TestOuterDrivenExplainIsNotTheProbePathsExplain(t *testing.T) {
	cat := outerFixture(t, 2048, 128, 0, true)
	spec := outerSpec(JoinInner)

	withoutProbe(func() {
		off, err := cat.ExplainJoin(spec, JoinLimits{})
		if err != nil {
			t.Fatal(err)
		}
		if off.Steps[0].ProbeAttr != "" {
			t.Fatalf("the probe gate is disabled yet still reports %q; the off switch did "+
				"not take and this test cannot discriminate", off.Steps[0].ProbeAttr)
		}

		var on JoinExplain
		var st *JoinStats
		withOuterDriven(func() {
			var eerr error
			if on, eerr = cat.ExplainJoin(spec, JoinLimits{}); eerr != nil {
				t.Fatal(eerr)
			}
			_, st = drainOrdered(t, cat, spec, JoinLimits{})
		})
		if st.OuterDrivenBatches == 0 {
			t.Fatal("the outer-driven path declined; nothing to describe")
		}
		if on.Steps[0].ProbeAttr == "" {
			t.Errorf("the join narrowed the step on %d keys and the explain says it could "+
				"not: %q -- the explain is still describing the probe path",
				st.ProbeSteps[0].Keys, on.Steps[0].ProbeWhy)
		}
		if on.Steps[0].ProbeWhy != "" {
			t.Errorf("the explain names a decline reason (%q) for a step it reports narrowed",
				on.Steps[0].ProbeWhy)
		}
	})
}

// TestOuterDrivenDefaultPolicy pins what the default actually decides. Nothing is forced
// here: every arm is what a caller gets.
//
// Over either memory bound the path is taken whatever it costs, because the alternative is
// not a slower plan but no plan. Under them it has to be cheaper, and the two ratios left
// alone here are outside the band where it is: the selective arm produces too FEW pairs,
// where the index probe reaches the same rows in one query, and the comparable arm too many,
// where holding the inner side once is as cheap. TestOuterDrivenEngagesOnTheMeasuredBand
// covers between them.
//
// "Too few pairs" means too few FOR THE PROBE, which is what the floor defers to, so the
// selective arm is sized where the probe really does take the join -- 64 keys reaching 64
// records of 4,096, inside its 0.135 share. At 128 keys the same fixture is past that share,
// the probe declines, and the floor lifts: that is the third arm, and on it this path runs
// at 0.11x of the hash (4.75ms against 534us, medians of 5).
func TestOuterDrivenDefaultPolicy(t *testing.T) {
	ratios := []struct {
		name       string
		big, small int
	}{
		{"selective FROM", 4096, 64},    // 1.6%, few enough pairs that the probe takes it
		{"comparable FROM", 4096, 2048}, // 50%, where it is measurably slower
	}
	for _, r := range ratios {
		t.Run(r.name+" within the bounds is left alone", func(t *testing.T) {
			cat := outerFixture(t, r.big, r.small, 0, true)
			rows, st := drainOrdered(t, cat, outerSpec(JoinInner), JoinLimits{})
			if st.Err != nil {
				t.Fatal(st.Err)
			}
			if st.OuterDrivenBatches != 0 {
				t.Errorf("the default ran %d batches for a FROM side %.1f%% of the inner "+
					"one, inside both memory bounds", st.OuterDrivenBatches,
					100*float64(r.small)/float64(r.big))
			}
			if len(rows) != r.small {
				t.Errorf("joined %d rows, want %d", len(rows), r.small)
			}
		})
	}

	t.Run("under the floor with no probe below it is taken", func(t *testing.T) {
		const big, small = 4096, 128
		cat := outerFixture(t, big, small, 0, true)
		spec := outerSpec(JoinInner)
		// The premise, asserted rather than assumed: with this path declined the probe
		// does not take the join either, so the floor has nothing to defer to and the
		// alternative is the hash reading all 4,096 rows.
		var pst *JoinStats
		withoutOuterDriven(func() { _, pst = drainOrdered(t, cat, spec, JoinLimits{}) })
		if pst.ProbeKeys != 0 {
			t.Fatalf("the probe took the join (%d keys); the arm is not the shape it claims",
				pst.ProbeKeys)
		}
		rows, st := drainOrdered(t, cat, spec, JoinLimits{})
		if st.Err != nil {
			t.Fatal(st.Err)
		}
		if st.OuterDrivenBatches == 0 {
			t.Errorf("the default left %d pairs of a %d-row inner side to the hash, with no "+
				"probe below the floor", len(rows), big)
		}
		if len(rows) != small {
			t.Errorf("joined %d rows, want %d", len(rows), small)
		}
	})

	t.Run("over the row bound is taken", func(t *testing.T) {
		const big, small = 4096, 2048
		cat := outerFixture(t, big, small, 0, true)
		lim := JoinLimits{MaxHashRows: big / 2}

		var hash *JoinStats
		withoutOuterDriven(func() { _, hash = drainOrdered(t, cat, outerSpec(JoinInner), lim) })
		if hash.Err == nil {
			t.Fatalf("the hash completed under MaxHashRows=%d; the limit is not binding",
				lim.MaxHashRows)
		}

		rows, st := drainOrdered(t, cat, outerSpec(JoinInner), lim)
		if st.OuterDrivenBatches == 0 {
			t.Fatalf("the default left a join to the hash that the hash refuses: %v", st.Err)
		}
		if st.Err != nil {
			t.Fatalf("the batched arm stopped early: %v", st.Err)
		}
		if len(rows) != small {
			t.Errorf("joined %d rows, want %d", len(rows), small)
		}
	})
}

// fanoutFixture gives the inner side several rows per key, which is what makes a batch's
// hold larger than the batch. With one row per key a batch can never hold more than
// joinBatchKeys, and the per-batch bound below could not fire however low the limit went.
func fanoutFixture(t testing.TB, clusters, fan int) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateArchiveTable("hist", ArchiveConfig{
		SegmentSize: 4096,
		ValueAttrs:  []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for c := 1; c <= clusters; c++ {
		for f := 0; f < fan; f++ {
			ad := classad.New()
			ad.InsertAttr("ClusterId", int64(c))
			ad.InsertAttr("ExitCode", int64(f))
			ad.InsertAttrString("Pad", strings.Repeat("x", 64))
			if aerr := hist.Append(ad); aerr != nil {
				t.Fatal(aerr)
			}
		}
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(c))
		ad.InsertAttrString("Owner", "alice")
		if aerr := jobs.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	return cat
}

// TestOuterDrivenRespectsMaxHashRowsPerBatch covers the bound on what a BATCH holds.
// MaxHashRows bounds rows held, and a batch's matches are the same resource a hash holds,
// so a path that ignored it would hold more than the caller allowed and report success.
func TestOuterDrivenRespectsMaxHashRowsPerBatch(t *testing.T) {
	const clusters, fan = 400, 10
	cat := fanoutFixture(t, clusters, fan)
	spec := outerSpec(JoinInner)
	// A limit at or above the batch size, so planOuterDriven does not decline outright,
	// but far below what a batch of joinBatchKeys keys pulls back at this fan-out.
	lim := JoinLimits{MaxHashRows: joinBatchKeys + 1}
	if joinBatchKeys*fan <= lim.MaxHashRows {
		t.Fatalf("a batch holds at most %d rows, within the %d limit; the bound cannot fire",
			joinBatchKeys*fan, lim.MaxHashRows)
	}

	var st *JoinStats
	withOuterDriven(func() { _, st = drainOrdered(t, cat, spec, lim) })
	if st.Err == nil {
		t.Fatalf("a batch held up to %d rows under a %d-row limit and the join reported no error",
			joinBatchKeys*fan, lim.MaxHashRows)
	}
	if !strings.Contains(st.Err.Error(), "batch") {
		t.Errorf("the error does not say a batch overran the limit: %v", st.Err)
	}
}

// TestOuterDrivenDeclinesBelowTheBatchSize covers the planner's half of the same rule. Under
// a limit smaller than one batch, no batch that matches anything can meet it, so taking the
// path would turn a plan the index probe might still have narrowed into a refusal.
func TestOuterDrivenDeclinesBelowTheBatchSize(t *testing.T) {
	cat := fanoutFixture(t, 400, 10)
	spec := outerSpec(JoinInner)
	var rows []string
	var st *JoinStats
	withOuterDriven(func() {
		rows, st = drainOrdered(t, cat, spec, JoinLimits{MaxHashRows: joinBatchKeys - 1})
	})
	if st.OuterDrivenBatches != 0 {
		t.Errorf("the path ran %d batches under a limit smaller than one batch",
			st.OuterDrivenBatches)
	}
	// A 63-row limit against this inner side has no plan at all, so the join does fail --
	// the rule is about WHOSE failure it is. Declining hands the join back with the hash's
	// own verdict, which names the side to narrow; taking it and then overrunning the
	// per-batch bound reports a batch instead, and leaves the batch count at 0 either way.
	// So the error text is what separates the two, not the count.
	_ = rows
	if st.Err == nil {
		t.Fatalf("a %d-row limit against a %d-row side produced no error at all",
			joinBatchKeys-1, 400*10)
	}
	if strings.Contains(st.Err.Error(), "batch") {
		t.Errorf("the join failed as a batch overrun (%v); declining should have left the "+
			"hash to refuse on its own terms", st.Err)
	}
}

// wideFixture builds an inner side of WIDE ads: few rows, many attributes. It is the shape
// that separates the two bounds -- a row count far inside MaxHashRows whose hold is far
// outside any sane byte budget.
func wideFixture(t testing.TB, rows, attrs int) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateArchiveTable("hist", ArchiveConfig{
		SegmentSize: 1 << 20,
		ValueAttrs:  []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= rows; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("ExitCode", int64(i%7))
		for a := 0; a < attrs; a++ {
			ad.InsertAttr(fmt.Sprintf("Attr%d", a), int64(a*7919+i))
		}
		if aerr := hist.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
		j := classad.New()
		j.InsertAttr("ClusterId", int64(i))
		if aerr := jobs.Append(j); aerr != nil {
			t.Fatal(aerr)
		}
	}
	return cat
}

// TestOuterDrivenTriggersOnBytesNotRows is the rule this path was rebuilt around. A row
// count cannot express a memory budget: the hold costs ~180 + 130*attrs bytes per row, so
// the same count is a few hundred megabytes for a narrow ad and gigabytes for a wide one.
//
// Here the inner side is far inside MaxHashRows and far outside the byte budget, so a rule
// that triggered on rows would leave the join to a hash that holds the lot.
func TestOuterDrivenTriggersOnBytesNotRows(t *testing.T) {
	const rows, attrs = 5000, 120
	cat := wideFixture(t, rows, attrs)
	spec := outerSpec(JoinInner)

	est := estHashBytes(rows, attrs+2)
	lim := JoinLimits{MaxHashBytes: est / 4}
	if rows >= DefaultJoinLimits().MaxHashRows {
		t.Fatalf("the fixture's %d rows reach MaxHashRows; the two bounds are not separated", rows)
	}

	var st *JoinStats
	_, st = drainOrdered(t, cat, spec, lim)
	if st.Err != nil {
		t.Fatalf("join stopped early: %v", st.Err)
	}
	if st.OuterDrivenBatches == 0 {
		t.Errorf("the default left a %d-row side to the hash though holding it is ~%d bytes, "+
			"over the %d budget -- the rule is still counting rows",
			rows, est, lim.MaxHashBytes)
	}

	// And with a budget the hold fits, the hash keeps the join: the trigger is the budget,
	// not merely the presence of wide ads.
	var fits *JoinStats
	_, fits = drainOrdered(t, cat, spec, JoinLimits{MaxHashBytes: est * 4})
	if fits.Err != nil {
		t.Fatalf("join stopped early: %v", fits.Err)
	}
	if fits.OuterDrivenBatches != 0 {
		t.Errorf("the path ran %d batches under a budget the hold fits", fits.OuterDrivenBatches)
	}
}

// TestHashRowByteEstimateTracksReality guards the model in db/joinmem.go against drift. The
// constants were measured; if the decoded representation changes shape, an estimate that
// silently stops matching would mis-plan every join rather than fail.
func TestHashRowByteEstimateTracksReality(t *testing.T) {
	for _, attrs := range []int{20, 120} {
		const rows = 20_000
		cat := wideFixture(t, rows, attrs)
		hist, ok := cat.ArchiveTable("hist")
		if !ok {
			t.Fatal("no hist table")
		}
		width, known := archiveAdWidth(hist)
		if !known {
			t.Fatal("the side's width could not be sampled")
		}
		if want := attrs + 2; width != want {
			t.Errorf("sampled width %d, the ads carry %d attributes", width, want)
		}

		base := heapNowForTest()
		buckets := make(map[string][]*joinRow, rows)
		ads, err := hist.Query("true")
		if err != nil {
			t.Fatal(err)
		}
		held := 0
		for ad := range ads {
			r := decodedRow(ad)
			var b strings.Builder
			if !appendKeyValue(&b, r.eval("ClusterId")) {
				continue
			}
			buckets[b.String()] = append(buckets[b.String()], r)
			held++
		}
		actual := float64(heapNowForTest() - base)
		runtime.KeepAlive(buckets)
		est := float64(estHashBytes(held, width))
		ratio := est / actual
		t.Logf("attrs=%d held=%d actual=%.0f est=%.0f ratio=%.2f", attrs, held, actual, est, ratio)
		// Generous, because this is a model and the heap is noisy -- but tight enough that
		// the order of magnitude cannot drift unnoticed, which is what mis-plans a join.
		if ratio < 0.5 || ratio > 2.0 {
			t.Errorf("attrs=%d: the estimate is %.2fx the measured hold; the model in "+
				"joinmem.go no longer describes what a row costs", attrs, ratio)
		}
	}
}

func heapNowForTest() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// keyFixture builds a join whose key attribute carries whatever values keyOf returns, so the
// batched path can be put down its non-integer route. The inner side indexes the key.
func keyFixture(t testing.TB, n int, putKey func(ad *classad.ClassAd, i int)) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateArchiveTable("hist", ArchiveConfig{
		SegmentSize: 4096,
		ValueAttrs:  []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		ad := classad.New()
		putKey(ad, i)
		ad.InsertAttr("ExitCode", int64(i%7))
		if aerr := hist.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
		aj := classad.New()
		putKey(aj, i)
		aj.InsertAttrString("Owner", "alice")
		if aerr := jobs.Append(aj); aerr != nil {
			t.Fatal(aerr)
		}
	}
	return cat
}

// TestOuterDrivenKeysThatAreNotIntegers puts the path down its other key route.
//
// A batch whose keys are all integers is handed to LookupKeys, which is the zone-hoisted
// lookup this path exists for. A batch holding a real cannot be: LookupKeys takes int64 keys,
// so the fractional ones would be truncated into keys that match the wrong rows. The fallback
// is an ordinary member() query over the values themselves, and the bar for it is the same as
// for the fast route -- the same rows the hash returns, in the same order.
func TestOuterDrivenKeysThatAreNotIntegers(t *testing.T) {
	kinds := []struct {
		name   string
		putKey func(ad *classad.ClassAd, i int)
	}{
		// Halves, so int64(f) != f for every key and the whole batch takes the fallback.
		{"reals", func(ad *classad.ClassAd, i int) {
			ad.InsertAttrFloat("ClusterId", float64(i)+0.5)
		}},
		// Every other key fractional: the batch is MIXED, so ints and reals must both
		// reach the query or half the matches go missing.
		{"mixed", func(ad *classad.ClassAd, i int) {
			if i%2 == 0 {
				ad.InsertAttr("ClusterId", int64(i))
				return
			}
			ad.InsertAttrFloat("ClusterId", float64(i)+0.5)
		}},
	}
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			cat := keyFixture(t, 300, k.putKey)
			want, wst := drainOrdered(t, cat, outerSpec(JoinInner), JoinLimits{})
			if wst.Err != nil {
				t.Fatalf("hash join stopped early: %v", wst.Err)
			}
			if wst.OuterDrivenBatches != 0 {
				t.Fatalf("the baseline ran batched (%d batches); it is meant to be the hash",
					wst.OuterDrivenBatches)
			}
			if len(want) == 0 {
				t.Fatal("the fixture joined nothing, so neither arm is being measured")
			}
			var got []string
			var gst *JoinStats
			withOuterDriven(func() { got, gst = drainOrdered(t, cat, outerSpec(JoinInner), JoinLimits{}) })
			if gst.Err != nil {
				t.Fatalf("batched join stopped early: %v", gst.Err)
			}
			if gst.OuterDrivenBatches == 0 {
				t.Fatal("the batched arm ran no batches, so it is the hash arm twice")
			}
			if firstDiff(got, want) != -1 {
				t.Errorf("batched returned %d rows, hash %d; first difference at %d",
					len(got), len(want), firstDiff(got, want))
			}
		})
	}
}

// firstDiff is where two row lists part, or -1. Reported instead of both lists because the
// lists are long and the index is what says whether a whole route went missing or one row did.
func firstDiff(a, b []string) int {
	for i := 0; i < min(len(a), len(b)); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return min(len(a), len(b))
	}
	return -1
}

// TestOuterDrivenKeysTheBatchCannotLookUp pins what happens to a FROM row whose key is not a
// number at all.
//
// Such a row contributes NOTHING to the batch -- a value index holds numbers, so there is no
// key to look up -- and the risk is that "contributes no key" quietly becomes "matches
// everything" or "is dropped from the output". An INNER join must lose the row because it
// matches nothing on either path, and a LEFT join must still null-extend it. Both are the
// hash path's answer, which is the only answer this path may give.
func TestOuterDrivenKeysTheBatchCannotLookUp(t *testing.T) {
	// Half the FROM rows carry a string key. They match no inner row on either path, so the
	// two arms must agree about them for opposite reasons: the hash buckets the string and
	// finds nothing, this folds it out of the batch entirely.
	strKey := func(ad *classad.ClassAd, i int) {
		if i%2 == 0 {
			ad.InsertAttrString("ClusterId", fmt.Sprintf("c%d", i))
			return
		}
		ad.InsertAttr("ClusterId", int64(i))
	}
	for _, kind := range []JoinKind{JoinInner, JoinLeft} {
		t.Run(fmt.Sprintf("kind%d", kind), func(t *testing.T) {
			cat := keyFixture(t, 200, strKey)
			want, wst := drainOrdered(t, cat, outerSpec(kind), JoinLimits{})
			if wst.Err != nil {
				t.Fatalf("hash join stopped early: %v", wst.Err)
			}
			if wst.OuterDrivenBatches != 0 {
				t.Fatalf("the baseline ran batched (%d batches)", wst.OuterDrivenBatches)
			}
			var got []string
			var gst *JoinStats
			withOuterDriven(func() { got, gst = drainOrdered(t, cat, outerSpec(kind), JoinLimits{}) })
			if gst.Err != nil {
				t.Fatalf("batched join stopped early: %v", gst.Err)
			}
			if gst.OuterDrivenBatches == 0 {
				t.Fatal("the batched arm ran no batches, so it is the hash arm twice")
			}
			if d := firstDiff(got, want); d != -1 {
				t.Errorf("batched returned %d rows, hash %d; first difference at %d",
					len(got), len(want), d)
			}
			// A LEFT join owes a row for every FROM row, matched or not. Without this the
			// test would pass on two arms that both silently dropped the string-keyed half.
			if kind == JoinLeft && len(want) != 200 {
				t.Fatalf("the LEFT join returned %d rows for 200 FROM rows; the unmatched "+
					"half is not being null-extended, so neither arm is being tested on it",
					len(want))
			}
		})
	}
}

// TestOuterDrivenStopsAtTheCallersLimit checks that a LIMIT ends the join part way through
// the FROM side rather than after it.
//
// This path reads the FROM side in batches, so a limit reached inside one batch has to stop
// the next batch being staged at all. A limit honoured only at the end would still return the
// right rows, and would read the whole inner side to do it.
func TestOuterDrivenStopsAtTheCallersLimit(t *testing.T) {
	cat := outerFixture(t, 4096, 1024, 0, true)
	spec := outerSpec(JoinInner)
	spec.Limit = 5
	var full, limited *JoinStats
	var got []string
	withOuterDriven(func() {
		_, full = drainOrdered(t, cat, outerSpec(JoinInner), JoinLimits{})
		got, limited = drainOrdered(t, cat, spec, JoinLimits{})
	})
	if full.OuterDrivenBatches == 0 || limited.OuterDrivenBatches == 0 {
		t.Fatalf("not the batched path: %d batches unlimited, %d limited",
			full.OuterDrivenBatches, limited.OuterDrivenBatches)
	}
	if len(got) != 5 {
		t.Fatalf("the limited join returned %d rows, want 5", len(got))
	}
	// The point of the limit: fewer batches staged, not just fewer rows yielded.
	if limited.OuterDrivenBatches >= full.OuterDrivenBatches {
		t.Errorf("the limit stopped after %d batches and the unlimited join took %d; "+
			"the limit is not ending the batch loop", limited.OuterDrivenBatches,
			full.OuterDrivenBatches)
	}
}

// A constrained inner side used to decline the path outright, which is most real joins: a
// query that reads all of history and narrows it afterwards is not one anybody writes. These
// pin that the constraint rides into the batch rather than being dropped or applied late.

// innerConstraints are the shapes a side's constraint takes, chosen by how the STORE sees
// them rather than by how they read: on the key's own index, on an attribute with no index
// at all, on one the side does not carry, and matching nothing.
var innerConstraints = []struct {
	name  string
	expr  string
	empty bool // matches no inner row at all, so an INNER join is expected to return none
}{
	{name: "on the key's own index", expr: "ClusterId > 1000"},
	{name: "on an unindexed attribute", expr: "ExitCode == 3"},
	{name: "both halves", expr: "ClusterId > 1000 && ExitCode == 3"},
	{name: "matching nothing", expr: "ExitCode == 99", empty: true},
	// UNDEFINED, not FALSE: hist carries no Owner. A record matches only on boolean TRUE,
	// so this matches nothing -- and it is here because an index on an attribute the side
	// does not carry is the shape most likely to be mishandled by a lookup.
	{name: "on an attribute the side lacks", expr: `Owner == "alice"`, empty: true},
}

func constrainedSpec(kind JoinKind, constraint string) JoinSpec {
	spec := outerSpec(kind)
	spec.Right.Constraint = constraint
	return spec
}

// TestOuterDrivenConstrainedInnerSideAgreesWithTheHash is the correctness half. A batch that
// dropped the constraint would join rows the query excluded; one that applied it to the wrong
// rows would drop rows it owed. Both show up as disagreement with the hash path, row for row
// and in order.
//
// LEFT is here for the case the INNER kind cannot see: a FROM row whose key exists in the
// inner side but whose every inner row the constraint excludes is owed a null extension, not
// a drop. A path that confused "the constraint matched nothing" with "the key matched
// nothing" answers that one wrong and INNER never notices.
func TestOuterDrivenConstrainedInnerSideAgreesWithTheHash(t *testing.T) {
	for _, c := range innerConstraints {
		for _, kind := range []JoinKind{JoinInner, JoinLeft} {
			t.Run(fmt.Sprintf("%s/kind%d", c.name, kind), func(t *testing.T) {
				cat := outerFixture(t, 2048, 128, 17, true)
				spec := constrainedSpec(kind, c.expr)

				var want []string
				var wst *JoinStats
				withoutOuterDriven(func() { want, wst = drainOrdered(t, cat, spec, JoinLimits{}) })
				if wst.Err != nil {
					t.Fatalf("hash join stopped early: %v", wst.Err)
				}
				if wst.OuterDrivenBatches != 0 {
					t.Fatalf("the baseline ran batched (%d batches)", wst.OuterDrivenBatches)
				}

				var got []string
				var gst *JoinStats
				withOuterDriven(func() { got, gst = drainOrdered(t, cat, spec, JoinLimits{}) })
				if gst.Err != nil {
					t.Fatalf("outer-driven join stopped early: %v", gst.Err)
				}
				// No skip on a decline. Admitting a constrained side is the change under
				// test, so a decline here is the failure, not a reason to measure nothing.
				if gst.OuterDrivenBatches == 0 {
					t.Fatalf("the outer-driven path declined constraint %q", c.expr)
				}
				if len(got) != len(want) {
					t.Fatalf("outer-driven returned %d rows, hash returned %d", len(got), len(want))
				}
				if i := firstDiff(got, want); i >= 0 {
					t.Fatalf("row %d: outer-driven %q, hash %q", i, got[i], want[i])
				}
				// A LEFT join always returns at least its FROM rows, so a count alone cannot
				// tell "the constraint matched nothing" from "the constraint was dropped".
				if kind == JoinInner && (len(want) == 0) != c.empty {
					t.Fatalf("constraint %q joined %d rows; expected empty=%v", c.expr, len(want), c.empty)
				}
			})
		}
	}
}

// TestOuterDrivenConstraintNarrowsTheLookupItself is the reason the constraint is passed to
// LookupKeysWhere rather than applied to its results. A batch's hold is the rows it keeps, so
// if the constraint reached the store the hold shrinks with it; if the batch looked up every
// row for its keys and filtered afterwards, the hold would be the same either way.
func TestOuterDrivenConstraintNarrowsTheLookupItself(t *testing.T) {
	cat := outerFixture(t, 4096, 256, 0, true)

	var wide, narrow *JoinStats
	withOuterDriven(func() { _, wide = drainOrdered(t, cat, outerSpec(JoinInner), JoinLimits{}) })
	withOuterDriven(func() {
		_, narrow = drainOrdered(t, cat, constrainedSpec(JoinInner, "ExitCode == 3"), JoinLimits{})
	})
	for _, st := range []*JoinStats{wide, narrow} {
		if st.Err != nil {
			t.Fatalf("join stopped early: %v", st.Err)
		}
		if st.OuterDrivenBatches == 0 {
			t.Fatal("a run was not batched, so its hold is the hash path's and says nothing")
		}
	}
	if narrow.HashRows >= wide.HashRows {
		t.Fatalf("a batch held %d rows under ExitCode == 3 and %d unconstrained; the "+
			"constraint did not reach the lookup", narrow.HashRows, wide.HashRows)
	}
}

// TestOuterDrivenDeclinesConstraintsItCannotPlan pins the two gates that admit a constrained
// side. A constraint that does not parse is not quietly dropped -- the scan paths reject it
// downstream where a parse error reports as one -- and a constraint that turns the batch's
// query into something other than an index walk makes this path strictly worse than reading
// the side once.
func TestOuterDrivenDeclinesConstraintsItCannotPlan(t *testing.T) {
	t.Run("does not parse", func(t *testing.T) {
		// The decline is not observable as a batch count, because the join never runs: the
		// scan path rejects the constraint and reports it AS a parse error. That is the
		// behaviour being pinned -- the alternative is a path that drops what it cannot
		// parse and answers a different query without saying so.
		cat := outerFixture(t, 2048, 128, 0, true)
		var st JoinStats
		var err error
		withOuterDriven(func() { _, err = cat.Join(constrainedSpec(JoinInner, "ExitCode == "), JoinLimits{}, &st) })
		if err == nil {
			t.Fatal("an unparseable constraint joined without error")
		}
		if !strings.Contains(err.Error(), "parse") {
			t.Fatalf("error does not name the parse failure: %v", err)
		}
		if st.OuterDrivenBatches != 0 {
			t.Fatalf("ran %d batches on a constraint that does not parse", st.OuterDrivenBatches)
		}
	})
	t.Run("plans as a union", func(t *testing.T) {
		cat := outerFixture(t, 2048, 128, 0, true)
		var st *JoinStats
		withOuterDriven(func() {
			_, st = drainOrdered(t, cat, constrainedSpec(JoinInner, `ExitCode == 3 || Owner == "bob"`), JoinLimits{})
		})
		if st.OuterDrivenBatches != 0 {
			t.Fatalf("took the batched path on a union plan (%d batches)", st.OuterDrivenBatches)
		}
	})
}

// TestOuterDrivenAdmitsAConstraintOnTheKeyItself is the sentinel's own test. planOuterDriven
// explains a batch query built on key 0, and a constraint on the key attribute contradicts
// that sentinel and plans "empty" while the same constraint against a batch's real keys plans
// "indexed". Reading that as the constraint's answer would decline the most ordinary narrowing
// there is.
func TestOuterDrivenAdmitsAConstraintOnTheKeyItself(t *testing.T) {
	cat := outerFixture(t, 2048, 128, 0, true)
	var st *JoinStats
	var got []string
	withOuterDriven(func() {
		got, st = drainOrdered(t, cat, constrainedSpec(JoinInner, "ClusterId > 1000"), JoinLimits{})
	})
	if st.Err != nil {
		t.Fatalf("join stopped early: %v", st.Err)
	}
	if st.OuterDrivenBatches == 0 {
		t.Fatal("declined a constraint on the key attribute; the sentinel key was read as the answer")
	}
	if len(got) == 0 {
		t.Fatal("joined no rows")
	}
}

// TestOuterDrivenConstrainedInnerSideOnKeysThatAreNotIntegers is the second site the
// constraint has to reach. A batch whose keys are not all integers cannot use LookupKeysWhere
// and builds its own query instead, and a constraint carried into one of those two and not
// the other is a join that answers correctly only for the key kinds the fixtures happen to
// use -- which is how the string keys were missed.
func TestOuterDrivenConstrainedInnerSideOnKeysThatAreNotIntegers(t *testing.T) {
	kinds := []struct {
		name   string
		putKey func(ad *classad.ClassAd, i int)
	}{
		{"reals", func(ad *classad.ClassAd, i int) { ad.InsertAttrFloat("ClusterId", float64(i)+0.5) }},
		{"strings", func(ad *classad.ClassAd, i int) {
			ad.InsertAttrString("ClusterId", fmt.Sprintf("key%d", i))
		}},
		{"mixed", func(ad *classad.ClassAd, i int) {
			if i%2 == 0 {
				ad.InsertAttr("ClusterId", int64(i))
				return
			}
			ad.InsertAttrFloat("ClusterId", float64(i)+0.5)
		}},
	}
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			cat := keyFixture(t, 300, k.putKey)
			spec := constrainedSpec(JoinInner, "ExitCode == 3")

			var want []string
			var wst *JoinStats
			withoutOuterDriven(func() { want, wst = drainOrdered(t, cat, spec, JoinLimits{}) })
			if wst.Err != nil {
				t.Fatalf("hash join stopped early: %v", wst.Err)
			}
			if len(want) == 0 {
				t.Fatal("the baseline joined no rows, so agreeing with it proves nothing")
			}

			var got []string
			var gst *JoinStats
			withOuterDriven(func() { got, gst = drainOrdered(t, cat, spec, JoinLimits{}) })
			if gst.Err != nil {
				t.Fatalf("outer-driven join stopped early: %v", gst.Err)
			}
			if gst.OuterDrivenBatches == 0 {
				t.Fatalf("the outer-driven path declined %s keys with a constraint", k.name)
			}
			if len(got) != len(want) {
				t.Fatalf("outer-driven returned %d rows, hash returned %d", len(got), len(want))
			}
			if i := firstDiff(got, want); i >= 0 {
				t.Fatalf("row %d: outer-driven %q, hash %q", i, got[i], want[i])
			}
		})
	}
}

// TestOuterDrivenEngagesOnTheMeasuredBand pins the cost rule at its three outcomes, with
// nothing forced: every arm is what a caller gets. The fixture is held fixed and only the
// FROM side's constraint moves, so what changes between arms is the pairs the join produces
// and nothing else.
func TestOuterDrivenEngagesOnTheMeasuredBand(t *testing.T) {
	const keys, fan = 312, 64 // 19,968 inner rows: floor 2,048 pairs, cap 4,992
	// The FROM side is indexed on the join key, so the store can price its constraint. An
	// unindexed one is priced at its whole table whatever the query narrows to, and the rule
	// refuses that rather than deciding on it.
	cat := bandFixture(t, keys, fan)
	inner, _, _, _, _ := cat.joinSideStats("hist", "")

	for _, c := range []struct {
		name    string
		limit   int // FROM-side keys, so limit*fan pairs
		batched bool
	}{
		{"below the floor, where the probe is quicker", 10, false},
		{"inside the band", 64, true},
		{"over the fraction, where the hash is as cheap", 200, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			spec := outerSpec(JoinInner)
			spec.Left.Constraint = fmt.Sprintf("ClusterId <= %d", c.limit)
			rows, st := drainOrdered(t, cat, spec, JoinLimits{})
			if st.Err != nil {
				t.Fatal(st.Err)
			}
			if want := c.limit * fan; len(rows) != want {
				t.Fatalf("joined %d rows, want %d; the arm is not the shape it claims", len(rows), want)
			}
			got := st.OuterDrivenBatches > 0
			if got != c.batched {
				t.Errorf("batched=%v, want %v for %d pairs of %d inner rows (%.1f%%)",
					got, c.batched, len(rows), inner, 100*float64(len(rows))/float64(inner))
			}
		})
	}
}

// TestOuterDrivenLiftsTheFloorWhereTheProbeDeclines pins the floor's condition at both
// outcomes on ONE fixture, so what separates the arms is the pairs and nothing else.
//
// The floor says the probe reaches these rows in one query for less. Where the probe runs,
// that is the measured answer and the floor stands. Where the probe declines on its own cost
// rule, the sentence is empty and the join falls to the hash instead -- which is what this
// lifts. Both halves are asserted, including that the probe really does decline, because a
// test of the first half alone would pass with the condition deleted.
func TestOuterDrivenLiftsTheFloorWhereTheProbeDeclines(t *testing.T) {
	// Fan-out 1, where a key count and a pair count are the same number: the probe's rule
	// is 4*keys + pairs <= 0.135 * 20,000, so it declines above about 540 pairs, well
	// under the 2,048-pair floor.
	cat := bandFixture(t, 20_000, 1)
	for _, c := range []struct {
		name    string
		limit   int
		keyCap  int  // maxProbeKeys for the arm, 0 for its production value
		probe   bool // whether the index probe takes the join with this path declined
		batched bool
	}{
		{"the probe runs, so the floor stands", 256, 0, true, false},
		{"the probe declines on cost, so the floor lifts", 1024, 0, false, true},
		// The key cap is the other way the probe declines. Reaching it at production
		// settings needs an inner side over about 38,000 rows -- below that the work rule
		// declines first -- so the cap is moved to the fixture rather than the fixture to
		// the cap. What is asserted is the same: the floor reads the cap.
		{"the probe declines on the key cap, so the floor lifts", 256, 100, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.keyCap > 0 {
				old := maxProbeKeys
				maxProbeKeys = c.keyCap
				defer func() { maxProbeKeys = old }()
			}
			spec := outerSpec(JoinInner)
			spec.Left.Constraint = fmt.Sprintf("ClusterId <= %d", c.limit)

			// What the probe does with this path out of the way, which is the thing the
			// floor defers to.
			var pst *JoinStats
			withoutOuterDriven(func() { _, pst = drainOrdered(t, cat, spec, JoinLimits{}) })
			if pst.Err != nil {
				t.Fatal(pst.Err)
			}
			if got := pst.ProbeKeys > 0; got != c.probe {
				t.Fatalf("the probe fired=%v, want %v (%q); the arm is not the shape it claims",
					got, c.probe, pst.ProbeWhy)
			}

			rows, st := drainOrdered(t, cat, spec, JoinLimits{})
			if st.Err != nil {
				t.Fatal(st.Err)
			}
			if len(rows) != c.limit {
				t.Fatalf("joined %d rows, want %d", len(rows), c.limit)
			}
			if got := st.OuterDrivenBatches > 0; got != c.batched {
				t.Errorf("batched=%v, want %v for %d pairs under the %d-pair floor",
					got, c.batched, len(rows), joinBatchedPairFloor)
			}
		})
	}
}

// TestOuterDrivenPairBoundTracksTheJoin keeps the bound honest about the thing it bounds. It
// is the only input the cost rule has, so a bound that drifted from the pairs a join really
// produces would move the policy silently -- engaging where the probe is quicker, or
// declining the band this path exists for -- with every test above still passing.
func TestOuterDrivenPairBoundTracksTheJoin(t *testing.T) {
	for _, fan := range []int{1, 8, 64} {
		t.Run(fmt.Sprintf("fan%d", fan), func(t *testing.T) {
			cat := bandFixture(t, 2000, fan)
			for _, limit := range []int{8, 32, 100} {
				spec := outerSpec(JoinInner)
				spec.Left.Constraint = fmt.Sprintf("ClusterId <= %d", limit)
				p, sides, err := planFor(cat, spec)
				if err != nil {
					t.Fatal(err)
				}
				tb, ok := cat.ArchiveTable(sides[p.steps[0].side].Table)
				if !ok {
					t.Fatal("no archive table for the inner side")
				}
				bound, _, ok := cat.outerDrivenPairEstimate(sides, p.steps[0], tb,
					p.steps[0].equiSideAttr[0])
				if !ok {
					t.Fatalf("limit=%d: the bound could not be computed", limit)
				}
				rows, st := drainOrdered(t, cat, spec, JoinLimits{})
				if st.Err != nil {
					t.Fatal(st.Err)
				}
				if bound < len(rows) {
					t.Errorf("limit=%d: bound %d is BELOW the %d pairs the join produced; "+
						"it is not a bound", limit, bound, len(rows))
				}
				// Loose enough for a sketched NDV's error on a small result, tight enough
				// to fail two things that have happened: an unpriced FROM side, which
				// returned the same number for every limit, and rounding the rows-per-key
				// ratio up to a whole number, which doubles every estimate on a table whose
				// keys are nearly unique.
				if bound > 2*len(rows) {
					t.Errorf("limit=%d: bound %d is more than 2x the %d pairs produced",
						limit, bound, len(rows))
				}
			}
		})
	}
}

// planFor builds the plan and sides the same two calls ExplainJoin makes, so a white-box
// test of a planning decision sees the same inputs the decision really gets.
func planFor(cat *Catalog, spec JoinSpec) (*joinPlan, []JoinSide, error) {
	p, err := compileJoin(spec, JoinLimits{})
	if err != nil {
		return nil, nil, err
	}
	sides, _, err := spec.chain()
	if err != nil {
		return nil, nil, err
	}
	_ = cat
	return p, sides, nil
}

// bandFixture is fanFixture with the FROM side indexed on the join key as well, which is what
// lets sideReadSize answer its constraint from statistics instead of returning the table.
func bandFixture(t testing.TB, keys, fan int) *Catalog {
	return fromSidedFixture(t, keys, fan, keys, true)
}

// fromSidedFixture is bandFixture with the FROM side's size and indexing under the test's
// control, so a case can put the FROM TABLE's own size inside the band while the query's
// share of it is somewhere else.
func fromSidedFixture(t testing.TB, keys, fan, fromKeys int, indexFrom bool) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateArchiveTable("hist", ArchiveConfig{
		SegmentSize: 65536, ValueAttrs: []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Small segments on the FROM side on purpose. Its rows are narrow, so at the inner
	// side's segment size the whole table sits in ONE OPEN segment, whose postings are not
	// built until something reads it -- and then whether its constraint can be priced
	// depends on what ran before, which is not something a policy test may turn on.
	jcfg := ArchiveConfig{SegmentSize: 4096}
	if indexFrom {
		jcfg.ValueAttrs = []string{"ClusterId"}
	}
	jobs, err := cat.CreateArchiveTable("jobs", jcfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= keys; i++ {
		for r := 0; r < fan; r++ {
			ad := classad.New()
			ad.InsertAttr("ClusterId", int64(i))
			ad.InsertAttr("ProcId", int64(r))
			ad.InsertAttrString("Pad", strings.Repeat("x", 64))
			if aerr := hist.Append(ad); aerr != nil {
				t.Fatal(aerr)
			}
		}
		if i > fromKeys {
			continue
		}
		aj := classad.New()
		aj.InsertAttr("ClusterId", int64(i))
		aj.InsertAttrString("Owner", "alice")
		if aerr := jobs.Append(aj); aerr != nil {
			t.Fatal(aerr)
		}
	}
	return cat
}

// TestOuterDrivenRefusesAnUnpricedFromSide is the gate that keeps the cost rule from deciding
// on a number the query cannot move. A FROM side with no index on its constraint is priced at
// its whole table, so the pairs built on it are the same whatever the caller narrowed to.
//
// The FROM TABLE is sized so that its own row count puts the estimate INSIDE the band. Without
// the gate the path is therefore taken, on a figure that would have been identical for any
// constraint at all; with it, the path is declined and the same shape over an indexed FROM
// side is taken. An earlier version of this test sized the FROM table like the inner one,
// where the unpriced estimate overshoots the fraction and the join declines for that reason
// instead -- it passed, and proved nothing about this gate.
func TestOuterDrivenRefusesAnUnpricedFromSide(t *testing.T) {
	const keys, fan, fromKeys = 312, 64, 60 // table estimate 60*64 = 3,840, inside [2048, 4992]
	unpriced := fromSidedFixture(t, keys, fan, fromKeys, false)
	priced := fromSidedFixture(t, keys, fan, fromKeys, true)

	spec := outerSpec(JoinInner)
	spec.Left.Constraint = "ClusterId <= 55" // priced estimate 55*64 = 3,520, also inside

	uRows, uSt := drainOrdered(t, unpriced, spec, JoinLimits{})
	pRows, pSt := drainOrdered(t, priced, spec, JoinLimits{})
	if uSt.Err != nil || pSt.Err != nil {
		t.Fatalf("joins stopped early: %v / %v", uSt.Err, pSt.Err)
	}
	if len(uRows) != len(pRows) || len(uRows) == 0 {
		t.Fatalf("the two fixtures joined %d and %d rows; they are not the same shape",
			len(uRows), len(pRows))
	}
	if pSt.OuterDrivenBatches == 0 {
		t.Fatal("the indexed FROM side declined the band, so the unpriced arm proves nothing")
	}
	if uSt.OuterDrivenBatches != 0 {
		t.Errorf("took the path on a FROM side priced at its whole table (%d batches)",
			uSt.OuterDrivenBatches)
	}
}

// TestOuterDrivenPairEstimateUnderSkew records what the estimate cannot do, so that the next
// reader finds it here rather than in a slow join.
//
// The fan factor is Records over Distinct -- an average -- and a skewed join key is precisely
// the case where one key holds far more rows than the average. The estimate then reads low and
// the path engages past the fraction it was meant to respect. That is not a cliff: past the
// fraction the hash is merely AS cheap, so what a skewed key costs here is parity. The test
// asserts the join is still CORRECT under skew, and reports the estimate's error rather than
// requiring it to be small.
func TestOuterDrivenPairEstimateUnderSkew(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateArchiveTable("hist", ArchiveConfig{
		SegmentSize: 65536, ValueAttrs: []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.CreateArchiveTable("jobs", ArchiveConfig{
		SegmentSize: 4096, ValueAttrs: []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// One key holds most of the side; the rest hold one row each. The average fan is near 1
	// and the key the join actually reaches holds thousands.
	const keys, heavy = 4000, 12000
	for i := 1; i <= keys; i++ {
		n := 1
		if i == 1 {
			n = heavy
		}
		for r := 0; r < n; r++ {
			ad := classad.New()
			ad.InsertAttr("ClusterId", int64(i))
			ad.InsertAttr("ProcId", int64(r))
			if aerr := hist.Append(ad); aerr != nil {
				t.Fatal(aerr)
			}
		}
		aj := classad.New()
		aj.InsertAttr("ClusterId", int64(i))
		if aerr := jobs.Append(aj); aerr != nil {
			t.Fatal(aerr)
		}
	}

	spec := outerSpec(JoinInner)
	// Wide enough that the store can price the side from its index -- a constraint reaching
	// a single key is answered at the table's size, which the rule refuses before any of this
	// -- and narrow enough that the heavy key is most of what the join reaches.
	const reach = 40
	spec.Left.Constraint = fmt.Sprintf("ClusterId <= %d", reach)
	want, wst := drainOrdered(t, cat, spec, JoinLimits{})
	if wst.Err != nil {
		t.Fatal(wst.Err)
	}
	if wantPairs := heavy + reach - 1; len(want) != wantPairs {
		t.Fatalf("joined %d rows, want %d; the fixture is not skewed as intended",
			len(want), wantPairs)
	}

	p, sides, err := planFor(cat, spec)
	if err != nil {
		t.Fatal(err)
	}
	tb, ok := cat.ArchiveTable(sides[p.steps[0].side].Table)
	if !ok {
		t.Fatal("no archive table for the inner side")
	}
	est, _, ok := cat.outerDrivenPairEstimate(sides, p.steps[0], tb, p.steps[0].equiSideAttr[0])
	if !ok {
		t.Fatal("the estimate could not be computed")
	}
	t.Logf("skewed key: estimate %d pairs, actual %d (%.0fx low)",
		est, len(want), float64(len(want))/float64(max(est, 1)))
	if est >= len(want) {
		t.Fatalf("the estimate (%d) covered the skew (%d actual); this fixture no longer "+
			"demonstrates the limitation it documents", est, len(want))
	}

	// Whatever the estimate decided, the answer is the answer.
	var got []string
	withOuterDriven(func() { got, _ = drainOrdered(t, cat, spec, JoinLimits{}) })
	if len(got) != len(want) {
		t.Fatalf("outer-driven returned %d rows under skew, hash returned %d", len(got), len(want))
	}
	if i := firstDiff(got, want); i >= 0 {
		t.Fatalf("row %d: outer-driven %q, hash %q", i, got[i], want[i])
	}
}

// The batched path engages on a FROM side whose own constraint the store can price, which is
// what the production shape has: a large history archive filtered on an attribute in
// ZoneAttrs. Both halves are asserted, because the path declining is indistinguishable from
// the path being slow unless the reason is checked.
//
// This exists because the fixture used to lack both, and the measurement built on it reported
// that the batched path never engages at any fan-out -- a statement about the fixture that
// read as a statement about the planner.
func TestOuterDrivenEngagesOnAPricedFromSide(t *testing.T) {
	const innerRows, fan = 20_000, 64
	keys := innerRows / fan
	cat := fanFixture(t, keys, fan, 65536)

	fromTotal, _, _, _, ok := cat.joinSideStats("jobs", "")
	if !ok {
		t.Fatal("the FROM side's size could not be learned")
	}
	spec := outerSpec(JoinInner)
	spec.Left.Constraint = fmt.Sprintf("ClusterId <= %d", keys/5)

	priced, sized := cat.sideReadSize(spec.Left)
	if !sized {
		t.Fatal("the FROM side's constraint could not be priced at all")
	}
	if priced >= fromTotal {
		t.Fatalf("the FROM side's constraint priced at %d of %d rows: nothing was pruned, so "+
			"the pair estimate cannot see what the caller narrowed to", priced, fromTotal)
	}

	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for range seq {
		rows++
	}
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if rows == 0 {
		t.Fatal("the join produced no rows, so nothing about the path was exercised")
	}
	if st.OuterDrivenBatches == 0 {
		t.Fatalf("the batched path declined a join of %d pairs over %d inner rows with the "+
			"FROM side priced at %d of %d", st.Pairs, innerRows, priced, fromTotal)
	}
}

// The rows-per-key ratio is multiplied in as a ratio, not rounded up to a whole number of
// rows first. On a table whose keys are nearly unique the true ratio is a little over 1, and
// rounding it up to 2 doubles every estimate built on it -- a systematic multiplier sitting
// exactly where the floor and the cap are, not the unit of slack it looks like.
//
// Asserted on the ESTIMATE rather than on which path ran, because the path is what the
// thresholds decide and this is about the number they are given.
func TestOuterDrivenPairEstimateKeepsTheRatio(t *testing.T) {
	cat := fanFixture(t, 20_000, 1, 65536)
	spec := outerSpec(JoinInner)
	spec.Left.Constraint = "ClusterId <= 4000"
	p, sides, err := planFor(cat, spec)
	if err != nil {
		t.Fatal(err)
	}
	tb, ok := cat.ArchiveTable(sides[p.steps[0].side].Table)
	if !ok {
		t.Fatal("no archive table for the inner side")
	}
	est, _, ok := cat.outerDrivenPairEstimate(sides, p.steps[0], tb, p.steps[0].equiSideAttr[0])
	if !ok {
		t.Fatal("the estimate could not be computed")
	}
	rows, st := drainOrdered(t, cat, spec, JoinLimits{})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	// One row per key, so the estimate should be within a sketch's error of the pairs. A
	// ratio rounded up to 2 lands at twice this and fails.
	if est > 3*len(rows)/2 {
		t.Errorf("estimate %d for %d pairs at a fan-out of 1: the rows-per-key ratio was "+
			"rounded up rather than multiplied in", est, len(rows))
	}
	if est < len(rows) {
		t.Errorf("estimate %d is below the %d pairs produced", est, len(rows))
	}
}
