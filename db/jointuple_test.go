package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// NARROWING A COMPOSITE-KEY JOIN ON THE TUPLE RATHER THAN ON EACH ATTRIBUTE.
//
// Without a composite index the two halves of a (ClusterId, ProcId) key become two
// membership constraints, and their conjunction admits the CROSS PRODUCT of the two key
// sets. The FROM side here holds 32 pairs drawn from 32 clusters and 4 procs, so the cross
// product is 128 and three quarters of what the narrowed scan reads cannot match anything.

// sparseTupleFixture builds a hashed side keyed uniquely by (ClusterId, ProcId) and a
// streamed side holding a SPARSE subset of the pairs -- one proc per cluster, cycling, so
// that both attributes take several values and their cross product is much larger than the
// set of pairs actually present.
//
// composite configures the (ClusterId, ProcId) index; without it the same fixture exercises
// the per-attribute form, which is what the two are compared on.
func sparseTupleFixture(tb testing.TB, big, small int, composite bool) *Catalog {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	// Ordered, so the small side streams and the large one is hashed: the orientation a
	// probe exists for.
	cfg := ArchiveConfig{SegmentSize: 4096, ValueAttrs: []string{"ClusterId", "ProcId"}}
	if composite {
		cfg.CompositeAttrs = [][]string{{"ClusterId", "ProcId"}}
	}
	mk := func(name string) *ArchiveTable {
		t, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			tb.Fatal(cerr)
		}
		return t
	}
	hist, jobs := mk("hist"), mk("jobs")
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
		// Twice each: the FROM side holds `small` ROWS of `small`/2 distinct pairs, so a
		// constraint that did not deduplicate would carry twice the terms it needs and
		// pay twice the lookups for the same candidates.
		for range 2 {
			ad := classad.New()
			ad.InsertAttr("ClusterId", int64((k/2)*stride))
			ad.InsertAttr("ProcId", int64((k/2)%4))
			ad.InsertAttrString("Owner", "alice")
			if aerr := jobs.Append(ad); aerr != nil {
				tb.Fatal(aerr)
			}
		}
	}
	// One row that names a cluster and no proc. It can match nothing -- an undefined half
	// makes `j.ProcId == h.ProcId` undefined, not true -- so it is entitled to no part of
	// the narrowing, and the tuple form gives it none. The per-attribute form cannot be so
	// careful: it takes the row's ClusterId on its own and widens the scan by a cluster.
	orphan := classad.New()
	orphan.InsertAttr("ClusterId", int64(big/4-1))
	if aerr := jobs.Append(orphan); aerr != nil {
		tb.Fatal(aerr)
	}
	return cat
}

// withoutTupleProbe runs fn with the whole-key form off, which is how the per-attribute
// form -- the thing it has to beat -- is still reachable for comparison.
func withoutTupleProbe(fn func()) {
	old := tupleProbes
	tupleProbes = false
	defer func() { tupleProbes = old }()
	fn()
}

func sparseTupleSpec() JoinSpec {
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

func runSparseJoin(tb testing.TB, cat *Catalog) (rows int, st JoinStats) {
	tb.Helper()
	seq, err := cat.Join(sparseTupleSpec(), JoinLimits{}, &st)
	if err != nil {
		tb.Fatal(err)
	}
	for range seq {
		rows++
	}
	if st.Err != nil {
		tb.Fatal(st.Err)
	}
	return rows, st
}

// The measurement, and the thing that surprised me: what narrows this scan is the SHAPE of
// the constraint, not the composite index.
//
// A disjunct pinning both components to one value is answered without any composite at all,
// by intersecting one cluster's posting with one proc's -- which is exactly that pair. The
// third arm here shows it: with the cost gate out of the way and no composite configured,
// the tuple spine hashes the same 32 rows. What the composite changes is the PRICE of a
// disjunct, one lookup instead of one per component, and on this fixture that is what brings
// the form inside the gate at all (32 tuples over 196 segments is 6272 lookups with the
// composite against 12544 without, over a read of 8192 records).
//
// So the composite is not what makes the tuple form selective; it is what makes it
// affordable. Worth stating because the two are easy to confuse, and because a cost model
// that stopped distinguishing them would still pass a test asserting only the row counts.
//
// Asserted on HashRows -- what the narrowed read actually produced -- rather than on the
// plan's labels, because the two can disagree and the rows read are the point.
func TestTupleProbeHashesOnlyTheTuplesHeld(t *testing.T) {
	// 64 rows of 32 distinct pairs, plus one row that can match nothing.
	var plainRows, bareRows int
	var plain, bare JoinStats
	withoutTupleProbe(func() {
		plainRows, plain = runSparseJoin(t, sparseTupleFixture(t, 8192, 64, false))
	})
	withProbeCaps(maxProbeKeys, 0, func() { // no composite, and the cost gate neutralised
		bareRows, bare = runSparseJoin(t, sparseTupleFixture(t, 8192, 64, false))
	})
	compRows, comp := runSparseJoin(t, sparseTupleFixture(t, 8192, 64, true))

	// Anti-vacuity: every arm must have been narrowed, and narrowed the way the arm is
	// named. A gate declining anywhere would make the comparison empty.
	if plain.ProbeKeys == 0 {
		t.Fatalf("the per-attribute arm was not narrowed at all: %s", plain.ProbeWhy)
	}
	if plain.ProbeSteps[0].Tuple != "" {
		t.Fatalf("the per-attribute arm narrowed on the tuple %q", plain.ProbeSteps[0].Tuple)
	}
	for _, arm := range []struct {
		name string
		st   JoinStats
		comp bool
	}{{"no composite", bare, false}, {"composite", comp, true}} {
		if arm.st.ProbeSteps[0].Tuple == "" {
			t.Fatalf("the %s arm did not narrow on the tuple: %s",
				arm.name, arm.st.ProbeSteps[0].TupleWhy)
		}
		if got := arm.st.ProbeSteps[0].TupleComposite; got != arm.comp {
			t.Fatalf("the %s arm reports TupleComposite=%v", arm.name, got)
		}
	}

	if plainRows != bareRows || plainRows != compRows {
		t.Errorf("the three forms joined different row counts: %d, %d, %d",
			plainRows, bareRows, compRows)
	}
	// The in-chains admit the cross product of 33 clusters (32 held as pairs, plus the
	// orphan's) and 4 procs: 132 rows, of which 100 cannot match.
	if plain.HashRows != 132 {
		t.Errorf("per-attribute narrowing hashed %d rows, want 132 (the cross product)",
			plain.HashRows)
	}
	// Both tuple arms admit the 32 pairs actually held. If the composite arm ever reads as
	// narrower, the per-component fallback inside a disjunct has broken.
	if bare.HashRows != 32 || comp.HashRows != 32 {
		t.Errorf("tuple narrowing hashed %d rows without a composite and %d with one, want 32 both",
			bare.HashRows, comp.HashRows)
	}
	// One term per DISTINCT tuple: 64 rows naming 32 pairs, and the orphan naming none.
	if comp.ProbeKeys != 32 {
		t.Errorf("the tuple constraint named %d terms, want 32 (one per distinct pair, "+
			"deduplicated, and nothing for the row with no ProcId)", comp.ProbeKeys)
	}
	t.Logf("per-attribute: hashed %d on %q to %d keys; tuple: hashed %d (no composite, gate "+
		"off) and %d (composite) to %d terms", plain.HashRows, plain.ProbeAttr,
		plain.ProbeKeys, bare.HashRows, comp.HashRows, comp.ProbeKeys)
}

// Without a composite each disjunct costs a lookup per component, and on this shape that is
// what the gate declines.
//
// Recorded as its own test because the decision is a COST one and the measurement disagrees
// with it: forcing the form through anyway ran the join in about 2.0-2.7 ms against the
// per-attribute form's 4.3-7.6 ms on the same machine. That says probeLookupCost -- one
// scanned record per lookup, swept for multi-value `in` probes -- prices a single-value
// lookup too dearly for this access pattern. Retuning it is a change to a constant with its
// own calibration behind it, and is not made here on the strength of one fixture.
//
// It was then swept properly (joinprobecost_bench_test.go) and the reading holds AT THIS
// SEGMENT COUNT and not in general: over 196 segments one single-value lookup is worth
// about 0.3 of a scanned record against a multi-value lookup's 1, so the decline below is
// indeed about 3x too eager. Over 13 segments the same sweep puts it at 1.9, where the gate
// is if anything too permissive. The spread is the segment factor, not the constant, so the
// constant is still not retuned -- see probeLookupCost for the tables.
func TestTupleProbeDeclinesWithoutACompositeOnThisShape(t *testing.T) {
	_, st := runSparseJoin(t, sparseTupleFixture(t, 8192, 64, false))
	if st.ProbeSteps[0].Tuple != "" {
		t.Fatalf("narrowed on the tuple with no composite to pay for it: %q",
			st.ProbeSteps[0].Tuple)
	}
	why := st.ProbeSteps[0].TupleWhy
	if !strings.Contains(why, "index lookups") {
		t.Errorf("the decline does not read as a cost decision: %q", why)
	}
	if st.ProbeKeys == 0 {
		t.Errorf("fell back to no narrowing at all: %s", st.ProbeWhy)
	}
	t.Logf("declined: %s", why)
}

// The narrowed join must answer exactly what the unnarrowed one answers. The constraint is a
// superset filter, so an error here drops rows rather than adding them.
func TestTupleProbeAnswersTheSameRows(t *testing.T) {
	cat := sparseTupleFixture(t, 8192, 32, true)
	key := func(ad *classad.ClassAd) string {
		c, _ := classad.ParseExpr("h.ClusterId")
		p, _ := classad.ParseExpr("h.ProcId")
		ci, _ := c.Eval(ad).IntValue()
		pi, _ := p.Eval(ad).IntValue()
		return fmt.Sprintf("%d/%d", ci, pi)
	}
	drain := func() []string {
		var st JoinStats
		seq, err := cat.Join(sparseTupleSpec(), JoinLimits{}, &st)
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
	var want []string
	withoutProbe(func() { want = drain() })
	got := drain()
	if len(want) == 0 {
		t.Fatal("the fixture joined nothing, so this compares two empty answers")
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the tuple-narrowed join returned %d rows, the full scan %d", len(got), len(want))
	}
}

// A composite covering only SOME of the attributes the step would narrow on is not used.
// Mixing a tuple spine with a leftover in-chain puts an `&&` at the top of the constraint,
// and the planner's normal form then reaches neither: the spine survives only as a top-level
// disjunction. The step falls back to the per-attribute form and says why.
func TestTupleProbeDeclinesAPartialComposite(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	cfg := ArchiveConfig{
		SegmentSize:    4096,
		ValueAttrs:     []string{"ClusterId", "ProcId", "Attempt"},
		CompositeAttrs: [][]string{{"ClusterId", "Attempt"}}, // covers neither key alone nor both
	}
	mk := func(name string) *ArchiveTable {
		t2, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			t.Fatal(cerr)
		}
		return t2
	}
	hist, jobs := mk("hist"), mk("jobs")
	for i := 0; i < 8192; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i/4))
		ad.InsertAttr("ProcId", int64(i%4))
		ad.InsertAttr("Attempt", int64(1))
		if aerr := hist.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	for k := 0; k < 32; k++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(k*64))
		ad.InsertAttr("ProcId", int64(k%4))
		if aerr := jobs.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	var st JoinStats
	n := 0
	withProbeCaps(maxProbeKeys, 0, func() { // the cost gate is not what is under test
		seq, jerr := cat.Join(sparseTupleSpec(), JoinLimits{}, &st)
		if jerr != nil {
			t.Fatal(jerr)
		}
		for range seq {
			n++
		}
	})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	// Anti-vacuity: the composite IS built here, so "not used" is a decision and not an
	// absence. Proven by asking the table for it.
	tbl, found := cat.ArchiveTable("hist")
	if !found {
		t.Fatal("no hist table")
	}
	if len(tbl.CompositeAttrs()) != 1 {
		t.Fatalf("the fixture configured no composite: %v", tbl.CompositeAttrs())
	}
	// The tuple form still runs -- it does not need a composite -- but this composite does
	// not answer it, so each disjunct is priced at one lookup per component.
	if st.ProbeSteps[0].Tuple == "" {
		t.Fatalf("the tuple form did not run at all: %s", st.ProbeSteps[0].TupleWhy)
	}
	if st.ProbeSteps[0].TupleComposite {
		t.Errorf("claimed a composite answers %q, but the only one configured covers "+
			"Attempt, which the key does not mention", st.ProbeSteps[0].Tuple)
	}
	if n != 32 {
		t.Errorf("joined %d rows, want 32", n)
	}
}

// A composite whose components the step pins in the OTHER order is still used: a composite
// matches a conjunction that pins each of its components, not one written in its order.
func TestTupleProbeMatchesEitherOrder(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	cfg := ArchiveConfig{
		SegmentSize:    4096,
		ValueAttrs:     []string{"ClusterId", "ProcId"},
		CompositeAttrs: [][]string{{"ProcId", "ClusterId"}}, // reversed against the join's order
	}
	mk := func(name string) *ArchiveTable {
		t2, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			t.Fatal(cerr)
		}
		return t2
	}
	hist, jobs := mk("hist"), mk("jobs")
	for i := 0; i < 8192; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i/4))
		ad.InsertAttr("ProcId", int64(i%4))
		if aerr := hist.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	for k := 0; k < 32; k++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(k*64))
		ad.InsertAttr("ProcId", int64(k%4))
		if aerr := jobs.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	var st JoinStats
	seq, jerr := cat.Join(sparseTupleSpec(), JoinLimits{}, &st)
	if jerr != nil {
		t.Fatal(jerr)
	}
	n := 0
	for range seq {
		n++
	}
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if st.ProbeSteps[0].Tuple == "" {
		t.Fatalf("a reversed composite was not matched: %s", st.ProbeSteps[0].TupleWhy)
	}
	if st.HashRows != 32 {
		t.Errorf("hashed %d rows, want 32", st.HashRows)
	}
	if n != 32 {
		t.Errorf("joined %d rows, want 32", n)
	}
}

// withProbeCaps runs fn with the key cap and the per-lookup cost moved.
//
// Both have to move together to reach the cap at all. The cost gate is priced per SEGMENT
// per key, and an archive holds far more segments than a reader expects -- the fixtures here
// spread 8192 records over 161 of them -- so for these shapes the cost gate bites long
// before the key cap does, whichever form is being priced. Pricing a lookup at zero takes
// the cost gate out of the way so the cap can be tested on its own; every soundness gate
// above it still applies.
func withProbeCaps(keys, cost int, fn func()) {
	oldKeys, oldCost := maxProbeKeys, probeLookupCost
	maxProbeKeys, probeLookupCost = keys, cost
	defer func() { maxProbeKeys, probeLookupCost = oldKeys, oldCost }()
	fn()
}

// Past the key cap the tuple form declines and the step falls back to narrowing per
// attribute, which is priced separately and here still pays. The decline is reported beside
// the decision that was taken, not instead of it: a step that narrowed per attribute because
// the composite was too expensive otherwise reads, in a plan stating only the outcome,
// exactly like one whose table has no composite at all.
//
// This is the shape where the tuple form is the one that blows the cap: both attributes fan
// out, so the pairs outnumber the key values badly. In the narrow shape the opposite holds
// and the tuple form is under the cap while the per-attribute form is not -- 32 pairs drawn
// from 32 clusters and 4 procs is 32 terms against 36 keys.
func TestTupleProbeDeclinesPastTheKeyCap(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	cfg := ArchiveConfig{
		SegmentSize:    4096,
		ValueAttrs:     []string{"ClusterId", "ProcId"},
		CompositeAttrs: [][]string{{"ClusterId", "ProcId"}},
	}
	mk := func(name string) *ArchiveTable {
		t2, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			t.Fatal(cerr)
		}
		return t2
	}
	hist, jobs := mk("hist"), mk("jobs")
	// 40 clusters x 40 procs: 1600 pairs from 80 key values.
	const side = 40
	for c := 0; c < side; c++ {
		for pr := 0; pr < side; pr++ {
			for _, tbl := range []*ArchiveTable{hist, jobs} {
				ad := classad.New()
				ad.InsertAttr("ClusterId", int64(c))
				ad.InsertAttr("ProcId", int64(pr))
				if aerr := tbl.Append(ad); aerr != nil {
					t.Fatal(aerr)
				}
			}
		}
	}
	var st JoinStats
	var n int
	withProbeCaps(100, 0, func() { // 1600 tuples is past 100; 80 keys is not
		seq, jerr := cat.Join(sparseTupleSpec(), JoinLimits{}, &st)
		if jerr != nil {
			t.Fatal(jerr)
		}
		for range seq {
			n++
		}
	})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if n != side*side {
		t.Errorf("joined %d rows, want %d", n, side*side)
	}
	if st.ProbeSteps[0].Tuple != "" {
		t.Errorf("narrowed through %q despite the pairs being past the cap",
			st.ProbeSteps[0].Tuple)
	}
	why := st.ProbeSteps[0].TupleWhy
	if !strings.Contains(why, "ClusterId+ProcId") || !strings.Contains(why, "distinct") {
		t.Errorf("the composite decline does not name the composite and the cap: %q", why)
	}
	// It fell back rather than giving up: 40 clusters and 40 procs is 80 keys, inside the
	// same cap. Without this the test would pass on a change that simply stopped narrowing.
	if st.ProbeKeys != 2*side {
		t.Errorf("fell back to %d per-attribute keys, want %d: %s",
			st.ProbeKeys, 2*side, st.ProbeWhy)
	}
}

// The tuple form is priced, not merely offered. One lookup per tuple per segment is the
// cost with a composite, one per component without, and a FROM side holding enough distinct
// tuples pays more than the scan it would replace costs.
//
// The fixture is sparse on purpose -- 1000 pairs drawn from a 16x64 cross product -- so that
// the cross-product gate below is not what declines it. The cap is raised out of the way for
// the same reason: the three gates are tested apart, because any one of them alone would
// hide a change that removed another.
func TestTupleProbeDeclinesOnCost(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	cfg := ArchiveConfig{
		SegmentSize:    4096,
		ValueAttrs:     []string{"ClusterId", "ProcId"},
		CompositeAttrs: [][]string{{"ClusterId", "ProcId"}},
	}
	mk := func(name string) *ArchiveTable {
		t2, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			t.Fatal(cerr)
		}
		return t2
	}
	hist, jobs := mk("hist"), mk("jobs")
	const side = 64
	for c := 0; c < side; c++ {
		for pr := 0; pr < side; pr++ {
			ad := classad.New()
			ad.InsertAttr("ClusterId", int64(c))
			ad.InsertAttr("ProcId", int64(pr))
			if aerr := hist.Append(ad); aerr != nil {
				t.Fatal(aerr)
			}
		}
	}
	const pairs = 1000 // 16 clusters x 64 procs is a cross product of 1024
	for k := 0; k < pairs; k++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(k/side))
		ad.InsertAttr("ProcId", int64(k%side))
		if aerr := jobs.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	var st JoinStats
	var n int
	withProbeCaps(1<<20, 1, func() { // no key cap in the way; the real per-lookup cost applies
		seq, jerr := cat.Join(sparseTupleSpec(), JoinLimits{}, &st)
		if jerr != nil {
			t.Fatal(jerr)
		}
		for range seq {
			n++
		}
	})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if n != pairs {
		t.Errorf("joined %d rows, want %d", n, pairs)
	}
	if st.ProbeSteps[0].Tuple != "" {
		t.Errorf("narrowed on %q despite the lookups outnumbering the records read",
			st.ProbeSteps[0].Tuple)
	}
	why := st.ProbeSteps[0].TupleWhy
	if !strings.Contains(why, "ClusterId+ProcId") || !strings.Contains(why, "index lookups") {
		t.Errorf("the decline does not read as a cost decision: %q", why)
	}
	t.Logf("declined: %s", why)
}

// When the FROM side's tuples ARE the whole cross product of its key values, the spine buys
// nothing the in-chains do not already give, and costs more lookups for it. That is what a
// key half taking a single value looks like -- and it is the common shape, which is why the
// gate is worth having: narrowing 32 clusters against one proc admits exactly the 32 pairs
// either way.
func TestTupleProbeDeclinesAFullCrossProduct(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	cfg := ArchiveConfig{
		SegmentSize:    4096,
		ValueAttrs:     []string{"ClusterId", "ProcId"},
		CompositeAttrs: [][]string{{"ClusterId", "ProcId"}},
	}
	mk := func(name string) *ArchiveTable {
		t2, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			t.Fatal(cerr)
		}
		return t2
	}
	hist, jobs := mk("hist"), mk("jobs")
	for i := 0; i < 8192; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i/4))
		ad.InsertAttr("ProcId", int64(i%4))
		if aerr := hist.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	// 32 clusters, every row naming proc 1: 32 pairs, and 32 x 1 is the cross product.
	for k := 0; k < 32; k++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(k*64))
		ad.InsertAttr("ProcId", int64(1))
		if aerr := jobs.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	var st JoinStats
	var n int
	withProbeCaps(maxProbeKeys, 0, func() { // cost is not what should decline this
		seq, jerr := cat.Join(sparseTupleSpec(), JoinLimits{}, &st)
		if jerr != nil {
			t.Fatal(jerr)
		}
		for range seq {
			n++
		}
	})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if n != 32 {
		t.Errorf("joined %d rows, want 32", n)
	}
	if st.ProbeSteps[0].Tuple != "" {
		t.Errorf("narrowed on %q, which admits exactly what the in-chains admit",
			st.ProbeSteps[0].Tuple)
	}
	if !strings.Contains(st.ProbeSteps[0].TupleWhy, "cross product") {
		t.Errorf("the decline does not say the spine buys nothing: %q",
			st.ProbeSteps[0].TupleWhy)
	}
	// It fell back, and the fallback is exactly as narrow here.
	if st.HashRows != 32 {
		t.Errorf("hashed %d rows, want 32", st.HashRows)
	}
}

// A composite covering a STRICT SUBSET of the attributes the step would narrow on is not
// used either, and this is the case that matters: such a composite is genuinely useful --
// its tuple is selective and the index can answer it -- and taking it would still be wrong.
//
// The constraint can carry either a tuple spine or a conjunction of in-chains, never both.
// Narrowing through (ClusterId, ProcId) while a third equality on Attempt went unused would
// trade an attribute's worth of selectivity for the pair's, and which of those is the better
// bargain depends on the dropped attribute's distribution in the hashed table. Nothing here
// measures that, so the step keeps the form whose cost it can price.
func TestTupleProbeDeclinesASubsetComposite(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	cfg := ArchiveConfig{
		SegmentSize:    4096,
		ValueAttrs:     []string{"ClusterId", "ProcId", "Attempt"},
		CompositeAttrs: [][]string{{"ClusterId", "ProcId"}}, // two of the key's three attributes
	}
	mk := func(name string) *ArchiveTable {
		t2, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			t.Fatal(cerr)
		}
		return t2
	}
	hist, jobs := mk("hist"), mk("jobs")
	// Every (cluster, proc) carries three attempts, so dropping Attempt from the constraint
	// is visible in what gets hashed.
	for c := 0; c < 256; c++ {
		for pr := 0; pr < 4; pr++ {
			for a := 0; a < 3; a++ {
				ad := classad.New()
				ad.InsertAttr("ClusterId", int64(c))
				ad.InsertAttr("ProcId", int64(pr))
				ad.InsertAttr("Attempt", int64(a))
				if aerr := hist.Append(ad); aerr != nil {
					t.Fatal(aerr)
				}
			}
		}
	}
	for k := 0; k < 32; k++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(k*8))
		ad.InsertAttr("ProcId", int64(k%4))
		ad.InsertAttr("Attempt", int64(1))
		if aerr := jobs.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	spec := sparseTupleSpec()
	spec.Cond = append(spec.Cond, "j.Attempt == h.Attempt")
	spec.Equi = append(spec.Equi, JoinEqui{Left: "Attempt", Right: "Attempt"})

	var st JoinStats
	n := 0
	withProbeCaps(maxProbeKeys, 0, func() { // the cost gate is not what is under test
		seq, jerr := cat.Join(spec, JoinLimits{}, &st)
		if jerr != nil {
			t.Fatal(jerr)
		}
		for range seq {
			n++
		}
	})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	// Anti-vacuity: the composite exists and its components are both pinned by the step, so
	// "not used" is a decision rather than an absence.
	tbl, found := cat.ArchiveTable("hist")
	if !found {
		t.Fatal("no hist table")
	}
	if len(tbl.CompositeAttrs()) != 1 {
		t.Fatalf("the fixture configured no composite: %v", tbl.CompositeAttrs())
	}
	// The tuple form runs over all THREE attributes. What it must not do is let the
	// two-attribute composite answer it and quietly drop Attempt.
	if st.ProbeSteps[0].Tuple == "" {
		t.Fatalf("the tuple form did not run at all: %s", st.ProbeSteps[0].TupleWhy)
	}
	if st.ProbeSteps[0].TupleComposite {
		t.Errorf("claimed the (ClusterId, ProcId) composite answers %q, which has three "+
			"components", st.ProbeSteps[0].Tuple)
	}
	if n != 32 {
		t.Errorf("joined %d rows, want 32", n)
	}
	// The 32 triples the FROM side holds. Narrowing through the pair composite and dropping
	// Attempt would hash 32 pairs x 3 attempts = 96.
	if st.HashRows != 32 {
		t.Errorf("hashed %d rows, want 32 (all three equalities narrowing)", st.HashRows)
	}
}
