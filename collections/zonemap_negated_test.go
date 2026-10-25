package collections

import (
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// The negated ordering comparison, at each place it is decided.
//
// `!(x <= c)` is NOT `x > c`: every comparison with a NaN is false, so a stored NaN satisfies the
// negation and not the flip. The probe therefore carries its own operator, which an index lookup
// must refuse and only two consumers may act on -- a float predicate over the stored value, and a
// zone map that has PROVEN there is no NaN (Full). These are the unit-level guards; the
// end-to-end differential check lives in the db suite.

// TestNegatedOrderingProbeOp pins the probe vocabulary: the negation is not flipped away.
func TestNegatedOrderingProbeOp(t *testing.T) {
	for _, tc := range []struct{ expr, op string }{
		{"!(Rate <= 10)", "!<="},
		{"!(Rate < 10)", "!<"},
		{"!(Rate > 10)", "!>"},
		{"!(Rate >= 10)", "!>="},
		{"!(10 <= Rate)", "!>="}, // operands swapped: `!(10 <= Rate)` is `!(Rate >= 10)`
		{"!(10 > Rate)", "!<"},
		{"!(Rate == 10)", "!="}, // equality DOES flip soundly: a NaN is != 10 and not == 10
		{"!(Rate != 10)", "=="},
		{"Rate > 10", ">"},
	} {
		q, err := vm.Parse(tc.expr)
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		probes := q.Probes()
		if len(probes) != 1 {
			t.Fatalf("%s: %d probes, want 1", tc.expr, len(probes))
		}
		if probes[0].Op != tc.op {
			t.Errorf("%s: probe op %q, want %q", tc.expr, probes[0].Op, tc.op)
		}
	}
}

// TestNegatedOrderingIndexRefused holds the line that an index posting lookup refuses a negated
// ordering probe. A posting range cannot see a NaN -- it is outside every range, which is exactly
// what makes the POSITIVE form prunable -- so a range scan over the postings would drop the records
// the negation keeps. The predicate path, which compares the stored value and negates the result,
// takes it instead.
func TestNegatedOrderingIndexRefused(t *testing.T) {
	for _, op := range []string{"!<", "!<=", "!>", "!>="} {
		p := vm.Probe{Attr: "Rate", Op: op, Vals: []classad.Value{classad.NewIntValue(10)}}
		if up, ok := valUsable(7, p); ok {
			t.Errorf("valUsable accepted %q as %q: an index posting range cannot see a NaN", op, up.op)
		}
		if _, ok := catUsable(7, p); ok {
			t.Errorf("catUsable accepted %q", op)
		}
		up, ok := numPredProbe(7, p)
		if !ok {
			t.Fatalf("numPredProbe declined %q: the predicate path is the one that CAN answer it", op)
		}
		if up.op != op || up.inexact || len(up.fvals) != 1 || up.fvals[0] != 10 {
			t.Errorf("numPredProbe(%q) = %+v, want the negation carried through unchanged", op, up)
		}
	}
	// An integer literal past 2^53 is refused outright rather than widened: the callers answer from
	// the probe without re-verifying, and a widened probe is only a candidate superset.
	big := vm.Probe{Attr: "Rate", Op: "!<=", Vals: []classad.Value{classad.NewIntValue(1 << 54)}}
	if _, ok := numPredProbe(7, big); ok {
		t.Error("numPredProbe accepted an integer literal past 2^53")
	}
}

// TestNegatedOrderingZonePrune: a negated ordering conjunct prunes a segment only from a Full zone,
// which is the only proof the zone map carries that no record holds a NaN.
func TestNegatedOrderingZonePrune(t *testing.T) {
	for _, tc := range []struct {
		name string
		z    zoneRange
		op   string
		val  float64
		want bool // zoneMayMatch: true means "cannot prune"
	}{
		// Full: the segment's values are exactly the numbers in [Min,Max], so the negation is the
		// flipped comparison over them and the ordinary test decides it.
		{"full/no match", zoneRange{Min: 1, Max: 5, Full: true}, "!<=", 10, false},
		{"full/match", zoneRange{Min: 1, Max: 50, Full: true}, "!<=", 10, true},
		{"full/inf bound", zoneRange{Min: 1, Max: 50, Full: true}, "!<=", math.Inf(1), false},
		{"full/ge no match", zoneRange{Min: 20, Max: 50, Full: true}, "!>=", 10, false},
		{"full/ge match", zoneRange{Min: 1, Max: 50, Full: true}, "!>=", 10, true},
		// Not Full: some record held no number, and the zone map does not record whether that was
		// an absent value (which cannot satisfy the negation) or a NaN (which does). Undecidable.
		{"sparse/would prune", zoneRange{Min: 1, Max: 5}, "!<=", 10, true},
		{"sparse/inf bound", zoneRange{Min: 1, Max: 50}, "!<=", math.Inf(1), true},
		{"sparse/ge", zoneRange{Min: 20, Max: 50}, "!>=", 10, true},
		// The positive form is unaffected by Full, which is the point: it must cost the same as it
		// always did.
		{"positive/sparse prunes", zoneRange{Min: 1, Max: 5}, ">", 10, false},
		{"positive/full prunes", zoneRange{Min: 1, Max: 5, Full: true}, ">", 10, false},
		{"positive/sparse keeps", zoneRange{Min: 1, Max: 50}, ">", 10, true},
	} {
		if got := zoneMayMatch(tc.z, tc.op, []float64{tc.val}); got != tc.want {
			t.Errorf("%s: zoneMayMatch(%v, %q, %v) = %v, want %v", tc.name, tc.z, tc.op, tc.val, got, tc.want)
		}
	}
}

// TestNegatedOrderingNumCmp: the float predicate negates the RESULT of the comparison, which is
// right for a NaN without anything having to know about NaN.
func TestNegatedOrderingNumCmp(t *testing.T) {
	nan := math.NaN()
	for _, tc := range []struct {
		op     string
		t      float64
		v      float64
		want   bool
		reason string
	}{
		{"!<=", 10, nan, true, "NaN <= 10 is false, so its negation is true"},
		{"!<", 10, nan, true, ""},
		{"!>", 10, nan, true, ""},
		{"!>=", 10, nan, true, ""},
		{"!<=", math.Inf(1), nan, true, "even against +Inf, where every ordinary number is excluded"},
		{"!<=", 10, 20, true, ""},
		{"!<=", 10, 5, false, ""},
		{"!<=", 10, 10, false, ""},
		{"!>", 10, 20, false, ""},
		{"!>", 10, 10, true, ""},
		{"<=", 10, nan, false, "the positive form is false of a NaN"},
		{">", 10, nan, false, ""},
	} {
		cmp, ok := numCmp(tc.op, tc.t)
		if !ok {
			t.Fatalf("numCmp(%q) declined", tc.op)
		}
		if got := cmp(tc.v); got != tc.want {
			t.Errorf("numCmp(%q, %v)(%v) = %v, want %v %s", tc.op, tc.t, tc.v, got, tc.want, tc.reason)
		}
	}
}

// TestNegatedOrderingConjunctiveRangesRefused: the index-resident GROUP BY counts
// (segmentVerdict / alwaysHolds / neverHolds / countAttrWhere) do not reason about probes at all --
// they re-read the constraint's syntax and accept only a conjunction of positive comparisons. A
// negation is refused there, so the inversion cannot reach them; this pins that.
func TestNegatedOrderingConjunctiveRangesRefused(t *testing.T) {
	for _, c := range []string{
		"!(Rate <= 10)",
		"!(Rate > 10) && Owner == \"u\"",
		"Rate > 10 && !(Clean < 2)",
		"!(Rate == 10)",
	} {
		if conds, ok := parseConjunctiveRanges(c); ok {
			t.Errorf("parseConjunctiveRanges(%q) accepted it as %+v", c, conds)
		}
	}
	if _, ok := parseConjunctiveRanges("Rate > 10 && Clean <= 50"); !ok {
		t.Error("parseConjunctiveRanges refused a positive conjunction: the fast path is gone, not just the negated one")
	}
}
