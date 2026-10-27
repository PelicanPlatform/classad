package collections

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// key1 packs one component the way the accumulator does, for a test that wants to look a
// single-column group up in the accumulation map.
func key1(c groupComp) groupKey1 {
	// Component 0 of a one-component key, which is the whole key: the accumulator's arity-1 path
	// stores exactly these two fields at a constant index.
	return groupKey1{raw: [1]int64{c.raw}, kind: [1]groupValKind{c.kind}}
}

// accFor1 is groupAccum.accFor for a single component, which is the shape most of these tests want.
func accFor1(a *groupAccum, c groupComp, nAgg int) (*groupAcc, bool) {
	var comps [maxGroupCols]groupComp
	comps[0] = c
	return a.accFor(&comps, nAgg)
}

// accForN is groupAccum.accFor over a component slice.
func accForN(a *groupAccum, cs []groupComp, nAgg int) (*groupAcc, bool) {
	var comps [maxGroupCols]groupComp
	copy(comps[:], cs)
	return a.accFor(&comps, nAgg)
}

// valueText renders a component value the way db.ValueText does. The two cannot be shared -- db
// imports collections, not the other way round -- so this is a transcription, and
// TestGroupKeyRefinesRenderedIdentity below is only as good as it is. It is kept to the exact shape
// of db.ValueText so a change there is visible as a diff here.
func valueText(v classad.Value) string {
	switch {
	case v.IsUndefined():
		return "undefined"
	case v.IsError():
		return "error"
	case v.IsBool():
		b, _ := v.BoolValue()
		return strconv.FormatBool(b)
	case v.IsString():
		s, _ := v.StringValue()
		return s
	case v.IsInteger():
		i, _ := v.IntValue()
		return strconv.FormatInt(i, 10)
	case v.IsReal():
		r, _ := v.RealValue()
		return strconv.FormatFloat(r, 'g', -1, 64)
	}
	return v.String()
}

// TestGroupKeyStringsAreExactBytes is the one that stops a confident wrong answer.
//
// The block dictionaries in this package are FOLD-ORDERED and the comparison path works in fold
// ranges, so keying a string component by its folded form -- or by the fold range it lands in -- is
// the natural thing to reach for and would be silently wrong: "u07" and "U07" are two groups to the
// record scan, which groups by exact text, and merging them reports one group's count where two
// belong. Nothing downstream could detect it, because the merge step at the db boundary only ever
// joins keys together, never splits one.
//
// Fold is for ORDERING AND PRUNING. Identity is exact bytes.
func TestGroupKeyStringsAreExactBytes(t *testing.T) {
	a := newGroupAccum([]int64{0})
	// Pairs that fold equal and must stay apart, plus one repeat of each spelling so the test also
	// says that the SAME bytes do land together.
	spellings := []string{"u07", "U07", "u07", "U07", "U07", "site-A", "SITE-a", "Site-A", "", ""}
	accs := map[string]*groupAcc{}
	for _, s := range spellings {
		g, ok := accFor1(a, a.strComp(s), 0)
		if !ok || g == nil {
			t.Fatalf("%q formed no group", s)
		}
		if prev, seen := accs[s]; seen && prev != g {
			t.Errorf("%q formed two groups: the same bytes must be one group", s)
		}
		accs[s] = g
	}
	// Every distinct spelling is its own group.
	want := map[string]bool{}
	for _, s := range spellings {
		want[s] = true
	}
	if a.nGroups() != len(want) {
		t.Fatalf("%d groups for %d distinct spellings (%v): case-folding merged groups the scan "+
			"keeps apart", a.nGroups(), len(want), want)
	}
	// Named explicitly, since a count can be right for the wrong reason.
	if accs["u07"] == accs["U07"] {
		t.Error(`"u07" and "U07" share a group: the key folded a string, and the record scan does not`)
	}
	if accs["site-A"] == accs["SITE-a"] || accs["site-A"] == accs["Site-A"] {
		t.Error("three spellings of one site folded into fewer groups than the scan gives")
	}
	// And the counts: five u07/U07 records, split 2/3 the way the exact bytes split them.
	if accs["u07"].n != 2 || accs["U07"].n != 3 {
		t.Errorf("u07 counted %d and U07 %d; want 2 and 3", accs["u07"].n, accs["U07"].n)
	}
}

// TestGroupKeyKindSeparatesRawBits pins the KIND half of the key.
//
// Several kinds share a raw int64 by construction: a boolean is 0 or 1, undefined and error are 0,
// and an integer may be any of those. Drop the kind from the key and they collide into one group --
// a COARSENING of the scan's partition, which the render-and-merge step downstream cannot undo
// because it only ever merges. The values here all render differently, so the scan has them as
// separate groups and so must this.
func TestGroupKeyKindSeparatesRawBits(t *testing.T) {
	a := newGroupAccum([]int64{0})
	comps := []groupComp{
		{kind: gkInt, raw: 0},
		{kind: gkInt, raw: 1},
		{kind: gkBool, raw: 0},
		{kind: gkBool, raw: 1},
		{kind: gkUndef, raw: 0},
		{kind: gkError, raw: 0},
		{kind: gkReal, raw: 0}, // +0.0: Float64bits is 0, exactly like the integer 0
	}
	seen := map[*groupAcc][]groupComp{}
	for _, c := range comps {
		g, ok := accFor1(a, c, 0)
		if !ok || g == nil {
			t.Fatalf("%+v formed no group", c)
		}
		seen[g] = append(seen[g], c)
	}
	if a.nGroups() != len(comps) {
		for g, cs := range seen {
			if len(cs) > 1 {
				t.Errorf("these share a group though they render differently: %+v (acc %p)", cs, g)
			}
		}
		t.Fatalf("%d components formed %d groups: the key is not separating them by kind",
			len(comps), a.nGroups())
	}
}

// TestGroupKeyRefinesRenderedIdentity is the identity contract itself, over the sharp values:
//
//	packed(a) == packed(b)  =>  text(a) == text(b)
//
// Equal keys must render alike. The converse is allowed to fail -- 42 and "42" are two keys and one
// rendered group -- and where it does, the db layer merges them by their rendered text. A key that
// could COARSEN has no such repair, so this is the direction that is checked.
func TestGroupKeyRefinesRenderedIdentity(t *testing.T) {
	a := newGroupAccum([]int64{0})
	comps := []groupComp{
		{kind: gkInt, raw: 42},
		a.strComp("42"),
		a.strComp("u07"),
		a.strComp("U07"),
		a.strComp("undefined"), // the literal text ...
		{kind: gkUndef},        // ... beside the value that renders as it
		{kind: gkError},
		a.strComp("error"),
		{kind: gkInt, raw: 3},
		{kind: gkReal, raw: int64(math.Float64bits(3.0))}, // renders "3", like the integer
		{kind: gkReal, raw: int64(math.Float64bits(3.5))},
		{kind: gkBool, raw: 1},
		a.strComp("true"),
		{kind: gkReal, raw: int64(math.Float64bits(math.NaN()))},
		{kind: gkReal, raw: int64(math.Float64bits(math.Inf(1)))},
		{kind: gkReal, raw: int64(math.Float64bits(0))},
		{kind: gkReal, raw: int64(math.Float64bits(math.Copysign(0, -1)))},
	}
	byKey := map[groupKey1]string{}
	for _, c := range comps {
		k := key1(c)
		text := valueText(keyValues(a, k)[0])
		if prev, seen := byKey[k]; seen && prev != text {
			t.Errorf("one key renders as both %q and %q: the key is coarser than the scan's identity",
				prev, text)
		}
		byKey[k] = text
	}
	// And the pairs that SHOULD be two keys rendering alike, so the merge downstream has something
	// to do and this test is not vacuously satisfied by every value being distinct.
	for _, pair := range [][2]groupComp{
		{{kind: gkInt, raw: 42}, a.strComp("42")},
		{{kind: gkUndef}, a.strComp("undefined")},
		{{kind: gkError}, a.strComp("error")},
		{{kind: gkInt, raw: 3}, {kind: gkReal, raw: int64(math.Float64bits(3.0))}},
		{{kind: gkBool, raw: 1}, a.strComp("true")},
	} {
		ka, kb := key1(pair[0]), key1(pair[1])
		if ka == kb {
			t.Errorf("%+v and %+v are one key; they are two storage values the renderer merges",
				pair[0], pair[1])
		}
		if ta, tb := valueText(keyValues(a, ka)[0]), valueText(keyValues(a, kb)[0]); ta != tb {
			t.Errorf("%+v renders %q and %+v renders %q: this pair no longer exercises the merge",
				pair[0], ta, pair[1], tb)
		}
	}
}

// TestGroupKeyDeclinesPastTheComponentCap pins the refusal rather than a truncation.
//
// Truncating a key past its last slot would merge every group that differs only in a dropped
// component and report the merge as an answer -- the one failure mode this whole file is arranged to
// prevent. The storage layer therefore declines the query, and the caller scans.
func TestGroupKeyDeclinesPastTheComponentCap(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	for i := 0; i < 2000; i++ {
		ad := classad.New()
		ad.InsertAttr("A", int64(i%3))
		ad.InsertAttr("B", int64(i%5))
		ad.InsertAttr("C", int64(i%7))
		ad.InsertAttr("D", int64(i%11))
		ad.InsertAttr("E", int64(i%13))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 16) {
		t.Skip("no sealed segments to accelerate")
	}
	all := []string{"A", "B", "C", "D", "E"}
	for n := 1; n <= len(all); n++ {
		specs := make([]GroupSpec, n)
		for i := range specs {
			specs[i] = GroupSpec{Attr: all[i]}
		}
		gs, ok := c.GroupStatsAllCols(specs, nil)
		if n > maxGroupCols {
			if ok {
				t.Errorf("%d group columns were SERVED with a %d-slot key: a truncated key merges "+
					"groups the scan keeps apart", n, maxGroupCols)
			}
			continue
		}
		if !ok {
			t.Fatalf("%d group columns declined", n)
		}
		for _, g := range gs {
			if len(g.Values) != n {
				t.Fatalf("%d group columns produced a %d-component group", n, len(g.Values))
			}
		}
	}
}

// TestGroupStatsColsMatchesTheProduct is the multi-column result against what the data says, rather
// than against another implementation of the same arithmetic: the attributes are independent moduli,
// so the group set is their cross product and each count is the number of records hitting that
// residue pair.
func TestGroupStatsColsMatchesTheProduct(t *testing.T) {
	const n = 3000
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	want := map[[2]int64]int{}
	for i := 0; i < n; i++ {
		ad := classad.New()
		ad.InsertAttr("A", int64(i%4))
		ad.InsertAttr("B", int64(i%6))
		ad.InsertAttrString("Owner", fmt.Sprintf("u%02d", i%5))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
		want[[2]int64{int64(i % 4), int64(i % 6)}]++
	}
	if !c.BuildAndEnableSchemaScan(4000, 16) {
		t.Skip("no sealed segments to accelerate")
	}
	gs, ok := c.GroupStatsAllCols([]GroupSpec{{Attr: "A"}, {Attr: "B"}}, nil)
	if !ok {
		t.Fatal("two numeric group columns declined")
	}
	if len(gs) != len(want) {
		t.Fatalf("%d groups, the cross product has %d", len(gs), len(want))
	}
	for _, g := range gs {
		a, _ := g.Values[0].IntValue()
		b, _ := g.Values[1].IntValue()
		if g.Count != want[[2]int64{a, b}] {
			t.Errorf("group (%d,%d) counted %d, want %d", a, b, g.Count, want[[2]int64{a, b}])
		}
	}
	// And the sort is by component, left to right, so the answer is deterministic.
	if !sort.SliceIsSorted(gs, func(i, j int) bool { return groupTupleLess(gs[i].Values, gs[j].Values) }) {
		t.Error("the group list is not sorted by its component tuple")
	}
}

// TestGroupStatsColsBucketsPerComponent is the motivating shape, "per owner per day": one raw string
// column beside one bucketed numeric one. The bucket applies to its OWN component only -- a string
// has no bucket and would drop the record if the width leaked onto it.
func TestGroupStatsColsBucketsPerComponent(t *testing.T) {
	const day = 86400
	const n = 4000
	base := int64(1700000000) / day * day
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	want := map[string]int{}
	for i := 0; i < n; i++ {
		ad := classad.New()
		owner := fmt.Sprintf("u%02d", i%5)
		ts := base + int64(i%3)*day + int64(i%day)
		ad.InsertAttrString("Owner", owner)
		ad.InsertAttr("Ts", ts)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
		want[fmt.Sprintf("%s|%d", owner, ts/day*day)]++
	}
	if !c.BuildAndEnableSchemaScan(4000, 16) {
		t.Skip("no sealed segments to accelerate")
	}
	gs, ok := c.GroupStatsAllCols([]GroupSpec{{Attr: "Owner"}, {Attr: "Ts", BucketWidth: day}}, nil)
	if !ok {
		t.Fatal("owner + bucketed timestamp declined")
	}
	if len(gs) != len(want) {
		t.Fatalf("%d groups, the data has %d owner-days", len(gs), len(want))
	}
	for _, g := range gs {
		o, _ := g.Values[0].StringValue()
		b, err := g.Values[1].IntValue()
		if err != nil {
			t.Fatalf("the bucketed component is %v, not an integer bucket start", g.Values[1])
		}
		k := fmt.Sprintf("%s|%d", o, b)
		if g.Count != want[k] {
			t.Errorf("group %s counted %d, want %d", k, g.Count, want[k])
		}
	}
}

// TestBucketMemoAcrossComponents pins the interval memo once a raw component sits beside a bucketed
// one: a hit needs BOTH the unbucketed components to match exactly and the bucketed one to fall in
// the remembered interval. Matching on the interval alone would attribute a record to whichever
// group the previous record happened to be in.
func TestBucketMemoAcrossComponents(t *testing.T) {
	const day = 86400
	a := newGroupAccum([]int64{0, day})
	owners := []groupComp{a.strComp("alice"), a.strComp("bob")}
	ts := func(f int64) groupComp { return groupComp{kind: gkInt, raw: f} }

	g1, _ := accForN(a, []groupComp{owners[0], ts(3*day + 10)}, 0)
	// Same day, DIFFERENT owner: the interval still holds, the key does not.
	g2, _ := accForN(a, []groupComp{owners[1], ts(3*day + 20)}, 0)
	if g1 == nil || g2 == nil {
		t.Fatal("a group was dropped")
	}
	if g1 == g2 {
		t.Fatal("two owners in the same day share a group: the memo matched on the interval alone")
	}
	// Same owner, same day as the first: a genuine hit, and it must find the FIRST group again
	// rather than the one the memo now holds.
	g3, _ := accForN(a, []groupComp{owners[0], ts(3*day + 30)}, 0)
	if g3 != g1 {
		t.Error("the same owner-day formed a second group")
	}
	// Same owner, next day: a miss on the interval.
	g4, _ := accForN(a, []groupComp{owners[0], ts(4*day + 1)}, 0)
	if g4 == g1 {
		t.Error("the next day landed in the previous day's group")
	}
	if a.nGroups() != 3 {
		t.Errorf("formed %d groups, want 3 (alice/day3, bob/day3, alice/day4)", a.nGroups())
	}
}

// TestBucketedComponentDropsTheWholeRecord pins what an unbucketable component does to a multi-column
// group: the RECORD leaves the series, not just that component. AggregateValues drops a row as soon
// as any one of its bucket keys fails to render, so a record whose owner is perfectly groupable is
// still gone if its timestamp is a NaN.
func TestBucketedComponentDropsTheWholeRecord(t *testing.T) {
	const day = 86400
	a := newGroupAccum([]int64{0, day})
	owner := a.strComp("alice")
	real := func(f float64) groupComp { return groupComp{kind: gkReal, raw: int64(math.Float64bits(f))} }
	for _, bad := range []groupComp{
		real(math.NaN()), real(math.Inf(1)), real(math.Inf(-1)),
		{kind: gkUndef}, {kind: gkBool, raw: 1}, a.strComp("tuesday"),
	} {
		g, ok := accForN(a, []groupComp{owner, bad}, 0)
		if !ok || g != nil {
			t.Errorf("a record with an unbucketable %+v grouped as %v; want nil, true", bad, g)
		}
	}
	if a.nGroups() != 0 {
		t.Fatalf("unbucketable records formed %d groups: %v", a.nGroups(), a.g2.acc)
	}
	// The memo must be left disarmed by those, so the next good record is not handed one of their
	// (never-created) groups.
	g, ok := accForN(a, []groupComp{owner, {kind: gkInt, raw: 3*day + 5}}, 0)
	if !ok || g == nil || a.nGroups() != 1 {
		t.Fatalf("a groupable record after the dropped ones gave %v, %v with %d groups", g, ok, a.nGroups())
	}
}

// TestBucketArithmeticTableDriven is the direct test of the floor itself, at the magnitudes and the
// non-finite values a differential fuzzer CANNOT see: its reference is the record scan, the scan
// derives its bucket with this same bucketOf, and a bug the two share cancels out of the comparison
// entirely. (Reverting the non-finite guard and running the sweep is green.) So the bucket
// arithmetic is asserted against what the values MEAN.
func TestBucketArithmeticTableDriven(t *testing.T) {
	for _, tc := range []struct {
		v     float64
		width int64
		want  int64
		ok    bool
		why   string
	}{
		{0, 1, 0, true, "zero, width 1"},
		{5, 1, 5, true, "width 1 is the identity on integers"},
		{-5, 1, -5, true, "... including negative ones"},
		{5.75, 1, 5, true, "width 1 floors a fraction"},
		{-5.25, 1, -6, true, "floor, not truncation: a negative rounds away from zero"},
		{86399, 86400, 0, true, "just inside the first day"},
		{86400, 86400, 86400, true, "a boundary belongs to the bucket it starts"},
		{-1, 86400, -86400, true, "one second before the epoch is the day before it"},
		{-86400, 86400, -86400, true, "exactly on a negative boundary"},
		{-86401, 86400, -172800, true, "one second before that"},
		// Around 2^53, where float64 stops counting by ones. The division is still exact enough
		// here that the bucket is the arithmetic one.
		{1 << 53, 86400, (1 << 53) / 86400 * 86400, true, "2^53"},
		{(1 << 53) - 1, 86400, ((1 << 53) - 1) / 86400 * 86400, true, "just below 2^53"},
		{(1 << 53) + 2, 86400, ((1 << 53) + 2) / 86400 * 86400, true, "just above 2^53"},
		// 1e18 and above are covered by TestBucketIsMonotoneAndAligned instead: the answer there
		// cannot be written down without re-deriving it from the implementation, so the test is a
		// property rather than a value.
		{math.NaN(), 86400, 0, false, "a NaN is in no half-open interval"},
		{math.Inf(1), 86400, 0, false, "nor is +Inf"},
		{math.Inf(-1), 86400, 0, false, "nor -Inf"},
		{math.NaN(), 1, 0, false, "width 1 does not make a NaN bucketable"},
	} {
		got, ok := bucketOf(tc.v, tc.width)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("bucketOf(%v, %d) = %d, %v; want %d, %v (%s)",
				tc.v, tc.width, got, ok, tc.want, tc.ok, tc.why)
		}
		if ok && tc.width > 0 && got%tc.width != 0 {
			t.Errorf("bucketOf(%v, %d) = %d, which is not a multiple of the width (%s)",
				tc.v, tc.width, got, tc.why)
		}
	}
}

// TestBucketedKeyMatchesBucketOfPerComponent runs the same arithmetic through the ACCUMULATOR, with
// the memo live, so the memo's interval is checked against bucketOf for every value above rather
// than only for the sequence the randomized memo test happens to draw.
func TestBucketedKeyMatchesBucketOfPerComponent(t *testing.T) {
	values := []float64{
		0, 1, -1, 86399, 86400, 86401, -86400, -86401, 5.75, -5.25,
		1 << 53, (1 << 53) - 1, (1 << 53) + 2, 1e18, 2e18, -1e18,
		math.NaN(), math.Inf(1), math.Inf(-1),
	}
	for _, width := range []int64{1, 2, 3600, 86400} {
		a := newGroupAccum([]int64{width})
		for _, v := range values {
			comp := groupComp{kind: gkReal, raw: int64(math.Float64bits(v))}
			g, ok := accFor1(a, comp, 0)
			if !ok {
				t.Fatalf("width %d value %v: declined", width, v)
			}
			b, inBucket := bucketOf(v, width)
			if !inBucket {
				if g != nil {
					t.Errorf("width %d value %v: grouped, but bucketOf puts it in no bucket", width, v)
				}
				continue
			}
			if g == nil {
				t.Errorf("width %d value %v: dropped, but bucketOf puts it in %d", width, v, b)
				continue
			}
			if want := a.g1.acc[key1(groupComp{kind: gkInt, raw: b})]; want != g {
				t.Errorf("width %d value %v: landed somewhere other than bucket %d", width, v, b)
			}
		}
	}
}

// TestBucketIsMonotoneAndAligned covers the magnitudes where the answer cannot be written down: past
// about 1e18 the float grid is coarser than a bucket width in places, so re-deriving the expected
// bucket with the same floor-and-divide the implementation uses would assert nothing. These are the
// two properties every tier actually relies on, and neither restates the arithmetic:
//
//   - a bucket start is a MULTIPLE of the width, so a bucket key names a point on the same grid
//     whatever the value's magnitude;
//   - bucketing is MONOTONE, so a later timestamp never lands in an earlier bucket -- the property
//     a time series is read with.
func TestBucketIsMonotoneAndAligned(t *testing.T) {
	for _, width := range []int64{1, 60, 3600, 86400, 604800} {
		for _, start := range []float64{-2e18, -1e18, -1 << 53, 0, 1 << 53, 1e18, 2e18, 4e18} {
			prev, havePrev := int64(0), false
			for step := 0; step < 64; step++ {
				v := math.Nextafter(start, math.Inf(1))
				for i := 0; i < step; i++ {
					v = math.Nextafter(v, math.Inf(1))
				}
				b, ok := bucketOf(v, width)
				if !ok {
					t.Errorf("bucketOf(%v, %d) declined a finite value", v, width)
					continue
				}
				if b%width != 0 {
					t.Errorf("bucketOf(%v, %d) = %d, not a multiple of the width", v, width, b)
				}
				if havePrev && b < prev {
					t.Errorf("bucketOf(%v, %d) = %d, below the previous value's bucket %d: bucketing "+
						"went backwards as the value went forwards", v, width, b, prev)
				}
				prev, havePrev = b, true
			}
		}
	}
}

// TestGroupKeyCaseSplitsThroughTheDictionaryReader is TestGroupKeyStringsAreExactBytes one layer
// down, through the reader where the trap actually lives.
//
// The exact-bytes test above drives strComp directly, which covers the intern table but not the
// path that reaches it. A string group component is normally resolved through the per-block
// DICTIONARY -- and the dictionary is where folding is everywhere: distinctFold sorts its entries
// with the evaluator's fold comparison, and dictRange hands back a RANGE of codes that fold equal.
// A component keyed by that range, or by a folded id, would merge the two spellings here while
// leaving the direct test above passing.
//
// So the dictionary is asserted to have FORMED before the groups are counted. Which reader a block
// uses is a property of the data, not of the query, so a fixture that quietly stopped clearing
// dictWorthwhile would move this test onto the positional walk and it would no longer be testing
// what it says.
func TestGroupKeyCaseSplitsThroughTheDictionaryReader(t *testing.T) {
	const n = 4000
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	want := map[string]int{}
	for i := 0; i < n; i++ {
		// Eight owners, two of them also spelled in upper case for part of their run. Low enough
		// cardinality that dictWorthwhile (distinct*2 <= block records) clears in every block.
		o := (i / 16) % 8
		owner := fmt.Sprintf("u%02d", o)
		if (o == 3 && i%3 == 0) || (o == 6 && i%5 == 0) {
			owner = strings.ToUpper(owner)
		}
		want[owner]++
		ad := classad.New()
		ad.InsertAttrString("Owner", owner)
		ad.InsertAttr("ProcId", int64(i%4))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 16) {
		t.Skip("no sealed segments to accelerate")
	}
	withDict, blocks := dictStats(t, c, "Owner")
	if blocks == 0 || withDict != blocks {
		t.Fatalf("%d of %d blocks carry an Owner dictionary: this fixture is on the positional "+
			"walk, so it is not testing the dictionary reader it claims to", withDict, blocks)
	}

	for _, specs := range [][]GroupSpec{
		{{Attr: "Owner"}},                   // the whole key: the code -> group memo
		{{Attr: "Owner"}, {Attr: "ProcId"}}, // one component of a tuple: the code -> id memo
	} {
		gs, ok := c.GroupStatsAllCols(specs, nil)
		if !ok {
			t.Fatalf("%d group column(s): declined", len(specs))
		}
		got := map[string]int{}
		for _, g := range gs {
			o, err := g.Values[0].StringValue()
			if err != nil {
				t.Fatalf("group component 0 is %v, not a string", g.Values[0])
			}
			got[o] += g.Count
		}
		if len(got) != len(want) {
			t.Errorf("%d group column(s): %d distinct owners, want %d -- the dictionary reader "+
				"merged spellings that fold equal", len(specs), len(got), len(want))
		}
		for o, k := range want {
			if got[o] != k {
				t.Errorf("%d group column(s): owner %q counted %d, want %d", len(specs), o, got[o], k)
			}
		}
		// Named, since a count can be right for the wrong reason.
		for _, pair := range [][2]string{{"u03", "U03"}, {"u06", "U06"}} {
			if got[pair[0]] == 0 || got[pair[1]] == 0 {
				t.Errorf("%d group column(s): %q counted %d and %q counted %d: one spelling "+
					"vanished into the other", len(specs), pair[0], got[pair[0]], pair[1], got[pair[1]])
			}
		}
	}
}

// tupleText renders a tuple of components the way the record scan renders the group it stands for:
// each component to its text, joined. It is the identity the storage key must refine, so it is what
// the arity tests below compare groups by.
func tupleText(a *groupAccum, cs []groupComp) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = valueText(a.compValue(c.raw, c.kind))
	}
	return strings.Join(parts, "|")
}

// TestGroupKeyArityKeepsEveryComponent runs every arity that has a key type, and checks at each one
// that a difference in ANY single component is a difference in the group.
//
// Each arity is its own key type with its own packing, its own map and its own case in the
// dispatch, so nothing the one-column tests establish carries to the two- and three-column ones. The
// failures this is here to catch are all silent counts:
//
//   - a component dropped from the packing, which merges tuples differing only in it;
//   - a component's KIND dropped, which merges the integer 1 with the boolean TRUE (both have a raw
//     of 1) though the scan renders them "1" and "true";
//   - a string component keyed by its FOLDED form, which merges "u07" with "U07" though the scan
//     keeps the exact bytes apart;
//   - a dispatch that sends an arity to another arity's path, which either drops the components past
//     that arity or accumulates into a map nothing then reads.
//
// The variants are placed at every position in turn, because a packing writes each slot separately
// and a slot can be missed on its own.
func TestGroupKeyArityKeepsEveryComponent(t *testing.T) {
	for n := 1; n <= maxGroupCols; n++ {
		a := newGroupAccum(make([]int64, n))
		variants := []groupComp{
			{kind: gkInt, raw: 1},
			{kind: gkInt, raw: 2},
			a.strComp("u07"),
			a.strComp("U07"),       // ... and the same name in another case: two groups, not one
			{kind: gkBool, raw: 1}, // raw 1, like the integer above, and renders "true"
			{kind: gkUndef},
		}
		base := make([]groupComp, n)
		for i := range base {
			base[i] = a.strComp("base")
		}
		want := map[string]int{}
		for pos := 0; pos < n; pos++ {
			for rep := 1; rep <= 2; rep++ { // twice, so a count is wrong if two tuples merge
				for _, v := range variants {
					cs := append([]groupComp(nil), base...)
					cs[pos] = v
					g, ok := accForN(a, cs, 0)
					if !ok || g == nil {
						t.Fatalf("arity %d: %s formed no group (ok=%v)", n, tupleText(a, cs), ok)
					}
					want[tupleText(a, cs)]++
				}
			}
		}
		got := map[string]int{}
		a.each(func(vals []classad.Value, g *groupAcc) {
			if len(vals) != n {
				t.Fatalf("arity %d: a group came back with %d component values", n, len(vals))
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				parts[i] = valueText(v)
			}
			got[strings.Join(parts, "|")] += g.n
		})
		if len(got) != len(want) {
			t.Errorf("arity %d: %d groups, the tuples fed are %d distinct ones", n, len(got), len(want))
		}
		if a.nGroups() != len(got) {
			t.Errorf("arity %d: nGroups reports %d, iterating yields %d", n, a.nGroups(), len(got))
		}
		for k, w := range want {
			if got[k] != w {
				t.Errorf("arity %d: tuple %q counted %d, fed %d", n, k, got[k], w)
			}
		}
		for k := range got {
			if _, ok := want[k]; !ok {
				t.Errorf("arity %d: tuple %q was never fed", n, k)
			}
		}
	}
}

// TestGroupAccumDeclinesPastTopArity pins the behaviour past the highest arity with a key type:
// accFor DECLINES, and nothing is accumulated.
//
// The callers check the arity before they read anything (resolveGroupAttrs), so this is the backstop
// rather than the gate -- and it is the one that matters, because the failure it guards against is
// not a decline but an ANSWER. A path that packed as many components as its widest key holds and
// dropped the rest would merge every pair of groups that differs only in a dropped component, and
// report the merged counts as the result; there is nothing downstream that could tell, since the
// rows look like ordinary rows.
func TestGroupAccumDeclinesPastTopArity(t *testing.T) {
	for n := maxGroupCols + 1; n <= maxGroupCols+2; n++ {
		a := newGroupAccum(make([]int64, n))
		if a.n != 0 {
			t.Fatalf("%d group columns built an accumulator of arity %d; no key type covers it", n, a.n)
		}
		// Two tuples that differ only in a component past the top arity: if anything truncated,
		// these would be one group and the second call would find the first one's accumulator.
		var comps [maxGroupCols]groupComp
		for i := range comps {
			comps[i] = groupComp{kind: gkInt, raw: int64(i)}
		}
		for _, last := range []int64{7, 8} {
			comps[maxGroupCols-1] = groupComp{kind: gkInt, raw: last}
			g, ok := a.accFor(&comps, 0)
			if ok || g != nil {
				t.Fatalf("%d group columns were SERVED (%v, %v): a key that holds %d components "+
					"cannot answer this without merging groups the scan keeps apart", n, g, ok, maxGroupCols)
			}
		}
		if a.nGroups() != 0 {
			t.Fatalf("%d group columns accumulated %d groups after declining", n, a.nGroups())
		}
	}
}

// TestBucketMemoAcrossThreeComponents is TestBucketMemoAcrossComponents at the top arity: two raw
// categorical components beside one bucketed time component, which is "per owner per site per day".
//
// The three-component bucketed path has its own key type, its own memo probe and its own packing, so
// the two-column test says nothing about it. The sharp case is the same one: a memo hit needs EVERY
// unbucketed component to match as well as the bucketed one to fall in the remembered interval, and
// a probe that compared only the first would attribute a record to the previous record's group.
func TestBucketMemoAcrossThreeComponents(t *testing.T) {
	const day = 86400
	a := newGroupAccum([]int64{0, 0, day})
	alice, bob := a.strComp("alice"), a.strComp("bob")
	sa, sb := a.strComp("site-a"), a.strComp("site-b")
	ts := func(f int64) groupComp { return groupComp{kind: gkInt, raw: f} }
	at := func(o, s, tv groupComp) *groupAcc {
		t.Helper()
		g, ok := accForN(a, []groupComp{o, s, tv}, 0)
		if !ok || g == nil {
			t.Fatalf("a record was dropped (ok=%v)", ok)
		}
		return g
	}
	g1 := at(alice, sa, ts(3*day+10))
	// Same day, same owner, DIFFERENT site: the interval holds and the first component matches, so
	// only the second component separates these.
	if g2 := at(alice, sb, ts(3*day+20)); g2 == g1 {
		t.Error("two sites in one owner-day share a group: the memo probe is not comparing every " +
			"unbucketed component")
	}
	// Same day, same site, different owner.
	if g3 := at(bob, sa, ts(3*day+30)); g3 == g1 {
		t.Error("two owners in one site-day share a group")
	}
	// A genuine hit: it has to find the FIRST group again, not the one the memo now holds.
	if g4 := at(alice, sa, ts(3*day+40)); g4 != g1 {
		t.Error("the same owner-site-day formed a second group")
	}
	// Next day, everything else the same: a miss on the interval.
	if g5 := at(alice, sa, ts(4*day+1)); g5 == g1 {
		t.Error("the next day landed in the previous day's group")
	}
	if a.nGroups() != 4 {
		t.Errorf("formed %d groups, want 4", a.nGroups())
	}
}
