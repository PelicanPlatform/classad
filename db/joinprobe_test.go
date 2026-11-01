package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// probeFixture builds a large indexed table to be hashed and a small one to stream, which
// is the shape an index-based equi join is for: most of the big table's buckets would be
// built and never probed.
func probeFixture(t testing.TB, big, small int, index bool) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	// A small segment size on purpose. Postings are built when a segment seals, so a
	// fixture whose rows all fit one open segment has the index configured and no index
	// built -- and every probe test would then be measuring a filter over a full scan
	// while reporting that it probed an index.
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
	// The streamed side names only every (big/small)'th cluster, so the probe has few keys
	// against many rows -- the regime where narrowing is supposed to win.
	stride := big / small
	for i := stride; i <= big; i += stride {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttrString("Owner", "alice")
		if aerr := jobs.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	return cat
}

func probeSpec(kind JoinKind) JoinSpec {
	return JoinSpec{
		Kind:  kind,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "hist", Alias: "h"},
		Cond:  []string{"j.ClusterId == h.ClusterId"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
}

// drainProbe collects the joined rows as "cluster/exit" and returns the stats alongside.
func drainProbe(t *testing.T, cat *Catalog, spec JoinSpec) ([]string, *JoinStats) {
	t.Helper()
	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	var out []string
	for row := range seq {
		out = append(out, evalText(row, "j.ClusterId")+"/"+evalText(row, "h.ExitCode"))
	}
	if st.Err != nil {
		t.Fatalf("join stopped early: %v", st.Err)
	}
	sort.Strings(out)
	return out, &st
}

// withoutProbe runs fn with the probe disabled, so the same join can be compared with and
// without it. Raising minProbeRows past any table in a test is the least invasive off
// switch: nothing else about the plan changes.
func withoutProbe(fn func()) {
	old := minProbeRows
	minProbeRows = 1 << 30
	defer func() { minProbeRows = old }()
	fn()
}

// forcingProbe runs fn with the COST GATE off but the probe itself on, which is what a
// benchmark needs in order to find where the gate belongs. Pricing a lookup at zero makes
// the comparison against the scan always favour probing; every soundness gate above it
// still applies.
func forcingProbe(fn func()) {
	oldCost, oldKeys := probeLookupCost, maxProbeKeys
	probeLookupCost, maxProbeKeys = 0, 1<<30
	defer func() { probeLookupCost, maxProbeKeys = oldCost, oldKeys }()
	fn()
}

func TestJoinIndexProbeNarrowsTheHashedSide(t *testing.T) {
	cat := probeFixture(t, 8192, 32, true)
	got, st := drainProbe(t, cat, probeSpec(JoinInner))
	if len(got) != 32 {
		t.Fatalf("joined %d rows, want 32", len(got))
	}
	if st.ProbeKeys != 32 {
		t.Errorf("ProbeKeys = %d (%q), want 32", st.ProbeKeys, st.ProbeWhy)
	}
	if st.ProbeAttr != "ClusterId" {
		t.Errorf("ProbeAttr = %q, want ClusterId", st.ProbeAttr)
	}
	// The point of the exercise: the hashed side was not read whole.
	if st.HashRows > 64 {
		t.Errorf("held %d rows of an 8192-row table; the probe did not narrow the scan", st.HashRows)
	}
}

// The equivalence test. An unsound narrowing drops rows that should have matched, and
// nothing else in the suite would notice.
func TestJoinIndexProbeAgreesWithTheFullScan(t *testing.T) {
	for _, kind := range []JoinKind{JoinInner, JoinLeft} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			cat := probeFixture(t, 4096, 16, true)
			with, st := drainProbe(t, cat, probeSpec(kind))
			if st.ProbeKeys == 0 {
				t.Fatalf("the probe did not fire, so there is nothing to compare: %s", st.ProbeWhy)
			}
			var without []string
			withoutProbe(func() {
				var wst JoinStats
				seq, err := cat.Join(probeSpec(kind), JoinLimits{}, &wst)
				if err != nil {
					t.Fatal(err)
				}
				for row := range seq {
					without = append(without, evalText(row, "j.ClusterId")+"/"+evalText(row, "h.ExitCode"))
				}
				if wst.ProbeKeys != 0 {
					t.Fatalf("the probe fired with it disabled")
				}
				sort.Strings(without)
			})
			if strings.Join(with, " ") != strings.Join(without, " ") {
				t.Errorf("the probe changed the answer.\n with: %v\n without: %v", with, without)
			}
		})
	}
}

// An integer key on one side and a real on the other is ClassAd-equal, so the pair must
// join. The hash normalizes both to one numeric form; the probe has to agree, because an
// index that segregates by type could answer an integer probe without the real rows.
//
// This is the narrowing's sharpest edge: nothing else in the join cares what numeric kind a
// key arrived as.
func TestJoinIndexProbeMatchesAcrossNumericKinds(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateArchiveTable("hist", ArchiveConfig{
		SegmentSize: 4096, ValueAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	// The hashed side stores REALS; filler keeps it over minProbeRows.
	for i := 1; i <= 2048; i++ {
		ad := classad.New()
		ad.InsertAttrFloat("ClusterId", float64(i))
		ad.InsertAttr("ExitCode", int64(i%7))
		if aerr := hist.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	// The streamed side names a handful of them as INTEGERS.
	for _, i := range []int64{7, 42, 1999} {
		ad := classad.New()
		ad.InsertAttr("ClusterId", i)
		if aerr := jobs.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	got, st := drainProbe(t, cat, probeSpec(JoinInner))
	if st.ProbeKeys != 3 {
		t.Fatalf("ProbeKeys = %d (%q), want 3", st.ProbeKeys, st.ProbeWhy)
	}
	var without []string
	withoutProbe(func() {
		var wst JoinStats
		seq, jerr := cat.Join(probeSpec(JoinInner), JoinLimits{}, &wst)
		if jerr != nil {
			t.Fatal(jerr)
		}
		for row := range seq {
			without = append(without, evalText(row, "j.ClusterId")+"/"+evalText(row, "h.ExitCode"))
		}
		sort.Strings(without)
	})
	if len(without) != 3 {
		t.Fatalf("the full scan joined %d rows, want 3; the fixture is wrong, not the probe", len(without))
	}
	if strings.Join(got, " ") != strings.Join(without, " ") {
		t.Errorf("int-vs-real keys: probe gave %v, full scan gave %v", got, without)
	}
}

// Without an index the constraint filters a full scan instead of replacing it, so it costs
// more than it saves. Declining says so rather than silently doing nothing.
func TestJoinIndexProbeDeclinesWithoutAnIndex(t *testing.T) {
	cat := probeFixture(t, 4096, 16, false)
	_, st := drainProbe(t, cat, probeSpec(JoinInner))
	if st.ProbeKeys != 0 {
		t.Fatalf("the probe fired against an unindexed attribute")
	}
	if !strings.Contains(st.ProbeWhy, "no index covers") {
		t.Errorf("ProbeWhy = %q, which does not name the missing index", st.ProbeWhy)
	}
}

// More index lookups than the scan has records: the probe would cost more than it saves.
// The gate has to decline on the COST, not on the key count -- maxProbeKeys is a separate
// backstop, and a test that tripped that one instead would say nothing about the model.
func TestJoinIndexProbeDeclinesWhenTheLookupsOutnumberTheRows(t *testing.T) {
	// 2048 rows across many segments, with a key for every row: segments*keys is far more
	// lookups than 2048 records, while the key count stays under maxProbeKeys.
	cat := probeFixture(t, 2048, 512, true)
	_, st := drainProbe(t, cat, probeSpec(JoinInner))
	if st.ProbeKeys != 0 {
		t.Fatalf("the probe fired when its lookups outnumbered the rows: %q", st.ProbeWhy)
	}
	if !strings.Contains(st.ProbeWhy, "index lookups") {
		t.Errorf("ProbeWhy = %q, which does not name the cost it declined on", st.ProbeWhy)
	}
}

// The key cap is the backstop below the cost model, and declines for its own reason.
func TestJoinIndexProbeDeclinesPastTheKeyCap(t *testing.T) {
	old := maxProbeKeys
	maxProbeKeys = 8
	defer func() { maxProbeKeys = old }()
	cat := probeFixture(t, 4096, 64, true)
	_, st := drainProbe(t, cat, probeSpec(JoinInner))
	if st.ProbeKeys != 0 {
		t.Fatalf("the probe fired past the key cap")
	}
	if !strings.Contains(st.ProbeWhy, "distinct keys") {
		t.Errorf("ProbeWhy = %q, which does not name the cap", st.ProbeWhy)
	}
}

// A CROSS join has no equality, so there is nothing to probe an index with -- and the
// reason has to say that rather than reporting a missing index.
func TestJoinIndexProbeDeclinesOnACrossJoin(t *testing.T) {
	cat := probeFixture(t, 2048, 8, true)
	spec := JoinSpec{
		Kind:  JoinCross,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "hist", Alias: "h"},
		Limit: 4,
	}
	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		t.Fatal(err)
	}
	for range seq {
	}
	if st.ProbeKeys != 0 {
		t.Fatal("the probe fired on a cross join")
	}
	if !strings.Contains(st.ProbeWhy, "no equality") {
		t.Errorf("ProbeWhy = %q, which does not say why", st.ProbeWhy)
	}
}

// A hashed side with its own constraint is not narrowed: its row count describes the table
// rather than the scan, so the trade cannot be priced from it.
func TestJoinIndexProbeDeclinesWhenTheHashedSideIsAlreadyFiltered(t *testing.T) {
	cat := probeFixture(t, 4096, 16, true)
	spec := probeSpec(JoinInner)
	spec.Right.Constraint = "ExitCode == 0"
	_, st := drainProbe(t, cat, spec)
	if st.ProbeKeys != 0 {
		t.Fatalf("the probe fired against an already-constrained side")
	}
	if !strings.Contains(st.ProbeWhy, "its own constraint") {
		t.Errorf("ProbeWhy = %q", st.ProbeWhy)
	}
}

// A streamed row whose key is undefined matches nothing through the equality, so it
// contributes no term -- and must not narrow the hashed side on its behalf, which would
// drop rows another streamed row needs.
func TestJoinIndexProbeIgnoresAnUndefinedKey(t *testing.T) {
	cat := probeFixture(t, 4096, 16, true)
	jobs, ok := cat.ArchiveTable("jobs")
	if !ok {
		t.Fatal("no jobs table")
	}
	keyless := classad.New()
	keyless.InsertAttrString("Owner", "bob") // no ClusterId at all
	if err := jobs.Append(keyless); err != nil {
		t.Fatal(err)
	}
	got, st := drainProbe(t, cat, probeSpec(JoinLeft))
	if st.ProbeKeys != 16 {
		t.Errorf("ProbeKeys = %d (%q), want 16 -- the keyless row should add no term",
			st.ProbeKeys, st.ProbeWhy)
	}
	// 16 matched rows plus the keyless one, null-extended.
	if len(got) != 17 {
		t.Errorf("joined %d rows, want 17 (16 matches and one null-extended)", len(got))
	}
}

// A table configured to index the join key, with nothing sealed and so nothing indexed.
//
// This is the case most easily mistaken for a working probe. The configuration says the
// attribute is indexed, the membership constraint still parses, and it still filters -- over
// a full scan, at a cost that grows with the key count. Reading the configuration instead of
// the postings is how an earlier draft of this measured a 45x pessimization and recorded it
// as evidence about index probes.
func TestJoinIndexProbeDeclinesWhenTheIndexIsConfiguredButNotBuilt(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	// A segment size nothing here will fill, so every row stays in the open segment and no
	// sidecar is ever written.
	hist, err := cat.CreateArchiveTable("hist", ArchiveConfig{
		SegmentSize: 1 << 26, ValueAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 1 << 26})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2048; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("ExitCode", int64(i%7))
		if aerr := hist.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	for i := 64; i <= 2048; i += 64 {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		if aerr := jobs.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	if sc := hist.SidecarSizes(); sc.Segments != 0 {
		t.Fatalf("the fixture sealed %d segments; it is meant to seal none", sc.Segments)
	}
	// ...while the configuration still claims the attribute is indexed.
	if _, v := hist.IndexedAttrs(); len(v) != 1 {
		t.Fatalf("IndexedAttrs reports %v; the fixture is wrong", v)
	}

	_, st := drainProbe(t, cat, probeSpec(JoinInner))
	if st.ProbeKeys != 0 {
		t.Fatalf("the probe fired against a configured-but-unbuilt index")
	}
	if !strings.Contains(st.ProbeWhy, "none is built") {
		t.Errorf("ProbeWhy = %q, which does not distinguish an unbuilt index from a missing one",
			st.ProbeWhy)
	}
}
