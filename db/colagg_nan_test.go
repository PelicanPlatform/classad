package db

import (
	"fmt"
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// MIN/MAX over a column holding a NaN.
//
// The columnar accumulator seeds MIN with +Inf and MAX with -Inf and keeps whatever wins a `<` / `>`.
// Every comparison against NaN is false, so a group whose values are all NaN never moved either seed
// and the fast path reported the seeds themselves: MAX(Rate) came back -Inf, a value that was never in
// the data, where the scan said NaN. These tests pin the reference's behaviour, the refusal it forces,
// and the fact that the refusal is confined to MIN/MAX over a column that actually holds a NaN.
//
// Fixture note: old-ClassAd text has no way to write a NaN -- `nan` lexes as a bare attribute reference
// and evaluates to UNDEFINED -- so the ads here are built programmatically with InsertAttrFloat. +Inf is
// writable as text (1.0e999) but is inserted the same way for symmetry.

// Buckets of nanRateFixture, by what their Rate column holds.
const (
	bucketPlain  = 0 // ordinary reals: the control
	bucketInf    = 1 // +Inf and -Inf alongside ordinary reals
	bucketAllNaN = 2 // every value NaN
	bucketSomNaN = 3 // some NaN, some ordinary reals
	// The NaN-free buckets sort first so `Bucket < nanBucketsFrom` is a single scalar comparison
	// naming a group set with no NaN in it -- the predicate analysis takes a conjunction of
	// comparisons, not a disjunction, so picking the groups out one by one would decline for a
	// reason that has nothing to do with NaN.
	nanBucketsFrom = bucketAllNaN
)

// nanRateFixture is a mutable table with the accelerator on, whose Rate column is a real holding, by
// bucket, ordinary values / all NaN / mixed NaN / legitimate infinities.
func nanRateFixture(t *testing.T) *DB {
	t.Helper()
	d, err := OpenConfig(Config{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	tx := d.Begin()
	for i := 0; i < 5000; i++ {
		ad := classad.New()
		bucket := i % 4
		ad.InsertAttr("Bucket", int64(bucket))
		var rate float64
		switch bucket {
		case bucketPlain:
			rate = float64(i%97) + 0.5
		case bucketAllNaN:
			rate = math.NaN()
		case bucketSomNaN:
			if i%3 == 0 {
				rate = math.NaN()
			} else {
				rate = float64(i%41) + 0.25
			}
		case bucketInf:
			switch i % 7 {
			case 0:
				rate = math.Inf(1)
			case 1:
				rate = math.Inf(-1)
			default:
				rate = float64(i % 13)
			}
		}
		ad.InsertAttrFloat("Rate", rate)
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
	return d
}

// TestReferenceMinMaxOverNaNIsOrderDependent is why the columnar path declines rather than picking a
// NaN rule of its own. The reference seeds from the first contributing element and then takes whatever
// wins a comparison, and NaN wins none -- so the SAME two values give two different answers depending
// on which one is seen first. No order-independent implementation can agree with that on every input,
// and nothing here promises an aggregate a visit order.
func TestReferenceMinMaxOverNaNIsOrderDependent(t *testing.T) {
	nan, five := classad.NewRealValue(math.NaN()), classad.NewRealValue(5)
	nanFirst := []classad.Value{nan, five}
	numFirst := []classad.Value{five, nan}

	if got, want := ValueText(classad.Min(nanFirst)), "NaN"; got != want {
		t.Errorf("min(NaN, 5) = %s, want %s: the reference no longer keeps a leading NaN", got, want)
	}
	if got, want := ValueText(classad.Min(numFirst)), "5"; got != want {
		t.Errorf("min(5, NaN) = %s, want %s: the reference no longer skips a trailing NaN", got, want)
	}
	if ValueText(classad.Max(nanFirst)) == ValueText(classad.Max(numFirst)) {
		t.Error("max over NaN is no longer order-dependent; the columnar refusal can be revisited")
	}
}

// TestColumnarMinMaxNaNDeclinesAndMatchesScan checks the ungrouped fast path against the scan per
// bucket: a NaN-bearing column declines MIN/MAX, and every other column is still served and still
// agrees -- including one holding real infinities, where the seeds ARE the right identities.
func TestColumnarMinMaxNaNDeclinesAndMatchesScan(t *testing.T) {
	d := nanRateFixture(t)
	for _, tc := range []struct {
		bucket     int
		wantServed bool
		why        string
	}{
		{bucketPlain, true, "ordinary reals: the control, and it must not get slower or shier"},
		{bucketAllNaN, false, "all NaN: the scan says NaN, the seeds said +Inf/-Inf"},
		{bucketSomNaN, false, "mixed: the scan's answer depends on where the NaN falls"},
		{bucketInf, true, "+Inf/-Inf are ordinary data; the seeds are min/max's identities"},
	} {
		constraint := fmt.Sprintf("Bucket == %d", tc.bucket)
		for _, fn := range []AggFunc{AggMin, AggMax} {
			aggs := []AggSpec{{Func: fn, Arg: "Rate"}}
			got, served := ColumnarAggregate(func(attr string) (NumStats, bool) {
				return d.NumStats(constraint, attr)
			}, nil, aggs)
			if served != tc.wantServed {
				t.Errorf("%s %v: served = %v, want %v (%s)", constraint, fn, served, tc.wantServed, tc.why)
			}
			if !served {
				continue
			}
			want := scanGroupedMutable(t, d, constraint, nil, aggs)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%s %v: columnar %v, scan %v", constraint, fn, got, want)
			}
		}
	}
}

// TestColumnarNaNRefusalIsOnlyMinMax holds the refusal to MIN/MAX. COUNT counts a NaN as a number on
// both paths and SUM/AVG propagate it through addition the same way, so those stay on the fast path
// over the very column that declines MIN/MAX. Widening the refusal to the whole column would be a
// silent performance regression with no correctness story behind it.
func TestColumnarNaNRefusalIsOnlyMinMax(t *testing.T) {
	d := nanRateFixture(t)
	constraint := fmt.Sprintf("Bucket == %d", bucketAllNaN)
	for _, fn := range []AggFunc{AggCount, AggSum, AggAvg} {
		aggs := []AggSpec{{Func: fn, Arg: "Rate"}}
		got, served := ColumnarAggregate(func(attr string) (NumStats, bool) {
			return d.NumStats(constraint, attr)
		}, nil, aggs)
		if !served {
			t.Errorf("%v over an all-NaN column declined; only MIN/MAX have a reason to", fn)
			continue
		}
		want := scanGroupedMutable(t, d, constraint, nil, aggs)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%v over an all-NaN column: columnar %v, scan %v", fn, got, want)
		}
	}
}

// TestGroupedColumnarMinMaxNaNDeclinesAndMatchesScan is the grouped form, which is where the pinned
// disagreement turned up: `GROUP BY Bucket` reported [-Inf +Inf] for the all-NaN group against the
// scan's [NaN NaN]. One tripped group declines the whole query -- half a columnar result cannot be
// stitched to half a scan -- so the test also checks that a group set with no NaN in it is still
// served and still matches.
func TestGroupedColumnarMinMaxNaNDeclinesAndMatchesScan(t *testing.T) {
	d := nanRateFixture(t)
	groupCols := []GroupCol{{Attr: "Bucket"}}
	minMax := []AggSpec{{Func: AggMin, Arg: "Rate"}, {Func: AggMax, Arg: "Rate"}}

	if _, served := GroupedFromColumns(d, "true", groupCols, minMax); served {
		t.Error("GROUP BY over a table with a NaN group served MIN/MAX columnar; it must decline")
	}

	// Counting the same groups needs no comparison against NaN, so it is still served.
	countAggs := []AggSpec{{Func: AggCount, Arg: "*"}}
	if _, served := GroupedFromColumns(d, "true", groupCols, countAggs); !served {
		t.Error("grouped COUNT(*) declined because of a NaN it never looks at")
	}

	// A group set whose Rate column holds no NaN is served, and agrees -- infinities included.
	constraint := fmt.Sprintf("Bucket < %d", nanBucketsFrom)
	got, served := GroupedFromColumns(d, constraint, groupCols, minMax)
	if !served {
		t.Fatalf("%s GROUP BY Bucket declined; no NaN is in range", constraint)
	}
	want := scanGroupedMutable(t, d, constraint, groupCols, minMax)
	gotM, wantM := groupedRows(t, got), groupedRows(t, want)
	if len(gotM) != len(wantM) {
		t.Fatalf("%s: %d groups, scan found %d", constraint, len(gotM), len(wantM))
	}
	for g, v := range wantM {
		if fmt.Sprint(gotM[g]) != fmt.Sprint(v) {
			t.Errorf("%s: group %q = %v, scan = %v", constraint, g, gotM[g], v)
		}
	}
}
