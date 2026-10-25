package collections

import (
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A bucketed GROUP BY over a column holding a NaN or an infinity.
//
// floor(v/width)*width is rendered as an int64 bucket key, and int64() of a non-finite float is
// implementation-defined in Go: on darwin/arm64 with width 86400 a NaN came out as bucket 0 --
// indistinguishable from the genuine epoch-0 records sitting next to it -- and a +Inf as bucket
// -86400, a bucket start before the epoch that no record's timestamp can produce. A differential
// test could not see any of it, because the record scan derives its key with the same arithmetic
// and agreed on the nonsense; these tests assert against what the values MEAN instead.
//
// Fixture note: old-ClassAd text cannot write a NaN (`nan` lexes as an attribute reference and
// evaluates to UNDEFINED), so NaN records are built with InsertAttrFloat. +Inf is writable as
// 1.0e999 but is inserted the same way here, so the stored value is a literal in both cases --
// the shape that reaches the columnar group path. An EXPRESSION-valued group column makes the
// columnar path hand the query back entirely, so it is covered on the db side instead.

// TestBucketOfDeclinesNonFinite pins the bucket arithmetic itself, at the site every tier shares.
// A refactor that reintroduces the bare cast fails here rather than in whichever aggregate happens
// to be asked for a non-finite value next.
func TestBucketOfDeclinesNonFinite(t *testing.T) {
	const day = 86400
	for _, tc := range []struct {
		v    float64
		want int64
	}{
		{0, 0},
		{1, 0},
		{day - 1, 0},
		{day, day},
		{1700000000, 1700000000 / day * day},
		{1700000000.75, 1700000000 / day * day},
		{-1, -day},   // floor, not truncation: a negative value rounds away from zero
		{-day, -day}, // exactly on a boundary: its own bucket, not the one below
	} {
		got, ok := bucketOf(tc.v, day)
		if !ok || got != tc.want {
			t.Errorf("bucketOf(%v, %d) = %d, %v; want %d, true", tc.v, day, got, ok, tc.want)
		}
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got, ok := bucketOf(v, day); ok {
			t.Errorf("bucketOf(%v, %d) = %d, true; want no bucket", v, day, got)
		}
	}
}

// TestZoneBucketDeclinesNonFiniteEdge covers the archive's whole-segment attribution, which places
// every record in a segment without reading one. A NaN never reaches a zone edge (it clears Full,
// which the caller requires), but an infinity does -- and Min == Max == +Inf would otherwise satisfy
// "the whole segment is in one bucket" and attribute every record in it to bucket -86400.
func TestZoneBucketDeclinesNonFiniteEdge(t *testing.T) {
	const day = 86400
	base := int64(1700000000) / day * day
	if b, ok := zoneBucket(zoneRange{Min: float64(base), Max: float64(base + 10), Full: true}, day); !ok || b != base {
		t.Errorf("zone inside one day = %d, %v; want %d, true", b, ok, base)
	}
	if _, ok := zoneBucket(zoneRange{Min: float64(base - 1), Max: float64(base + 1), Full: true}, day); ok {
		t.Error("zone straddling a boundary claimed one bucket")
	}
	for _, z := range []zoneRange{
		{Min: math.Inf(1), Max: math.Inf(1), Full: true},
		{Min: math.Inf(-1), Max: math.Inf(-1), Full: true},
		{Min: float64(base), Max: math.Inf(1), Full: true},
		{Min: math.Inf(-1), Max: float64(base), Full: true},
	} {
		if b, ok := zoneBucket(z, day); ok {
			t.Errorf("zone [%v,%v] = bucket %d; want no bucket", z.Min, z.Max, b)
		}
	}
}

// TestGroupAccumDropsNonFiniteBucket pins the accumulator's own decision: a non-finite group value
// forms no group, the same (nil, true) a string gets, rather than creating one under a bogus key.
func TestGroupAccumDropsNonFiniteBucket(t *testing.T) {
	const day = 86400
	realKey := func(f float64) groupComp {
		return groupComp{kind: gkReal, raw: int64(math.Float64bits(f))}
	}
	a := newGroupAccum([]int64{day})
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		g, ok := accFor1(a, realKey(f), 0)
		if !ok || g != nil {
			t.Errorf("accFor(%v) = %v, %v; want nil, true (in no bucket)", f, g, ok)
		}
	}
	if len(a.acc) != 0 {
		t.Fatalf("non-finite values created %d group(s): %v", len(a.acc), a.acc)
	}
	// The finite neighbours still group, and a real and an int that floor alike share one group --
	// the property the bucket exists for, which the guard must not disturb.
	for _, k := range []groupComp{realKey(100), realKey(day + 0.5), {kind: gkInt, raw: day + 1}} {
		if g, ok := accFor1(a, k, 0); !ok || g == nil {
			t.Fatalf("accFor(%+v) = %v, %v; want a group", k, g, ok)
		}
	}
	if len(a.acc) != 2 {
		t.Errorf("finite values formed %d groups, want 2", len(a.acc))
	}
}

// nonFiniteStampFixture is a collection whose Stamp column is a real holding, by record, ordinary
// unix seconds across three days plus NaN, +Inf and -Inf. Owner gives the groups a second dimension
// so a dropped record is visible as a missing count rather than only as a missing bucket.
func nonFiniteStampFixture(tb testing.TB, n int) (*Collection, map[int64]int) {
	tb.Helper()
	const day = 86400
	base := int64(1700000000) / day * day
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	want := map[int64]int{}
	for i := 0; i < n; i++ {
		ad := classad.New()
		ad.InsertAttr("ProcId", int64(i%7))
		ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%4))
		var stamp float64
		switch i % 50 {
		case 0:
			stamp = math.NaN()
		case 1:
			stamp = math.Inf(1)
		case 2:
			stamp = math.Inf(-1)
		default:
			// Three days' worth, so several buckets exist and a dropped record cannot hide in
			// a single catch-all group.
			stamp = float64(base + int64(i%3)*day + int64(i%day))
			want[int64(stamp)/day*day]++
		}
		ad.InsertAttrFloat("Stamp", stamp)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			tb.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 16) {
		tb.Skip("no sealed segments to accelerate")
	}
	return c, want
}

// TestBucketedGroupStatsExcludeNonFinite is the semantic test: the bucketed group set is asserted
// against what the stored values mean -- one group per day actually present in the data -- not
// against another implementation of the same arithmetic. A NaN and an infinity are in no day, so
// they are in no group, and the counts are exactly the finite records'.
func TestBucketedGroupStatsExcludeNonFinite(t *testing.T) {
	const day = 86400
	c, want := nonFiniteStampFixture(t, 4000)

	gs, ok := c.GroupStatsAllBucketed("Stamp", day, nil)
	if !ok {
		t.Fatal("bucketed group stats declined on a columnar collection")
	}
	got := map[int64]int{}
	for _, g := range gs {
		i, err := g.Values[0].IntValue()
		if err != nil {
			t.Fatalf("bucket key %v is not an integer: %v", g.Values[0], err)
		}
		if _, dup := got[i]; dup {
			t.Fatalf("bucket %d appears twice", i)
		}
		got[i] = g.Count
	}
	if len(got) != len(want) {
		t.Fatalf("buckets = %v, want %v", got, want)
	}
	for b, w := range want {
		if got[b] != w {
			t.Errorf("bucket %d count = %d, want %d", b, got[b], w)
		}
	}
	// Named explicitly, because "the counts match" would also hold if a bogus bucket existed with
	// the non-finite records in it and the loop above happened not to look at it.
	for _, b := range bucketKeys(got) {
		if b < 0 {
			t.Errorf("bucket %d: a negative bucket start is what int64(+Inf/width) renders as", b)
		}
	}
	if _, isZeroBucket := got[0]; isZeroBucket {
		t.Error("bucket 0 exists: int64(NaN) renders as 0 and the fixture has no epoch-0 record")
	}
}

func bucketKeys(m map[int64]int) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
