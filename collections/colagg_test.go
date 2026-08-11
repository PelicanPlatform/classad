package collections

import (
	"fmt"
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// aggStore seeds a store with an int field, a REAL field, and a field whose values outgrow their
// chosen width (so some records escape to the cold tail), then enables the accelerator with all
// three demanded so they land in the hot tier.
//
//	Cpus         1..8
//	Memory       1024..17152, plus a few enormous values that escape
//	WallClock    real, i + 0.5
func aggStore(t *testing.T, n int) *Collection {
	t.Helper()
	c := New(Options{Shards: 2, SegmentSize: 1 << 12})
	for i := 0; i < n; i++ {
		mem := 1024 + (i%64)*256
		if i%500 == 499 {
			mem = 1 << 40 // escapes whatever width the sample chose
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, fmt.Sprintf(
			"Cpus=%d\nMemory=%d\nWallClock=%d.5", 1+i%8, mem, i))); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []string{"Cpus >= 0", "Memory >= 0", "WallClock >= 0"} {
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 25; i++ {
			for range c.Query(q) {
			}
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		t.Fatal("BuildAndEnableSchemaScan false")
	}
	return c
}

// rowStats is the ground truth: every visible record's value for attr, aggregated by the row
// path, with no columnar block involved.
func rowStats(t *testing.T, c *Collection, attr string, keep func(float64) bool) NumStats {
	t.Helper()
	q, err := vm.Parse(attr + " =!= undefined")
	if err != nil {
		t.Fatal(err)
	}
	out := NumStats{Min: math.Inf(1), Max: math.Inf(-1)}
	for ad := range c.Query(q) {
		v, ok := ad.EvaluateAttrNumber(attr)
		if !ok {
			continue
		}
		if keep != nil && !keep(v) {
			continue
		}
		out.N++
		out.Sum += v
		if v < out.Min {
			out.Min = v
		}
		if v > out.Max {
			out.Max = v
		}
	}
	if out.N == 0 {
		out.Min, out.Max = 0, 0
	}
	return out
}

func sameStats(t *testing.T, what string, got, want NumStats) {
	t.Helper()
	if got.N != want.N {
		t.Errorf("%s: N = %d, want %d", what, got.N, want.N)
	}
	if math.Abs(got.Sum-want.Sum) > 1e-6*math.Max(1, math.Abs(want.Sum)) {
		t.Errorf("%s: Sum = %v, want %v", what, got.Sum, want.Sum)
	}
	if got.Min != want.Min {
		t.Errorf("%s: Min = %v, want %v", what, got.Min, want.Min)
	}
	if got.Max != want.Max {
		t.Errorf("%s: Max = %v, want %v", what, got.Max, want.Max)
	}
}

// TestNumStatsMatchesRowScan is the correctness case: the columnar aggregate must agree with a
// row scan, including over records whose value escaped to the cold tail.
func TestNumStatsMatchesRowScan(t *testing.T) {
	c := aggStore(t, 2000)
	defer c.Close()

	for _, attr := range []string{"Cpus", "Memory"} {
		got, ok := c.NumStatsQuery(nil, attr)
		if !ok {
			t.Fatalf("%s: columnar aggregate declined", attr)
		}
		sameStats(t, attr, got, rowStats(t, c, attr, nil))
	}
}

// TestNumStatsRealField is the bits trap. A real field's fixed slot holds math.Float64bits, and
// the columnar scan hands those back as an int64 -- reading them as an integer yields values
// around 4.6e18 instead of a few thousand. Nothing else in the package exercised a real column,
// because the count fast path refused them.
func TestNumStatsRealField(t *testing.T) {
	c := aggStore(t, 2000)
	defer c.Close()

	got, ok := c.NumStatsQuery(nil, "WallClock")
	if !ok {
		t.Fatal("WallClock: columnar aggregate declined")
	}
	want := rowStats(t, c, "WallClock", nil)
	sameStats(t, "WallClock", got, want)
	// Independent of the ground truth: the values are i+0.5 for i in [0,2000), so the maximum is
	// 1999.5. A bits misread would be astronomically larger.
	if got.Max != 1999.5 {
		t.Errorf("WallClock Max = %v, want 1999.5 (a raw-bits misread gives ~4.6e18)", got.Max)
	}
	if got.Min != 0.5 {
		t.Errorf("WallClock Min = %v, want 0.5", got.Min)
	}
}

// TestNumStatsWithSameFieldPredicate covers the constrained form: a predicate on the aggregate's
// own field is in scope, and must narrow the aggregate the same way a row scan would.
func TestNumStatsWithSameFieldPredicate(t *testing.T) {
	c := aggStore(t, 2000)
	defer c.Close()

	for _, expr := range []string{"Memory >= 4096", "Memory >= 2048 && Memory < 8192", "Cpus == 4"} {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		attr := "Memory"
		if expr == "Cpus == 4" {
			attr = "Cpus"
		}
		got, ok := c.NumStatsQuery(q, attr)
		if !ok {
			t.Fatalf("%s: declined", expr)
		}
		// The ground truth applies the same predicate over the row path.
		keep := func(v float64) bool {
			switch expr {
			case "Memory >= 4096":
				return v >= 4096
			case "Memory >= 2048 && Memory < 8192":
				return v >= 2048 && v < 8192
			default:
				return v == 4
			}
		}
		sameStats(t, expr, got, rowStats(t, c, attr, keep))
	}
}

// TestNumStatsDeclines pins the boundaries, because each of these would otherwise be answered
// from the wrong data rather than refused.
func TestNumStatsDeclines(t *testing.T) {
	c := aggStore(t, 2000)
	defer c.Close()

	// A predicate on a DIFFERENT field than the aggregate is now SERVED (see
	// schemaScanStatsMulti): the predicate columns narrow the candidates and the aggregated column
	// is read for the survivors. It was declined when the pass could read only one column, which was
	// a limit of the implementation rather than a property of the question -- so the check is now
	// that it answers, and answers correctly.
	q, err := vm.Parse("Cpus > 2")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.NumStatsQuery(q, "Memory")
	if !ok {
		t.Error("aggregating Memory under a predicate on Cpus should be served")
	} else {
		var want NumStats
		want.Min, want.Max = math.Inf(1), math.Inf(-1)
		for ad := range c.Query(q) {
			v := ad.EvaluateAttr("Memory")
			iv, err := v.IntValue()
			if err != nil {
				continue
			}
			want.N++
			want.IntSum += iv
			if f := float64(iv); f < want.Min {
				want.Min = f
			}
			if f := float64(iv); f > want.Max {
				want.Max = f
			}
		}
		if got.N != want.N || got.IntSum != want.IntSum || got.Max != want.Max {
			t.Errorf("cross-column aggregate = (n %d, sum %d, max %v), want (n %d, sum %d, max %v)",
				got.N, got.IntSum, got.Max, want.N, want.IntSum, want.Max)
		}
	}
	// A non-numeric attribute.
	if _, ok := c.NumStatsQuery(nil, "Owner"); ok {
		t.Error("a non-numeric attribute must decline")
	}
	// An attribute that is not in the schema at all.
	if _, ok := c.NumStatsQuery(nil, "NoSuchAttr"); ok {
		t.Error("an unknown attribute must decline")
	}
	// The accelerator off entirely.
	plain := New(Options{Shards: 1})
	defer plain.Close()
	if _, ok := plain.NumStatsQuery(nil, "Memory"); ok {
		t.Error("with no accelerator the aggregate must decline")
	}
}

// TestNumStatsAcrossMVCC checks the visibility rule: an updated key contributes its new value
// once, not both values, and a deleted key contributes nothing.
func TestNumStatsAcrossMVCC(t *testing.T) {
	c := aggStore(t, 2000)
	defer c.Close()

	// Update 200 keys to a distinctive value and delete 100 others.
	for i := 0; i < 200; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, fmt.Sprintf(
			"Cpus=%d\nMemory=%d\nWallClock=%d.5", 2, 7777, i))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 500; i < 600; i++ {
		c.Delete([]byte(fmt.Sprintf("k%d", i)))
	}
	got, ok := c.NumStatsQuery(nil, "Memory")
	if !ok {
		t.Fatal("declined")
	}
	sameStats(t, "Memory after updates+deletes", got, rowStats(t, c, "Memory", nil))
}

// TestNumStatsIncludesEscapedValues pins that a value too wide for its fixed slot still reaches
// the aggregate, read from the cold tail at its true magnitude. aggStore plants one 1<<40 every
// 500 records precisely so this cannot pass by accident.
func TestNumStatsIncludesEscapedValues(t *testing.T) {
	c := aggStore(t, 2000)
	defer c.Close()

	const escaped = float64(int64(1) << 40)
	q, err := vm.Parse("Memory >= 1099511627776") // == 1<<40
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.NumStatsQuery(q, "Memory")
	if !ok {
		t.Fatal("declined")
	}
	if got.N != 4 { // i%500 == 499 over 2000 records
		t.Errorf("N = %d, want the 4 planted escapees", got.N)
	}
	if got.Min != escaped || got.Max != escaped {
		t.Errorf("Min/Max = %v/%v, want both %v -- an escaped value must come back at its real "+
			"magnitude, not truncated to the slot width", got.Min, got.Max, escaped)
	}
	sameStats(t, "escaped only", got, rowStats(t, c, "Memory", func(v float64) bool { return v >= escaped }))
}

// TestNumStatsEmpty reports N=0 rather than an infinite Min left over from initialization.
func TestNumStatsEmpty(t *testing.T) {
	c := aggStore(t, 2000)
	defer c.Close()

	// Above every value present, including the 1<<40 escapees (~1.1e12).
	q, err := vm.Parse("Memory > 1000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.NumStatsQuery(q, "Memory")
	if !ok {
		t.Fatal("declined")
	}
	if got.N != 0 || got.Min != 0 || got.Max != 0 || got.Sum != 0 {
		t.Errorf("empty result = %+v, want a zero NumStats (no infinities)", got)
	}
}

// BenchmarkNumStats bounds the columnar pass itself against a materializing row scan. Note the
// baseline decodes a ClassAd per record, which is NOT what the server does -- it projects
// wire-native (QueryProject) -- so this overstates the end-to-end gain. BenchmarkAggregateMax in
// dbrpc measures the real server paths against each other.
func BenchmarkNumStats(b *testing.B) {
	const n = 50000
	c := New(Options{Shards: 2, SegmentSize: 1 << 20})
	defer c.Close()
	for i := 0; i < n; i++ {
		ad, err := classad.ParseOld(fmt.Sprintf("ProcId=%d\nMemory=%d\nWallClock=%d.5", i%10000, 1024+(i%64)*256, i))
		if err != nil {
			b.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			b.Fatal(err)
		}
	}
	q, err := vm.Parse("ProcId >= 0")
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		for range c.Query(q) {
		}
	}

	b.Run("row", func(b *testing.B) {
		b.ReportAllocs()
		all, err := vm.Parse("ProcId =!= undefined")
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < b.N; i++ {
			max := math.Inf(-1)
			for ad := range c.Query(all) {
				if v, ok := ad.EvaluateAttrNumber("ProcId"); ok && v > max {
					max = v
				}
			}
			if math.IsInf(max, -1) {
				b.Fatal("no values")
			}
		}
	})
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		b.Fatal("BuildAndEnableSchemaScan false")
	}
	b.Run("columnar", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ns, ok := c.NumStatsQuery(nil, "ProcId")
			if !ok {
				b.Fatal("declined")
			}
			if ns.N == 0 {
				b.Fatal("no values")
			}
		}
	})
}
