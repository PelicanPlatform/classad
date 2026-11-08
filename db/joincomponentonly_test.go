package db

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// NARROWING A JOIN THROUGH A COMPOSITE WHOSE COMPONENTS CARRY NO INDEX OF THEIR OWN.
//
// A component-only attribute is correctly reported as not indexed -- no constraint on it
// alone is index-answered -- and the probe gate used to drop it there, which left a join on
// (ClusterId, ProcId) unable to use the very composite configured for it. The gate now keeps
// the whole probeable key beside the indexed subset, and offers the whole key to the tuple
// planner, which takes it when a composite covers it.

// componentOnlyFixture builds the sparse-pair shape against a table whose ProcId is a
// composite component and nothing else. indexProc makes ProcId a real index instead, which
// is the configuration the same join used before component-only attributes existed.
func componentOnlyFixture(tb testing.TB, indexProc bool) *Catalog {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	cfg := ArchiveConfig{
		SegmentSize:    4096,
		ValueAttrs:     []string{"ClusterId"},
		CompositeAttrs: [][]string{{"ClusterId", "ProcId"}},
	}
	if indexProc {
		cfg.ValueAttrs = append(cfg.ValueAttrs, "ProcId")
	} else {
		cfg.ValueComponentAttrs = []string{"ProcId"}
	}
	mk := func(name string) *ArchiveTable {
		t, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			tb.Fatal(cerr)
		}
		return t
	}
	hist, jobs := mk("hist"), mk("jobs")
	for i := 0; i < 8192; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i/4))
		ad.InsertAttr("ProcId", int64(i%4))
		ad.InsertAttrString("Pad", strings.Repeat("x", 64))
		if aerr := hist.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	for k := 0; k < 32; k++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(k*64))
		ad.InsertAttr("ProcId", int64(k%4)) // 32 pairs out of a 32x4 cross product
		if aerr := jobs.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	return cat
}

// The gap this closes: with ProcId a component only, the join narrows on the tuple exactly
// as it does when ProcId carries its own index.
func TestJoinNarrowsThroughAComponentOnlyComposite(t *testing.T) {
	for _, arm := range []struct {
		name      string
		indexProc bool
	}{{"ProcId indexed", true}, {"ProcId component-only", false}} {
		t.Run(arm.name, func(t *testing.T) {
			cat := componentOnlyFixture(t, arm.indexProc)
			var st JoinStats
			seq, err := cat.Join(sparseTupleSpec(), JoinLimits{}, &st)
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
				t.Errorf("joined %d rows, want 32", n)
			}
			if st.ProbeSteps[0].Tuple == "" {
				t.Fatalf("did not narrow on the tuple: %s / %s",
					st.ProbeSteps[0].TupleWhy, st.ProbeSteps[0].Why)
			}
			if !st.ProbeSteps[0].TupleComposite {
				t.Errorf("narrowed on %q without reporting the composite that answers it",
					st.ProbeSteps[0].Tuple)
			}
			// The whole point: the pairs held, not their cross product.
			if st.HashRows != 32 {
				t.Errorf("hashed %d rows, want 32", st.HashRows)
			}
			t.Logf("tuple=%q composite=%v hashed=%d keys=%d",
				st.ProbeSteps[0].Tuple, st.ProbeSteps[0].TupleComposite,
				st.HashRows, st.ProbeKeys)
		})
	}
}

// Anti-vacuity for the test above: a component-only attribute really is not indexed, so the
// per-attribute form cannot narrow on it and the tuple form is doing the work.
func TestComponentOnlyAttrIsNotReportedIndexed(t *testing.T) {
	cat := componentOnlyFixture(t, false)
	tbl, found := cat.ArchiveTable("hist")
	if !found {
		t.Fatal("no hist table")
	}
	_, vals := tbl.IndexedAttrs()
	for _, v := range vals {
		if strings.EqualFold(v, "ProcId") {
			t.Fatalf("ProcId is reported as a value index: %v", vals)
		}
	}
	if !containsFold(vals, "ClusterId") {
		t.Fatalf("ClusterId should still be indexed: %v", vals)
	}
	// And the composite over it IS configured, so "not indexed" is the component-only
	// state rather than an absent configuration.
	if got := tbl.CompositeAttrs(); len(got) != 1 {
		t.Fatalf("composite not configured: %v", got)
	}
}

// A join whose key is answerable ONLY as a whole: neither half carries an index. Before the
// gate kept the unfiltered key this declined outright, because no candidate survived the
// indexed filter and the step never reached the tuple planner.
func TestJoinNarrowsWhenNoComponentIsIndexed(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	cfg := ArchiveConfig{
		SegmentSize:         4096,
		ValueComponentAttrs: []string{"ClusterId", "ProcId"}, // neither indexed alone
		CompositeAttrs:      [][]string{{"ClusterId", "ProcId"}},
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
	if n != 32 {
		t.Errorf("joined %d rows, want 32", n)
	}
	if st.ProbeSteps[0].Tuple == "" {
		t.Fatalf("a key answerable only as a whole was not narrowed: %s / %s",
			st.ProbeSteps[0].TupleWhy, st.ProbeSteps[0].Why)
	}
	if !st.ProbeSteps[0].TupleComposite {
		t.Errorf("narrowed on %q without a composite, but neither component is indexed",
			st.ProbeSteps[0].Tuple)
	}
	if st.HashRows != 32 {
		t.Errorf("hashed %d rows, want 32", st.HashRows)
	}
}

// The narrowed join must answer exactly what the unnarrowed one answers.
func TestComponentOnlyJoinAnswersTheSameRows(t *testing.T) {
	cat := componentOnlyFixture(t, false)
	drain := func() int {
		var st JoinStats
		seq, err := cat.Join(sparseTupleSpec(), JoinLimits{}, &st)
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
		return n
	}
	var want int
	withoutProbe(func() { want = drain() })
	if want == 0 {
		t.Fatal("the fixture joined nothing, so this compares two empty answers")
	}
	if got := drain(); got != want {
		t.Errorf("narrowed join returned %d rows, the full scan %d", got, want)
	}
}

// When a composite covers the indexed subset AND a wider one covers the whole key, the whole
// key wins. The wider tuple admits exactly the triples the FROM side holds; the narrower one
// admits pairs and leaves the component-only attribute unconstrained, because an attribute
// with no index of its own contributes no membership constraint to fall back on.
func TestJoinPrefersTheWidestTupleAComposite(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	cfg := ArchiveConfig{
		SegmentSize:         4096,
		ValueAttrs:          []string{"ClusterId", "ProcId"},
		ValueComponentAttrs: []string{"Attempt"},
		CompositeAttrs: [][]string{
			{"ClusterId", "ProcId"},            // covers the indexed subset
			{"ClusterId", "ProcId", "Attempt"}, // covers the whole key
		},
	}
	mk := func(name string) *ArchiveTable {
		t2, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			t.Fatal(cerr)
		}
		return t2
	}
	hist, jobs := mk("hist"), mk("jobs")
	// Three attempts per (cluster, proc), so leaving Attempt unconstrained is visible in
	// what gets hashed.
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
	seq, jerr := cat.Join(spec, JoinLimits{}, &st)
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
	// Anti-vacuity: both composites really are configured, so the choice between them is a
	// decision rather than an absence.
	tbl, found := cat.ArchiveTable("hist")
	if !found {
		t.Fatal("no hist table")
	}
	if got := tbl.CompositeAttrs(); len(got) != 2 {
		t.Fatalf("want both composites configured, got %v", got)
	}
	if n != 32 {
		t.Errorf("joined %d rows, want 32", n)
	}
	if got := st.ProbeSteps[0].Tuple; !strings.Contains(strings.ToLower(got), "attempt") {
		t.Errorf("narrowed on %q, want the whole key including Attempt", got)
	}
	// 32 triples. Narrowing on the pair instead would hash 32 x 3 attempts = 96, because
	// nothing else constrains Attempt.
	if st.HashRows != 32 {
		t.Errorf("hashed %d rows, want 32 (the triples held)", st.HashRows)
	}
}

// A component-only declaration must survive a reopen. If it does not, newCompositeSpecs
// refuses the tuple whose component it declared, the composite silently vanishes, and every
// query that relied on it gets slower with no error anywhere.
func TestComponentOnlyDeclarationSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	cfg := ArchiveConfig{
		SegmentSize:         4096,
		ValueAttrs:          []string{"ClusterId"},
		ValueComponentAttrs: []string{"ProcId"},
		CompositeAttrs:      [][]string{{"ClusterId", "ProcId"}},
	}
	tbl, err := openArchiveTable(dir, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8192; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i/4))
		ad.InsertAttr("ProcId", int64(i%4))
		if aerr := tbl.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	if got := tbl.CompositeAttrs(); len(got) != 1 {
		t.Fatalf("before reopen the composite is already missing: %v", got)
	}
	if cerr := tbl.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	// Reopen with an EMPTY config, the way a restart does: the file is authoritative.
	re, err := openArchiveTable(dir, ArchiveConfig{}, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer re.Close()
	if got := re.CompositeAttrs(); len(got) != 1 {
		t.Fatalf("the composite did not survive the reopen: %v", got)
	}
	_, vals := re.IndexedAttrs()
	for _, v := range vals {
		if v == "ProcId" || v == "procid" {
			t.Fatalf("ProcId came back as a full index, not a component: %v", vals)
		}
	}
	t.Logf("composites=%v value indexes=%v", re.CompositeAttrs(), vals)
}

// A composite whose component cannot be resolved is refused SILENTLY -- newCompositeSpecs
// skips the tuple and nothing returns an error. That is the failure mode a reopen that lost
// the component declarations would have produced, so it is worth pinning on its own: the
// symptom is not a broken table, it is a table that quietly stopped having the index.
func TestCompositeWithAnUnresolvableComponentIsSilentlyDropped(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	tbl, cerr := cat.CreateArchiveTable("hist", ArchiveConfig{
		SegmentSize:    4096,
		ValueAttrs:     []string{"ClusterId"},
		CompositeAttrs: [][]string{{"ClusterId", "ProcId"}}, // ProcId neither indexed nor declared
	})
	if cerr != nil {
		t.Fatalf("creating the table reported an error: %v", cerr)
	}
	if got := tbl.CompositeAttrs(); len(got) != 0 {
		t.Fatalf("the composite was kept despite an unresolvable component: %v", got)
	}
	// And the same configuration WITH the declaration keeps it, so the refusal above is
	// about the component rather than about composites in general.
	ok, oerr := cat.CreateArchiveTable("hist2", ArchiveConfig{
		SegmentSize:         4096,
		ValueAttrs:          []string{"ClusterId"},
		ValueComponentAttrs: []string{"ProcId"},
		CompositeAttrs:      [][]string{{"ClusterId", "ProcId"}},
	})
	if oerr != nil {
		t.Fatal(oerr)
	}
	if got := ok.CompositeAttrs(); len(got) != 1 {
		t.Fatalf("the declared configuration lost its composite too: %v", got)
	}
}
