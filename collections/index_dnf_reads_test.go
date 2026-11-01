package collections

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// dnfReadQueries are the filters the read paths are checked with: top-level ORs of indexable
// comparisons, an OR scoped by a conjunct (the shape a time-windowed listing sends), ORs of
// recent-only ranges an archive's zone maps can prune, and two that must not read through the
// union -- one with an unindexed disjunct, and one whose every disjunct is impossible.
var dnfReadQueries = []struct {
	expr  string
	union bool // planned through the union of the disjuncts' candidates
}{
	{`ClusterId == 5 || Owner == "user1"`, true},
	{`ClusterId < 20 || QDate >= 1000560`, true},
	{`(ClusterId > 100 && ClusterId < 120) || Owner == "user3"`, true},
	{`Owner is undefined || ClusterId == 7`, true},
	{`Owner == "user2" && (QDate >= 1000500 || ClusterId < 30)`, true},
	{`QDate >= 1000590 || ClusterId >= 595`, true},
	{`(ClusterId > 10 && ClusterId < 5) || (QDate > 3 && QDate < 1)`, true},
	{`ClusterId == 5 || Pad01 == 3`, false},
}

// dnfReference is expr with a disjunct no index or zone map can use, which the planner must then
// read with a plain walk: the same answer, in the order a walk of the collection gives it.
func dnfReference(expr string) string { return "(" + expr + ") || Pad07 == -999" }

// dnfReads renders what every filtered read path returns for expr, in the order it returns it, and
// the first few rows of each when the consumer stops early.
func dnfReads(t *testing.T, c *Collection, expr string) map[string][]string {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	proj := []string{"ClusterId", "Owner", "QDate"}
	raw := func(r RawAd) string {
		parts := make([]string, len(r.Exprs))
		for i, e := range r.Exprs {
			parts[i] = string(e)
		}
		return r.MyType + "|" + strings.Join(parts, ";")
	}
	out := map[string][]string{}
	for ad := range c.Query(q) {
		out["Query"] = append(out["Query"], adSig(ad))
	}
	for r := range c.QueryRaw(q) {
		out["QueryRaw"] = append(out["QueryRaw"], raw(r))
	}
	for r := range c.QueryRawProjected(q, proj, false, false) {
		out["QueryRawProjected"] = append(out["QueryRawProjected"], raw(r))
	}
	for r := range c.QueryRawProjectedStats(q, proj, true, false, &ScanStats{}) {
		out["QueryRawProjectedRefs"] = append(out["QueryRawProjectedRefs"], raw(r))
	}
	for w := range c.QueryRawWireStats(q, proj, false, &ScanStats{}) {
		out["QueryRawWire"] = append(out["QueryRawWire"], string(w))
	}
	for vals := range c.QueryProjectStats(q, proj, &ScanStats{}) {
		out["QueryProject"] = append(out["QueryProject"], fmt.Sprint(vals))
	}
	// Paged, a few rows a page, across every page.
	var cur SeqCursor
	for pages := 0; ; pages++ {
		seq, page := c.QueryRawFromSeq(q, proj, cur, 7)
		for r := range seq {
			out["QueryRawFromSeq"] = append(out["QueryRawFromSeq"], raw(r))
		}
		if page.Err != nil {
			t.Fatalf("%s: QueryRawFromSeq: %v", expr, page.Err)
		}
		if !page.More {
			break
		}
		if pages > 1000 {
			t.Fatalf("%s: QueryRawFromSeq did not finish", expr)
		}
		cur = page.Next
	}
	// A consumer that stops early: a pushed-down LIMIT, the newest rows on an archive.
	const limit = 3
	for r := range c.QueryRaw(q) {
		if out["QueryRaw/limit"] = append(out["QueryRaw/limit"], raw(r)); len(out["QueryRaw/limit"]) == limit {
			break
		}
	}
	for ad := range c.Query(q) {
		if out["Query/limit"] = append(out["Query/limit"], adSig(ad)); len(out["Query/limit"]) == limit {
			break
		}
	}
	return out
}

// seqCandidates counts the records the paged read's runs hand its visitor for expr, across every
// shard -- the candidates its plan leaves, before the query re-verifies them -- and the segments it
// opens a run on, which excludes the ones its zone maps rule out.
func seqCandidates(t *testing.T, c *Collection, expr string) (cand, segs int) {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	qp := c.newQueryPlan(q, nil, nil, false)
	rp := c.planReadQuery(qp)
	for i, sh := range c.shards {
		sh.mu.RLock()
		s0 := sh.commitSeq
		sh.mu.RUnlock()
		runs, release := c.shardRuns(sh, s0, SeqCursor{Shard: uint32(i), Snapshot: s0}, qp, rp)
		segs += len(runs)
		var group []groupRec
		for {
			if group = nextGroup(runs, s0, 0, group[:0]); len(group) == 0 {
				break
			}
			cand += len(group)
		}
		release()
	}
	return cand, segs
}

// checkDNFReads requires every read path to return exactly, and in the same order, what it returns
// for the same filter read with a plain walk; the union queries to read through the index, as
// ExplainQuery says they will, with the per-segment verdicts it reports; and the others not to.
func checkDNFReads(t *testing.T, label string, c *Collection) {
	t.Helper()
	sidecar, total := dnfSidecarCounts(c)
	if sidecar == 0 {
		t.Fatalf("%s: no sealed segment is served by a sidecar index (%d segments)", label, total)
	}
	nonEmpty := map[string]bool{}
	for _, tc := range dnfReadQueries {
		q, err := vm.Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := vm.Parse(dnfReference(tc.expr))
		if err != nil {
			t.Fatal(err)
		}
		if rp := c.planReadQuery(c.newQueryPlan(ref, nil, nil, false)); rp.disjunctive() || len(rp.usable) > 0 {
			t.Fatalf("%s: the reference for %s is index-planned; it would not be a plain walk", label, tc.expr)
		}
		if rp := c.planReadQuery(c.newQueryPlan(q, nil, nil, false)); rp.disjunctive() != tc.union {
			t.Fatalf("%s: %s planned disjunctive=%v, want %v", label, tc.expr, rp.disjunctive(), tc.union)
		}
		got, want := dnfReads(t, c, tc.expr), dnfReads(t, c, dnfReference(tc.expr))
		t.Logf("%s %s got=%d want=%d", label, tc.expr, len(got["Query"]), len(want["Query"]))
		for path, w := range want {
			g := got[path]
			if len(w) > 0 {
				nonEmpty[path] = true
			}
			if len(g) != len(w) {
				t.Errorf("%s: %s: %s returned %d rows, the plain walk %d", label, path, tc.expr, len(g), len(w))
				continue
			}
			for i := range w {
				if g[i] != w[i] {
					t.Errorf("%s: %s: %s row %d differs from the plain walk\n got:  %s\n want: %s", label, path, tc.expr, i, g[i], w[i])
					break
				}
			}
		}
		// The plain walk is itself the scan's answer.
		sigsEqual(t, label+" Query("+tc.expr+") vs scan", got["Query"], scanMatches(t, c, q))

		ex := c.ExplainQuery(q)
		var st ScanStats
		for range c.QueryRawProjectedStats(q, []string{"ClusterId"}, false, false, &st) {
		}
		switch {
		case !tc.union:
			if ex.Plan == "indexed-union" || st.SegmentsIndexed != 0 {
				t.Errorf("%s: %s: read through the union (explain %q, %d segments from postings); one disjunct is unindexed", label, tc.expr, ex.Plan, st.SegmentsIndexed)
			}
			if !strings.Contains(ex.Reason, "disjunct") {
				t.Errorf("%s: %s: explain reason %q does not say which disjunct defeats the index", label, tc.expr, ex.Reason)
			}
		case len(got["Query"]) == 0 && ex.Plan == "empty":
			// Every disjunct is impossible: nothing is read at all.
			if st.SegmentsTotal != 0 {
				t.Errorf("%s: %s: explain says empty, but the read opened %d segments", label, tc.expr, st.SegmentsTotal)
			}
		default:
			if ex.Plan != "indexed-union" || len(ex.Groups) < 2 {
				t.Errorf("%s: %s: explain plan %q with %d groups, want indexed-union; reason %q", label, tc.expr, ex.Plan, len(ex.Groups), ex.Reason)
			}
			// What explain predicts is what the read does, segment by segment.
			if ex.IndexSegments != st.SegmentsIndexed || ex.IndexSkippedSegments != st.SegmentsIndexSkipped ||
				ex.UnindexedSegments != st.SegmentsUnindexed || ex.ZonePrunedSegments != st.SegmentsPruned {
				t.Errorf("%s: %s: explain (indexed %d, skipped %d, unindexed %d, zone-pruned %d) disagrees with the read %+v",
					label, tc.expr, ex.IndexSegments, ex.IndexSkippedSegments, ex.UnindexedSegments, ex.ZonePrunedSegments, st)
			}
			// The paged read narrows to the union's candidates too.
			if cand, _ := seqCandidates(t, c, tc.expr); cand >= c.Len() {
				t.Errorf("%s: %s: the paged read visits %d candidates of %d live records; it is not narrowed", label, tc.expr, cand, c.Len())
			}
			if st.SegmentsIndexed+st.SegmentsIndexSkipped+st.SegmentsPruned < sidecar {
				t.Errorf("%s: %s: only %d of the %d sidecar-indexed segments were answered by postings or zone maps; stats %+v",
					label, tc.expr, st.SegmentsIndexed+st.SegmentsIndexSkipped+st.SegmentsPruned, sidecar, st)
			}
		}
	}
	for _, path := range []string{"Query", "QueryRaw", "QueryRawProjected", "QueryRawProjectedRefs", "QueryRawWire", "QueryProject", "QueryRawFromSeq"} {
		if !nonEmpty[path] {
			t.Errorf("%s: %s returned nothing for every query; the comparison is vacuous", label, path)
		}
	}
}

// TestDNFReadPathsTable: every filtered read of a mutable persistent table plans a top-level OR
// through the union of its disjuncts' index candidates -- sealed sidecars included, with delta
// records or time-checkpoint markers in the segments and deleted keys -- and returns what a plain
// walk does, in the same order, before and after a reopen.
func TestDNFReadPathsTable(t *testing.T) {
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
				ad.Delete("Owner")
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
		// Patches that move keys into and out of the probed values, some of them in a commit of
		// several keys (one sequence, so the paged read orders them by key).
		for i := 0; i < n; i += 9 {
			w := c.Begin()
			for j := i; j < i+9 && j < n; j += 3 {
				patch := classad.New()
				patch.InsertAttrString("Owner", fmt.Sprintf("user%d", (j+1)%5))
				patch.InsertAttr("N", int64(j))
				w.PatchAttrs([]byte(fmt.Sprintf("k%04d.0", j)), patch, nil)
			}
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
		checkDNFReads(t, label, c)
		if cerr := c.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		c, err = Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		checkDNFReads(t, label+" reopened", c)
		if cerr := c.Close(); cerr != nil {
			t.Fatal(cerr)
		}
	}
}

// TestDNFReadPathsArchive is TestDNFReadPathsTable for a newest-first archive with zone maps, with
// and without interning at seal (a dictionary record in each sealed segment): the reads come back
// newest-first through the union, a consumer that stops early gets the newest matches, and the
// zone maps drop the segments no disjunct can match.
func TestDNFReadPathsArchive(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	for _, intern := range []bool{false, true} {
		opts := ArchiveOptions{Dir: t.TempDir(), SegmentSize: 1 << 13, InternAtSeal: intern,
			ValueAttrs: []string{"ClusterId", "QDate"}, CategoricalAttrs: []string{"Owner"},
			ZoneAttrs: []string{"ClusterId", "QDate"}}
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
		check := func(label string) {
			checkDNFReads(t, label, a.c)
			// Both disjuncts select only the newest records, so every older segment is ruled out
			// by its zone map before its index is read.
			q, qerr := vm.Parse(`QDate >= 1000590 || ClusterId >= 595`)
			if qerr != nil {
				t.Fatal(qerr)
			}
			var st ScanStats
			for range a.c.QueryRawProjectedStats(q, []string{"ClusterId"}, false, false, &st) {
			}
			if st.SegmentsPruned == 0 {
				t.Errorf("%s: no segment was zone-pruned for a query of the newest records only; stats %+v", label, st)
			}
			if _, segs := seqCandidates(t, a.c, `QDate >= 1000590 || ClusterId >= 595`); segs != st.SegmentsTotal-st.SegmentsPruned {
				t.Errorf("%s: the paged read opened %d segments, the query %d of %d", label, segs, st.SegmentsTotal-st.SegmentsPruned, st.SegmentsTotal)
			}
		}
		label := fmt.Sprintf("archive intern=%v", intern)
		// A segment interned at seal is rewritten, and carries the zone map its source had.
		check(label)
		if cerr := a.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		a, err = OpenArchive(opts)
		if err != nil {
			t.Fatal(err)
		}
		check(label + " reopened")
		if cerr := a.Close(); cerr != nil {
			t.Fatal(cerr)
		}
	}
}
