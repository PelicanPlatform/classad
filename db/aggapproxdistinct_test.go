package db

import (
	"fmt"
	"math"
	"strconv"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// approxDistinctAggregate presents the engine's own sketch accumulator as an Aggregate, which
// is the only thing CheckAggregate knows how to test. It adds nothing: NewAccumulator returns
// the type AggApproxCountDistinct reduces with, so the laws below are checked against the code
// that runs in a query rather than against a restatement of it that could drift from it.
type approxDistinctAggregate struct{}

func (approxDistinctAggregate) NewAccumulator() Accumulator { return newApproxDistinctAcc() }

// TestApproxCountDistinctMergeLaws is the one that matters most and the one a hand-written test
// would get wrong. The columnar pass fans out across segment windows and folds a partial state
// per worker, and which worker draws which segment is a race -- so a merge that is merely
// associative, or associative and commutative only for the orders its author happened to try, is
// right in every test and intermittently wrong in production, where nothing in the answer says
// so. CheckAggregate runs the laws over splits and orders the author did not choose.
//
// The comparison is EXACT (CheckAggregate rather than CheckAggregateEqual with a tolerance), and
// that is a deliberate, stronger claim: a register-wise maximum over fixed hashes produces the
// same register array whatever the split, so the estimator runs over identical input and returns
// identical bits. A sketch needing slack here would be one whose merge loses information, which
// is the failure the check exists to catch.
func TestApproxCountDistinctMergeLaws(t *testing.T) {
	// The values an approximate distinct count really sees, including the awkward ones:
	// repeats (which must raise a register once, not twice), a string and the number it
	// spells, an integer and the real equal to it, undefined and error.
	samples := []classad.Value{
		classad.NewStringValue("alice"), classad.NewStringValue("bob"),
		classad.NewStringValue("alice"), classad.NewStringValue("carol"),
		classad.NewStringValue("4"), classad.NewIntValue(4),
		classad.NewIntValue(1), classad.NewRealValue(1.0),
		classad.NewIntValue(7), classad.NewIntValue(8), classad.NewIntValue(9),
		classad.NewRealValue(1.5), classad.NewBoolValue(true),
		classad.NewUndefinedValue(), classad.NewErrorValue(),
	}
	if err := CheckAggregate(approxDistinctAggregate{}, samples); err != nil {
		t.Fatalf("the per-group sketch breaks a law the parallel reduction relies on: %v", err)
	}
}

// TestApproxCountDistinctMergeIsTheUnion pins what the merge MEANS, which the laws alone do not
// say: a value both sides saw is one value, not two. An implementation that added its sides'
// estimates would satisfy associativity and commutativity and still double-count every value
// two segments share -- the exact thing a distinct count exists to avoid.
func TestApproxCountDistinctMergeIsTheUnion(t *testing.T) {
	left, right := newApproxDistinctAcc(), newApproxDistinctAcc()
	for i := range 20000 {
		left.Add(classad.NewStringValue("v" + strconv.Itoa(i)))
	}
	for i := 10000; i < 30000; i++ { // half of them are the same values left saw
		right.Add(classad.NewStringValue("v" + strconv.Itoa(i)))
	}
	left.Merge(right)
	// 30000 distinct values across the two, not 40000 additions.
	assertApproxWithin(t, "the union of two overlapping halves", left.estimate(), 30000)
}

// assertApproxWithin checks an estimate against the truth at three standard errors.
//
// THE TEST CANNOT FLAP, and not because the bound is wide. The sketch hashes with a fixed
// function (hashApproxDistinctKey takes no seed and Go's map iteration never reaches it), so a
// given set of values produces one register array and one estimate on every run, on every
// platform and in every process. The bound is three standard errors because that is the claim
// worth pinning -- an estimate that drifts outside it is a broken sketch rather than an unlucky
// one -- and the observed error is logged, so a change that degrades accuracy while staying
// inside the bound is still visible to whoever reads the run.
func assertApproxWithin(t *testing.T, what string, got uint64, want int) {
	t.Helper()
	rel := math.Abs(float64(got)-float64(want)) / float64(want)
	bound := 3 * ApproxDistinctRelErr
	t.Logf("%s: estimate %d for %d distinct values, relative error %.4f%% (bound %.4f%%)",
		what, got, want, 100*rel, 100*bound)
	if rel > bound {
		t.Errorf("%s: estimate %d for %d distinct values is %.3f%% out, past the %.3f%% bound",
			what, got, want, 100*rel, 100*bound)
	}
}

// TestApproxCountDistinctAccuracy pins the accuracy the aggregate's documentation promises, over
// four orders of magnitude of cardinality. The small end is where linear counting takes over
// from the raw estimator, and it is exact there; the large end is the case the aggregate exists
// for, where the exact count would be holding a million strings.
func TestApproxCountDistinctAccuracy(t *testing.T) {
	for _, n := range []int{100, 1000, 10000, 100000, 1000000} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			acc := newApproxDistinctAcc()
			for i := range n {
				// Keys that differ only in their last characters, which is what a job id
				// or a host name looks like and what a hash with a weak finalizer
				// collapses.
				acc.Add(classad.NewStringValue(fmt.Sprintf("host%08d.example.org", i)))
			}
			assertApproxWithin(t, "distinct string values", acc.estimate(), n)
		})
	}
}

// TestApproxCountDistinctMemoryIsFlat is the property the whole aggregate is for: the state is
// the same size for a thousand distinct values as for a million, where the exact count's grows
// with its own answer.
func TestApproxCountDistinctMemoryIsFlat(t *testing.T) {
	small, large := newApproxDistinctAcc(), newApproxDistinctAcc()
	for i := range 1000 {
		small.Add(classad.NewIntValue(int64(i)))
	}
	for i := range 1000000 {
		large.Add(classad.NewIntValue(int64(i)))
	}
	if len(small.reg) != len(large.reg) || len(large.reg) != approxDistinctRegisters {
		t.Fatalf("the sketch is meant to be a fixed %d registers whatever the cardinality; got %d and %d",
			approxDistinctRegisters, len(small.reg), len(large.reg))
	}
	// Stated so the cost is in the test rather than only in a comment: one group of one
	// approximate aggregate is this many bytes.
	t.Logf("%d registers, %d bytes per group per aggregate", approxDistinctRegisters, len(large.reg))
}

// TestApproxCountDistinctDoesNotDisplaceTheExactOne is the refusal the issue behind this
// aggregate turns on. A request carrying both gets a true count from AggCountDistinct and an
// estimate from AggApproxCountDistinct, at a cardinality where the two visibly differ -- so a
// change that quietly routed the exact function through the sketch to save memory fails here.
func TestApproxCountDistinctDoesNotDisplaceTheExactOne(t *testing.T) {
	const n = 50000
	rows := make([][]classad.Value, 0, n)
	for i := range n {
		rows = append(rows, []classad.Value{classad.NewStringValue("job" + strconv.Itoa(i))})
	}
	aggs := []AggSpec{
		{Func: AggCountDistinct, Arg: "ID"},
		{Func: AggApproxCountDistinct, Arg: "ID"},
	}
	attrs, groupCol, aggCol := AggProjection(nil, aggs)
	out, err := AggregateValues(rowSeq(rows), attrs, nil, aggs, groupCol, aggCol, nil)
	if err != nil {
		t.Fatalf("AggregateValues: %v", err)
	}
	if got := out[0].Values[0]; got != strconv.Itoa(n) {
		t.Errorf("COUNT(DISTINCT) = %s, want the exact %d: the exact function must never be answered from a sketch", got, n)
	}
	est, err := strconv.Atoi(out[0].Values[1])
	if err != nil {
		t.Fatalf("the approximate column is not a number: %q", out[0].Values[1])
	}
	// This cardinality sits just above the linear-counting handover, where the raw estimator
	// reads high (see approxDistinctAcc.estimate), so the observed error is near the bound
	// rather than near zero. It is fixed, not lucky: the same values hash the same way every
	// run.
	assertApproxWithin(t, "through the reduction engine", uint64(est), n)
}

// TestApproxCountDistinctComposes is the reason the sketch is built during the scan instead of
// merged out of the persisted per-segment ones: those cover a whole segment and an indexed
// attribute, so they could answer none of the three shapes below. Each is checked against the
// EXACT function in the same request, which is the oracle -- at these cardinalities linear
// counting is exact, so the two must agree digit for digit and a disagreement is a real fault
// rather than the error bar.
func TestApproxCountDistinctComposes(t *testing.T) {
	a := distinctArchive(t)

	t.Run("ungrouped", func(t *testing.T) {
		assertExactAndApproxAgree(t, a, "true", nil, "Host", "")
	})
	t.Run("constrained", func(t *testing.T) {
		assertExactAndApproxAgree(t, a, `Owner == "alice"`, nil, "Host", "")
	})
	t.Run("grouped", func(t *testing.T) {
		assertExactAndApproxAgree(t, a, "true", []string{"Owner"}, "Host", "")
	})
	t.Run("per-aggregate filter", func(t *testing.T) {
		assertExactAndApproxAgree(t, a, "true", []string{"Owner"}, "Host", "JobStatus == 4")
	})
	t.Run("constrained and grouped and filtered", func(t *testing.T) {
		assertExactAndApproxAgree(t, a, `JobStatus == 4 || JobStatus == 2`, []string{"Owner"}, "Host", "JobStatus == 2")
	})
}

// assertExactAndApproxAgree runs COUNT(DISTINCT attr) and its approximate twin side by side under
// the same constraint, grouping and per-aggregate filter, and requires one answer.
func assertExactAndApproxAgree(t *testing.T, a *ArchiveTable, constraint string, groupBy []string, attr, filter string) {
	t.Helper()
	rows, err := a.Aggregate(constraint, groupBy, []AggSpec{
		{Func: AggCountDistinct, Arg: attr, Filter: filter},
		{Func: AggApproxCountDistinct, Arg: attr, Filter: filter},
	})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows; the comparison would be vacuous")
	}
	for _, r := range rows {
		if r.Values[0] != r.Values[1] {
			t.Errorf("group %v: COUNT(DISTINCT %s) = %s but the approximate one = %s",
				r.Group, attr, r.Values[0], r.Values[1])
		}
	}
}

// TestApproxCountDistinctTimeSeries covers the bucketed GROUP BY a dashboard asks for -- the
// shape whose group key is computed from the row rather than read off it -- against the exact
// function as the oracle.
func TestApproxCountDistinctTimeSeries(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("h", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	// Three days, each with a different number of distinct hosts.
	const day = 86400
	rows := []struct {
		t    int64
		host string
	}{
		{day, "a"}, {day + 10, "a"}, {day + 20, "b"},
		{2 * day, "c"}, {2*day + 5, "d"}, {2*day + 6, "e"},
		{3 * day, "f"},
	}
	for i, r := range rows {
		if appendErr := a.AppendOld(fmt.Sprintf("CompletionDate = %d\nHost = %q\nID = %d\n", r.t, r.host, i)); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	out, err := a.AggregateCols("true", []GroupCol{{Attr: "CompletionDate", BucketWidth: day}}, []AggSpec{
		{Func: AggCountDistinct, Arg: "Host"},
		{Func: AggApproxCountDistinct, Arg: "Host"},
	})
	if err != nil {
		t.Fatalf("AggregateCols: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("want one row per day, got %d: %+v", len(out), out)
	}
	for _, r := range out {
		if r.Values[0] != r.Values[1] {
			t.Errorf("bucket %v: exact %s, approximate %s", r.Group, r.Values[0], r.Values[1])
		}
	}
}

// TestApproxCountDistinctSkipsUndefined checks that a row not carrying the attribute is not a
// distinct value of it, as it is not for the exact function: otherwise every group with a
// missing value counts one more than it has.
func TestApproxCountDistinctSkipsUndefined(t *testing.T) {
	rows := [][]classad.Value{
		{classad.NewStringValue("a")},
		{classad.NewStringValue("b")},
		{classad.NewUndefinedValue()},
		{classad.NewErrorValue()},
	}
	aggs := []AggSpec{{Func: AggApproxCountDistinct, Arg: "Host"}}
	attrs, groupCol, aggCol := AggProjection(nil, aggs)
	out, err := AggregateValues(rowSeq(rows), attrs, nil, aggs, groupCol, aggCol, nil)
	if err != nil {
		t.Fatalf("AggregateValues: %v", err)
	}
	if out[0].Values[0] != "2" {
		t.Errorf("approximate distinct = %s, want 2: undefined and error are not values", out[0].Values[0])
	}
}

// TestApproxCountDistinctEmptyGroup covers the group every one of whose rows its own filter
// excludes. It still appears -- that is what a per-aggregate filter means -- and its answer is
// zero, which the sketch has to give without ever having been allocated.
func TestApproxCountDistinctEmptyGroup(t *testing.T) {
	rows := [][]classad.Value{
		{classad.NewStringValue("alice"), classad.NewStringValue("a"), classad.NewIntValue(2)},
		{classad.NewStringValue("alice"), classad.NewStringValue("b"), classad.NewIntValue(2)},
	}
	groups := []GroupCol{{Attr: "Owner"}}
	aggs := []AggSpec{{Func: AggApproxCountDistinct, Arg: "Host", Filter: "JobStatus == 4"}}
	attrs, groupCol, aggCol := AggProjection(groups, aggs)
	out, err := AggregateValues(rowSeq(rows), attrs, groups, aggs, groupCol, aggCol, nil)
	if err != nil {
		t.Fatalf("AggregateValues: %v", err)
	}
	if len(out) != 1 || out[0].Values[0] != "0" {
		t.Fatalf("a group whose rows all fail the filter should report 0, got %+v", out)
	}
}

// TestApproxCountDistinctStar is refused for the reason the exact one is: a row is not a value.
func TestApproxCountDistinctStar(t *testing.T) {
	a := distinctArchive(t)
	if _, err := a.Aggregate("true", nil, []AggSpec{{Func: AggApproxCountDistinct, Arg: "*"}}); err == nil {
		t.Error("APPROX_COUNT_DISTINCT(*) should be refused")
	}
}

// TestApproxCountDistinctNumericIdentity checks that the sketch keys values the way the group
// tuple does, so an approximate distinct count and a GROUP BY over the same attribute count the
// same partition -- 1 and 1.0 being one value, "4" and 4 being two.
func TestApproxCountDistinctNumericIdentity(t *testing.T) {
	acc := newApproxDistinctAcc()
	for _, v := range []classad.Value{
		classad.NewIntValue(1), classad.NewRealValue(1.0), classad.NewIntValue(2),
		classad.NewStringValue("2"),
	} {
		acc.Add(v)
	}
	if got := acc.estimate(); got != 3 {
		t.Errorf("estimate = %d, want 3: 1 and 1.0 are one value, 2 and \"2\" are two", got)
	}
}

// TestApproxCountDistinctHashSpreadsSimilarKeys guards the finalizer. Short keys differing in
// their last character are what this aggregate is pointed at, and a hash whose top bits barely
// move between them crowds the register array -- which shows up not as an error but as an
// estimate that is quietly, consistently low.
func TestApproxCountDistinctHashSpreadsSimilarKeys(t *testing.T) {
	seen := map[uint64]int{}
	const n = 4096
	for i := range n {
		// Only the register index matters here: that is the part a weak hash collapses.
		seen[hashApproxDistinctKey("v"+strconv.Itoa(i))>>(64-approxDistinctPrecision)]++
	}
	// With n keys over approxDistinctRegisters buckets the expected occupancy is high; a hash
	// that did not mix would put them in a handful of buckets. Half the ideal is far below
	// anything a working hash produces and far above anything a broken one does.
	ideal := float64(approxDistinctRegisters) * (1 - math.Exp(-float64(n)/float64(approxDistinctRegisters)))
	if float64(len(seen)) < ideal/2 {
		t.Errorf("%d keys landed in %d of %d registers; a mixing hash fills about %.0f",
			n, len(seen), approxDistinctRegisters, ideal)
	}
}
