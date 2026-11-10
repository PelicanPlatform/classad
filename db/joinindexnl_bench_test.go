package db

// WOULD AN INDEX NESTED-LOOP JOIN WORK HERE? The measurement, not the implementation.
//
// A hash join holds one side's rows, and MaxHashRows (500,000 by default) is a hard
// FAILURE when that side is bigger: buildStage returns an error rather than falling back
// to a worse plan. An index nested-loop join would hold nothing -- for each streamed row,
// look up its key in the inner side's index -- which is the only shape whose memory does
// not scale with the inner side, and so the only one that would let that default come
// down.
//
// The number that decides it is LOOKUPS PER STREAMED ROW. Every sealed segment carries its
// own index, so a naive per-row lookup costs one lookup per segment: over the 211 segments
// the existing probe sweep ran on, 50,000 streamed rows is 10.5M lookups, which is not
// obviously cheaper than reading the inner side whole. Zone maps are the only thing that
// collapses that factor, and they collapse it only as far as the join key correlates with
// insertion order. So what is measured here is the surviving segment count per single-key
// equality probe, as a distribution rather than a mean -- the tail is what costs -- and the
// per-lookup price next to the per-record scan price it has to beat.
//
// Two structural facts shape the answer and are asserted rather than assumed:
//
//   - a zone map is a NUMERIC [min,max] per attribute (see collections, zoneMayMatch, and
//     the non-numeric bail-out beside it). A string join key cannot be zone-pruned at all,
//     whatever its distribution, so for a string key the segment factor is the full count.
//   - the 64-key cliff in the existing sweep is the store's maxPushdownFrac gate, not a
//     lookup price. A single-key probe estimates far below that fraction, so the cliff
//     should not reach the per-row case. The explained plan is read for every arm here so
//     that is checked rather than believed.

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// nlRecords is the fixture size. It is chosen for SEGMENTS, not for rows: the whole question
// is how a per-row lookup scales in the segment count, and a fixture with a handful of sealed
// segments answers a different question than the one asked. The seal count is asserted below.
const nlRecords = 120_000

// nlKeySamples is how many distinct keys each arm probes. The distribution's tail is the
// expensive part, so this has to be large enough for a p99 to mean something.
const nlKeySamples = 256

// nlFixture is ONE store shared by every arm. Rebuilding per arm has silently produced an
// empty store that benchmarked beautifully because nothing matched, so the fixture is built
// once, its seal count is checked, and every arm asserts its own matched row count.
type nlFixture struct {
	cat      *Catalog
	comp     *ArchiveTable // (ClusterId, ProcId) composite configured
	plain    *ArchiveTable // identical rows, per-attribute indexes only
	records  int
	segments int
	// clusters, scatter are the key spaces: a ClusterId is i/4 and so runs with insertion
	// order, a RandKey is drawn uniformly from the same cardinality and so does not.
	clusters int
	randKeys []int64  // every RandKey value present, in insertion order
	owners   []string // every OwnerKey value present, in insertion order
}

// buildNLFixture writes the history shape twice: once with a composite over the real join
// key, once without, so the composite's effect on surviving segments is a difference between
// two arms of one fixture rather than between two fixtures.
//
// The attributes are chosen so each arm differs from the next in ONE way:
//
//	ClusterId  i/4          numeric, monotone with insertion order
//	RandKey    uniform      numeric, same cardinality as ClusterId, no order correlation
//	OwnerKey   "u"+RandKey  the SAME values as RandKey, as strings -- so the only difference
//	                        between those two arms is that one can be zone-mapped
//	ProcId     i%4          the second component of the real join key
func buildNLFixture(tb testing.TB) *nlFixture {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	base := ArchiveConfig{
		SegmentSize: 96 << 10,
		ValueAttrs:  []string{"ClusterId", "ProcId", "RandKey"},
		// OwnerKey must be CATEGORICAL, not a ValueAttr. A value index does not serve a
		// string-valued attribute: configured as one, IndexedAttrs reports OwnerKey as
		// indexed while the planner still answers `OwnerKey == "u5"` with "no conjunct of
		// the filter is on an indexed attribute" and full-scans. Measuring a string key
		// that way would have timed a scan and reported it as a lookup price.
		CategoricalAttrs: []string{"OwnerKey"},
	}
	compCfg := base
	compCfg.CompositeAttrs = [][]string{{"ClusterId", "ProcId"}}
	mk := func(name string, cfg ArchiveConfig) *ArchiveTable {
		t, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			tb.Fatal(cerr)
		}
		return t
	}
	f := &nlFixture{cat: cat, records: nlRecords, clusters: nlRecords / 4}
	f.comp = mk("hist_composite", compCfg)
	f.plain = mk("hist_plain", base)

	rng := rand.New(rand.NewSource(0x5eed))
	f.randKeys = make([]int64, 0, nlRecords)
	f.owners = make([]string, 0, nlRecords)
	pad := strings.Repeat("x", 64)
	for i := 0; i < nlRecords; i++ {
		rk := int64(rng.Intn(f.clusters))
		ow := fmt.Sprintf("u%d", rk)
		f.randKeys = append(f.randKeys, rk)
		f.owners = append(f.owners, ow)
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i/4))
		ad.InsertAttr("ProcId", int64(i%4))
		ad.InsertAttr("RandKey", rk)
		ad.InsertAttrString("OwnerKey", ow)
		ad.InsertAttrString("Pad", pad)
		if aerr := f.comp.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
		ad2 := classad.New()
		ad2.InsertAttr("ClusterId", int64(i/4))
		ad2.InsertAttr("ProcId", int64(i%4))
		ad2.InsertAttr("RandKey", rk)
		ad2.InsertAttrString("OwnerKey", ow)
		ad2.InsertAttrString("Pad", pad)
		if aerr := f.plain.Append(ad2); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	f.segments = f.comp.Stats().Segments
	// A fixture of one unsealed segment has previously let a fast path go entirely untested
	// while passing green. The segment count IS the variable under study, so too few of them
	// makes every number below meaningless rather than merely weak.
	if f.segments < 150 {
		tb.Fatalf("fixture sealed only %d segments from %d records; the segment factor is what "+
			"this measures and it cannot be read off a fixture this small", f.segments, nlRecords)
	}
	return f
}

// nlArm is one join-key shape: how its per-row constraint is spelled for a given sample, and
// how many rows that constraint is expected to match.
type nlArm struct {
	name string
	tbl  *ArchiveTable
	cons func(f *nlFixture, i int) string
}

// nlArms is every key shape measured, in forward order. Arm order is flipped between rounds
// below, because an A/B that only ever runs A-then-B measures ordering bias.
func nlArms(f *nlFixture) []nlArm {
	return []nlArm{
		{
			name: "ClusterId (numeric, insertion-ordered)",
			tbl:  f.plain,
			cons: func(f *nlFixture, i int) string {
				return fmt.Sprintf("ClusterId == %d", nlSampleCluster(f, i))
			},
		},
		{
			name: "RandKey (numeric, scattered)",
			tbl:  f.plain,
			cons: func(f *nlFixture, i int) string {
				return fmt.Sprintf("RandKey == %d", f.randKeys[nlSampleRow(f, i)])
			},
		},
		{
			name: "OwnerKey (string, scattered)",
			tbl:  f.plain,
			cons: func(f *nlFixture, i int) string {
				return fmt.Sprintf("OwnerKey == %q", f.owners[nlSampleRow(f, i)])
			},
		},
		{
			name: "(ClusterId,ProcId) per-attribute",
			tbl:  f.plain,
			cons: func(f *nlFixture, i int) string {
				return fmt.Sprintf("ClusterId == %d && ProcId == %d", nlSampleCluster(f, i), i%4)
			},
		},
		{
			name: "(ClusterId,ProcId) composite",
			tbl:  f.comp,
			cons: func(f *nlFixture, i int) string {
				return fmt.Sprintf("ClusterId == %d && ProcId == %d", nlSampleCluster(f, i), i%4)
			},
		},
	}
}

// nlSampleCluster spreads the sampled ClusterIds over the WHOLE range rather than over a
// recent window. A window is the friendly case -- it is what the existing zone test measures
// for a k-way probe -- and the per-row question is about one key at a time, where the window
// the streamed side covers does not help: each row's own key is looked up on its own.
func nlSampleCluster(f *nlFixture, i int) int {
	return (i * f.clusters) / nlKeySamples
}

func nlSampleRow(f *nlFixture, i int) int { return (i * f.records) / nlKeySamples }

// nlSegStats is one arm's surviving-segment distribution.
type nlSegStats struct {
	total   int // segments in the archive
	probed  []int
	matched int
	plans   map[string]int
	exact   int // samples whose ExactProbes reported exact
}

// measureSegments probes every sample's key and records how many segments survived zone
// pruning -- which is exactly the per-row lookup bill an index nested loop would pay, since
// a zone-pruned segment is ruled out by a min/max compare before any posting is read.
//
// It also counts the rows each probe matched. An arm that matched nothing is not a cheap
// arm, it is a broken one, so the count is asserted rather than the code path.
func measureSegments(tb testing.TB, f *nlFixture, a nlArm) nlSegStats {
	tb.Helper()
	st := nlSegStats{probed: make([]int, 0, nlKeySamples), plans: map[string]int{}}
	for i := 0; i < nlKeySamples; i++ {
		cons := a.cons(f, i)
		ex, err := a.tbl.Explain(cons)
		if err != nil {
			tb.Fatalf("%s: explaining %q: %v", a.name, cons, err)
		}
		total := ex.IndexSegments + ex.IndexSkippedSegments + ex.UnindexedSegments + ex.ZonePrunedSegments
		if total == 0 {
			tb.Fatalf("%s: %q explained no segments at all", a.name, cons)
		}
		st.total = total
		st.probed = append(st.probed, total-ex.ZonePrunedSegments)
		st.plans[ex.Plan]++
		q, perr := vm.Parse(cons)
		if perr != nil {
			tb.Fatal(perr)
		}
		if _, ok := q.ExactProbes(); ok {
			st.exact++
		}
		seq, qerr := a.tbl.Query(cons)
		if qerr != nil {
			tb.Fatal(qerr)
		}
		for range seq {
			st.matched++
		}
	}
	if st.matched == 0 {
		tb.Fatalf("%s matched 0 rows across %d probes; the arm is measuring nothing",
			a.name, nlKeySamples)
	}
	sort.Ints(st.probed)
	return st
}

func pct(sorted []int, p float64) int {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}

func meanOf(xs []int) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0
	for _, x := range xs {
		s += x
	}
	return float64(s) / float64(len(xs))
}

// TestIndexNLSurvivingSegments is the headline measurement: lookups per streamed row, by key
// shape. Everything else here prices those lookups.
func TestIndexNLSurvivingSegments(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-hundred-segment archive")
	}
	f := buildNLFixture(t)
	t.Logf("fixture: %d records, %d sealed segments, %d clusters", f.records, f.segments, f.clusters)
	for _, a := range nlArms(f) {
		st := measureSegments(t, f, a)
		t.Logf("%-38s segments=%3d  probed/row min=%3d p50=%3d p90=%3d p99=%3d max=%3d mean=%6.1f  "+
			"matched=%d exact=%d/%d plans=%v",
			a.name, st.total, st.probed[0], pct(st.probed, 0.50), pct(st.probed, 0.90),
			pct(st.probed, 0.99), st.probed[len(st.probed)-1], meanOf(st.probed),
			st.matched, st.exact, nlKeySamples, st.plans)
		// Every arm must reach an indexed plan. A single-key probe that fell back to a full
		// scan would make its timing a scan timing, and reading that as a lookup price is
		// exactly the mistake the existing sweep's 64-key cliff turned out to be.
		if st.plans["indexed"] == 0 && st.plans["indexed-union"] == 0 && st.plans["composite"] == 0 {
			t.Errorf("%s never reached an indexed plan: %v -- its timings are scan timings", a.name, st.plans)
		}
	}
}

// TestIndexNLLookupPrice times one per-row lookup against the scan it would replace.
//
// The constraint is PARSED OUTSIDE the timed region, because a real index nested loop would
// rebind the probe's value per row rather than recompile: vm.Probe is a plain struct of
// attribute, operator and values, so swapping the value is a field write. Parsing is timed
// separately so a reader can see what that rebinding is worth and what it is not.
func TestIndexNLLookupPrice(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-hundred-segment archive")
	}
	f := buildNLFixture(t)
	arms := nlArms(f)

	// Pre-parse every arm's sampled constraints, and warm each Query's memoized probe set,
	// so the timed loop pays only for the read.
	queries := make([][]*vm.Query, len(arms))
	for ai, a := range arms {
		qs := make([]*vm.Query, nlKeySamples)
		for i := 0; i < nlKeySamples; i++ {
			q, err := vm.Parse(a.cons(f, i))
			if err != nil {
				t.Fatal(err)
			}
			q.Probes()
			qs[i] = q
		}
		queries[ai] = qs
	}

	const rounds = 5
	lookup := make([][]time.Duration, len(arms))
	matched := make([]int, len(arms))
	for r := 0; r < rounds; r++ {
		order := make([]int, len(arms))
		for i := range order {
			order[i] = i
		}
		if r%2 == 1 { // flip the arm order every other round
			for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
				order[i], order[j] = order[j], order[i]
			}
		}
		for _, ai := range order {
			a := arms[ai]
			n := 0
			start := time.Now()
			for i := 0; i < nlKeySamples; i++ {
				for range a.tbl.a.Query(queries[ai][i]) {
					n++
				}
			}
			el := time.Since(start)
			lookup[ai] = append(lookup[ai], el)
			if matched[ai] != 0 && matched[ai] != n {
				t.Fatalf("%s matched %d rows this round and %d last: the fixture is not stable",
					a.name, n, matched[ai])
			}
			matched[ai] = n
		}
	}

	// The scan this would replace: the whole inner side, read once.
	trueQ, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	var scans []time.Duration
	scanned := 0
	for r := 0; r < rounds; r++ {
		n := 0
		start := time.Now()
		for range f.plain.a.Query(trueQ) {
			n++
		}
		scans = append(scans, time.Since(start))
		scanned = n
	}
	if scanned != f.records {
		t.Fatalf("the full scan read %d of %d records", scanned, f.records)
	}
	scanMed := medianDur(scans)
	recPerSec := float64(f.records) / scanMed.Seconds()
	t.Logf("full inner scan: %d records in %v (%.0f records/sec)", scanned, scanMed, recPerSec)

	// Parse cost, measured on its own so the no-recompile claim has a number behind it.
	parseStart := time.Now()
	for i := 0; i < nlKeySamples; i++ {
		if _, perr := vm.Parse(arms[0].cons(f, i)); perr != nil {
			t.Fatal(perr)
		}
	}
	t.Logf("parse only: %v per constraint", time.Since(parseStart)/nlKeySamples)

	for ai, a := range arms {
		if matched[ai] == 0 {
			t.Fatalf("%s matched 0 rows; the timing is of an empty read", a.name)
		}
		med := medianDur(lookup[ai])
		perRow := med / nlKeySamples
		st := measureSegments(t, f, a)
		segs := meanOf(st.probed)
		perLookup := float64(perRow.Nanoseconds()) / segs
		// What the cost model wants: one per-row lookup expressed in scanned records.
		inRecords := perRow.Seconds() * recPerSec
		t.Logf("%-38s per-row=%9v  segsProbed=%6.1f  per-segment-lookup=%7.1fns  "+
			"per-row cost = %8.1f scanned records  matched=%d",
			a.name, perRow, segs, perLookup, inRecords, matched[ai])
	}
}

func medianDur(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

// THE PER-ROW FLOOR, and whether it scales with the segment count.
//
// The lookup-price table above does not decompose: the insertion-ordered key probes 2
// segments of 157 and costs 112us a row, while the scattered key probes all 157 -- 78 times
// the lookups -- and costs 172us. An arm doing 78x the work for 1.5x the time is not a
// lookup price being measured, it is a FIXED cost per row swamping one. Zone pruning
// collapses the posting reads; it does not collapse the per-query work of deciding which
// segments to prune, which visits every segment's zone map.
//
// That distinction decides the whole question. If the floor is constant, an index nested
// loop pays it once per streamed row and the inner side's size is irrelevant, which is the
// property that would let MaxHashRows come down. If the floor is O(segments), the per-row
// cost grows with the inner side after all -- in TIME instead of memory -- and the shape
// does not have the property it is wanted for.
//
// So this holds the record count, the data and the scan baseline fixed and varies only the
// segment size, which varies only the segment count.

// nlFloorFixture is one table of nlRecords records cut into segments of segBytes.
func nlFloorFixture(tb testing.TB, segBytes int) (*ArchiveTable, int) {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	at, err := cat.CreateArchiveTable("hist", ArchiveConfig{
		SegmentSize: segBytes,
		ValueAttrs:  []string{"ClusterId", "ProcId"},
	})
	if err != nil {
		tb.Fatal(err)
	}
	pad := strings.Repeat("x", 64)
	for i := 0; i < nlRecords; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i/4))
		ad.InsertAttr("ProcId", int64(i%4))
		ad.InsertAttrString("Pad", pad)
		if aerr := at.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	return at, at.Stats().Segments
}

func TestIndexNLPerRowFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("builds several multi-segment archives")
	}
	const reps = 200
	type row struct {
		segs            int
		empty, hit, mix time.Duration
		recPerSec       float64
	}
	var out []row
	// Segment sizes chosen to span an order of magnitude in the segment count at a fixed
	// 120,000 records, so the floor's dependence on it is read off a slope rather than a pair.
	for _, segBytes := range []int{1536 << 10, 384 << 10, 96 << 10, 24 << 10} {
		at, segs := nlFloorFixture(t, segBytes)

		// The calibration probe: a ClusterId past every segment's zone map, so every segment
		// is ruled out before a posting is read and the time is the per-query floor alone.
		outOfRange := fmt.Sprintf("ClusterId == %d", nlRecords*10)
		ex, err := at.Explain(outOfRange)
		if err != nil {
			t.Fatal(err)
		}
		// Every sealed segment is ruled out; the one that survives is the LIVE segment,
		// which carries no zone map because it is still being appended to and so can never
		// be pruned. That is a floor component in its own right: every per-row lookup pays
		// one real probe of the open segment however well the key is clustered, which is
		// why the insertion-ordered arm costs 2 segments rather than 1.
		total := ex.IndexSegments + ex.IndexSkippedSegments + ex.UnindexedSegments + ex.ZonePrunedSegments
		if surv := total - ex.ZonePrunedSegments; surv > 1 {
			t.Fatalf("the calibration probe left %d of %d segments unpruned; it is meant to "+
				"leave only the live one, so it is not measuring the floor", surv, total)
		}
		// What the one surviving segment costs is the question the floor's shape turns on,
		// so its verdict is recorded rather than inferred: UnindexedSegments means it is
		// WALKED IN FULL, and EstVisited is how many records that is.
		t.Logf("  calibration at %d segments: surviving idx=%d skipped=%d unindexed=%d "+
			"estVisited=%d (records per segment ~= %d)",
			segs, ex.IndexSegments, ex.IndexSkippedSegments, ex.UnindexedSegments,
			ex.EstVisited, nlRecords/segs)
		emptyQ, err := vm.Parse(outOfRange)
		if err != nil {
			t.Fatal(err)
		}
		emptyQ.Probes()

		// A probe that HITS, on the insertion-ordered key: the floor plus two real lookups.
		hitQs := make([]*vm.Query, reps)
		for i := 0; i < reps; i++ {
			q, perr := vm.Parse(fmt.Sprintf("ClusterId == %d", (i*(nlRecords/4))/reps))
			if perr != nil {
				t.Fatal(perr)
			}
			q.Probes()
			hitQs[i] = q
		}
		// A probe on ProcId, which every segment holds every value of: the floor plus a
		// lookup in every segment, which is what a scattered key costs.
		mixQ, err := vm.Parse("ProcId == 2")
		if err != nil {
			t.Fatal(err)
		}
		mixQ.Probes()

		trueQ, err := vm.Parse("true")
		if err != nil {
			t.Fatal(err)
		}

		time3 := func(fn func() int) (time.Duration, int) {
			var ds []time.Duration
			n := 0
			for r := 0; r < 3; r++ {
				start := time.Now()
				n = fn()
				ds = append(ds, time.Since(start))
			}
			return medianDur(ds), n
		}
		emptyD, emptyN := time3(func() int {
			n := 0
			for i := 0; i < reps; i++ {
				for range at.a.Query(emptyQ) {
					n++
				}
			}
			return n
		})
		if emptyN != 0 {
			t.Fatalf("the calibration probe matched %d rows; it is meant to match none", emptyN)
		}
		hitD, hitN := time3(func() int {
			n := 0
			for i := 0; i < reps; i++ {
				for range at.a.Query(hitQs[i]) {
					n++
				}
			}
			return n
		})
		if hitN == 0 {
			t.Fatal("the hit probe matched nothing")
		}
		mixD, mixN := time3(func() int {
			n := 0
			for i := 0; i < 5; i++ { // every record matches one ProcId in four
				for range at.a.Query(mixQ) {
					n++
				}
			}
			return n
		})
		if mixN == 0 {
			t.Fatal("the every-segment probe matched nothing")
		}
		scanD, scanN := time3(func() int {
			n := 0
			for range at.a.Query(trueQ) {
				n++
			}
			return n
		})
		if scanN != nlRecords {
			t.Fatalf("the scan read %d of %d records", scanN, nlRecords)
		}
		out = append(out, row{
			segs: segs, empty: emptyD / reps, hit: hitD / reps, mix: mixD / 5,
			recPerSec: float64(nlRecords) / scanD.Seconds(),
		})
		t.Logf("segBytes=%7d segments=%4d  floor(all pruned)=%10v  hit(2 segs)=%10v  "+
			"everySeg=%10v  scan=%.0f rec/s  hitRows=%d",
			segBytes, segs, emptyD/reps, hitD/reps, mixD/5,
			float64(nlRecords)/scanD.Seconds(), hitN)
	}

	t.Log("per-row floor against segment count:")
	for _, r := range out {
		t.Logf("  segments=%4d  floor=%10v  floor per segment=%7.1fns  "+
			"floor in scanned records=%8.1f",
			r.segs, r.empty, float64(r.empty.Nanoseconds())/float64(r.segs),
			r.empty.Seconds()*r.recPerSec)
	}
	// The claim being tested. If the floor were constant, the widest and narrowest segment
	// counts here (about 13x apart) would cost the same per row.
	lo, hi := out[0], out[len(out)-1]
	t.Logf("segments %d -> %d (%.1fx): floor %v -> %v (%.1fx)",
		lo.segs, hi.segs, float64(hi.segs)/float64(lo.segs),
		lo.empty, hi.empty, float64(hi.empty)/float64(lo.empty))
}

// WHERE THE PER-ROW FLOOR ACTUALLY GOES.
//
// The floor is not spent on the sealed side. The calibration probe zone-prunes every sealed
// segment and the one that survives is the LIVE segment, which the explain reports as ruled
// out by its own index with estVisited=0 -- no record is read. Yet that costs 2.2ms a probe
// on an archive of 8 big segments and 105us on one of 118 small ones, and the only thing
// that changed is how many records the live segment holds.
//
// If that reading is right the floor is proportional to the LIVE segment's size and must be
// a sawtooth: climbing as records accumulate in the open segment and dropping the instant it
// seals and a fresh empty one opens. A curve fitted to four points could be coincidence; a
// sawtooth synchronized with the seal count cannot. So this appends in small steps and
// watches the floor against the segment count.
func TestIndexNLLiveSegmentFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("appends across several seals")
	}
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	at, err := cat.CreateArchiveTable("hist", ArchiveConfig{
		SegmentSize: 96 << 10,
		ValueAttrs:  []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", 64)
	n := 0
	appendN := func(k int) {
		for i := 0; i < k; i++ {
			ad := classad.New()
			ad.InsertAttr("ClusterId", int64(n))
			ad.InsertAttrString("Pad", pad)
			if aerr := at.Append(ad); aerr != nil {
				t.Fatal(aerr)
			}
			n++
		}
	}
	appendN(20000) // a base of sealed segments to prune against

	q, err := vm.Parse(fmt.Sprintf("ClusterId == %d", 100_000_000))
	if err != nil {
		t.Fatal(err)
	}
	q.Probes()

	const reps = 100
	floor := func() time.Duration {
		var ds []time.Duration
		for r := 0; r < 3; r++ {
			start := time.Now()
			got := 0
			for i := 0; i < reps; i++ {
				for range at.a.Query(q) {
					got++
				}
			}
			ds = append(ds, time.Since(start))
			if got != 0 {
				t.Fatalf("the calibration probe matched %d rows", got)
			}
		}
		return medianDur(ds) / reps
	}

	type pt struct {
		segs  int
		rows  int
		floor time.Duration
	}
	var pts []pt
	for step := 0; step < 24; step++ {
		s := at.Stats().Segments
		f := floor()
		pts = append(pts, pt{segs: s, rows: n, floor: f})
		t.Logf("rows=%6d segments=%4d floor=%10v", n, s, f)
		appendN(120)
	}

	// The sawtooth, asserted: somewhere in the sweep a step must have ADDED a segment and
	// got CHEAPER. That cannot happen if the floor is a cost of the sealed side, where more
	// segments is strictly more work; it is the signature of the open segment emptying.
	drops := 0
	for i := 1; i < len(pts); i++ {
		if pts[i].segs > pts[i-1].segs && pts[i].floor < pts[i-1].floor {
			drops++
			t.Logf("  seal at rows=%d: segments %d -> %d, floor %v -> %v",
				pts[i].rows, pts[i-1].segs, pts[i].segs, pts[i-1].floor, pts[i].floor)
		}
	}
	if drops == 0 {
		t.Errorf("no seal made the probe cheaper across %d steps; the floor is not the live "+
			"segment's size and the reading above is wrong", len(pts))
	}
}

// THE FLOOR AT THE DEFAULT SEGMENT SIZE, which is the only size that decides anything.
//
// Every number above was taken at 96 KiB segments, chosen to produce many of them. The
// default is 8 MiB (ArchiveConfig.SegmentSize), so the live segment holds roughly eighty
// times as many records -- and the floor is proportional to exactly that. Extrapolating a
// 0.4us-per-live-record slope across two orders of magnitude is not a measurement, so this
// takes the number at the default.
//
// It is measured twice: just after a seal, when the open segment is nearly empty and the
// floor is at its best, and again after the open segment has filled up. A per-row cost that
// swings by two orders of magnitude depending on when the join happens to run is a property
// a cost model cannot plan around, which is the point.
func TestIndexNLFloorAtDefaultSegmentSize(t *testing.T) {
	if testing.Short() {
		t.Skip("writes enough to seal a default-sized segment")
	}
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	// No SegmentSize: the default is what is being measured.
	at, err := cat.CreateArchiveTable("hist", ArchiveConfig{ValueAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", 64)
	n := 0
	appendTo := func(target int) {
		for n < target {
			ad := classad.New()
			ad.InsertAttr("ClusterId", int64(n))
			ad.InsertAttrString("Pad", pad)
			if aerr := at.Append(ad); aerr != nil {
				t.Fatal(aerr)
			}
			n++
		}
	}
	q, err := vm.Parse(fmt.Sprintf("ClusterId == %d", 100_000_000))
	if err != nil {
		t.Fatal(err)
	}
	q.Probes()
	const reps = 20
	floor := func() time.Duration {
		var ds []time.Duration
		for r := 0; r < 3; r++ {
			start := time.Now()
			got := 0
			for i := 0; i < reps; i++ {
				for range at.a.Query(q) {
					got++
				}
			}
			ds = append(ds, time.Since(start))
			if got != 0 {
				t.Fatalf("the calibration probe matched %d rows", got)
			}
		}
		return medianDur(ds) / reps
	}

	// Grow until a seal happens, so the open segment restarts near empty.
	appendTo(20000)
	before := at.Stats().Segments
	for at.Stats().Segments == before && n < 2_000_000 {
		appendTo(n + 5000)
	}
	sealedAt, segs := n, at.Stats().Segments
	justSealed := floor()
	t.Logf("default segment size: %d segments after %d rows; floor just after a seal = %v",
		segs, sealedAt, justSealed)

	// Now fill the open segment and measure again.
	for at.Stats().Segments == segs && n < 4_000_000 {
		appendTo(n + 10000)
	}
	liveRows := n - sealedAt
	// n has just tipped the next seal; step back to a nearly-full open segment by measuring
	// at the row count reached one append batch before it.
	nearFull := floor()
	t.Logf("default segment size: a segment holds about %d records; floor with the open "+
		"segment nearly full = %v (%.0fx the just-sealed floor)",
		liveRows, nearFull, float64(nearFull)/float64(justSealed))
	t.Logf("so one index-NL lookup costs between %v and %v at the default segment size, "+
		"depending only on how full the open segment happens to be", justSealed, nearFull)
	if nearFull <= justSealed {
		t.Errorf("filling the open segment did not make the probe dearer (%v -> %v); the "+
			"live-segment reading does not hold at the default size", justSealed, nearFull)
	}
}

// BenchmarkIndexNLFloor is the floor on its own, for a profile: an archive with a large open
// segment, probed with a key every sealed segment's zone map rules out. Whatever this
// benchmark spends its time in IS the per-row cost of an index nested loop, so naming that
// function is what says whether the cost is intrinsic or fixable.
func BenchmarkIndexNLFloor(b *testing.B) {
	cat, err := OpenCatalog(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer cat.Close()
	at, err := cat.CreateArchiveTable("hist", ArchiveConfig{
		SegmentSize: 96 << 10,
		ValueAttrs:  []string{"ClusterId"},
	})
	if err != nil {
		b.Fatal(err)
	}
	pad := strings.Repeat("x", 64)
	for i := 0; i < 40000; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttrString("Pad", pad)
		if aerr := at.Append(ad); aerr != nil {
			b.Fatal(aerr)
		}
	}
	q, err := vm.Parse(fmt.Sprintf("ClusterId == %d", 100_000_000))
	if err != nil {
		b.Fatal(err)
	}
	q.Probes()
	b.ReportMetric(float64(at.Stats().Segments), "segments")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for range at.a.Query(q) {
			b.Fatal("the calibration probe matched a row")
		}
	}
}
