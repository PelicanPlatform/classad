package collections

import (
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// A columnar block whose cold numeric stream will not decode.
//
// Every cold numeric column of a block lives in that one compressed stream, so losing it loses every
// cold field of every record in the block at once. The aggregate scans used to treat that as "this
// block has nothing to say" and move on, which drops the block's records from SUM/COUNT/MIN/MAX, the
// TOP-K cutoff and the counts, and -- in the predicate narrowing -- let every record through a
// predicate nobody tested. The answer must instead be the one the row path gives over the same store.

// coldStreamFixture builds a collection whose JobStatus is the only HOT numeric column, so ClusterId,
// ProcId and RequestMemory are read from the cold numeric stream. persistent columnarizes the segments,
// so the columns are the records' only copy of those values; otherwise the block is an accelerator
// beside intact records.
func coldStreamFixture(t *testing.T, persistent bool) *Collection {
	t.Helper()
	var c *Collection
	if persistent {
		var err error
		c, err = Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
		if err != nil {
			t.Fatal(err)
		}
	} else {
		c = New(Options{Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
	}
	for i := 0; i < 6000; i++ {
		src := fmt.Sprintf("ClusterId = %d\nJobStatus = %d\nRequestMemory = %d\nOwner = \"u%d\"",
			i, 1+i%5, 1024+(i%32)*512, i%7)
		switch {
		case i%997 == 3:
			src += "\nProcId = 9000000000" // too wide for its slot: escapes to the cold tail
		case i%991 == 5:
			src += "\nProcId = 7.5" // a real in an int column: escapes on type
		case i%983 == 7:
			// no ProcId at all
		default:
			src += fmt.Sprintf("\nProcId = %d", i%1000)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	s, _, ok := c.deriveSchema(4096, 0)
	if !ok {
		t.Skip("no schema could be derived")
	}
	js, ok := c.intern.LookupID("JobStatus")
	if !ok {
		t.Fatal("JobStatus not interned")
	}
	idx, ok := s.byID[js]
	if !ok {
		t.Fatal("JobStatus not in the schema")
	}
	hot := []int{idx}
	if persistent {
		if !c.installSchemaScan(s, hot) {
			t.Skip("schema scan did not enable")
		}
		if c.columnarizeSealed() == 0 {
			t.Skip("no sealed segment was columnarized")
		}
	} else {
		c.EnableSchemaScan(s, hot)
	}
	return c
}

// breakColdNumStream makes the cold numeric stream of one block undecodable and gives the block a
// fresh cache identity, so no decoded copy of the old stream answers for it. It returns how many
// records the block covers.
func breakColdNumStream(t *testing.T, c *Collection) int {
	t.Helper()
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			if cs == nil {
				continue
			}
			for _, blk := range cs.blocks {
				if len(blk.coldFields()) == 0 || len(blk.coldNumComp) == 0 {
					continue
				}
				if _, hot := blk.hotColStart(cs.schema().byID[mustIntern(t, c, "JobStatus")]); !hot {
					t.Fatal("fixture: JobStatus is not hot")
				}
				blk.coldNumComp = []byte("not a compressed stream")
				blk.id = colBlockSeq.Add(1)
				releaseWindows(wins)
				return blk.n
			}
		}
		releaseWindows(wins)
	}
	t.Fatal("no columnar block with a cold numeric stream")
	return 0
}

// coldQuery parses a constraint, with "" meaning no constraint (a nil query).
func coldQuery(t *testing.T, expr string) *vm.Query {
	t.Helper()
	if expr == "" {
		return nil
	}
	return mustQuery(t, expr)
}

var coldStreamStats = []struct{ where, attr string }{
	{"", "ProcId"},                        // cold aggregate, no predicate
	{"JobStatus == 4", "ProcId"},          // cold aggregate, hot predicate
	{"ProcId >= 500", "ProcId"},           // cold aggregate, its own predicate
	{"RequestMemory > 4096", "JobStatus"}, // hot aggregate, cold predicate
	{"RequestMemory > 4096", "ClusterId"}, // cold aggregate, cold predicate
	{"JobStatus >= 2 && ClusterId < 4000", "RequestMemory"},
}

func checkStats(t *testing.T, label string, got, want NumStats) {
	t.Helper()
	if got.N != want.N || got.IntSum != want.IntSum || got.AnyReal != want.AnyReal ||
		got.Min != want.Min || got.Max != want.Max || math.Abs(got.Sum-want.Sum) > 1e-6 {
		t.Errorf("%s: got n=%d intSum=%d sum=%v min=%v max=%v real=%v; row path n=%d intSum=%d sum=%v min=%v max=%v real=%v",
			label, got.N, got.IntSum, got.Sum, got.Min, got.Max, got.AnyReal,
			want.N, want.IntSum, want.Sum, want.Min, want.Max, want.AnyReal)
	}
}

// refTopK is the row path's k-th best order value and the number of matching rows with a numeric one.
func refTopK(t *testing.T, c *Collection, where, attr string, desc bool, k int) (float64, int) {
	t.Helper()
	expr := where
	if expr == "" {
		expr = "true"
	}
	var vals []float64
	for ad := range c.Query(coldQuery(t, expr)) {
		v := ad.EvaluateAttr(attr)
		if f, err := v.NumberValue(); err == nil && (v.IsInteger() || v.IsReal()) {
			vals = append(vals, f)
		}
	}
	sort.Float64s(vals)
	if desc {
		sort.Sort(sort.Reverse(sort.Float64Slice(vals)))
	}
	if len(vals) < k {
		return 0, len(vals)
	}
	return vals[k-1], len(vals)
}

// refGroups is the row path's GROUP BY group: count and integer sum of agg per group, plus how many of
// each group's records pass filter.
func refGroups(t *testing.T, c *Collection, where, group, agg, filter string) map[string][3]int64 {
	t.Helper()
	fq := coldQuery(t, filter)
	out := map[string][3]int64{}
	for ad := range c.Query(coldQuery(t, where)) {
		key := ad.EvaluateAttr(group).String()
		e := out[key]
		e[0]++
		if n, err := ad.EvaluateAttr(agg).IntValue(); err == nil {
			e[1] += n
		}
		if fq != nil {
			if fq.Matches(ad) {
				e[2]++
			}
		}
		out[key] = e
	}
	return out
}

func checkGroups(t *testing.T, c *Collection, label, where, group, agg, filter string, got []GroupStats, ok bool) {
	t.Helper()
	if !ok {
		t.Logf("%s: declined to the row path", label)
		return // the caller answers from the row path, which is the reference
	}
	want := refGroups(t, c, where, group, agg, filter)
	gotM := map[string][3]int64{}
	for _, g := range got {
		e := [3]int64{int64(g.Count), g.Stats[0].IntSum, 0}
		if len(g.CountOf) > 0 {
			e[2] = int64(g.CountOf[0])
		}
		gotM[g.Values[0].String()] = e
	}
	if len(gotM) != len(want) {
		t.Errorf("%s: %d groups, row path %d", label, len(gotM), len(want))
	}
	for k, w := range want {
		if filter == "" {
			w[2] = 0
		}
		if gotM[k] != w {
			t.Errorf("%s: group %s = [count sum filtered] %v, row path %v", label, k, gotM[k], w)
		}
	}
}

// TestColdStreamUnreadableAggregatesMatchRowPath breaks one block's cold numeric stream under an
// accelerator whose records are intact, so the row path still reads every value: the columnar
// aggregates must answer the broken block's records from those records rather than leave them out.
func TestColdStreamUnreadableAggregatesMatchRowPath(t *testing.T) {
	c := coldStreamFixture(t, false)
	defer c.Close()

	// The reference over the healthy store, which the broken store must still give: nothing about
	// the records changed.
	healthy := make([]NumStats, len(coldStreamStats))
	for i, tc := range coldStreamStats {
		healthy[i] = refStats(t, c, tc.where, tc.attr)
	}
	n := breakColdNumStream(t, c)
	t.Logf("broke the cold numeric stream of a %d-record block", n)

	for i, tc := range coldStreamStats {
		label := fmt.Sprintf("%s where %q", tc.attr, tc.where)
		want := refStats(t, c, tc.where, tc.attr)
		checkStats(t, label+" (row path, broken vs healthy)", want, healthy[i])
		// Served, not declined: the broken block has a per-block fallback, so it must not cost the
		// rest of the query its columns.
		got, served := c.NumStatsQuery(coldQuery(t, tc.where), tc.attr)
		if !served {
			t.Errorf("%s: declined", label)
			continue
		}
		checkStats(t, label, got, want)
	}

	// The single-field value scan CountQuery takes for a lone comparison.
	st := c.schemaScan.Load()
	for _, attr := range []string{"ProcId", "RequestMemory", "JobStatus"} {
		got, served := c.schemaScanCount(mustIntern(t, c, attr), st.cache, func(float64) bool { return true })
		want := refStats(t, c, "", attr).N
		if !served || got != want {
			t.Errorf("schemaScanCount(%s) = %d (served %v), row path %d", attr, got, served, want)
		}
	}

	for _, where := range []string{"RequestMemory > 4096", "JobStatus == 4", "RequestMemory > 4096 && JobStatus == 4",
		"ClusterId < 3000"} {
		want := 0
		for range c.Query(coldQuery(t, where)) {
			want++
		}
		if got, served := c.CountQuery(coldQuery(t, where)); !served || got != want {
			t.Errorf("COUNT where %q = %d (served %v), row path %d", where, got, served, want)
		}
		// The column count itself, which CountQuery would otherwise cover for by declining to the
		// vectorized tier.
		preds, ok := c.numPredsCT(coldQuery(t, where), st.schema)
		if !ok {
			t.Fatalf("%q: not a column-count shape", where)
		}
		if got, served := c.schemaScanCountMulti(preds, st.cache); !served || got != want {
			t.Errorf("column COUNT where %q = %d (served %v), row path %d", where, got, served, want)
		}
	}

	for _, tc := range []struct {
		where, attr string
		desc        bool
		k           int
	}{
		{"", "ClusterId", true, 10},
		{"", "ClusterId", false, 10},
		{"JobStatus == 4", "ClusterId", true, 5},
		{"RequestMemory > 4096", "JobStatus", false, 3},
		{"RequestMemory > 4096", "ClusterId", false, 7},
	} {
		label := fmt.Sprintf("TOP %d %s desc=%v where %q", tc.k, tc.attr, tc.desc, tc.where)
		var ss ScanStats
		th, seen, served := c.TopKOrderThreshold(coldQuery(t, tc.where), tc.attr, tc.desc, tc.k, &ss)
		if !served {
			t.Errorf("%s: declined", label)
			continue
		}
		wantTh, wantSeen := refTopK(t, c, tc.where, tc.attr, tc.desc, tc.k)
		if seen != wantSeen || th != wantTh {
			t.Errorf("%s: threshold=%v seen=%d, row path threshold=%v seen=%d", label, th, seen, wantTh, wantSeen)
		}
	}

	for _, tc := range []struct{ where, group, agg, filter string }{
		{"JobStatus >= 0", "Owner", "ProcId", ""},
		{"RequestMemory > 4096", "JobStatus", "JobStatus", ""},
		{"JobStatus >= 0", "JobStatus", "JobStatus", "RequestMemory > 4096"},
	} {
		label := fmt.Sprintf("GROUP BY %s SUM(%s) where %q filter %q", tc.group, tc.agg, tc.where, tc.filter)
		var got []GroupStats
		var ok bool
		if tc.filter == "" {
			got, ok = c.GroupStatsConstraint(tc.where, tc.group, []string{tc.agg})
		} else {
			got, ok = c.GroupStatsConstraintAggs(tc.where, []GroupSpec{{Attr: tc.group}},
				[]GroupAggCol{{Attr: tc.agg}}, []string{tc.filter})
		}
		checkGroups(t, c, label, tc.where, tc.group, tc.agg, tc.filter, got, ok)
	}
}

// TestColdStreamFallbackCounted pins that the records a broken block hands to the row read show up in
// the TOP-K cutoff's scan stats -- reassembled rather than column-decided -- and that an aggregate
// whose columns are all HOT, and so never reads the broken stream, does not fall back at all.
func TestColdStreamFallbackCounted(t *testing.T) {
	c := coldStreamFixture(t, false)
	defer c.Close()
	hotQ := coldQuery(t, "JobStatus == 4")
	cutoff := func(q *vm.Query, attr string) ScanStats {
		t.Helper()
		var ss ScanStats
		if _, _, ok := c.TopKOrderThreshold(q, attr, true, 10, &ss); !ok {
			t.Fatalf("cutoff on %s declined", attr)
		}
		return ss
	}
	cold0, hot0 := cutoff(nil, "ClusterId"), cutoff(hotQ, "JobStatus")
	n := breakColdNumStream(t, c)

	cold1 := cutoff(nil, "ClusterId")
	if cold1.RecordsReassembled != cold0.RecordsReassembled+n ||
		cold1.RecordsColumnDecided != cold0.RecordsColumnDecided-n {
		t.Errorf("cold cutoff: reassembled %d -> %d, column-decided %d -> %d; want the broken block's %d records moved",
			cold0.RecordsReassembled, cold1.RecordsReassembled, cold0.RecordsColumnDecided, cold1.RecordsColumnDecided, n)
	}

	before := ColumnarFallbacks()
	hot1 := cutoff(hotQ, "JobStatus")
	if _, ok := c.NumStatsQuery(hotQ, "JobStatus"); !ok {
		t.Fatal("hot aggregate declined")
	}
	if hot1 != hot0 || ColumnarFallbacks() != before {
		t.Errorf("a hot-only aggregate fell back: stats %+v -> %+v, fallbacks +%d", hot0, hot1, ColumnarFallbacks()-before)
	}
}

// TestColdStreamUnreadableColumnarizedAgreesWithRowPath breaks the stream in a COLUMNARIZED segment,
// where the columns are the records' only copy of their schema'd values: the row path cannot read
// the block's records either, skips them and counts each refusal. The aggregates must give that same
// answer and count the same refusals, not drop the block without a trace.
func TestColdStreamUnreadableColumnarizedAgreesWithRowPath(t *testing.T) {
	c := coldStreamFixture(t, true)
	defer c.Close()
	healthy := refStats(t, c, "", "ProcId")
	n := breakColdNumStream(t, c)

	for _, tc := range coldStreamStats {
		if tc.attr == "JobStatus" {
			// A hot aggregate reads nothing from the broken stream, so it still sees the block's
			// values the row path can no longer reassemble; that is not the comparison here.
			continue
		}
		label := fmt.Sprintf("%s where %q", tc.attr, tc.where)
		want := refStats(t, c, tc.where, tc.attr)
		before := readSkipTotal()
		got, served := c.NumStatsQuery(coldQuery(t, tc.where), tc.attr)
		if !served {
			continue
		}
		checkStats(t, label, got, want)
		if readSkipTotal() == before {
			t.Errorf("%s: answered without counting the block's unreadable records", label)
		}
	}
	if got := refStats(t, c, "", "ProcId"); got.N >= healthy.N {
		t.Errorf("row path still reads all %d ProcIds; the broken %d-record block should be unreadable", got.N, n)
	}

	before := readSkipTotal()
	th, seen, ok := c.TopKOrderThreshold(nil, "ClusterId", false, 10, nil)
	wantTh, wantSeen := refTopK(t, c, "", "ClusterId", false, 10)
	if ok && (th != wantTh || seen != wantSeen) {
		t.Errorf("TOP 10 ClusterId: threshold=%v seen=%d, row path threshold=%v seen=%d", th, seen, wantTh, wantSeen)
	}
	if ok && readSkipTotal() == before {
		t.Error("TOP-K cutoff answered without counting the block's unreadable records")
	}
}
