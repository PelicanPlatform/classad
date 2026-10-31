package db

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// Differential test for computed values: schema fields whose cells are mostly an expression over the
// record's own attributes, which columnarizing computes, mixed in the same blocks with literal cells and
// with cells no record can compute. Every aggregate answered by whichever fast path takes it must equal
// the brute-force answer over every record, on an archive and on a mutable table, after the first
// columnarization and after the re-columnarization that adopting group schemas performs.

// computedHistoryAd is a history-shaped record. MemoryUsage is `((ResidentSetSize + 1023) / 1024)`
// in most records and a literal in about one in ten; ResidentSetSize is sometimes absent and sometimes
// itself an expression. One record in excEvery holds a MemoryUsage no record can compute (it names
// TARGET), which the readers must evaluate on its own; with cycles, a rare one is a cycle. Tag holds two different string expressions and a
// literal, so blocks hold several expression texts for one field; BigNum is past 2^53.
func computedHistoryAd(rng *rand.Rand, i, excEvery int, exprs, cycles bool) string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	cid := 1000 + i/10
	t0 := 1700000000 + i*7
	owner := fmt.Sprintf("user%02d", rng.Intn(30))
	w("ClusterId = %d", cid)
	w("ProcId = %d", i%10)
	w("Owner = %q", owner)
	w("QDate = %d", t0-500)
	w("EnteredHistoryTime = %d", t0+5)
	w("JobStatus = %d", []int{3, 4, 4, 4}[rng.Intn(4)])
	w("JobUniverse = %d", []int{5, 5, 7}[rng.Intn(3)])
	w("Requirements = (TARGET.Memory >= RequestMemory)")
	w("RequestMemory = %d", 1024*(1+rng.Intn(8)))
	switch rng.Intn(40) {
	case 0:
		// absent: MemoryUsage evaluates to UNDEFINED
	case 1:
		w("ResidentSetSize = RequestMemory * 1000")
	case 2:
		w("ResidentSetSize = %d.5", rng.Intn(1<<23)) // a real: MemoryUsage evaluates to a real
	default:
		w("ResidentSetSize = %d", rng.Intn(1<<23))
	}
	switch {
	case !exprs:
		w("MemoryUsage = %d", rng.Intn(8192))
	case excEvery > 0 && i%excEvery == excEvery/2:
		w("MemoryUsage = TARGET.Memory / 2")
	case cycles && i%301 == 5:
		w("MemoryUsage = MemoryUsage + 1") // a cycle: ERROR, which a SUM must propagate
	case rng.Intn(10) == 0:
		w("MemoryUsage = %d", rng.Intn(8192))
	default:
		w("MemoryUsage = ((ResidentSetSize + 1023) / 1024)")
	}
	switch r := rng.Intn(3); {
	case !exprs || r == 0:
		w("Tag = %q", fmt.Sprintf("t%d", rng.Intn(4)))
	case r == 1:
		w("Tag = toUpper(Owner)")
	default:
		w("Tag = strcat(Owner, \"@\", JobUniverse)")
	}
	if exprs && rng.Intn(2) == 0 {
		w("BigNum = ClusterId + 9007199254740993")
	} else {
		w("BigNum = %d", 9007199254740993+int64(cid)+int64(rng.Intn(3)))
	}
	return b.String()
}

var computedConstraints = []string{
	"true",
	"MemoryUsage > 4095",
	"MemoryUsage > 100 && EnteredHistoryTime > 1700003000",
	"MemoryUsage is undefined",
	"MemoryUsage isnt undefined",
	"(MemoryUsage ?: 0) == 0",
	"MemoryUsage <= 2 || JobStatus == 3",
	`Tag == "USER03"`,
	`Tag == "t1"`,
	"BigNum > 9007199254741500",
	"JobStatus == 4 && MemoryUsage >= 1024",
}

func computedShapes() []struct {
	groupCols []GroupCol
	aggs      []AggSpec
} {
	count := []AggSpec{{Func: AggCount, Arg: "*"}}
	mem := []AggSpec{{Func: AggMax, Arg: "MemoryUsage"}, {Func: AggMin, Arg: "MemoryUsage"},
		{Func: AggSum, Arg: "MemoryUsage"}, {Func: AggCount, Arg: "MemoryUsage"}}
	return []struct {
		groupCols []GroupCol
		aggs      []AggSpec
	}{
		{nil, count},
		{nil, mem[:1]}, {nil, mem[1:2]}, {nil, mem[2:3]}, {nil, mem[3:4]},
		{nil, []AggSpec{{Func: AggMax, Arg: "BigNum"}}},
		{[]GroupCol{{Attr: "MemoryUsage", BucketWidth: 1024}}, count},
		{[]GroupCol{{Attr: "JobStatus"}}, mem},
		{[]GroupCol{{Attr: "Tag"}}, count},
	}
}

// refAggregateCols is refAggregate with bucketed group columns.
func refAggregateCols(t *testing.T, ads []*classad.ClassAd, constraint string, groupCols []GroupCol, aggs []AggSpec) []AggRow {
	t.Helper()
	expr, err := classad.ParseExpr(constraint)
	if err != nil {
		t.Fatalf("%s: %v", constraint, err)
	}
	attrs, groupCol, aggCol := AggProjection(groupCols, aggs)
	seq := func(yield func([]classad.Value) bool) {
		for _, ad := range ads {
			if b, berr := ad.EvaluateExprWithTarget(expr, nil).BoolValue(); berr != nil || !b {
				continue
			}
			vals := make([]classad.Value, len(attrs))
			for i, a := range attrs {
				vals[i] = ad.EvaluateAttr(a)
			}
			if !yield(vals) {
				return
			}
		}
	}
	rows, err := AggregateValues(seq, attrs, groupCols, aggs, groupCol, aggCol, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// refTopK is the order values of the k largest numeric MemoryUsage among the matching records.
func refTopK(t *testing.T, ads []*classad.ClassAd, constraint string, k int) []string {
	t.Helper()
	expr, err := classad.ParseExpr(constraint)
	if err != nil {
		t.Fatal(err)
	}
	var vals []float64
	for _, ad := range ads {
		if b, berr := ad.EvaluateExprWithTarget(expr, nil).BoolValue(); berr != nil || !b {
			continue
		}
		if f, ok := numberOf(ad.EvaluateAttr("MemoryUsage")); ok {
			vals = append(vals, f)
		}
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(vals)))
	if len(vals) > k {
		vals = vals[:k]
	}
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = trimFloat(v)
	}
	return out
}

type computedTable struct {
	agg  func(constraint string, groupCols []GroupCol, aggs []AggSpec, stats *collections.ScanStats) ([]AggRow, error)
	topK func(constraint string, attrs []string, orderAttr string, desc bool, k int) ([][]classad.Value, error)
	all  func() []*classad.ClassAd
}

func checkComputedTable(t *testing.T, label string, tb computedTable, cycles bool) {
	t.Helper()
	ads := tb.all()
	for _, c := range computedConstraints {
		for _, sh := range computedShapes() {
			want := refAggregateCols(t, ads, c, sh.groupCols, sh.aggs)
			got, err := tb.agg(c, sh.groupCols, sh.aggs, nil)
			if err != nil {
				t.Fatalf("%s: WHERE %s GROUP BY %v: %v", label, c, sh.groupCols, err)
			}
			if d := diffRows(got, want); d != "" {
				t.Errorf("%s: WHERE %s GROUP BY %v %v: %s", label, c, sh.groupCols, sh.aggs, d)
			}
		}
		rows, err := tb.topK(c, []string{"MemoryUsage"}, "MemoryUsage", true, 10)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(rows))
		for i, r := range rows {
			f, _ := numberOf(r[0])
			got[i] = trimFloat(f)
		}
		if want := refTopK(t, ads, c, 10); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: TopK(%s) by MemoryUsage = %v, want %v", label, c, got, want)
		}
	}
	if cycles {
		return // an ERROR value is the record scan's to aggregate, so nothing more is expected
	}
	// The aggregate over the computed field answers from the columns rather than a record scan.
	var st collections.ScanStats
	if _, err := tb.agg("true", nil, []AggSpec{{Func: AggMax, Arg: "MemoryUsage"}}, &st); err != nil {
		t.Fatal(err)
	}
	// An archive names the tier; the mutable ladder names none, and its record scan is the one rung
	// that counts reassembled records.
	if st.Tier != collections.TierColumnar && (st.Tier != collections.TierUnreported || st.RecordsReassembled != 0) {
		t.Errorf("%s: MAX(MemoryUsage) was answered by tier %d reassembling %d records, want the columns",
			label, st.Tier, st.RecordsReassembled)
	}
}

func TestComputedValuesMatchBruteForce(t *testing.T) {
	for _, tc := range []struct {
		name     string
		excEvery int
		archive  bool
		cycles   bool
	}{
		{"archive/0.1%", 1000, true, false},
		{"archive/5%", 20, true, false},
		{"archive/cycles", 20, true, true},
		{"mutable/0.1%", 1000, false, false},
		{"mutable/5%", 20, false, false},
		{"mutable/cycles", 20, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.archive {
				checkComputedArchive(t, tc.excEvery, tc.cycles)
			} else {
				checkComputedMutable(t, tc.excEvery, tc.cycles)
			}
		})
	}
}

func checkComputedArchive(t *testing.T, excEvery int, cycles bool) {
	dir := t.TempDir()
	cfg := ArchiveConfig{SegmentSize: 64 << 10, ZoneAttrs: []string{"EnteredHistoryTime"}, GroupStabilityRuns: 1}
	var cat *Catalog
	var h *ArchiveTable
	open := func() {
		var err error
		if cat, err = OpenCatalog(dir); err != nil {
			t.Fatal(err)
		}
		if h, err = cat.CreateArchiveTable("history", cfg); err != nil {
			t.Fatal(err)
		}
	}
	open()
	t.Cleanup(func() { cat.Close() })
	rng := rand.New(rand.NewSource(int64(excEvery)))
	n := 0
	appendN := func(k int, exprs bool) {
		for j := 0; j < k; j++ {
			if err := h.AppendOld(computedHistoryAd(rng, n, excEvery, exprs, cycles)); err != nil {
				t.Fatal(err)
			}
			n++
		}
	}
	tb := computedTable{
		agg: func(c string, g []GroupCol, a []AggSpec, st *collections.ScanStats) ([]AggRow, error) {
			return h.AggregateColsStats(c, g, a, st)
		},
		topK: func(c string, attrs []string, o string, desc bool, k int) ([][]classad.Value, error) {
			return h.TopK(c, attrs, o, desc, k)
		},
		all: func() []*classad.ClassAd {
			seq, err := h.Query("true")
			if err != nil {
				t.Fatal(err)
			}
			var ads []*classad.ClassAd
			for ad := range seq {
				ads = append(ads, ad)
			}
			if len(ads) != h.Count() {
				t.Fatalf("scan returned %d of %d records", len(ads), h.Count())
			}
			return ads
		},
	}
	appendN(1500, true)
	h.MergePass(MergeOptions{})
	h.BuildAndEnableSchemaScan(2000, 32)
	h.GroupSchemas(2000, 0)
	cat.Close()
	open()
	// The open segment holds expressions too, as a live history table's does.
	appendN(200, true)
	checkComputedTable(t, "first columnarization", tb, cycles)
	h.MergePass(MergeOptions{})
	h.BuildAndEnableSchemaScan(2000, 32)
	h.GroupSchemas(2000, 0)
	h.BuildAndEnableSchemaScan(2000, 32)
	checkComputedTable(t, "re-columnarized", tb, cycles)
	if info := h.SchemaScanInfo(); info.CoveredSegments == 0 {
		t.Fatal("nothing was columnarized")
	}
}

func checkComputedMutable(t *testing.T, excEvery int, cycles bool) {
	dir := t.TempDir()
	open := func() *DB {
		d, err := OpenConfig(Config{Dir: dir, SegmentSize: 8 << 10}) // small: the table spreads over 16 shards
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := open()
	rng := rand.New(rand.NewSource(int64(excEvery)))
	put := func(from, to int) {
		tx := d.Begin()
		for i := from; i < to; i++ {
			ad, err := classad.ParseOld(computedHistoryAd(rng, i, excEvery, true, cycles))
			if err != nil {
				t.Fatal(err)
			}
			tx.NewClassAd("k"+strconv.Itoa(i), ad)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	put(0, 1500)
	if !d.EnableSchemaScan(2000, 8) {
		t.Fatal("no schema derived")
	}
	d.Close()
	d = open()
	t.Cleanup(func() { d.Close() })
	d.EnableSchemaScan(2000, 8)
	put(1500, 1700)
	tb := computedTable{
		agg: func(c string, g []GroupCol, a []AggSpec, st *collections.ScanStats) ([]AggRow, error) {
			return d.AggregateColsStats(c, g, a, st, nil)
		},
		topK: d.TopK,
		all: func() []*classad.ClassAd {
			seq, err := d.Query("true")
			if err != nil {
				t.Fatal(err)
			}
			var ads []*classad.ClassAd
			for ad := range seq {
				ads = append(ads, ad)
			}
			return ads
		},
	}
	checkComputedTable(t, "mutable", tb, cycles)
	if info := d.SchemaScanInfo(); info.CoveredSegments == 0 {
		t.Fatal("nothing was columnarized")
	}
}
