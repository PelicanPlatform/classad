package db

import (
	"fmt"
	"iter"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// valueSeq replays rows of already-projected values, which is what AggregateValues consumes.
func valueSeq(rows [][]classad.Value) iter.Seq[[]classad.Value] {
	return func(yield func([]classad.Value) bool) {
		for _, r := range rows {
			if !yield(r) {
				return
			}
		}
	}
}

// AggRow.TypedValues must say exactly what AggRow.Values says, for every aggregate.
//
// They are two renderings of one answer and nothing enforces that at compile time, so this
// is what keeps them from drifting -- the hazard being that a consumer reading the typed
// half would then report a different number from the table everyone else sees. The
// comparison is against each aggregate's OWN renderer: AggList renders through
// GroupValueText (its text has to be invertible), every other aggregate through ValueText.
func TestAggResultValueMatchesResultText(t *testing.T) {
	// A mix that exercises the numeric aggregates, the distinct counters and the list:
	// integers, a repeat (so DISTINCT differs from COUNT) and a non-integral mean.
	rows := [][]classad.Value{
		{classad.NewIntValue(4)},
		{classad.NewIntValue(7)},
		{classad.NewIntValue(4)},
		{classad.NewIntValue(10)},
	}
	specs := []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggCount, Arg: "a"},
		{Func: AggCountDistinct, Arg: "a"},
		{Func: AggSum, Arg: "a"},
		{Func: AggAvg, Arg: "a"},
		{Func: AggMin, Arg: "a"},
		{Func: AggMax, Arg: "a"},
		{Func: AggList, Arg: "a"},
	}
	for _, spec := range specs {
		spec := spec
		t.Run(fmt.Sprintf("%v(%s)", spec.Func, spec.Arg), func(t *testing.T) {
			// The internal entry point, because a list-valued aggregate needs a bound
			// that the public signature has no field for.
			got, err := aggregateValues(valueSeq(rows), []string{"a"}, nil,
				[]AggSpec{spec}, nil, []int{0}, nil, 16)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d rows, want 1", len(got))
			}
			row := got[0]
			if len(row.TypedValues) != 1 {
				t.Fatalf("TypedValues not filled: %v", row.TypedValues)
			}
			render := ValueText
			if spec.Func == AggList {
				render = GroupValueText
			}
			if want, have := row.Values[0], render(row.TypedValues[0]); want != have {
				t.Fatalf("Values[0] = %q but TypedValues[0] renders as %q", want, have)
			}
		})
	}
}

// The typed half must carry the KIND, not just a matching rendering -- that is the whole
// reason it exists.
//
// MIN/MAX over a string column is NOT the case to test here, though it looks like it:
// classad.Min does not order strings, so that answer is `error` and the text says so too.
// The aggregates whose answer can genuinely BE a string are a user-defined one and the
// elements of a list, and the list is the one reachable without registering a function.
// Its text is a ClassAd literal precisely because the text had to be invertible; the typed
// value needs no inversion, and this pins that it really is a list of strings.
func TestAggTypedListKeepsElementKinds(t *testing.T) {
	rows := [][]classad.Value{
		{classad.NewStringValue("bob")},
		{classad.NewStringValue("alice")},
	}
	got, err := aggregateValues(valueSeq(rows), []string{"a"}, nil,
		[]AggSpec{{Func: AggList, Arg: "a"}}, nil, []int{0}, nil, 16)
	if err != nil {
		t.Fatal(err)
	}
	v := got[0].TypedValues[0]
	if !v.IsList() {
		t.Fatalf("LIST aggregate gave a %v, want a list value", v)
	}
	elems, err := v.ListValue()
	if err != nil {
		t.Fatal(err)
	}
	if len(elems) != 2 {
		t.Fatalf("got %d elements, want 2", len(elems))
	}
	for i, e := range elems {
		if !e.IsString() {
			t.Fatalf("element %d is a %v, want a string value", i, e)
		}
	}
}

// COUNT is an integer, not a string that looks like one. The aggregate's output is what
// INSERT ... SELECT writes into a summary table, and a count stored as "12" would compare
// and sort as text for the rest of that table's life.
func TestAggTypedCountIsAnInteger(t *testing.T) {
	rows := [][]classad.Value{{classad.NewIntValue(1)}, {classad.NewIntValue(2)}}
	got, err := AggregateValues(valueSeq(rows), []string{"a"}, nil,
		[]AggSpec{{Func: AggCount, Arg: "*"}}, nil, []int{0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := got[0].TypedValues[0]
	if !v.IsInteger() {
		t.Fatalf("COUNT(*) gave a %v, want an integer value", v)
	}
	if n, _ := v.IntValue(); n != 2 {
		t.Fatalf("COUNT(*) = %d, want 2", n)
	}
}
