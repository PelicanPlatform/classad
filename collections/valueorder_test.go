package collections

import (
	"math"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestCompareValuesOrdersLargeIntegersExactly is the reason two integers are not compared as
// floats. Past 2^53 a float64 cannot hold consecutive integers apart, so the conversion makes two
// distinct job ids equal -- and "equal" to a sort means whichever came first stays first, which
// is a wrong answer that no assertion about ordering elsewhere would catch.
func TestCompareValuesOrdersLargeIntegersExactly(t *testing.T) {
	lo := classad.NewIntValue(9007199254740992)
	hi := classad.NewIntValue(9007199254740993)
	if float64(9007199254740992) != float64(9007199254740993) {
		t.Skip("float64 separates these on this platform; the test has nothing to pin")
	}
	if c := CompareValues(lo, hi); c >= 0 {
		t.Errorf("CompareValues(2^53, 2^53+1) = %d, want negative", c)
	}
	if c := CompareValues(hi, lo); c <= 0 {
		t.Errorf("CompareValues(2^53+1, 2^53) = %d, want positive", c)
	}
	if c := CompareValues(hi, hi); c != 0 {
		t.Errorf("CompareValues(v, v) = %d, want 0", c)
	}
}

// TestCompareValuesIsTotalAcrossClasses pins the class precedence and, more importantly, that
// sorting a mixed slice is deterministic at all: ClassAd's own `<` is undefined across types, so
// without a fixed precedence the same input could come back in a different order each run.
func TestCompareValuesIsTotalAcrossClasses(t *testing.T) {
	ad := classad.New()
	ad.InsertAttr("X", int64(1))
	// In the order they must sort.
	want := []classad.Value{
		classad.NewIntValue(-5),
		classad.NewRealValue(2.5),
		classad.NewIntValue(7),
		classad.NewRealValue(math.NaN()),
		classad.NewStringValue("alpha"),
		classad.NewStringValue("beta"),
		classad.NewBoolValue(false),
		classad.NewBoolValue(true),
		classad.NewUndefinedValue(),
		classad.NewErrorValue(),
	}
	// Shuffled deterministically by reversing, which is the worst case for a stable sort that
	// is not actually ordering anything.
	got := make([]classad.Value, len(want))
	for i := range want {
		got[i] = want[len(want)-1-i]
	}
	sort.SliceStable(got, func(a, b int) bool { return CompareValues(got[a], got[b]) < 0 })
	for i := range want {
		if CompareValues(got[i], want[i]) != 0 {
			t.Fatalf("sorted[%d] = %v, want %v (full order: %v)", i, got[i], want[i], got)
		}
	}
	// NaN is the case a non-total comparator gets wrong without looking wrong: it must compare
	// greater than a number in one direction and less in the other, not false in both.
	nan, five := classad.NewRealValue(math.NaN()), classad.NewIntValue(5)
	if CompareValues(nan, five) <= 0 || CompareValues(five, nan) >= 0 {
		t.Errorf("NaN does not order against 5: %d / %d", CompareValues(nan, five), CompareValues(five, nan))
	}
}

// TestCompareValuesEqualityFollowsClassAd pins the two places where "equal" is ClassAd's answer
// rather than Go's: an integer equals the real that holds the same number, and two strings that
// differ only by case are ordered rather than collapsed, since a scan groups by exact text.
func TestCompareValuesEqualityFollowsClassAd(t *testing.T) {
	if c := CompareValues(classad.NewIntValue(2), classad.NewRealValue(2.0)); c != 0 {
		t.Errorf("CompareValues(2, 2.0) = %d, want 0", c)
	}
	a, b := classad.NewStringValue("Alice"), classad.NewStringValue("alice")
	if CompareValues(a, b) == 0 {
		t.Error("two strings differing only by case compared equal; distinct values need distinct places")
	}
	if CompareValues(a, b) != -CompareValues(b, a) {
		t.Error("the case tiebreak is not antisymmetric")
	}
}
