package collections

// A value index maps each value to the bitmap of records holding it, which makes every key both
// a map key and a sort key. NaN is the one float64 that can be neither: it does not equal itself,
// so Go's map inserts a fresh entry for each NaN posted and then finds none of them back
// (post[NaN] is always nil, which nil-dereferenced the stats pass and took the process down on an
// ordinary Append), and sort.Float64s gives it no position, so a range probe's boundary search
// could never reach it either. These tests pin where a NaN goes instead -- the exception set,
// which every candidate set unions in -- and check that it stays findable.

import (
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// nanIdxRecords spreads over two shards, so each has its own index over the same mix. Sealed
// segments and their sidecars are covered separately, by the archive test at the bottom.
const nanIdxRecords = 4000

// nanIdxEvery makes roughly 1 in 37 records carry a NaN -- frequent enough that every segment
// holds some, rare enough that the rest of the column still indexes normally.
const nanIdxEvery = 37

// buildNaNRateCollection fills a Rate-value-indexed collection where one record in nanIdxEvery
// carries a NaN. literal selects how the NaN is stored: as a stored real literal
// (InsertAttrFloat, which the encoder may put in a column slot) or as the expression 0.0/0.0,
// which is never a literal and so reaches the indexer by a different path. It returns the Ids
// that carry a NaN.
func buildNaNRateCollection(t *testing.T, literal bool) (*Collection, []int64) {
	t.Helper()
	c := New(Options{Shards: 2, ValueAttrs: []string{"Rate"}})
	var nanIDs []int64
	putFloat := func(i int, f float64) {
		ad := classad.New()
		ad.InsertAttr("Id", int64(i))
		ad.InsertAttrFloat("Rate", f)
		c.Put([]byte(fmt.Sprintf("k%d", i)), ad)
	}
	for i := 0; i < nanIdxRecords; i++ {
		switch {
		case i%nanIdxEvery == 0:
			nanIDs = append(nanIDs, int64(i))
			if literal {
				putFloat(i, math.NaN())
				continue
			}
			// An expression is never a literal, so this reaches the indexer by the
			// computed-expression path instead of the real-literal one.
			c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[ Id=%d; Rate = 0.0/0.0 ]`, i)))
		// A handful of stored infinities: +/-Inf is a totally ordered float64 and must keep
		// indexing normally rather than being swept in with NaN.
		case i%997 == 1:
			putFloat(i, math.Inf(1))
		case i%997 == 2:
			putFloat(i, math.Inf(-1))
		default:
			c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[ Id=%d; Rate = %d.5 ]`, i, i%100)))
		}
	}
	c.Reindex()
	return c, nanIDs
}

// valIndexSummary totals one value index across every segment of a collection: the distinct keys
// posted, the exception count, and whether any key or range bound is a NaN. It reads the postings
// directly because the point of the test is the index's internal state -- a test that passed
// because indexing quietly declined would prove nothing.
type valIndexSummary struct {
	segments  int
	ndv       uint64
	indexable uint64
	exc       uint64
	nanKeys   int
	unsorted  int
	nanBound  int
	noHist    int
}

func summarizeValIndex(t *testing.T, c *Collection) valIndexSummary {
	t.Helper()
	var sum valIndexSummary
	for _, sh := range c.shards {
		sh.mu.RLock()
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil {
				continue
			}
			si := seg.idx.Load()
			if si == nil {
				continue
			}
			for _, vp := range si.val {
				sum.segments++
				sum.ndv += vp.stats.ndv
				sum.exc += vp.stats.exc
				sum.indexable += vp.stats.covered - vp.stats.exc
				for _, k := range vp.sortedKeys {
					if math.IsNaN(k) {
						sum.nanKeys++
					}
					// Every key must be readable back out of the map it was posted into.
					if vp.post[k] == nil {
						t.Errorf("key %v has no bitmap: posted under a key that cannot be looked up", k)
					}
				}
				if !sort.Float64sAreSorted(vp.sortedKeys) {
					sum.unsorted++
				}
				if vp.stats.hasRange && (math.IsNaN(vp.stats.min) || math.IsNaN(vp.stats.max)) {
					sum.nanBound++
				}
				if len(vp.sortedKeys) > 0 && vp.stats.hist == nil {
					sum.noHist++
				}
			}
		}
	}
	return sum
}

// TestValueIndexNaNBuildsAndExcepts: building a value index over a NaN-bearing column must not
// crash, must actually build (postings, sorted keys, a range and a histogram), and must put every
// NaN-valued record in the exception set rather than in a posting no lookup could reach.
func TestValueIndexNaNBuildsAndExcepts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		literal bool
	}{
		{"StoredLiteral", true},
		{"FromArithmetic", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, nanIDs := buildNaNRateCollection(t, tc.literal)
			sum := summarizeValIndex(t, c)
			// The index must exist and be populated -- otherwise everything below is vacuous.
			if sum.segments == 0 || sum.ndv == 0 {
				t.Fatalf("no value index built: %+v", sum)
			}
			if sum.nanKeys != 0 {
				t.Errorf("%d NaN keys in sortedKeys: a NaN cannot be a map key or a sort key", sum.nanKeys)
			}
			if sum.unsorted != 0 {
				t.Errorf("%d segments have unsorted keys (a NaN has no position in an ordering)", sum.unsorted)
			}
			if sum.nanBound != 0 {
				t.Errorf("%d segments have a NaN min/max: every comparison against it is false, so the "+
					"segment-skip test would rule the segment out and drop real matches", sum.nanBound)
			}
			if sum.noHist != 0 {
				t.Errorf("%d segments have keys but no histogram", sum.noHist)
			}
			// Each NaN record is covered by the index as an exception, not as a posting. The
			// column holds no other non-numeric value, so exc is exactly the NaN records.
			if sum.exc != uint64(len(nanIDs)) {
				t.Errorf("exception count %d, want %d (one per NaN record)", sum.exc, len(nanIDs))
			}
			if want := uint64(nanIdxRecords - len(nanIDs)); sum.indexable != want {
				t.Errorf("indexable %d, want %d (every non-NaN record, infinities included)", sum.indexable, want)
			}
			// ndv counts DISTINCT values; a NaN posting inflated it by one per NaN record,
			// because each one landed in its own map entry.
			if sum.ndv > sum.indexable {
				t.Errorf("ndv %d exceeds the indexable record count %d", sum.ndv, sum.indexable)
			}
		})
	}
}

// TestValueIndexNaNInfinitiesStillIndexed: +/-Inf orders like any other float64, so it stays a
// posting and bounds the segment range. Sweeping it in with NaN would silently widen the
// exception set and forfeit every segment skip on the column.
func TestValueIndexNaNInfinitiesStillIndexed(t *testing.T) {
	t.Parallel()
	c, _ := buildNaNRateCollection(t, true)
	posInf, negInf := false, false
	for _, sh := range c.shards {
		sh.mu.RLock()
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil {
				continue
			}
			si := seg.idx.Load()
			if si == nil {
				continue
			}
			for _, vp := range si.val {
				for _, k := range vp.sortedKeys {
					if math.IsInf(k, 1) {
						posInf = true
					}
					if math.IsInf(k, -1) {
						negInf = true
					}
				}
			}
		}
	}
	if !posInf || !negInf {
		t.Errorf("+Inf posted=%v, -Inf posted=%v; both are ordered values and belong in the postings", posInf, negInf)
	}
}

// nanIdxOracle evaluates expr against every record's ad, independently of the store's scan and
// index, and returns the matching Ids ascending. This is the answer the index must reproduce.
func nanIdxOracle(t *testing.T, c *Collection, expr string) []int64 {
	t.Helper()
	e, err := classad.ParseExpr(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	var ids []int64
	for ad := range c.Scan() {
		b, err := ad.EvaluateExprWithTarget(e, nil).BoolValue()
		if err != nil || !b {
			continue
		}
		id, err := ad.EvaluateAttr("Id").IntValue()
		if err != nil {
			t.Fatalf("Id: %v", err)
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// nanIdxQuery runs the constraint through the indexed query path and returns the matching Ids
// ascending, plus the scan's stats so the test can confirm the index actually served it.
func nanIdxQuery(t *testing.T, c *Collection, expr string) ([]int64, ScanStats) {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatalf("vm.Parse %q: %v", expr, err)
	}
	var st ScanStats
	var ids []int64
	for vals := range c.QueryProjectStats(q, []string{"Id"}, &st) {
		id, err := vals[0].IntValue()
		if err != nil {
			t.Fatalf("Id: %v", err)
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, st
}

// TestValueIndexNaNProbesMatchEvaluation: with a NaN in the column, every probe the value index
// serves must still return exactly what evaluating the expression against each ad returns. A
// candidate set may be a superset (the caller re-verifies), but a record the index leaves out is
// a silent wrong answer -- which is what a posting under an unreachable NaN key would have been.
func TestValueIndexNaNProbesMatchEvaluation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		literal bool
	}{
		{"StoredLiteral", true},
		{"FromArithmetic", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, nanIDs := buildNaNRateCollection(t, tc.literal)
			if len(nanIDs) == 0 {
				t.Fatal("no NaN records")
			}
			indexed := 0
			for _, expr := range []string{
				"Rate > 50.0",
				"Rate >= 50.5",
				"Rate < 10.0",
				"Rate <= 0.5",
				"Rate == 37.5",
				"Rate != 37.5",
				"Rate > 20.0 && Rate < 30.0",
				"Rate >= -1.0e300 && Rate <= 1.0e300",
				"Rate > 1.0e300",
				"isUndefined(Rate)",
			} {
				got, st := nanIdxQuery(t, c, expr)
				want := nanIdxOracle(t, c, expr)
				if len(got) != len(want) {
					t.Errorf("%s: %d rows, evaluation says %d", expr, len(got), len(want))
					continue
				}
				for i := range got {
					if got[i] != want[i] {
						t.Errorf("%s: row %d is Id %d, evaluation says %d", expr, i, got[i], want[i])
						break
					}
				}
				if st.SegmentsIndexed > 0 {
					indexed++
				}
			}
			if indexed == 0 {
				t.Error("no probe was served from the index; the test would pass on an unindexed scan")
			}
			// Findability, stated both ways. An ordering comparison against a NaN is false, so
			// no NaN record may come back from `Rate > 50.0`; a negated equality against one is
			// TRUE, so every NaN record must come back from `Rate != 37.5` -- and it can only do
			// that if the index kept it a candidate. A posting under an unreachable NaN key
			// would have dropped them all from the second, silently.
			absent, _ := nanIdxQuery(t, c, "Rate > 50.0")
			present, _ := nanIdxQuery(t, c, "Rate != 37.5")
			has := func(ids []int64, id int64) bool {
				i := sort.Search(len(ids), func(j int) bool { return ids[j] >= id })
				return i < len(ids) && ids[i] == id
			}
			for _, id := range nanIDs {
				if has(absent, id) {
					t.Errorf("NaN record Id %d matched `Rate > 50.0`", id)
					break
				}
				if !has(present, id) {
					t.Errorf("NaN record Id %d is missing from `Rate != 37.5`: indexed but unfindable", id)
					break
				}
			}
		})
	}
}

// TestValueIndexNaNEqualityFindsNonNaNRows: the sharpest form of the unreachability failure. A NaN
// that reached the min/max fold first left min==max==NaN with an empty exception set, and every
// comparison against a NaN is false -- so the equality skip test ("is the probe value inside
// [min,max]?") answered no for EVERY value and skipped the whole indexed prefix. The rows are
// there, the index says the segment holds none, and the query returns nothing.
func TestValueIndexNaNEqualityFindsNonNaNRows(t *testing.T) {
	t.Parallel()
	// One NaN first, then many ordinary values: if the NaN is folded into the range at all, it
	// is the first key seen and poisons min and max for good.
	c := New(Options{Shards: 1, ValueAttrs: []string{"Rate"}})
	nan := classad.New()
	nan.InsertAttr("Id", int64(0))
	nan.InsertAttrFloat("Rate", math.NaN())
	c.Put([]byte("k0"), nan)
	for i := 1; i < 500; i++ {
		c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[ Id=%d; Rate = %d.5 ]`, i, i%10)))
	}
	c.Reindex()
	got, st := nanIdxQuery(t, c, "Rate == 3.5")
	want := nanIdxOracle(t, c, "Rate == 3.5")
	if len(want) == 0 {
		t.Fatal("oracle found no rows; the fixture is wrong")
	}
	if len(got) != len(want) {
		t.Fatalf("Rate == 3.5 returned %d rows, evaluation says %d (%+v)", len(got), len(want), st)
	}
	if st.SegmentsIndexed == 0 {
		t.Error("the index did not serve the query, so the skip path was never exercised")
	}
}

// TestValueIndexNaNThresholdProbe: the mirror of the stored-NaN case. A NaN can be the probe's
// value too -- `Rate >= 0.0/0.0` folds to a probe whose threshold is a NaN -- and a NaN is no
// more usable as a SEARCH key than as a stored one. The histogram's boundary search matched no
// bucket, indexed one past the last, and took the process down from an ordinary query. The answer
// is zero rows (no value compares true against a NaN), which the index must reach without
// crashing and without disagreeing with evaluation.
func TestValueIndexNaNThresholdProbe(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1, ValueAttrs: []string{"Rate", "Other"}})
	for i := 0; i < 2000; i++ {
		c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[ Id=%d; Rate = %d.5; Other = %d ]`, i, i%100, i)))
	}
	// A NaN-valued record too, so the exception set is non-empty while the threshold is a NaN.
	nan := classad.New()
	nan.InsertAttr("Id", int64(-1))
	nan.InsertAttrFloat("Rate", math.NaN())
	nan.InsertAttr("Other", int64(0))
	c.Put([]byte("knan"), nan)
	c.Reindex()
	indexed := 0
	for _, expr := range []string{
		// Two probes, so the planner orders them by selectivity -- which is what reads the
		// histogram with the NaN threshold.
		"Rate >= 0.0/0.0 && Other > 10",
		"Rate > 0.0/0.0 && Other > 10",
		"Rate < 0.0/0.0 && Other > 10",
		"Rate <= 0.0/0.0 && Other > 10",
		"Rate > 0.0/0.0 && Rate < 100.0",
		"Rate >= 0.0/0.0",
	} {
		got, st := nanIdxQuery(t, c, expr)
		want := nanIdxOracle(t, c, expr)
		if len(got) != 0 || len(want) != 0 {
			t.Errorf("%s: %d rows (evaluation says %d), want 0 -- nothing compares true against a NaN",
				expr, len(got), len(want))
		}
		if st.SegmentsIndexed > 0 {
			indexed++
		}
	}
	if indexed == 0 {
		t.Error("no probe was served from the index; the histogram path was never reached")
	}
}

// TestArchiveValueIndexNaNSidecar: an archive writes each sealed segment's index to a sidecar,
// and the writer reads every posting back out of the map by its key too (emit(post[k])), so a NaN
// key nil-dereferenced there as well. Appending NaN-bearing history must build those sidecars and
// query correctly through them.
func TestArchiveValueIndexNaNSidecar(t *testing.T) {
	t.Parallel()
	a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir(), SegmentSize: 1 << 12, ValueAttrs: []string{"Rate"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const n = 1200
	nanRows, wantHigh := 0, 0
	for i := 0; i < n; i++ {
		ad := classad.New()
		ad.InsertAttr("N", int64(i))
		if i%7 == 0 {
			ad.InsertAttrFloat("Rate", math.NaN())
			nanRows++
		} else {
			ad.InsertAttrFloat("Rate", float64(i%100)+0.5)
			if i%100 >= 50 {
				wantHigh++
			}
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	sealed, indexed := sealedIndexed(a)
	if sealed < 2 {
		t.Fatalf("%d sealed segments; the sidecar writer was never exercised", sealed)
	}
	if indexed != sealed {
		t.Errorf("%d/%d sealed segments carry a sidecar index (want all)", indexed, sealed)
	}
	q, err := vm.Parse("Rate > 50.0")
	if err != nil {
		t.Fatal(err)
	}
	var st ScanStats
	got := 0
	for range a.QueryRawProjectedStats(q, nil, false, false, &st) {
		got++
	}
	if got != wantHigh {
		t.Errorf("Rate > 50.0 returned %d rows, want %d (%d of them NaN-valued and matching nothing)",
			got, wantHigh, nanRows)
	}
	if st.SegmentsIndexed == 0 {
		t.Error("no segment answered from its sidecar index")
	}
}
