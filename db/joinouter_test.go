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
			want, wst := drainOrdered(t, cat, outerSpec(kind), JoinLimits{})
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
	_, wst := drainOrdered(t, cat, outerSpec(JoinInner), JoinLimits{})
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

// TestOuterDrivenDeclinesAConstrainedInnerSide covers the gate that keeps the join from
// widening to rows the caller excluded: a lookup resolves the key and nothing else, so a
// side carrying its own constraint cannot be served by one.
func TestOuterDrivenDeclinesAConstrainedInnerSide(t *testing.T) {
	cat := outerFixture(t, 512, 32, 0, true)
	spec := outerSpec(JoinInner)
	spec.Right.Constraint = "ExitCode == 0"
	var got []string
	var gst *JoinStats
	withOuterDriven(func() { got, gst = drainOrdered(t, cat, spec, JoinLimits{}) })
	if gst.OuterDrivenBatches != 0 {
		t.Fatalf("the path ran %d batches against a constrained side", gst.OuterDrivenBatches)
	}
	// And the constraint still did its work, so the decline is a decline and not a skip.
	want, _ := drainOrdered(t, cat, spec, JoinLimits{})
	if len(got) != len(want) {
		t.Errorf("constrained join returned %d rows with the gate on, %d with it off", len(got), len(want))
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

	// With the path off, the explain must not claim it.
	off, err := cat.ExplainJoin(spec, JoinLimits{})
	if err != nil {
		t.Fatal(err)
	}
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
// The policy is the MEMORY bounds and nothing else. Within them the join is left alone
// whatever the ratio of the sides, including the selective shape where
// TestOuterDrivenCrossoverAgainstTheHash shows this path running several times faster --
// because that benchmark's baseline is the build-side plan and not the index probe, and
// the probe is what would really run there. Taking the path on selectivity needs that
// comparison first.
func TestOuterDrivenDefaultPolicy(t *testing.T) {
	ratios := []struct {
		name       string
		big, small int
	}{
		{"selective FROM", 4096, 128},   // 3.1%, where the path is likely faster
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
