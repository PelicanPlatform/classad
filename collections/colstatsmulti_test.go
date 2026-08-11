package collections

import (
	"fmt"
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// MIN/MAX/SUM/AVG/COUNT(attr) with a predicate on OTHER columns.
//
// The scan mechanics are the count scan's and are already covered; what needs testing here is that
// the VALUES and their TYPES survive -- SUM stays integral until a real appears, an escaped value's
// kind comes from its stored node rather than from the schema field it did not fit, and a boolean is
// flagged so the caller can decline.

// statsFixture builds records where the aggregated attribute and the predicated ones are DIFFERENT,
// and where the aggregated attribute has exceptional values: one too wide for its slot (escapes and is
// the true MAX), one stored as a real (escapes on type and makes SUM non-integral), one missing.
func statsFixture(tb testing.TB, n int) *Collection {
	tb.Helper()
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	for i := 0; i < n; i++ {
		procid := fmt.Sprintf("%d", i%1000)
		switch {
		case i%997 == 3:
			procid = "9000000000" // too wide for the fitted slot: escapes, and is the max
		case i%991 == 5:
			procid = "7.5" // a real: escapes on type, so SUM must promote
		case i%983 == 7:
			procid = "" // missing: contributes to nothing
		}
		src := fmt.Sprintf("ClusterId = %d\nJobStatus = %d\nRequestMemory = %d\nRequestCpus = %d\nOwner = \"u%d\"",
			i, 1+i%5, 1024+(i%32)*512, 1+i%8, i%32)
		if procid != "" {
			src += "\nProcId = " + procid
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(tb, src)); err != nil {
			tb.Fatal(err)
		}
	}
	for _, e := range []string{"ProcId >= 0", "JobStatus >= 0", "RequestMemory >= 0", "ClusterId >= 0"} {
		q, err := vm.Parse(e)
		if err != nil {
			tb.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			for range c.Query(q) {
			}
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		tb.Skip("no sealed segments")
	}
	return c
}

// refStats computes the aggregate independently, by iterating the matching ads and reading the
// attribute through the ordinary evaluator -- not through any columnar code.
func refStats(t *testing.T, c *Collection, where, attr string) NumStats {
	t.Helper()
	// NumStatsQuery takes a nil query to mean every record; Collection.Query does not, so the
	// reference uses an explicit match-all.
	expr := where
	if expr == "" {
		expr = "true"
	}
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	out := NumStats{Min: math.Inf(1), Max: math.Inf(-1)}
	for ad := range c.Query(q) {
		v := ad.EvaluateAttr(attr)
		var f float64
		var iv int64
		switch {
		case v.IsInteger():
			iv, _ = v.IntValue()
			f = float64(iv)
			out.IntSum += iv
		case v.IsReal():
			f, _ = v.RealValue()
			out.AnyReal = true
		default:
			continue
		}
		out.N++
		out.Sum += f
		if f < out.Min {
			out.Min = f
		}
		if f > out.Max {
			out.Max = f
		}
	}
	if out.N == 0 {
		out.Min, out.Max = 0, 0
	}
	return out
}

var statsCases = []struct{ where, attr string }{
	{"", "ProcId"},                                       // unconstrained
	{"ProcId >= 5", "ProcId"},                            // predicate on the SAME attr (old behaviour)
	{"JobStatus == 4", "ProcId"},                         // predicate on a DIFFERENT attr
	{"JobStatus == 4 && RequestMemory > 2048", "ProcId"}, // multi-field
	{"RequestCpus >= 4 && RequestMemory > 4096 && ProcId < 500", "ProcId"}, // three, one on the agg attr
	{"ClusterId >= 1000 && ClusterId < 1100", "RequestMemory"},             // clustered predicate, other agg
	{"JobStatus == 99", "ProcId"},                                          // nothing matches
}

// TestStatsMultiFieldMatchesReference is the equivalence gate, field by field so a promotion bug
// cannot hide behind a matching Sum.
func TestStatsMultiFieldMatchesReference(t *testing.T) {
	c := statsFixture(t, 5000)
	defer c.Close()
	for _, tc := range statsCases {
		var q *vm.Query
		if tc.where != "" {
			var err error
			q, err = vm.Parse(tc.where)
			if err != nil {
				t.Fatal(err)
			}
		}
		got, served := c.NumStatsQuery(q, tc.attr)
		if !served {
			t.Errorf("%s where %q: declined", tc.attr, tc.where)
			continue
		}
		want := refStats(t, c, tc.where, tc.attr)
		if got.N != want.N {
			t.Errorf("%s where %q: N = %d, want %d", tc.attr, tc.where, got.N, want.N)
		}
		if got.IntSum != want.IntSum {
			t.Errorf("%s where %q: IntSum = %d, want %d", tc.attr, tc.where, got.IntSum, want.IntSum)
		}
		if got.AnyReal != want.AnyReal {
			t.Errorf("%s where %q: AnyReal = %v, want %v", tc.attr, tc.where, got.AnyReal, want.AnyReal)
		}
		if got.Min != want.Min || got.Max != want.Max {
			t.Errorf("%s where %q: [min,max] = [%v,%v], want [%v,%v]",
				tc.attr, tc.where, got.Min, got.Max, want.Min, want.Max)
		}
		if math.Abs(got.Sum-want.Sum) > 1e-6 {
			t.Errorf("%s where %q: Sum = %v, want %v", tc.attr, tc.where, got.Sum, want.Sum)
		}
		t.Logf("%-12s where %-56q n=%-6d max=%-12v intSum=%-14d anyReal=%v",
			tc.attr, tc.where, got.N, got.Max, got.IntSum, got.AnyReal)
	}
}

// TestStatsMultiFieldSeesEscapedExtremes is the case a column-only read would get wrong: the true MAX
// is a value too wide for its slot, so it lives in the cold tail and is absent from the column.
func TestStatsMultiFieldSeesEscapedExtremes(t *testing.T) {
	c := statsFixture(t, 5000)
	defer c.Close()
	got, served := c.NumStatsQuery(nil, "ProcId")
	if !served {
		t.Fatal("declined")
	}
	if got.Max != 9000000000 {
		t.Errorf("MAX(ProcId) = %v, want 9000000000 (the escaped, too-wide value)", got.Max)
	}
	if !got.AnyReal {
		t.Error("AnyReal is false, but a ProcId is stored as 7.5: SUM would be reported as integral")
	}
	// And with a predicate on another column that still admits the wide record.
	q, err := vm.Parse("RequestMemory > 0")
	if err != nil {
		t.Fatal(err)
	}
	got2, served := c.NumStatsQuery(q, "ProcId")
	if !served {
		t.Fatal("declined with a predicate on another column")
	}
	if got2.Max != got.Max {
		t.Errorf("MAX changed under an always-true predicate: %v vs %v", got2.Max, got.Max)
	}
}

// TestStatsMultiFieldMVCC checks supersession: an update that changes the aggregated value must move
// the answer, and the old version must not contribute.
func TestStatsMultiFieldMVCC(t *testing.T) {
	c := statsFixture(t, 3000)
	defer c.Close()
	for i := 0; i < 200; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("ClusterId = %d\nJobStatus = 4\nRequestMemory = 8192\nRequestCpus = 8\nProcId = 1", i))); err != nil {
			t.Fatal(err)
		}
	}
	q, err := vm.Parse("JobStatus == 4 && RequestMemory > 2048")
	if err != nil {
		t.Fatal(err)
	}
	got, served := c.NumStatsQuery(q, "ProcId")
	if !served {
		t.Fatal("declined")
	}
	want := refStats(t, c, "JobStatus == 4 && RequestMemory > 2048", "ProcId")
	if got.N != want.N || got.IntSum != want.IntSum || got.Max != want.Max {
		t.Errorf("after supersession: (n %d, sum %d, max %v), want (n %d, sum %d, max %v)",
			got.N, got.IntSum, got.Max, want.N, want.IntSum, want.Max)
	}
}

// BenchmarkStatsMultiField measures the shape that used to decline.
func BenchmarkStatsMultiField(b *testing.B) {
	c := statsFixture(b, 60000)
	defer c.Close()
	for _, tc := range []struct{ name, where string }{
		{"sameAttr", "ProcId >= 5"},
		{"otherAttr", "JobStatus == 4"},
		{"multiField", "JobStatus == 4 && RequestMemory > 2048"},
		{"clusteredPrune", "ClusterId >= 1000 && ClusterId < 1100"},
	} {
		q, err := vm.Parse(tc.where)
		if err != nil {
			b.Fatal(err)
		}
		if _, ok := c.NumStatsQuery(q, "ProcId"); !ok {
			b.Fatalf("%s declined", tc.where)
		}
		b.Run("columnar/"+tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c.NumStatsQuery(q, "ProcId")
			}
		})
		b.Run("rowScan/"+tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				n := 0
				for range c.Query(q) {
					n++
				}
			}
		})
	}
}

// BenchmarkStatsSameAttrRoute guards the shape that was ALREADY served. A predicate on the aggregated
// attribute used to be one pass over one column (scanNumValues with a keep closure); it is now a
// narrowing pass plus a value pass over that same column, so it could have got slower.
func BenchmarkStatsSameAttrRoute(b *testing.B) {
	c := statsFixture(b, 60000)
	defer c.Close()
	st := c.schemaScan.Load()
	q, err := vm.Parse("ProcId >= 5")
	if err != nil {
		b.Fatal(err)
	}
	id, _ := c.intern.LookupID("ProcId")
	preds, ok := c.numPredsOnFields(q, st.schema)
	if !ok {
		b.Fatal("multi analysis declined")
	}
	_, keep, ok := c.numPredOnField(q, st.schema)
	if !ok {
		b.Fatal("single analysis declined")
	}
	// The old shape, reproduced: one pass, filtering inside the callback.
	oldPath := func() NumStats {
		acc := newStatsAccum()
		c.scanNumValues(id, st.cache, func(nv colVal) {
			if keep(nv.f) {
				acc.add(nv)
			}
		})
		return acc.result()
	}
	if a, bb := c.schemaScanStatsMulti(id, preds, st.cache), oldPath(); a.N != bb.N || a.IntSum != bb.IntSum {
		b.Fatalf("paths disagree: (n %d sum %d) vs (n %d sum %d)", a.N, a.IntSum, bb.N, bb.IntSum)
	}
	b.Run("newTwoPass", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.schemaScanStatsMulti(id, preds, st.cache)
		}
	})
	b.Run("oldOnePass", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			oldPath()
		}
	})
}
