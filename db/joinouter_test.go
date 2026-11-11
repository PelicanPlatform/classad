package db

import (
	"fmt"
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
func withOuterDriven(fn func()) {
	old := joinOuterDrivenEnabled
	joinOuterDrivenEnabled = true
	defer func() { joinOuterDrivenEnabled = old }()
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

	_, wst := drainOrdered(t, cat, outerSpec(JoinInner), lim)
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
