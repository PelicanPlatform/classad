package db

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// What one index lookup is worth, measured for a SINGLE-VALUE lookup rather than for the
// multi-value `in` probe probeLookupCost was swept on.
//
// The two access patterns are priced by the same constant and are not the same operation.
// The per-attribute form folds k values into one OR-chain on one attribute, which the
// planner keeps as a single `in` leaf and resolves as one multi-value probe per segment. The
// tuple spine folds t tuples into a disjunction of per-tuple conjunctions, which the planner
// turns into t DNF GROUPS and unions their candidates; without a composite each disjunct
// intersects its components' postings, with one it is answered by a single lookup. The cost
// model multiplies segments * tuples * perTuple and compares the product with the records a
// scan would read, so the price of one of those lookups is what decides whether the form
// runs at all.
//
// These benchmarks hold the hashed side's record count fixed, sweep each factor of that
// product on its own, and time the probe against the scan it replaces, so the crossover is
// read off the clock rather than assumed.

// costSweepFixture builds the (ClusterId, ProcId) shape with every factor the cost model
// multiplies exposed: how many records the hashed side holds, how many segments it is
// spread across, how many distinct tuples the streamed side names, and whether a composite
// index answers a whole tuple in one lookup.
//
// segBytes rather than a segment count, because segments are what the store seals and a
// count is what it reports; the caller sweeps the bytes and the benchmark names the count
// the fixture actually produced.
func costSweepFixture(tb testing.TB, records, segBytes, tuples int, composite bool) *Catalog {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	cfg := ArchiveConfig{SegmentSize: segBytes, ValueAttrs: []string{"ClusterId", "ProcId"}}
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
	// Four procs per cluster, so both halves of the key fan out and the per-attribute
	// form's cross product is four times the tuple set. A fixture where either attribute
	// took one value would make the two forms do the same work.
	for i := 0; i < records; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i/4))
		ad.InsertAttr("ProcId", int64(i%4))
		ad.InsertAttrString("Pad", strings.Repeat("x", 64))
		if aerr := hist.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	for _, ad := range costSweepTupleAds(tb, records, tuples) {
		if aerr := jobs.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	return cat
}

// costSweepTupleAds is the streamed side: `tuples` distinct pairs spread across the whole
// cluster range, so the narrowed read touches segments throughout the archive rather than a
// prefix of it.
func costSweepTupleAds(tb testing.TB, records, tuples int) []*classad.ClassAd {
	tb.Helper()
	clusters := records / 4
	if tuples > clusters {
		tb.Fatalf("asked for %d tuples from %d clusters", tuples, clusters)
	}
	stride := clusters / tuples
	out := make([]*classad.ClassAd, 0, tuples)
	for k := 0; k < tuples; k++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(k*stride))
		ad.InsertAttr("ProcId", int64(k%4))
		ad.InsertAttrString("Owner", "alice")
		out = append(out, ad)
	}
	return out
}

// costSweepSpinePlan reports the ACCESS PATH the hashed side's read actually takes under the
// tuple constraint the join would build, by rebuilding that constraint and explaining it.
//
// This is the measurement's anti-vacuity check, and it earned its place: the spine's cost is
// not a smooth function of its lookup bill, because past a disjunct count the store's own
// selectivity gate (collections, maxPushdownFrac) abandons the index union and reads the
// whole table with the spine as a filter. A timing that crossed over for that reason, and a
// table reporting only nanoseconds, would be read as the lookup price being found when what
// was found is a different gate entirely.
func costSweepSpinePlan(tb testing.TB, cat *Catalog, records, tuples int) string {
	tb.Helper()
	tbl, ok := cat.ArchiveTable("hist")
	if !ok {
		tb.Fatal("no hist table")
	}
	terms := make([]string, 0, tuples)
	for _, ad := range costSweepTupleAds(tb, records, tuples) {
		c, _ := classad.ParseExpr("ClusterId")
		p, _ := classad.ParseExpr("ProcId")
		ci, _ := c.Eval(ad).IntValue()
		pi, _ := p.Eval(ad).IntValue()
		terms = append(terms, fmt.Sprintf("(ClusterId == %d && ProcId == %d)", ci, pi))
	}
	sort.Strings(terms) // the same order planIndexProbes renders
	ex, err := tbl.Explain(strings.Join(terms, " || "))
	if err != nil {
		tb.Fatal(err)
	}
	return ex.Plan
}

// forcingTupleProbe runs fn with the whole-key form on and the cost gate off, which is how
// the spine is reached at tuple counts the gate declines.
func forcingTupleProbe(fn func()) {
	oldTuple, oldCost, oldKeys := tupleProbes, probeLookupCost, maxProbeKeys
	tupleProbes, probeLookupCost, maxProbeKeys = true, 0, 1<<30
	defer func() { tupleProbes, probeLookupCost, maxProbeKeys = oldTuple, oldCost, oldKeys }()
	fn()
}

// forcingPerAttrProbe runs fn with the spine off and the cost gate off, so the per-attribute
// form -- the multi-value `in` the constant was swept on -- runs at every key count.
func forcingPerAttrProbe(fn func()) {
	oldTuple, oldCost, oldKeys := tupleProbes, probeLookupCost, maxProbeKeys
	tupleProbes, probeLookupCost, maxProbeKeys = false, 0, 1<<30
	defer func() { tupleProbes, probeLookupCost, maxProbeKeys = oldTuple, oldCost, oldKeys }()
	fn()
}

// costSweepArm is one access path, named so the table reads as the decision does.
type costSweepArm struct {
	name string
	wrap func(func())
}

// costSweepOrder returns the arms in the order this round wants them, reversing when
// COSTSWEEP_REVERSE is set.
//
// Flipped between rounds because on a loaded machine the absolutes move with the order while
// the ratios hold, and a table built in one order alone cannot tell those two apart. Read
// from the environment rather than from a flag so both orders come from one binary over one
// set of fixtures.
func costSweepOrder(arms []costSweepArm) []costSweepArm {
	if os.Getenv("COSTSWEEP_REVERSE") == "" {
		return arms
	}
	out := make([]costSweepArm, len(arms))
	for i, a := range arms {
		out[len(arms)-1-i] = a
	}
	return out
}

// runCostSweepJoin runs the join once and returns what it actually read, which is what the
// fixture assertions are made on.
func runCostSweepJoin(tb testing.TB, cat *Catalog) (rows int, st JoinStats) {
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

// costSweepSegments is how many segments the fixture's hashed side sealed into.
func costSweepSegments(tb testing.TB, cat *Catalog) int {
	tb.Helper()
	a, ok := cat.ArchiveTable("hist")
	if !ok {
		tb.Fatal("no hist table")
	}
	return a.Stats().Segments
}

// BenchmarkProbeCostTupleSweep sweeps the tuple count at a fixed record and segment count,
// with and without a composite.
//
// Both composite settings, because they are the same access path at two prices -- one lookup
// per disjunct against one per component -- and a model that is right about one and wrong
// about the other is a model of the composite rather than of the lookup.
func BenchmarkProbeCostTupleSweep(b *testing.B) {
	const records, segBytes = 8192, 4096
	for _, composite := range []bool{true, false} {
		for _, tuples := range []int{16, 32, 64, 96, 128, 256} {
			cat := costSweepFixture(b, records, segBytes, tuples, composite)
			segs := costSweepSegments(b, cat)
			plan := costSweepSpinePlan(b, cat, records, tuples)

			// Anti-vacuity, before anything is timed: every arm must join the same rows,
			// and each must have read the side the arm is named for. A fixture that
			// narrowed to nothing, or one whose spine silently fell back to the
			// per-attribute form, would otherwise be timed and reported as a win.
			var spineRows, perAttrRows, scanRows int
			var spineSt, perAttrSt, scanSt JoinStats
			forcingTupleProbe(func() { spineRows, spineSt = runCostSweepJoin(b, cat) })
			forcingPerAttrProbe(func() { perAttrRows, perAttrSt = runCostSweepJoin(b, cat) })
			withoutProbe(func() { scanRows, scanSt = runCostSweepJoin(b, cat) })
			if spineRows != scanRows || perAttrRows != scanRows {
				b.Fatalf("tuples=%d: arms joined %d, %d and %d rows",
					tuples, spineRows, perAttrRows, scanRows)
			}
			if spineSt.ProbeSteps[0].Tuple == "" {
				b.Fatalf("tuples=%d: the spine arm did not narrow on the tuple: %s",
					tuples, spineSt.ProbeSteps[0].TupleWhy)
			}
			if spineSt.ProbeSteps[0].TupleComposite != composite {
				b.Fatalf("tuples=%d: TupleComposite=%v, want %v", tuples,
					spineSt.ProbeSteps[0].TupleComposite, composite)
			}
			if spineSt.HashRows != tuples {
				b.Fatalf("tuples=%d: the spine hashed %d rows, want one per tuple",
					tuples, spineSt.HashRows)
			}
			if perAttrSt.ProbeSteps[0].Tuple != "" {
				b.Fatalf("tuples=%d: the per-attribute arm narrowed on the tuple", tuples)
			}
			if perAttrSt.HashRows <= spineSt.HashRows {
				b.Fatalf("tuples=%d: the per-attribute arm hashed %d rows against the "+
					"spine's %d; the cross product is not being admitted, so the two "+
					"forms are not being compared", tuples, perAttrSt.HashRows,
					spineSt.HashRows)
			}
			if scanSt.HashRows != records {
				b.Fatalf("tuples=%d: the full-scan arm hashed %d of %d records",
					tuples, scanSt.HashRows, records)
			}

			perTuple := 2
			if composite {
				perTuple = 1
			}
			lookups := segs * tuples * perTuple
			stem := fmt.Sprintf("comp=%v/tuples=%d/segs=%d/lookups=%d/records=%d/plan=%s",
				composite, tuples, segs, lookups, records, plan)
			arms := costSweepOrder([]costSweepArm{
				{"spine", forcingTupleProbe},
				{"per-attr", forcingPerAttrProbe},
				{"full-scan", withoutProbe},
			})
			hashed := map[string]int{
				"spine": spineSt.HashRows, "per-attr": perAttrSt.HashRows,
				"full-scan": scanSt.HashRows,
			}
			for _, arm := range arms {
				n := hashed[arm.name]
				b.Run(stem+"/"+arm.name, func(b *testing.B) {
					arm.wrap(func() {
						for b.Loop() {
							runCostSweepJoin(b, cat)
						}
					})
					b.ReportMetric(float64(n), "hashed")
					b.ReportMetric(float64(lookups), "lookups")
				})
			}
		}
	}
}

// BenchmarkProbeCostSegmentSweep varies the SEGMENT count and the tuple count together at a
// fixed record count.
//
// The cost model's claim is that a lookup is paid per segment, so segments and tuples ought
// to be interchangeable in the bill: 196 segments of 32 tuples should cost what 49 segments
// of 128 tuples cost, both being 6272 lookups over the same 8192 records. If the crossover
// instead sits at the same TUPLE count whatever the segment count, the model is multiplying
// by a factor the clock cannot see, and no value of probeLookupCost repairs that.
func BenchmarkProbeCostSegmentSweep(b *testing.B) {
	const records = 8192
	for _, segBytes := range []int{4096, 16384, 65536} {
		for _, tuples := range []int{32, 64, 128, 256} {
			cat := costSweepFixture(b, records, segBytes, tuples, true)
			segs := costSweepSegments(b, cat)
			plan := costSweepSpinePlan(b, cat, records, tuples)
			var spineRows, scanRows int
			var spineSt, scanSt JoinStats
			forcingTupleProbe(func() { spineRows, spineSt = runCostSweepJoin(b, cat) })
			withoutProbe(func() { scanRows, scanSt = runCostSweepJoin(b, cat) })
			if spineRows != scanRows {
				b.Fatalf("segBytes=%d tuples=%d: arms joined %d and %d rows",
					segBytes, tuples, spineRows, scanRows)
			}
			if spineSt.ProbeSteps[0].Tuple == "" {
				b.Fatalf("segBytes=%d tuples=%d: the spine did not run: %s",
					segBytes, tuples, spineSt.ProbeSteps[0].TupleWhy)
			}
			if spineSt.HashRows != tuples {
				b.Fatalf("segBytes=%d tuples=%d: the spine hashed %d rows, want %d",
					segBytes, tuples, spineSt.HashRows, tuples)
			}
			if scanSt.HashRows != records {
				b.Fatalf("segBytes=%d tuples=%d: the scan hashed %d of %d records",
					segBytes, tuples, scanSt.HashRows, records)
			}
			lookups := segs * tuples
			stem := fmt.Sprintf("segs=%d/tuples=%d/lookups=%d/records=%d/plan=%s",
				segs, tuples, lookups, records, plan)
			arms := costSweepOrder([]costSweepArm{
				{"spine", forcingTupleProbe},
				{"full-scan", withoutProbe},
			})
			for _, arm := range arms {
				n := spineSt.HashRows
				if arm.name == "full-scan" {
					n = scanSt.HashRows
				}
				b.Run(stem+"/"+arm.name, func(b *testing.B) {
					arm.wrap(func() {
						for b.Loop() {
							runCostSweepJoin(b, cat)
						}
					})
					b.ReportMetric(float64(n), "hashed")
					b.ReportMetric(float64(lookups), "lookups")
				})
			}
		}
	}
}

// BenchmarkProbeCostKeySweep re-runs the ORIGINAL sweep -- the multi-value `in` on one
// attribute -- on the same machine and in the same binary as the tuple sweeps.
//
// Without it the two prices are compared across machines and across months, which on a box
// whose load average moves between 7 and 24 is not a comparison at all. The record count and
// the segment size match the tuple sweep, so the only thing differing is the shape of the
// probe.
func BenchmarkProbeCostKeySweep(b *testing.B) {
	const records = 8192
	for _, keys := range []int{16, 32, 64, 128, 256} {
		cat := probeFixture(b, records, keys, true)
		segs := costSweepSegments(b, cat)
		spec := probeSpec(JoinInner)
		run := func() (int, JoinStats) {
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
			return n, st
		}
		var probeRows, scanRows int
		var probeSt, scanSt JoinStats
		forcingProbe(func() { probeRows, probeSt = run() })
		withoutProbe(func() { scanRows, scanSt = run() })
		if probeRows != scanRows {
			b.Fatalf("keys=%d: probe joined %d rows, scan %d", keys, probeRows, scanRows)
		}
		if probeSt.ProbeKeys != keys {
			b.Fatalf("keys=%d: the forced probe named %d keys (%q); it did not run",
				keys, probeSt.ProbeKeys, probeSt.ProbeWhy)
		}
		if scanSt.HashRows != records {
			b.Fatalf("keys=%d: the full-scan arm hashed %d of %d records",
				keys, scanSt.HashRows, records)
		}
		lookups := segs * keys
		stem := fmt.Sprintf("keys=%d/segs=%d/lookups=%d/records=%d", keys, segs, lookups, records)
		arms := costSweepOrder([]costSweepArm{
			{"probe", forcingProbe},
			{"full-scan", withoutProbe},
		})
		for _, arm := range arms {
			n := probeSt.HashRows
			if arm.name == "full-scan" {
				n = scanSt.HashRows
			}
			b.Run(stem+"/"+arm.name, func(b *testing.B) {
				arm.wrap(func() {
					for b.Loop() {
						run()
					}
				})
				b.ReportMetric(float64(n), "hashed")
				b.ReportMetric(float64(lookups), "lookups")
			})
		}
	}
}
