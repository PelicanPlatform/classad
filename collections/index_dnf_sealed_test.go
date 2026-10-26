package collections

import (
	"fmt"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// dnfQueries are top-level ORs of indexable comparisons -- the disjunctive candidate path -- plus
// one whose second disjunct is on an unindexed attribute, which must disable the narrowing.
var dnfQueries = []struct {
	expr     string
	prunable bool
}{
	{`ClusterId == 5 || Owner == "user1"`, true},
	{`ClusterId < 20 || QDate >= 1000560`, true},
	{`(ClusterId > 100 && ClusterId < 120) || Owner == "user3"`, true},
	{`Owner is undefined || ClusterId == 7`, true},
	{`Owner == "nobody" || ClusterId > 100000`, true},
	{`ClusterId == 5 || Pad01 == 3`, false},
}

// dnfSidecarCounts reports how many sealed segments of c answer from a mapped sidecar index (the
// heap index released) and how many segments there are in all.
func dnfSidecarCounts(c *Collection) (sidecar, total int) {
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			total++
			if seg != sh.act && seg.msidx.Load() != nil && seg.idx.Load() == nil {
				sidecar++
			}
		}
		sh.mu.RUnlock()
	}
	return sidecar, total
}

// visibleDeltas counts the delta records of c a read sees now: the current versions that are
// fragments, which an index can only reach through the delta scopes.
func visibleDeltas(c *Collection) int {
	n := 0
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		forEachVisibleRef(s0, wins, func(r recRef) bool {
			if recIsDelta(r.w.data, r.off) {
				n++
			}
			return true
		})
		releaseWindows(wins)
	}
	return n
}

// hasUncoveredTail reports whether some indexed segment of c holds records past what its index
// covers, which a candidate walk visits in full.
func hasUncoveredTail(c *Collection) bool {
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if si := seg.readIdx(); si != nil && int(si.coveredUpto()) < seg.used {
				sh.mu.RUnlock()
				return true
			}
		}
		sh.mu.RUnlock()
	}
	return false
}

// dnfStats runs the disjunctive index path for expr over every shard of c, as Query does, and
// returns its scan statistics and the number of records the query accepted.
func dnfStats(t *testing.T, c *Collection, expr string) (ScanStats, int) {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	plan := q.ProbePlan()
	groups, prunable := c.planIndexGroups(plan)
	if len(plan) < 2 || !prunable {
		t.Fatalf("%s: not planned through the disjunctive index path", expr)
	}
	var st ScanStats
	qp := c.newQueryPlan(q, nil, &st)
	rows := 0
	for _, sh := range c.shards {
		c.scanShardIndexedGroups(sh, groups, plan, qp, func([]byte, *segDictHandle) bool {
			rows++
			return true
		})
	}
	return st, rows
}

// checkDNF compares every dnfQueries answer against a plain scan, and requires the prunable ones
// to answer each sealed sidecar-indexed segment from its postings.
func checkDNF(t *testing.T, label string, c *Collection) {
	t.Helper()
	sidecar, total := dnfSidecarCounts(c)
	if sidecar == 0 {
		t.Fatalf("%s: no sealed segment is served by a sidecar index (%d segments)", label, total)
	}
	for _, tc := range dnfQueries {
		q, err := vm.Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		if len(q.ProbePlan()) < 2 {
			t.Fatalf("%s: %s is not a disjunctive plan", label, tc.expr)
		}
		if _, prunable := c.planIndexGroups(q.ProbePlan()); prunable != tc.prunable {
			t.Fatalf("%s: %s planned prunable=%v, want %v", label, tc.expr, prunable, tc.prunable)
		}
		want := scanMatches(t, c, q)
		sigsEqual(t, label+" Query("+tc.expr+")", collectQuery(t, c, tc.expr), want)
		if !tc.prunable {
			continue
		}
		st, rows := dnfStats(t, c, tc.expr)
		if rows != len(want) {
			t.Errorf("%s: %s: disjunctive path accepted %d records, scan %d", label, tc.expr, rows, len(want))
		}
		// Only the active segments have no sidecar; every sealed one answers from its postings,
		// unless its zone map ruled it out first.
		if st.SegmentsIndexed+st.SegmentsIndexSkipped+st.SegmentsPruned < sidecar {
			t.Errorf("%s: %s: %d segments answered from postings (+%d skipped by the index, %d zone-pruned), want all %d sidecar-indexed ones; stats %+v",
				label, tc.expr, st.SegmentsIndexed, st.SegmentsIndexSkipped, st.SegmentsPruned, sidecar, st)
		}
		if st.SegmentsUnindexed > total-sidecar {
			t.Errorf("%s: %s: %d segments walked in full, but only %d lack a sidecar; stats %+v",
				label, tc.expr, st.SegmentsUnindexed, total-sidecar, st)
		}
	}
}

// TestDNFQuerySealedSidecar: a top-level OR of indexable comparisons is served by the segment
// indexes, including a sealed segment's mapped sidecar -- before and after a reopen, with deletes
// and either delta records or time-checkpoint markers in the segments (a store holds one or the
// other) -- and returns exactly what a plain scan does.
func TestDNFQuerySealedSidecar(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	for _, deltas := range []bool{true, false} {
		opts := Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 14,
			ValueAttrs: []string{"ClusterId", "QDate"}, CategoricalAttrs: []string{"Owner"}}
		label := "markers"
		if deltas {
			opts.DeltaMax = 16
			label = "deltas"
		} else {
			opts.TimeTravel = &TimeTravelOptions{MaxDistance: time.Hour, CheckpointInterval: time.Millisecond}
		}
		c, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		put := func(i int) {
			ad := jobAd(i, map[string]int64{"QDate": int64(1000000 + i)})
			if i%11 == 0 {
				ad.Delete("Owner") // absent: what `Owner is undefined` finds
			} else {
				ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%5))
			}
			if perr := c.Put([]byte(fmt.Sprintf("k%04d.0", i)), ad); perr != nil {
				t.Fatal(perr)
			}
			if !deltas && i%25 == 0 {
				time.Sleep(2 * time.Millisecond) // let a time checkpoint land between records
			}
		}
		const n = 600
		for i := 0; i < n; i++ {
			put(i)
		}
		// Patches that move keys into and out of the probed values (deltas where enabled), and
		// deletes of matching keys.
		for i := 0; i < n; i += 3 {
			patch := classad.New()
			patch.InsertAttrString("Owner", fmt.Sprintf("user%d", (i+1)%5))
			patch.InsertAttr("N", int64(i))
			w := c.Begin()
			w.PatchAttrs([]byte(fmt.Sprintf("k%04d.0", i)), patch, nil)
			if r := w.Commit(); r.Conflicted() {
				t.Fatalf("patch %d conflicted", i)
			}
		}
		for i := 0; i < n; i += 7 {
			c.Delete([]byte(fmt.Sprintf("k%04d.0", i)))
		}
		if d, _ := c.DeltaStats(); deltas && d == 0 {
			t.Fatal("no deltas written; the test is not exercising what it claims")
		}
		c.Reindex()
		// A few records after the index: an active segment's un-indexed tail. Few, because rolling
		// segments compacts them, which rewrites the delta records into whole ones. Where the
		// segments roll is not fixed -- time checkpoints land where the clock says -- so a record
		// that opens a new, unindexed segment is followed by indexing that one.
		for i := n; !hasUncoveredTail(c); i++ {
			if i == n+50 {
				t.Fatal("no indexed segment has an un-indexed tail; the test is not exercising what it claims")
			}
			put(i)
			if !hasUncoveredTail(c) {
				c.Reindex()
			}
		}
		if deltas && visibleDeltas(c) == 0 {
			t.Fatal("no delta record is visible; the test is not exercising what it claims")
		}
		checkDNF(t, label, c)
		if cerr := c.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		c, err = Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		checkDNF(t, label+" reopened", c)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDNFArchiveSealedSidecar is TestDNFQuerySealedSidecar for a newest-first archive, whose
// sealed segments are interned (a dictionary record in each) and indexed as they seal.
func TestDNFArchiveSealedSidecar(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	opts := ArchiveOptions{Dir: dir, SegmentSize: 1 << 13, InternAtSeal: true,
		ValueAttrs: []string{"ClusterId", "QDate"}, CategoricalAttrs: []string{"Owner"}}
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		ad := jobAd(i, map[string]int64{"QDate": int64(1000000 + i)})
		if i%11 == 0 {
			ad.Delete("Owner")
		} else {
			ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%5))
		}
		if aerr := a.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	if !a.c.reverseScan {
		t.Fatal("the archive does not scan newest-first; the test would not reach the reverse walk")
	}
	checkDNF(t, "archive", a.c)
	if cerr := a.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	a, err = OpenArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	checkDNF(t, "archive reopened", a.c)
}

// TestDNFCandidatesSkipMarkers: the disjunctive candidate walk hands its visitor keyed records
// only. A time checkpoint, a segment dictionary or a columnar record is not an ad; passed on, its
// payload is decompressed as one -- under a compressing codec that fails -- or, uncompressed,
// evaluated as an ad with no attributes, which an `is undefined` disjunct accepts. The decode
// that follows drops it, but counts it as a skipped record (RecordReadSkips): a read failure
// that never happened.
//
// Not parallel: RecordReadSkips is process-wide.
func TestDNFCandidatesSkipMarkers(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	for _, tc := range []struct{ zstd, reverse bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		zstd := tc.zstd
		opts := Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 13, ReverseScan: tc.reverse,
			ValueAttrs: []string{"ClusterId"}, CategoricalAttrs: []string{"Owner"},
			TimeTravel: &TimeTravelOptions{MaxDistance: time.Hour, CheckpointInterval: time.Millisecond}}
		if zstd {
			codec, err := NewZSTDCodec(nil)
			if err != nil {
				t.Fatal(err)
			}
			opts.Codec = codec
		}
		c, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		const n = 60
		for i := 0; i < n; i++ {
			ad := mustAd(t, fmt.Sprintf(`[ClusterId=%d; Owner="user%d"]`, i, i%3))
			if perr := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); perr != nil {
				t.Fatal(perr)
			}
			time.Sleep(2 * time.Millisecond) // a checkpoint marker after each record
		}
		markers := 0
		sh := c.shards[0]
		_, wins := sh.snapshot()
		for _, w := range wins {
			for off := uint32(0); int(off) < w.used; {
				total := recTotalLen(w.data, off)
				if total == 0 {
					break
				}
				if recIsMarker(w.data, off) {
					markers++
				}
				off += total
			}
		}
		releaseWindows(wins)
		if markers == 0 {
			t.Fatal("no marker was written; the test is not exercising what it claims")
		}
		// No index is built, so every window is walked in full, markers and all: the visitors must
		// see exactly the n live records -- the query's (in this collection's walk order) and the
		// matchmaking one.
		q, err := vm.Parse(`Owner == "user1" || ClusterId == 5`)
		if err != nil {
			t.Fatal(err)
		}
		groups, prunable := c.planIndexGroups(q.ProbePlan())
		if !prunable {
			t.Fatal("the OR is not planned through the disjunctive index path")
		}
		if st, _ := dnfStats(t, c, `Owner == "user1" || ClusterId == 5`); st.RecordsVisited != n {
			t.Errorf("zstd=%v reverse=%v: query visitor saw %d records, want the %d live ones (%d markers in the segments)",
				zstd, tc.reverse, st.RecordsVisited, n, markers)
		}
		seen, notAds := 0, 0
		c.scanShardCandidatesGroups(sh, groups, func(w []byte, _ *segDictHandle) bool {
			seen++
			if len(w) == 0 || w[0] != 0xCA {
				notAds++
			}
			return true
		})
		if seen != n || notAds != 0 {
			t.Errorf("zstd=%v: match visitor saw %d records (%d not an ad), want the %d live ones (%d markers in the segments)",
				zstd, seen, notAds, n, markers)
		}
		// Through Query: a marker's payload does not decompress under zstd, and, uncompressed, it
		// has no attributes for the matcher to find, so `Owner is undefined` accepts it and only
		// the decode that follows rejects it. Either way the record is dropped and counted as a
		// read skip.
		for _, expr := range []string{`Owner == "user1" || ClusterId == 5`, `Owner is undefined || ClusterId == 5`} {
			eq, err := vm.Parse(expr)
			if err != nil {
				t.Fatal(err)
			}
			before := RecordReadSkips()
			rows := len(collectQuery(t, c, expr))
			after := RecordReadSkips()
			for reason, v := range after {
				if v != before[reason] {
					t.Errorf("zstd=%v: %s counted %d %q read skips over intact records", zstd, expr, v-before[reason], reason)
				}
			}
			if want := len(scanMatches(t, c, eq)); rows != want {
				t.Errorf("zstd=%v: %s: Query returned %d rows, scan %d", zstd, expr, rows, want)
			}
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
