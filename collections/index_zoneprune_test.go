package collections

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// lateIndexArchive is the shape that made the indexed plan slow: an archive whose zone-mapped
// attributes were value-indexed only after most of its segments had sealed, with the backfill
// bounded so the old segments keep no postings for them. A recent-window query then plans
// through the index, and every segment the index does not cover had to be walked in full even
// though its zone map proves it holds no match.
//
// Ts is a timestamp: old records sit about a million seconds in the past, the newest ones in
// the future, so `Ts >= now` and `Ts >= CurrentTime - 600` both select exactly the new phase
// whatever the clock does while the test runs. Other lies far below any Ts and falls as Ts
// rises, so a zone map applied to the wrong attribute would prune segments that hold matches. One old
// record defines CurrentTime itself: the CurrentTime-folded probes do not describe it, so its
// segment must never be pruned on them -- and it matches `Ts >= CurrentTime - 600`.
type lateIndexArchive struct {
	dir      string
	now      int64
	old, new int
	poison   int // N of the record that defines CurrentTime
}

const (
	lateIndexSegSize = 1 << 12
	otherBase        = 1_000_000
)

func (f *lateIndexArchive) opts(zone, value []string) ArchiveOptions {
	return ArchiveOptions{
		Dir: f.dir, SegmentSize: lateIndexSegSize,
		// Owner is indexed from the start, so every old segment seals with an index -- one that
		// does not hold Ts or Other.
		CategoricalAttrs: []string{"Owner"},
		ZoneAttrs:        zone, ValueAttrs: value,
		// Only the newest few segments pick up an index added later.
		IndexBackfillBytes: 3 * lateIndexSegSize,
	}
}

func (f *lateIndexArchive) append(t *testing.T, a *Archive, from, to int, ts func(i int) int64) {
	t.Helper()
	for i := from; i < to; i++ {
		src := fmt.Sprintf(`[ N = %d; Ts = %d; Other = %d; Owner = "u%d"; Pad = %q ]`, i, ts(i), otherBase-i, i%7, strings.Repeat("p", 16))
		if i == f.poison {
			src = fmt.Sprintf(`[ N = %d; Ts = %d; Other = %d; CurrentTime = 0 ]`, i, ts(i), otherBase-i)
		}
		ad, err := classad.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
}

func newLateIndexArchive(t *testing.T) (*lateIndexArchive, *Archive) {
	t.Helper()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	f := &lateIndexArchive{dir: t.TempDir(), now: time.Now().Unix(), old: 3000, new: 150}
	f.poison = f.old / 2
	a, err := CreateArchive(f.opts([]string{"Ts", "Other"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	f.append(t, a, 0, f.old, func(i int) int64 { return f.now - 1_000_000 + int64(i) })
	a.AddIndex(nil, []string{"Ts", "Other"})
	a.Reindex()
	f.append(t, a, f.old, f.old+f.new, func(i int) int64 { return f.now + 1000 + int64(i) })
	a.Reindex()
	return f, a
}

// plainMatches answers q by evaluating it against every record from an unfiltered scan, which
// neither zone-prunes nor consults an index: the ground truth.
func plainMatches(t *testing.T, a *Archive, q *vm.Query) []int64 {
	t.Helper()
	var out []int64
	for ad := range a.c.Scan() {
		if q.Matches(ad) {
			n, ok := ad.EvaluateAttrInt("N")
			if !ok {
				t.Fatal("record without N")
			}
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

func projectedNs(t *testing.T, a *Archive, q *vm.Query, st *ScanStats) []int64 {
	t.Helper()
	var out []int64
	for row := range a.QueryProjectStats(q, []string{"N"}, st) {
		n, err := row[0].IntValue()
		if err != nil {
			t.Fatalf("projected N is %v", row[0])
		}
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// check runs the recent-window queries over a and asserts each returns exactly the plain
// answer while the index plan zone-prunes the segments it does not cover.
func (f *lateIndexArchive) check(t *testing.T, a *Archive, label string) {
	t.Helper()
	newest := make([]int64, 0, f.new)
	for i := f.old; i < f.old+f.new; i++ {
		newest = append(newest, int64(i))
	}
	withPoison := append([]int64{int64(f.poison)}, newest...)
	cases := []struct {
		expr string
		want []int64
	}{
		{fmt.Sprintf(`Ts >= %d`, f.now), newest},
		{fmt.Sprintf(`Other <= %d`, otherBase-f.old), newest},
		{fmt.Sprintf(`Ts >= %d && Other > 0`, f.now+1000+int64(f.old+f.new-10)), newest[f.new-10:]},
		// Reads CurrentTime: the folded probe prunes only segments proven free of it, so the
		// poison record's segment is walked and its record returned.
		{`Ts >= CurrentTime - 600`, withPoison},
	}
	for _, tc := range cases {
		q, err := vm.Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		if plain := plainMatches(t, a, q); !slices.Equal(plain, tc.want) {
			t.Fatalf("%s: fixture is off: plain scan of %s returned %d rows, want %d", label, tc.expr, len(plain), len(tc.want))
		}
		ex := a.ExplainQuery(q)
		if ex.Plan != "indexed" {
			t.Fatalf("%s: %s planned %q, not through the index (%s)", label, tc.expr, ex.Plan, ex.Reason)
		}
		if ex.ZonePrunedSegments == 0 {
			t.Errorf("%s: EXPLAIN %s reports no zone-pruned segments (%s)", label, tc.expr, ex.Reason)
		}
		var st ScanStats
		got := projectedNs(t, a, q, &st)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %s returned %d rows, want %d", label, tc.expr, len(got), len(tc.want))
		}
		if st.SegmentsPruned == 0 || st.SegmentsPruned+st.SegmentsScanned != st.SegmentsTotal {
			t.Errorf("%s: %s: segments total=%d pruned=%d scanned=%d; the indexed plan did not zone-prune",
				label, tc.expr, st.SegmentsTotal, st.SegmentsPruned, st.SegmentsScanned)
		}
		if st.SegmentsPruned != ex.ZonePrunedSegments {
			t.Errorf("%s: %s: EXPLAIN predicts %d zone-pruned segments, the scan pruned %d",
				label, tc.expr, ex.ZonePrunedSegments, st.SegmentsPruned)
		}
		// Uncovered segments the zones rule out are never walked: what is left is the active
		// segment, the poison record's segment for the CurrentTime query, and the covered
		// segments' candidates.
		if st.RecordsVisited > f.old/10 {
			t.Errorf("%s: %s visited %d records (unindexed segments walked: %d); the uncovered old segments were walked",
				label, tc.expr, st.RecordsVisited, st.SegmentsUnindexed)
		}
		// Newest-first order through the reverse candidate walk.
		var order []int64
		for ad := range a.Query(q) {
			n, _ := ad.EvaluateAttrInt("N")
			order = append(order, n)
		}
		if len(order) != len(tc.want) || !slices.IsSortedFunc(order, func(x, y int64) int { return int(y - x) }) {
			t.Errorf("%s: Query(%s) returned %d rows (newest first: %v), want %d", label, tc.expr, len(order),
				slices.IsSortedFunc(order, func(x, y int64) int { return int(y - x) }), len(tc.want))
		}
	}
}

// TestIndexedPlanZonePrunesUncoveredSegments: the indexed plan applies the scan's zone pruning
// before walking candidates, so a segment the index does not cover is skipped when its zone map
// rules it out -- in the session that built the zones, after a reopen that adopts them from the
// sidecars, and after a configuration change that hands this process's attribute ids out in a
// different order.
func TestIndexedPlanZonePrunesUncoveredSegments(t *testing.T) {
	f, a := newLateIndexArchive(t)
	if _, sealed := a.StaleIndexSegments(); sealed < 20 {
		t.Fatalf("fixture sealed only %d segments", sealed)
	}
	if ex := a.ExplainQuery(mustQuery(t, fmt.Sprintf(`Ts >= %d`, f.now))); ex.IndexSegments+ex.IndexSkippedSegments > 8 {
		t.Fatalf("fixture indexed %d segments; most should be uncovered", ex.IndexSegments+ex.IndexSkippedSegments)
	}
	f.check(t, a, "built")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	reopen := func(zone, value []string) (*Archive, OpenTiming) {
		t.Helper()
		var diag OpenIndexDiag
		prev := OpenIndexDiagHook
		OpenIndexDiagHook = func(d OpenIndexDiag) { diag = d }
		defer func() { OpenIndexDiagHook = prev }()
		reopened, err := OpenArchive(f.opts(zone, value))
		if err != nil {
			t.Fatal(err)
		}
		return reopened, diag.Timing
	}

	// Same configuration: every sealed segment adopts its persisted zone map.
	a, tm := reopen([]string{"Ts", "Other"}, []string{"Ts", "Other"})
	if tm.ZoneRecomputed != 0 {
		t.Errorf("reopen recomputed %d zone maps instead of adopting them", tm.ZoneRecomputed)
	}
	f.check(t, a, "reopened")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	// A configuration change: two attributes zoned and indexed ahead of the old ones, so this
	// process interns names in a different order than the one that wrote the sidecars. A zone
	// map bound by position rather than by name would apply Other's range to Ts.
	a, _ = reopen([]string{"Extra", "Ts", "Other"}, []string{"Extra2", "Ts", "Other"})
	defer a.Close()
	tsID, ok1 := a.c.intern.LookupID("Ts")
	otherID, ok2 := a.c.intern.LookupID("Other")
	if !ok1 || !ok2 {
		t.Fatal("zone attributes not interned")
	}
	sh := a.c.shards[0]
	zoned := 0
	sh.mu.RLock()
	for _, seg := range sh.segs {
		if seg == nil || seg == sh.act || seg.zones == nil {
			continue
		}
		zoned++
		if z, ok := seg.zones[tsID]; ok && z.Min < float64(f.now-2_000_000) {
			t.Errorf("Ts zone [%v,%v] is not a Ts range", z.Min, z.Max)
		}
		if z, ok := seg.zones[otherID]; ok && z.Max > otherBase {
			t.Errorf("Other zone [%v,%v] is not an Other range", z.Min, z.Max)
		}
	}
	sh.mu.RUnlock()
	if zoned == 0 {
		t.Fatal("no sealed segment carries a zone map after the reconfigured reopen")
	}
	f.check(t, a, "reconfigured")
}

// TestIndexedDNFPlanZonePrunes is the disjunctive index path's half: a window is skipped when
// its zone map rules out every disjunct (or a required conjunct), and the union still returns
// every match.
func TestIndexedDNFPlanZonePrunes(t *testing.T) {
	t.Parallel()
	f, a := newLateIndexArchive(t)
	defer a.Close()
	c := a.c
	for _, tc := range []struct {
		expr   string
		pruned bool
	}{
		// Each disjunct is ruled out by a different attribute's zone in the old segments.
		{fmt.Sprintf(`Ts >= %d || Other <= %d`, f.now+1000+int64(f.old+f.new-5), otherBase-f.old-f.new+20), true},
		// A required conjunct prunes on its own.
		{fmt.Sprintf(`Ts >= %d && (Other == %d || Other == %d)`, f.now, otherBase-f.old, otherBase-f.old-2), true},
		// One disjunct matches everywhere: nothing may be pruned.
		{fmt.Sprintf(`Ts >= %d || Other > 0`, f.now), false},
	} {
		q := mustQuery(t, tc.expr)
		plan := q.ProbePlan()
		groups, prunable := c.planIndexGroups(plan)
		if !prunable {
			t.Fatalf("%s: not index-prunable", tc.expr)
		}
		var st ScanStats
		var got []int64
		qp := c.newQueryPlan(q, nil, &st)
		for _, sh := range c.shards {
			c.scanShardIndexedGroups(sh, groups, plan, qp, func(w []byte, dict *segDictHandle) bool {
				ad, err := c.decodeWireDict(dict, w)
				if err != nil {
					t.Fatal(err)
				}
				n, _ := classad.FromAST(ad).EvaluateAttrInt("N")
				got = append(got, n)
				return true
			})
		}
		slices.Sort(got)
		if want := plainMatches(t, a, q); !slices.Equal(got, want) {
			t.Errorf("%s: %d rows, want %d", tc.expr, len(got), len(want))
		}
		if tc.pruned != (st.SegmentsPruned > 0) {
			t.Errorf("%s: pruned %d of %d segments, want pruning=%v", tc.expr, st.SegmentsPruned, st.SegmentsTotal, tc.pruned)
		}
	}
}
