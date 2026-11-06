package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A JOIN ON A COMPOSITE KEY.
//
// Jobs are identified by (ClusterId, ProcId), so a join between two job tables is joined on
// two equalities -- and that was exactly the shape the index probe refused, on the grounds
// that a membership constraint narrows on one attribute. The join that most wants an index
// was the one declining it.

// multiKeyFixture builds a large table keyed by (ClusterId, ProcId) and a small one naming
// a few of its rows. Both attributes are indexed, so the planner has a choice to make.
func multiKeyFixture(tb testing.TB, big, small int) *Catalog {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	// Ordered on purpose: an archive that keeps its newest-first promise cannot have its
	// sides exchanged, so the small table streams and the LARGE one is hashed -- which is
	// the orientation a probe is for. Left unordered, the build-side planner would hash the
	// small table and there would be nothing worth narrowing.
	cfg := ArchiveConfig{SegmentSize: 4096, ValueAttrs: []string{"ClusterId", "ProcId"}}
	mk := func(name string) *ArchiveTable {
		t, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			tb.Fatal(cerr)
		}
		return t
	}
	hist, jobs := mk("hist"), mk("jobs")
	// ProcId takes four values across the whole table, ClusterId takes big/4. A probe that
	// narrowed on ProcId alone would match every row; one on ClusterId would not. Which is
	// why the choice cannot be made on key count.
	for i := 0; i < big; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i/4))
		ad.InsertAttr("ProcId", int64(i%4))
		ad.InsertAttrString("Pad", strings.Repeat("x", 64))
		if aerr := hist.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	stride := (big / 4) / small
	for k := 0; k < small; k++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(k*stride))
		ad.InsertAttr("ProcId", int64(1))
		ad.InsertAttrString("Owner", "alice")
		if aerr := jobs.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	return cat
}

func multiKeySpec() JoinSpec {
	return JoinSpec{
		Kind:  JoinInner,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "hist", Alias: "h"},
		Cond:  []string{"j.ClusterId == h.ClusterId", "j.ProcId == h.ProcId"},
		Equi: []JoinEqui{
			{Left: "ClusterId", Right: "ClusterId"},
			{Left: "ProcId", Right: "ProcId"},
		},
	}
}

// A two-equality join is narrowed, and narrowed on BOTH attributes.
func TestJoinProbesACompositeKey(t *testing.T) {
	cat := multiKeyFixture(t, 8192, 32)
	var st JoinStats
	seq, err := cat.Join(multiKeySpec(), JoinLimits{}, &st)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if n != 32 {
		t.Errorf("joined %d rows, want 32 (one per row of the small side)", n)
	}
	if st.ProbeKeys == 0 {
		t.Fatalf("a composite-key join was not narrowed: %s", st.ProbeWhy)
	}
	// Both equalities have to be APPLIED, which is asserted on what was hashed rather than
	// on the attributes the plan names: the two can disagree, and the rows held are what
	// the constraint is for. Each of the 32 streamed rows names one cluster, and each
	// cluster holds four rows differing only in ProcId -- so narrowing on ClusterId alone
	// hashes 128 and narrowing on both hashes 32. An answer of 128 means one conjunct was
	// built and reported but never reached the read.
	if st.HashRows != 32 {
		t.Errorf("hashed %d rows, want 32: ClusterId alone would hash 128, so the ProcId "+
			"half of the key did not narrow anything (narrowed on %q)",
			st.HashRows, st.ProbeAttr)
	}
	for _, want := range []string{"ClusterId", "ProcId"} {
		if !strings.Contains(st.ProbeAttr, want) {
			t.Errorf("narrowed on %q, want it to include %s", st.ProbeAttr, want)
		}
	}
	t.Logf("hashed %d, streamed %d, pairs %d, narrowed on %s to %d keys",
		st.HashRows, st.ScanRows, st.Pairs, st.ProbeAttr, st.ProbeKeys)
}

// The narrowed join must answer exactly what the unnarrowed one answers. The constraint is
// a superset filter, so getting it wrong drops rows rather than adding them.
func TestJoinCompositeKeyAnswersTheSameRows(t *testing.T) {
	cat := multiKeyFixture(t, 8192, 32)
	spec := multiKeySpec()

	key := func(ad *classad.ClassAd) string {
		c, _ := classad.ParseExpr("h.ClusterId")
		p, _ := classad.ParseExpr("h.ProcId")
		ci, _ := c.Eval(ad).IntValue()
		pi, _ := p.Eval(ad).IntValue()
		return fmt.Sprintf("%d/%d", ci, pi)
	}
	drain := func() []string {
		var st JoinStats
		seq, err := cat.Join(spec, JoinLimits{}, &st)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for ad := range seq {
			out = append(out, key(ad))
		}
		if st.Err != nil {
			t.Fatal(st.Err)
		}
		return out
	}
	var want, got []string
	withoutProbe(func() { want = drain() })
	got = drain()
	if len(want) == 0 {
		t.Fatal("the fixture joined nothing, so this compares two empty answers")
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("narrowed join returned %d rows, the full scan %d", len(got), len(want))
	}
}

// A step whose equalities are all unprobeable still declines, and says which test it failed
// rather than reporting the old "more than one equality" refusal that no longer applies.
func TestJoinCompositeKeyDeclinesWhenNothingIsIndexed(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	cfg := ArchiveConfig{SegmentSize: 4096} // no ValueAttrs: nothing indexed
	for _, name := range []string{"hist", "jobs"} {
		tbl, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			t.Fatal(cerr)
		}
		for i := 0; i < 4096; i++ {
			ad := classad.New()
			ad.InsertAttr("ClusterId", int64(i/4))
			ad.InsertAttr("ProcId", int64(i%4))
			if aerr := tbl.Append(ad); aerr != nil {
				t.Fatal(aerr)
			}
		}
	}
	var st JoinStats
	seq, err := cat.Join(multiKeySpec(), JoinLimits{}, &st)
	if err != nil {
		t.Fatal(err)
	}
	for range seq {
	}
	if st.ProbeKeys != 0 {
		t.Fatalf("narrowed on an unindexed table")
	}
	if !strings.Contains(st.ProbeWhy, "no index covers") {
		t.Errorf("declined with %q, want it to say no index covers the attributes", st.ProbeWhy)
	}
	if strings.Contains(st.ProbeWhy, "narrow on only one") {
		t.Errorf("still refusing a composite key for having several equalities: %q", st.ProbeWhy)
	}
}

// The build-side cost model has to see the same plan the probe would build.
//
// It prices a hashed side at what an index probe would make its read, and it used to do
// that only for a single-equality join -- so a composite key was priced at a full read of
// the hashed side however well an index covered it. That is the orientation the probe now
// wins on, and a cost model that cannot see the win recommends against it.
func TestBuildSideCostSeesACompositeKeyProbe(t *testing.T) {
	cat := multiKeyFixture(t, 8192, 32)
	spec := multiKeySpec()
	// Pricing is on the sides as written: the small table streams, the large one hashes.
	sides, _, err := spec.chain()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := cat.orientationCost(sides[0], sides[1], spec.Equi, false)
	if !ok {
		t.Fatal("the orientation could not be priced")
	}
	whole, ok := cat.sideReadSize(sides[1])
	if !ok {
		t.Fatal("the hashed side's size could not be read")
	}
	if whole < 8000 {
		t.Fatalf("the hashed side reads %d records, too few for this to mean anything", whole)
	}
	// With the probe seen, the hashed side is priced at index lookups rather than at its
	// whole size, so the total must come in under a full read of it.
	if got.total >= whole {
		t.Errorf("priced at %d records against a whole read of %d: the composite key's "+
			"index was not taken into account", got.total, whole)
	}
}

// equiHashAttrs names every bare attribute of the hashed halves, and skips computed ones
// rather than giving up on the whole join.
func TestEquiHashAttrs(t *testing.T) {
	equi := []JoinEqui{
		{Left: "ClusterId", Right: "ClusterId"},
		{Left: "ProcId", Right: "ProcId"},
		{Left: "X", Right: "substr(Y, 0, 2)"}, // computed: no attribute to probe
	}
	if got := strings.Join(equiHashAttrs(equi, false), ","); got != "ClusterId,ProcId" {
		t.Errorf("hashed halves %q, want ClusterId,ProcId", got)
	}
	// Swapped, the row halves are the hashed ones -- and all three are bare there.
	if got := strings.Join(equiHashAttrs(equi, true), ","); got != "ClusterId,ProcId,X" {
		t.Errorf("swapped hashed halves %q, want ClusterId,ProcId,X", got)
	}
	if got := equiHashAttrs(nil, false); len(got) != 0 {
		t.Errorf("a join with no equalities named %v", got)
	}
}
