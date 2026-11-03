package db

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// GROUPING BY WHAT ClassAd CALLS IDENTICAL.
//
// A group key partitions rows, so it has to be an equivalence relation. `==` is not one:
// `undefined == undefined` is undefined, so an undefined value is not equal to itself and
// belongs in no group, and `4 == "4"` is error rather than "different". `=?=` is total and
// reflexive, which is why it is the relation here -- the same reason SQL defines GROUP BY on
// "is not distinct from" rather than on `=`.
//
// Rendering the group was doing neither. ValueText renders for DISPLAY, so integer 4 and
// string "4" both became "4" and were merged, as were true/"true" and undefined/"undefined";
// eight stored values produced five groups where `=?=` makes seven. Nothing failed: a merged
// group reports a count that is the sum of two, under a label naming one of them.

// groupRelationCases are the pairs that define the rule. The sameGroup column is not a
// judgement about what grouping ought to do -- it is what `a =?= b` evaluates to, and the test
// checks that premise against the evaluator rather than trusting this table.
var groupRelationCases = []struct {
	a, b      string
	sameGroup bool
	why       string
}{
	{`undefined`, `undefined`, true, `=?= is reflexive where == answers undefined`},
	{`error`, `error`, true, `likewise for error`},
	{`4`, `"4"`, false, `4 =?= "4" is false; == answers error, which is not "different"`},
	{`true`, `"true"`, false, `a bool is not the word`},
	{`undefined`, `"undefined"`, false, `an undefined value is not the word`},
	{`"alice"`, `"Alice"`, false, `=?= is case-SENSITIVE, unlike ==`},
	{`"alice"`, `"alice"`, true, `the same string`},
	{`true`, `1`, false, `== calls these equal; =?= keeps the types apart`},
	{`false`, `0`, false, `likewise`},
	{`1`, `1.0`, true, `one quantity written two ways; =?= splits them and should not`},
	{`2.0`, `2`, true, `in either order`},
	{`1`, `1.5`, false, `plainly different numbers`},
	{`4`, `4`, true, `the same integer`},
	{`"a"`, `"b"`, false, `plainly different strings`},
}

// TestGroupRelationMatchesTheLanguage checks the renderer against the evaluator directly. It is the
// only place the rule comes from, and the only test here that can tell a wrong rule from a
// wrong rendering.
func TestGroupRelationMatchesTheLanguage(t *testing.T) {
	for _, tc := range groupRelationCases {
		av, bv := evalLiteral(t, tc.a), evalLiteral(t, tc.b)
		same := GroupValueText(av) == GroupValueText(bv)
		if same != tc.sameGroup {
			t.Errorf("GroupValueText(%s)=%q and GroupValueText(%s)=%q %s, want %s (%s)",
				tc.a, GroupValueText(av), tc.b, GroupValueText(bv),
				agreeWord(same), agreeWord(tc.sameGroup), tc.why)
		}
		// Where an operator DOES answer, the case has to agree with it, so a case cannot
		// drift into pinning the renderer to something the language disagrees with. Numbers
		// are the one place the relation is `==` rather than `=?=`, and the table says so.
		op := "=?="
		if isNumericPair(t, tc.a, tc.b) {
			op = "=="
		}
		if got, answered := boolOf(t, tc.a+" "+op+" "+tc.b); answered && got != tc.sameGroup {
			t.Errorf("the case is wrong: %s %s %s is %v, but the case says sameGroup=%v",
				tc.a, op, tc.b, got, tc.sameGroup)
		}
	}
}

// TestGroupRenderingIsFaithful pins the property that lets ONE rendering be both the key and
// the label: values in different groups never read alike.
//
// Without it the key could be made finer than the label -- which is what a type-tagged key
// over ValueText's labels did -- and the result is two output rows with the same name, which
// no caller can tell apart. TestArchiveGroupedStringColumn catches that one with the words
// "group "undefined" appears twice".
func TestGroupRenderingIsFaithful(t *testing.T) {
	seen := map[string]string{}
	for _, src := range []string{
		`4`, `"4"`, `true`, `false`, `"true"`, `undefined`, `"undefined"`, `error`, `"error"`,
		`"alice"`, `"Alice"`, `1`, `1.5`, `0`, `{1,2}`, `[a=1]`,
	} {
		got := GroupValueText(evalLiteral(t, src))
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s both render %q, so two groups would share one name", prev, src, got)
			continue
		}
		seen[got] = src
	}
}

// TestIntegralRealReadsAsItsInteger pins the one rendering the canonical unparser does not
// give on its own, and the reason it must not go through float64.
func TestIntegralRealReadsAsItsInteger(t *testing.T) {
	// 1e18 is the case that matters and the one a small fixture misses: 'g' renders a small
	// integral real as its integer anyway ("3"), so only a magnitude where 'g' reaches for an
	// exponent shows whether the rule is there. A unix time in nanoseconds is 1.7e18.
	for _, f := range []float64{1, 0, -0, 2, -7, 1e15, 1e18, 1.7e18} {
		got := GroupValueText(classad.NewRealValue(f))
		want := GroupValueText(classad.NewIntValue(int64(f)))
		if got != want {
			t.Errorf("real %v renders %q but the integer renders %q; they are one group", f, got, want)
		}
	}
	if got := GroupValueText(classad.NewRealValue(1.5)); got != "1.5" {
		t.Errorf("1.5 renders %q, want unchanged", got)
	}
	// A real at or past 2^63 has no int64, and int64() of one is implementation-defined in
	// Go, so the conversion must not be reached. It renders as a float instead.
	for _, f := range []float64{1 << 63, -(1 << 63) * 2, 1e300} {
		got := GroupValueText(classad.NewRealValue(f))
		if !strings.ContainsAny(got, ".eE+") {
			t.Errorf("real %v renders %q; it went through an int64 conversion it cannot survive", f, got)
		}
	}
	// MinInt64 itself IS representable, so it still renders as its integer.
	if got, want := GroupValueText(classad.NewRealValue(-(1 << 63))), GroupValueText(classad.NewIntValue(math.MinInt64)); got != want {
		t.Errorf("real MinInt64 renders %q but the integer renders %q", got, want)
	}
	// Two integers one float64 apart are two numbers, so the key must not round them
	// together -- which is what rendering an integer through 'g' would do.
	a := GroupValueText(classad.NewIntValue(9007199254740993))
	b := GroupValueText(classad.NewIntValue(9007199254740992))
	if a == b {
		t.Errorf("9007199254740993 and 9007199254740992 both render %q; the key went through float64", a)
	}
}

// groupFixtureValues are the eight stored values the end-to-end tests group over. They are
// pairwise distinct under `=?=` -- including "alice"/"Alice", since `=?=` is case-sensitive --
// so each is its own group.
var groupFixtureValues = []string{
	`4`, `"4"`, `true`, `"true"`, `"alice"`, `"Alice"`, `undefined`, `"undefined"`,
}

// Before this they produced FIVE groups: 4/"4", true/"true" and undefined/"undefined" each
// merged, because ValueText renders both members of each pair the same way.
const groupFixtureWantGroups = 8

func TestGroupByDoesNotMergeDistinctValues(t *testing.T) {
	_, d := groupFixture(t)
	rows, err := d.AggregateColsStats("true", []GroupCol{{Attr: "K"}},
		[]AggSpec{{Func: AggCount, Arg: "*"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != groupFixtureWantGroups {
		t.Fatalf("got %d groups, want %d\n%s", len(rows), groupFixtureWantGroups, describeGroups(rows))
	}
	for _, r := range rows {
		if r.Values[0] != "1" {
			t.Errorf("group %v has count %s; every stored value is distinct under =?=",
				r.Group, r.Values[0])
		}
	}
}

// TestCountDistinctUsesTheSameRelation: COUNT(DISTINCT) partitions by the same relation and
// had the same bug. Seven of the eight values are defined, and all seven are distinct.
func TestCountDistinctUsesTheSameRelation(t *testing.T) {
	_, d := groupFixture(t)
	rows, err := d.AggregateColsStats("true", nil,
		[]AggSpec{{Func: AggCountDistinct, Arg: "K"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[0].Values[0]; got != "7" {
		t.Errorf("COUNT(DISTINCT K) = %s, want 7 (eight values, one of them undefined)", got)
	}
}

// TestArchiveGroupsAsTheMutableTableDoes pins the two paths against each other. They render
// group keys in different files, and a fix applied to one is a divergence rather than a fix --
// which is this repo's recurring shape.
func TestArchiveGroupsAsTheMutableTableDoes(t *testing.T) {
	cat, d := groupFixture(t)
	at, err := cat.CreateArchiveTable("h", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range groupFixtureValues {
		ad, perr := classad.Parse(fmt.Sprintf(`[ K = %s ]`, src))
		if perr != nil {
			t.Fatal(perr)
		}
		if aerr := at.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	arc, err := at.Aggregate("true", []string{"K"}, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	mut, err := d.AggregateColsStats("true", []GroupCol{{Attr: "K"}},
		[]AggSpec{{Func: AggCount, Arg: "*"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedGroups(arc), sortedGroups(mut); got != want {
		t.Errorf("archive groups %s\nmutable groups %s", got, want)
	}
}

func groupFixture(t *testing.T) (*Catalog, *DB) {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	d, err := cat.CreateTable("t")
	if err != nil {
		t.Fatal(err)
	}
	for i, src := range groupFixtureValues {
		ad, perr := classad.Parse(fmt.Sprintf(`[ K = %s ]`, src))
		if perr != nil {
			t.Fatal(perr)
		}
		if werr := d.Put(fmt.Sprintf("k%d", i), ad); werr != nil {
			t.Fatal(werr)
		}
	}
	return cat, d
}

func evalLiteral(t *testing.T, src string) classad.Value {
	t.Helper()
	ex, err := classad.ParseExpr(src)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return ex.Eval(classad.New())
}

// boolOf evaluates src, reporting answered=false when the operator declines -- which is the
// whole trouble with `==`: it answers undefined or error for exactly the pairs a group key
// most needs an answer about.
func boolOf(t *testing.T, src string) (val, answered bool) {
	t.Helper()
	v := evalLiteral(t, src)
	if !v.IsBool() {
		return false, false
	}
	b, err := v.BoolValue()
	if err != nil {
		t.Fatal(err)
	}
	return b, true
}

func isNumericPair(t *testing.T, a, b string) bool {
	t.Helper()
	return isNumber(evalLiteral(t, a)) && isNumber(evalLiteral(t, b))
}

func isNumber(v classad.Value) bool { return v.IsInteger() || v.IsReal() }

func agreeWord(b bool) string {
	if b {
		return "agree"
	}
	return "differ"
}

func describeGroups(rows []AggRow) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "  %v -> %v\n", r.Group, r.Values)
	}
	return b.String()
}

// sortedGroups renders the groups order-independently: the two paths need not agree on the
// order groups come out in, only on which groups there are and what they count.
func sortedGroups(rows []AggRow) string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%v=%v", r.Group, r.Values))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}
