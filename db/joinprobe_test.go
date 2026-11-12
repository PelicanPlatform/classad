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
// benchmark needs in order to find where the gate belongs. Pricing the probe's work at zero
// and allowing it any share of the scan makes the comparison always favour probing; every
// soundness gate above it still applies.
//
// Both gates are turned off, not just one. The cost model has a rule for a step whose key
// fan-out it can price and a fallback for one it cannot, and a benchmark arm that silently
// took the fallback would be measuring the gate rather than the probe.
func forcingProbe(fn func()) {
	oldCost, oldKeys, oldFrac := probeLookupCost, maxProbeKeys, probeWorkFrac
	probeLookupCost, maxProbeKeys, probeWorkFrac = 0, 1<<30, 1<<30
	defer func() { probeLookupCost, maxProbeKeys, probeWorkFrac = oldCost, oldKeys, oldFrac }()
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

// The probe's own work outweighing the scan it replaces: it would cost more than it saves.
// The gate has to decline on the COST, not on the key count -- maxProbeKeys is a separate
// backstop, and a test that tripped that one instead would say nothing about the model.
func TestJoinIndexProbeDeclinesWhenItsWorkOutweighsTheScan(t *testing.T) {
	// 2048 rows with a key for every row: the probe would look up 512 keys and read the
	// 512 records they reach, a quarter of the table, while the key count stays under
	// maxProbeKeys so the backstop above is not what declines.
	cat := probeFixture(t, 2048, 512, true)
	_, st := drainProbe(t, cat, probeSpec(JoinInner))
	if st.ProbeKeys != 0 {
		t.Fatalf("the probe fired when its own work outweighed the scan: %q", st.ProbeWhy)
	}
	if !strings.Contains(st.ProbeWhy, "reaching about") {
		t.Errorf("ProbeWhy = %q, which does not name the records it would read", st.ProbeWhy)
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

// A hashed side whose own constraint no index covers still reads every record, so the whole
// table is what narrowing would save and the probe is priced against it -- and fires.
//
// This is the case the decision used to decline outright, on the grounds that a constrained
// side's row count describes the table rather than the scan. Here they are the same number,
// and the join gets its index probe back.
func TestJoinIndexProbePricesAnUnindexedConstraintAtTheWholeTable(t *testing.T) {
	cat := probeFixture(t, 4096, 16, true)
	spec := probeSpec(JoinInner)
	spec.Right.Constraint = "ExitCode == 0" // no index covers ExitCode: the read scans
	_, st := drainProbe(t, cat, spec)
	if st.ProbeKeys == 0 {
		t.Fatalf("the probe declined a side whose constraint narrows no read: %s", st.ProbeWhy)
	}
	if got, want := st.ProbeSteps[0].EstRecords, 4096; got != want {
		t.Errorf("priced the scan at %d records, want the whole table (%d)", got, want)
	}
}

// A hashed side whose own constraint an index DOES cover reads only part of the table, and
// the decision has to price the part rather than the whole: narrowing a read that is already
// small is what the key cap and the row floor exist to refuse.
func TestJoinIndexProbePricesAnIndexedConstraintAtWhatItReads(t *testing.T) {
	cat := probeFixture(t, 4096, 16, true)
	spec := probeSpec(JoinInner)
	spec.Right.Constraint = "ClusterId > 4000" // value-indexed: the read visits ~96 records
	_, st := drainProbe(t, cat, spec)
	if st.ProbeKeys != 0 {
		t.Fatalf("the probe narrowed a read of %d records", st.ProbeSteps[0].EstRecords)
	}
	if got := st.ProbeSteps[0].EstRecords; got == 0 || got > 512 {
		t.Errorf("priced the read at %d records, want the small part the index returns", got)
	}
	if !strings.Contains(st.ProbeWhy, "worth deciding") {
		t.Errorf("ProbeWhy = %q, which does not say the read was already small", st.ProbeWhy)
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

// A key whose fan-out makes the probe read most of the table. The lookups alone are cheap --
// 156 keys over 30 segments is well under the 20,000 records of the scan, so the rule this
// replaces took it -- but each key reaches 64 records, so the probe reads half the table to
// save the other half and runs several times slower than the hash.
//
// This is the case the cost model was blind to: it is decided entirely by a quantity the
// lookup count does not contain.
func TestJoinIndexProbeDeclinesAKeyWithHighFanOut(t *testing.T) {
	cat := fanFixture(t, 312, 64, 65536)
	spec := outerSpec(JoinInner)
	spec.Left.Constraint = "ClusterId <= 156"
	var st JoinStats
	withoutOuterDriven(func() {
		seq, err := cat.Join(spec, JoinLimits{}, &st)
		if err != nil {
			t.Fatal(err)
		}
		for range seq {
		}
	})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if st.ProbeKeys != 0 {
		t.Fatalf("the probe fired on a key reaching %d records of %d: %q",
			st.Pairs, 20_000, st.ProbeWhy)
	}
	if !strings.Contains(st.ProbeWhy, "reaching about") {
		t.Errorf("ProbeWhy = %q, which does not name the records it would read", st.ProbeWhy)
	}
}

// The same model has to keep taking a probe that a segment COUNT would refuse. 8,192 records
// over ~196 segments is 42 rows each; 128 keys priced at segments*keys is 25,088 lookups
// against 8,192 records, which the rule this replaces declined -- while the probe actually
// runs about eight times quicker than the hash, because a zone map rules nearly every
// segment out before a posting is read.
func TestJoinIndexProbeTakesALowFanOutKeyAcrossManySegments(t *testing.T) {
	cat := fanFixture(t, 8192, 1, 4096)
	_, segments, _, _, ok := cat.joinSideStats("hist", "")
	if !ok || segments < 100 {
		t.Fatalf("fixture has %d segments, want the many-segment shape this is about", segments)
	}
	spec := outerSpec(JoinInner)
	spec.Left.Constraint = "ClusterId <= 128"
	var st JoinStats
	withoutOuterDriven(func() {
		seq, err := cat.Join(spec, JoinLimits{}, &st)
		if err != nil {
			t.Fatal(err)
		}
		for range seq {
		}
	})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if st.ProbeKeys != 128 {
		t.Fatalf("ProbeKeys = %d across %d segments, want 128: %q",
			st.ProbeKeys, segments, st.ProbeWhy)
	}
}
