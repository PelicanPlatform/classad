package collections

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// The tier-1 grouped-scan benchmark: the shape a history dashboard asks constantly.
//
//	SELECT bucket(EnteredHistoryTime, 86400), COUNT(*), <aggregates>
//	WHERE EnteredHistoryTime >= T && JobStatus == 4
//	GROUP BY bucket
//
// That constraint is a conjunction of scalar numeric comparisons on schema fields, which is exactly what
// numPredsOnFields recognizes, so it routes to schemaScanGroupStats rather than to the vector executor.
// Nothing else in the suite measures that tier at a size where the per-record loops dominate: the existing
// grouping benchmarks use 3k-60k records of a 5-to-10-attribute ad, where the whole block fits in L2 and
// the fixed costs per block are the measurement.
//
// Both predicate columns are deliberately NARROW. On a real archive EnteredHistoryTime fits four bytes and
// JobStatus fits one, so a compare that widens to int64 first has thrown away half and seven eighths of its
// lanes respectively before the kernel sees anything. The fixture reproduces those widths (see
// TestNarrowFixtureWidths, which fails if the schema ever stops fitting them that way) so the benchmark
// measures the regime production is in rather than an 8-byte one where widening is free.

// narrowFixtureRecords is the default corpus size. Large enough that the segment's record headers and the
// predicate columns both exceed L2, which is the regime where the visibility walk's scattered reads and the
// column passes' sequential ones separate; small enough to build in a few seconds.
//
// CLASSAD_NARROW_RECORDS overrides it, for profiling at a production-shaped few hundred thousand.
const narrowFixtureRecords = 120_000

func narrowRecordCount() int {
	if s := os.Getenv("CLASSAD_NARROW_RECORDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return narrowFixtureRecords
}

// histAd renders one history-shaped completed-job ad: ~40 attributes spanning the widths and kinds a real
// one does, so the block layout (hot set, fitted widths, string dictionary, cold tail) resembles production
// rather than a toy with four columns.
//
// day spreads EnteredHistoryTime over a month so a day bucket produces ~30 groups, and JobStatus is skewed
// the way a history table is -- mostly 4 (completed) with a minority of 3 (removed) and 5 (held) -- so the
// equality predicate is weakly selective, which is the case where the surviving-record loop still runs.
func histAd(i int) string {
	const day = 86400
	status := 4
	switch i % 10 {
	case 7:
		status = 3
	case 8:
		status = 5
	}
	return fmt.Sprintf(
		"ClusterId = %d\nProcId = %d\nJobStatus = %d\nEnteredHistoryTime = %d\nCompletionDate = %d\n"+
			"QDate = %d\nJobStartDate = %d\nJobCurrentStartDate = %d\nRequestMemory = %d\n"+
			"RequestCpus = %d\nRequestDisk = %d\nNumJobStarts = %d\nNumShadowStarts = %d\n"+
			"NumRestarts = %d\nExitCode = %d\nExitStatus = %d\nExitBySignal = %t\n"+
			"JobUniverse = 5\nMemoryUsage = %d\nResidentSetSize = %d\nDiskUsage = %d\n"+
			"ImageSize = %d\nBytesSent = %d\nBytesRecvd = %d\nRemoteWallClockTime = %d.5\n"+
			"RemoteUserCpu = %d.25\nRemoteSysCpu = %d.5\nCumulativeSuspensionTime = %d\n"+
			"CommittedTime = %d\nLastJobStatus = %d\nTransferInputSizeMB = %d\n"+
			"Owner = %q\nAccountingGroup = %q\nGlobalJobId = %q\nCmd = %q\nIwd = %q\n"+
			"Args = %q\nLastRemoteHost = %q\nProjectName = %q\nWantCheckpoint = %t\n",
		1000+i/4, i%4, status,
		1700000000+(i%30)*day+(i%day), 1700000000+(i%30)*day+(i%day)-30,
		1699900000+i, 1699950000+i, 1699950000+i,
		1024+(i%16)*512, 1+(i%8), 10240+(i%32)*1024, 1+(i%3), 1+(i%3),
		i%2, i%3, (i%3)*256, i%5 == 0,
		900+(i%4096), 900+(i%4096), 20480+(i%8192), 15000+(i%4096),
		1<<20+i, 1<<19+i, 60+(i%3600), 30+(i%1800), 5+(i%600), i%64,
		60+(i%3600), 2, i%512,
		fmt.Sprintf("user%03d", i%256), fmt.Sprintf("grp%02d", i%16),
		fmt.Sprintf("ap2001.chtc.wisc.edu#%d.%d#%d", 1000+i/4, i%4, 1700000000+i),
		"/home/user/payload.sh", fmt.Sprintf("/scratch/user%03d/run%d", i%256, i%1024),
		fmt.Sprintf("--input in%d.dat --output out%d.dat --seed %d", i, i, i),
		fmt.Sprintf("slot1_%d@e%04d.chtc.wisc.edu", 1+i%8, i%2048), "proj42", i%7 == 0,
	)
}

var (
	narrowFixtureOnce sync.Once
	narrowFixtureColl *Collection
	narrowFixtureN    int
)

// narrowFixture builds (once per process) the history-shaped collection the tier-1 BENCHMARKS share. It is
// never closed, for the reason groupReadFixture is not: every user of it only reads.
//
// The tests below build their own small one instead. This is a six-figure corpus and takes tens of seconds
// to put, which under the race detector is minutes -- enough on its own to push the collections race job
// past its CI budget, where it would time out inside some unrelated test.
func narrowFixture(tb testing.TB) *Collection {
	tb.Helper()
	narrowFixtureOnce.Do(func() {
		n := narrowRecordCount()
		narrowFixtureColl, narrowFixtureN = buildHistFixture(tb, n), n
	})
	if narrowFixtureColl == nil {
		tb.Skip("narrow fixture unavailable (no sealed segments)")
	}
	return narrowFixtureColl
}

// histGuardFixture is the small shared fixture the two guard tests read. They only assert properties of
// the SCHEMA -- which attribute is stored at which width, and which constraint shape routes to tier 1 --
// and neither depends on corpus size, so they share one cheap build rather than each paying for a
// history-shaped ad corpus under the race detector.
var (
	histGuardOnce sync.Once
	histGuardColl *Collection
)

func histGuardFixture(tb testing.TB) *Collection {
	tb.Helper()
	histGuardOnce.Do(func() { histGuardColl = buildHistFixture(tb, 2500) })
	if histGuardColl == nil {
		tb.Skip("history guard fixture unavailable")
	}
	return histGuardColl
}

// buildHistFixture puts n history-shaped ads and builds the columnar accelerator over them.
func buildHistFixture(tb testing.TB, n int) *Collection {
	tb.Helper()
	cd, err := NewZSTDCodec(nil)
	if err != nil {
		tb.Fatal(err)
	}
	// 4 MiB segments so a few hundred thousand records make tens of segments, each with many row
	// groups -- the block count production sees, not one block holding everything.
	c := New(Options{Shards: 1, SegmentSize: 4 << 20, Codec: cd})
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("j%d", i)), mustAdOld(tb, histAd(i))); err != nil {
			tb.Fatal(err)
		}
	}
	// Train the hot set on the attributes the queries touch, so they land in the uncompressed hot
	// region as they would on a real archive serving these queries.
	for _, e := range []string{"EnteredHistoryTime >= 0", "JobStatus >= 0", "RemoteWallClockTime >= 0",
		"RequestMemory >= 0", "RequestCpus >= 0", "BytesSent >= 0"} {
		q, err := vm.Parse(e)
		if err != nil {
			tb.Fatal(err)
		}
		for j := 0; j < 20; j++ {
			for range c.Query(q) {
			}
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 16) {
		tb.Skip("no sealed segments")
	}
	return c
}

// narrowQuery is the dashboard constraint: a month window plus a status equality. The window keeps roughly
// four fifths of the corpus, which is what a dashboard's default range does -- a predicate that threw away
// almost everything would measure zone pruning instead of the per-record loops.
const narrowQuery = "EnteredHistoryTime >= 1700000000 && JobStatus == 4"

// TestNarrowFixtureWidths pins the premise the benchmark rests on: that the two predicate columns are stored
// NARROW. If a schema change widened either to eight bytes the benchmark would quietly become a measurement
// of the case where there is nothing to win, and still pass.
func TestNarrowFixtureWidths(t *testing.T) {
	c := histGuardFixture(t)
	st := c.schemaScan.Load()
	if st == nil {
		t.Fatal("no schema scan state")
	}
	for _, want := range []struct {
		attr  string
		width int
	}{{"EnteredHistoryTime", 4}, {"JobStatus", 1}} {
		id, ok := c.intern.LookupID(want.attr)
		if !ok {
			t.Fatalf("%s not interned", want.attr)
		}
		idx, ok := st.schema.byID[id]
		if !ok {
			t.Fatalf("%s is not a schema field", want.attr)
		}
		f := st.schema.fields[idx]
		if f.width != want.width {
			t.Errorf("%s stored at width %d, fixture assumes %d", want.attr, f.width, want.width)
		}
	}
}

// TestNarrowFixtureRoutesToTier1 asserts the benchmark measures the tier it claims to. The routing is a
// property of the constraint's shape, and a change to numPredsOnFields that declined this shape would send
// every one of these benchmarks through the vector executor while they kept reporting numbers.
func TestNarrowFixtureRoutesToTier1(t *testing.T) {
	c := histGuardFixture(t)
	st := c.schemaScan.Load()
	if st == nil {
		t.Fatal("no schema scan state")
	}
	q, err := vm.Parse(narrowQuery)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.numPredsOnFields(q, st.schema); !ok {
		t.Fatalf("%q no longer analyzes as a conjunction of field comparisons: tier 1 is not being measured",
			narrowQuery)
	}
}

// BenchmarkNarrowGroupStats is the headline: the whole tier-1 query, with and without value aggregates.
//
// The two variants separate the narrowing from the reading. "count" runs the visibility walk, both column
// passes and the group-key derivation; "stats3" adds three aggregate column reads for every surviving
// record, which is what a dashboard panel with MIN/MAX/AVG asks for and what makes the surviving-record
// loop rather than the narrowing the larger half.
func BenchmarkNarrowGroupStats(b *testing.B) {
	c := narrowFixture(b)
	q, err := vm.Parse(narrowQuery)
	if err != nil {
		b.Fatal(err)
	}
	const day = 86400
	for _, v := range []struct {
		name string
		aggs []string
	}{
		{"count", nil},
		{"stats3", []string{"RemoteWallClockTime", "RequestMemory", "BytesSent"}},
	} {
		if _, ok := c.GroupStatsQueryBucketed(q, "EnteredHistoryTime", day, v.aggs); !ok {
			b.Fatalf("%s declined", v.name)
		}
		b.Run(v.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				gs, ok := c.GroupStatsQueryBucketed(q, "EnteredHistoryTime", day, v.aggs)
				if !ok || len(gs) == 0 {
					b.Fatal("declined or empty")
				}
			}
		})
	}
	b.Logf("fixture: %d records", narrowFixtureN)
}

// BenchmarkNarrowPhases times the three per-record loops in isolation, against the same blocks the full
// query walks, so the profile has numbers per phase rather than one total.
//
//	visibility  the pass that is never pruned: one gathered read of each record's header
//	narrow1/2   one column pass per predicate
//	group       deriving and scattering the group key for the survivors
//
// They are measured cumulatively (visibility; visibility+narrow1; ...) because a later phase cannot run
// without the earlier one's mask -- the difference between two rows is the phase's own cost.
func BenchmarkNarrowPhases(b *testing.B) {
	c := narrowFixture(b)
	st := c.schemaScan.Load()
	if st == nil {
		b.Fatal("no schema scan state")
	}
	q, err := vm.Parse(narrowQuery)
	if err != nil {
		b.Fatal(err)
	}
	preds, ok := c.numPredsOnFields(q, st.schema)
	if !ok {
		b.Fatal("constraint no longer routes to tier 1")
	}
	for _, phases := range []int{0, 1, 2} {
		name := [...]string{"visibility", "+narrow1", "+narrow2"}[phases]
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				benchNarrowPhases(b, c, preds[:min(phases, len(preds))], st.cache)
			}
		})
	}
}

// benchNarrowPhases walks the same blocks schemaScanGroupStats does, running only the first nPred narrowing
// passes after the visibility pass. A copy rather than a flag on the real function: a flag would put a
// branch in the loop being measured.
func benchNarrowPhases(b *testing.B, c *Collection, preds []fieldPred, bc *blockCache) {
	var cand candSet
	var sc predScratch
	live := 0
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			idxs, resolved := resolveFields(cs, preds)
			if !resolved {
				continue
			}
			base := 0
			for _, blk := range cs.blocks {
				sc.prepare(blk.n)
				cand.setVisible(cs, w, s0, base, blk.n)
				for i := range preds {
					if cand.live == 0 {
						break
					}
					if !narrowByField(blk, idxs[i], preds[i], bc, &cand, &sc) {
						b.Fatal("narrowByField declined")
					}
				}
				live += cand.live
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	if live == 0 {
		b.Fatal("no live records")
	}
}

// setAll marks every record a candidate. Only a benchmark wants this: production always arrives here
// through setVisible, and measuring the narrowing on its own means not paying for that first.
func (c *candSet) setAll(n int) {
	c.reset(n)
	for i := range c.w {
		c.w[i] = ^uint64(0)
	}
	if tail := uint(n % 64); tail != 0 && len(c.w) > 0 {
		c.w[len(c.w)-1] = (uint64(1) << tail) - 1
	}
	c.live = n
}

// BenchmarkNarrowField measures ONE narrowing pass with nothing else in the frame -- no visibility
// walk, no group accumulation -- against the same blocks the full query walks.
//
// This is the benchmark that separates the build configurations, and it is deliberately per COLUMN
// WIDTH, because that is what decides whether an engine has lanes for it:
//
//	EnteredHistoryTime  four bytes: 4 lanes on NEON, 8 on AVX2
//	JobStatus           one byte:   no engine has a kernel for it, so this arm measures the scalar
//	                                bitplane path in BOTH builds and must not differ between them
//
// Run it both ways to see the engine's contribution and to check the scalar path did not regress:
//
//	go test -run XXX -bench NarrowField ./collections/
//	GOEXPERIMENT=simd go test -run XXX -bench NarrowField ./collections/
func BenchmarkNarrowField(b *testing.B) {
	c := narrowFixture(b)
	st := c.schemaScan.Load()
	if st == nil {
		b.Fatal("no schema scan state")
	}
	for _, tc := range []struct{ name, expr string }{
		{"width4/EnteredHistoryTime", "EnteredHistoryTime >= 1700000000"},
		{"width1/JobStatus", "JobStatus == 4"},
	} {
		q, err := vm.Parse(tc.expr)
		if err != nil {
			b.Fatal(err)
		}
		preds, ok := c.numPredsOnFields(q, st.schema)
		if !ok || len(preds) != 1 {
			b.Fatalf("%q is not one field predicate", tc.expr)
		}
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var cand candSet
			var sc predScratch
			recs := 0
			for i := 0; i < b.N; i++ {
				recs = 0
				for _, sh := range c.shards {
					_, wins := sh.snapshot()
					for _, w := range wins {
						cs := w.seg.colblk.Load()
						idxs, resolved := resolveFields(cs, preds)
						if !resolved {
							continue
						}
						for _, blk := range cs.blocks {
							sc.prepare(blk.n)
							cand.setAll(blk.n)
							if !narrowByField(blk, idxs[0], preds[0], st.cache, &cand, &sc) {
								b.Fatal("narrowByField declined")
							}
							recs += blk.n
						}
					}
					releaseWindows(wins)
				}
			}
			// Per record rather than per call: the blocks differ in size, and what is being
			// compared across builds is the cost of one column element.
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(recs), "ns/rec")
		})
	}
}
