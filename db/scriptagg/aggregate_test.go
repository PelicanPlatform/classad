package scriptagg_test

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/db"
	"github.com/PelicanPlatform/classad/db/scriptagg"
)

// avgSrc is the worked example, and the one that could not be written before a module function
// could build a list: AVG needs a sum AND a count at once, so its state is two numbers, and a
// scalar state carries one.
//
// It skips a value that is not a number, which is the same decision the built-in SUM and AVG make
// and is visible here as a line of script rather than as engine behaviour.
const avgSrc = `
def init():
    return {0, 0}

def add(s, v):
    return isReal(v) || isInteger(v) ? {s[0] + v, s[1] + 1} : s

def merge(a, b):
    return {a[0] + b[0], a[1] + b[1]}

def result(s):
    return s[1] > 0 ? real(s[0]) / s[1] : undefined
`

// secondSrc is the shape whose merge LOOKS order-sensitive: a top-K selection, K being 2, whose
// answer is the second largest value in the group.
//
// Nothing about it is a running total. Each partial state is the two largest values that worker
// saw, and merging two of them has to pick the two largest of four while keeping track of which
// slots are empty -- an operation a reader has every reason to suspect of caring which side is
// which. It does not, and CheckAggregate is how that stops being a claim. A reservoir has the same
// shape and the same suspicion; this one is deterministic, so a test can state its answer.
//
// UNDEFINED IS THE EMPTY SLOT, which is also why this one exercises the awkward values: an
// undefined in a slot and an undefined arriving from a row that does not carry the attribute meet
// in the same comparisons.
const secondSrc = `
def best(x, y):
    return isUndefined(x) ? y : (isUndefined(y) ? x : (x > y ? x : y))

def rest(x, y):
    return isUndefined(x) ? undefined : (isUndefined(y) ? undefined : (x > y ? y : x))

def init():
    return {undefined, undefined}

def merge(a, b):
    return isUndefined(b[0]) ? a : (isUndefined(a[0]) ? b : {best(a[0], b[0]), best(rest(a[0], b[0]), a[0] >= b[0] ? a[1] : b[1])})

def add(s, v):
    return isReal(v) || isInteger(v) ? merge(s, {v, undefined}) : s

def result(s):
    return s[1]
`

// brokenSrc is an aggregate whose merge keeps the side it was called on and drops the other. It is
// right for a serial fold, right for a query that happened not to fan out, and wrong the moment one
// does -- which is exactly the failure CheckAggregate exists to catch before a caller ships it.
const brokenSrc = `
def init():
    return 0

def add(s, v):
    return isReal(v) || isInteger(v) ? s + v : s

def merge(a, b):
    return a

def result(s):
    return s
`

func mustModule(t *testing.T, src string) *vm.ScriptModule {
	t.Helper()
	m, err := vm.ParseScriptModule(src)
	if err != nil {
		t.Fatalf("ParseScriptModule: %v", err)
	}
	return m
}

func mustAggregate(t *testing.T, src string, spec scriptagg.Spec) *scriptagg.Aggregate {
	t.Helper()
	a, err := scriptagg.New(mustModule(t, src), spec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// samples are the values a row can genuinely hold, not just the ones an aggregate was written for.
// A row may not carry the attribute (undefined), its expression may have failed (error), and the
// attribute may be a string on some ads and a number on others.
func samples() []classad.Value {
	return []classad.Value{
		classad.NewIntValue(4),
		classad.NewIntValue(1),
		classad.NewRealValue(2.5),
		classad.NewIntValue(9),
		classad.NewStringValue("lots"),
		classad.NewUndefinedValue(),
		classad.NewErrorValue(),
		classad.NewIntValue(7),
	}
}

func fold(a db.Aggregate, vals []classad.Value) db.Accumulator {
	acc := a.NewAccumulator()
	for _, v := range vals {
		acc.Add(v)
	}
	return acc
}

// goAvg is the Go aggregate avgSrc imitates, written the way anyone would write it, so that the
// script's answers have something to be compared against that is not another script.
type goAvg struct{}

func (goAvg) NewAccumulator() db.Accumulator { return &goAvgAcc{} }

type goAvgAcc struct {
	sum float64
	n   int64
}

func (a *goAvgAcc) Add(v classad.Value) {
	if !v.IsInteger() && !v.IsReal() {
		return
	}
	x, err := v.NumberValue()
	if err != nil {
		return
	}
	a.sum += x
	a.n++
}

func (a *goAvgAcc) Merge(other db.Accumulator) {
	b := other.(*goAvgAcc)
	a.sum += b.sum
	a.n += b.n
}

func (a *goAvgAcc) Result() classad.Value {
	if a.n == 0 {
		return classad.NewUndefinedValue()
	}
	return classad.NewRealValue(a.sum / float64(a.n))
}

// TestScriptAvgMatchesTheGoAggregate is the parity statement: the same rows, the same splits, the
// same answers.
//
// It compares over several divisions of the rows rather than only the serial fold, because the two
// implementations differ in where the arithmetic happens -- the script sums in ClassAd integers and
// divides once, the Go one sums in float64 -- and a split is where that difference would show.
func TestScriptAvgMatchesTheGoAggregate(t *testing.T) {
	script := mustAggregate(t, avgSrc, scriptagg.Spec{})
	vals := samples()
	for _, k := range []int{0, 1, 3, len(vals) - 1, len(vals)} {
		got := mergedAt(script, vals, k)
		want := mergedAt(goAvg{}, vals, k)
		if !sameNumber(got, want) {
			t.Errorf("split at %d: script says %s, the Go aggregate says %s",
				k, db.ValueText(got), db.ValueText(want))
		}
	}
}

// mergedAt folds vals[:k] and vals[k:] separately, merges, and renders.
func mergedAt(a db.Aggregate, vals []classad.Value, k int) classad.Value {
	left := fold(a, vals[:k])
	left.Merge(fold(a, vals[k:]))
	return left.Result()
}

func sameNumber(a, b classad.Value) bool {
	x, xerr := a.NumberValue()
	y, yerr := b.NumberValue()
	if xerr != nil || yerr != nil {
		return db.ValueText(a) == db.ValueText(b)
	}
	return math.Abs(x-y) <= 1e-12*math.Max(1, math.Abs(x))
}

// TestCheckAggregateAcceptsBothWorkedAggregates runs the engine's own law checker, which is the
// only thing here that tests associativity and commutativity rather than assuming them.
func TestCheckAggregateAcceptsBothWorkedAggregates(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"avg", avgSrc},
		{"second largest", secondSrc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := db.CheckAggregate(mustAggregate(t, tc.src, scriptagg.Spec{}), samples()); err != nil {
				t.Fatalf("CheckAggregate: %v", err)
			}
		})
	}
}

// TestCheckAggregateAcceptsAColumnarOptIn is the second half of the check, reached only for an
// aggregate that declared itself fit for the columnar pass: AddNum must agree with Add over the
// same numbers, and must not care what order they arrive in.
func TestCheckAggregateAcceptsAColumnarOptIn(t *testing.T) {
	a := mustAggregate(t, avgSrc, scriptagg.Spec{Columnar: true})
	if !a.Columnar() {
		t.Fatal("Columnar() is false for an aggregate that declared it")
	}
	if _, ok := a.NewAccumulator().(db.ColumnAccumulator); !ok {
		t.Fatal("a columnar aggregate's accumulator does not satisfy db.ColumnAccumulator")
	}
	if err := db.CheckAggregate(a, samples()); err != nil {
		t.Fatalf("CheckAggregate: %v", err)
	}
}

// TestColumnarIsNotDeclaredByAccident pins the other half of that promise. An aggregate that did
// not opt in must not satisfy the interface, because the engine reads the interface AS the opt-in
// and there is nothing else for it to read.
func TestColumnarIsNotDeclaredByAccident(t *testing.T) {
	a := mustAggregate(t, avgSrc, scriptagg.Spec{})
	if a.Columnar() {
		t.Fatal("Columnar() is true for an aggregate that did not declare it")
	}
	if _, ok := a.NewAccumulator().(db.ColumnAccumulator); ok {
		t.Fatal("an aggregate that did not opt in satisfies db.ColumnAccumulator anyway")
	}
}

// TestCheckAggregateRejectsABrokenMerge is the test of the test. A checker that accepts everything
// reads like a check and is not one, so the suite has to contain something it refuses.
func TestCheckAggregateRejectsABrokenMerge(t *testing.T) {
	a := mustAggregate(t, brokenSrc, scriptagg.Spec{})
	// The serial fold is right, which is the whole trap: nothing short of a merge finds this.
	if got := db.ValueText(fold(a, samples()).Result()); got != "23.5" {
		t.Fatalf("the serial fold is %s, want 23.5 -- the fixture is not doing what it claims", got)
	}
	err := db.CheckAggregate(a, samples())
	if err == nil {
		t.Fatal("CheckAggregate accepted an aggregate whose merge drops one side")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "merge") {
		t.Errorf("the refusal does not mention the merge: %v", err)
	}
}

// TestMergeAcrossManyPartialStates is what the fan-out actually does: more than two partial states,
// combined in an order nothing promises. Both directions of every pair and both associations of
// every triple have to give the one answer.
func TestMergeAcrossManyPartialStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"avg", avgSrc},
		{"second largest", secondSrc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := mustAggregate(t, tc.src, scriptagg.Spec{})
			vals := samples()
			want := db.ValueText(fold(a, vals).Result())

			// Four parts, which is one more than the three-way split CheckAggregate makes, and
			// enough for a permutation to regroup rather than merely swap.
			parts := [][]classad.Value{vals[:2], vals[2:4], vals[4:6], vals[6:]}
			for _, order := range [][]int{
				{0, 1, 2, 3}, {3, 2, 1, 0}, {2, 0, 3, 1}, {1, 3, 0, 2},
			} {
				acc := a.NewAccumulator()
				for _, i := range order {
					acc.Merge(fold(a, parts[i]))
				}
				if got := db.ValueText(acc.Result()); got != want {
					t.Errorf("merging in order %v gives %s, the serial fold gives %s", order, got, want)
				}
			}

			// A tree reduction rather than a left fold, which is the other shape a fan-out can
			// take and the one that tells associativity from commutativity.
			left, right := a.NewAccumulator(), a.NewAccumulator()
			left.Merge(fold(a, parts[0]))
			left.Merge(fold(a, parts[1]))
			right.Merge(fold(a, parts[2]))
			right.Merge(fold(a, parts[3]))
			right.Merge(left)
			if got := db.ValueText(right.Result()); got != want {
				t.Errorf("a tree reduction gives %s, the serial fold gives %s", got, want)
			}
		})
	}
}

// TestEmptyGroupReachesInitAlone covers the state no row ever touches. A group that contributed
// nothing still has an answer, it comes from init by way of result, and asking twice gives the same
// one.
func TestEmptyGroupReachesInitAlone(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"avg", avgSrc, "undefined"},
		{"second largest", secondSrc, "undefined"},
		{"a scalar state", brokenSrc, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acc := mustAggregate(t, tc.src, scriptagg.Spec{}).NewAccumulator()
			if got := db.ValueText(acc.Result()); got != tc.want {
				t.Fatalf("an empty group answers %s, want %s", got, tc.want)
			}
			if again := db.ValueText(acc.Result()); again != tc.want {
				t.Fatalf("the second Result answers %s, want %s", again, tc.want)
			}
		})
	}
}

// TestEveryAccumulatorStartsFromTheIdentity pins that the identity is shared without being shared
// mutable state: one accumulator's folding must not move another's starting point, which is the
// failure mode of caching a value that something writes through.
func TestEveryAccumulatorStartsFromTheIdentity(t *testing.T) {
	a := mustAggregate(t, avgSrc, scriptagg.Spec{})
	first := fold(a, samples())
	second := a.NewAccumulator()
	if got := db.ValueText(second.Result()); got != "undefined" {
		t.Fatalf("an accumulator made after another folded answers %s, want undefined", got)
	}
	if got := db.ValueText(first.Result()); got != "4.7" {
		t.Fatalf("the first accumulator answers %s after a second was made, want 4.7", got)
	}
}

// TestUndefinedAndErrorReachAdd states what the engine hands an aggregate, because it is more than
// the values the aggregate was written for. The script decides what to do with them; this pins that
// it is the script deciding and not something upstream filtering.
func TestUndefinedAndErrorReachAdd(t *testing.T) {
	// A counting aggregate that ignores nothing, so what reaches Add is countable.
	const countAll = `
def init():
    return 0

def add(s, v):
    return s + 1

def merge(a, b):
    return a + b

def result(s):
    return s
`
	a := mustAggregate(t, countAll, scriptagg.Spec{})
	if got := db.ValueText(fold(a, samples()).Result()); got != strconv.Itoa(len(samples())) {
		t.Fatalf("Add saw %s of %d values", got, len(samples()))
	}

	// And the aggregates that DO ignore them ignore exactly those: four numbers of eight values.
	avg := mustAggregate(t, avgSrc, scriptagg.Spec{})
	acc := avg.NewAccumulator()
	for _, v := range samples() {
		acc.Add(v)
	}
	if got := db.ValueText(acc.Result()); got != "4.7" {
		t.Fatalf("avg over the sample is %s, want 4.7 -- the mean of 4, 1, 2.5, 9 and 7", got)
	}
}

// TestResultFollowsTheStateItWasAskedAbout covers the other half of the Result contract. The
// answer is cached so that two calls agree, which is only right if everything that can change the
// state drops the cache -- a Result read before the last row would otherwise be the answer.
func TestResultFollowsTheStateItWasAskedAbout(t *testing.T) {
	a := mustAggregate(t, avgSrc, scriptagg.Spec{})
	acc := a.NewAccumulator()
	acc.Add(classad.NewIntValue(2))
	if got := db.ValueText(acc.Result()); got != "2" {
		t.Fatalf("after one row the answer is %s, want 2", got)
	}
	acc.Add(classad.NewIntValue(4))
	if got := db.ValueText(acc.Result()); got != "3" {
		t.Fatalf("after a second row the answer is %s, want 3 -- the cache outlived the state", got)
	}
	acc.Merge(fold(a, []classad.Value{classad.NewIntValue(6)}))
	if got := db.ValueText(acc.Result()); got != "4" {
		t.Fatalf("after a merge the answer is %s, want 4 -- the cache outlived the state", got)
	}
}

// TestNewRefusesWhatItCannotRun covers the registration-time checks, one case per reason. Each of
// these would otherwise be a query that fails for every caller, in a place nobody is watching.
func TestNewRefusesWhatItCannotRun(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{
			"a missing function",
			"def init():\n    return 0\n",
			"no function named",
		},
		{
			"the wrong arity",
			avgSrc + "\ndef add2(s):\n    return s\n",
			"declares 1",
		},
		{
			"a function that reaches the ad",
			strings.Replace(avgSrc, "def init():\n    return {0, 0}", "def init():\n    return Cpus", 1),
			"reads the attribute Cpus",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := scriptagg.Spec{}
			if tc.name == "the wrong arity" {
				spec.Add = "add2"
			}
			_, err := scriptagg.New(mustModule(t, tc.src), spec)
			if err == nil {
				t.Fatal("New succeeded, want a refusal")
			}
			if !errors.Is(err, scriptagg.ErrBadSpec) {
				t.Errorf("error %v is not an ErrBadSpec", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestSpecNamesSeveralAggregatesInOneModule covers the reason Spec carries names at all.
func TestSpecNamesSeveralAggregatesInOneModule(t *testing.T) {
	const two = `
def zeroI():
    return 0

def sumA(s, v):
    return isInteger(v) ? s + v : s

def sumM(a, b):
    return a + b

def ident(s):
    return s

def maxA(s, v):
    return isInteger(v) ? (isUndefined(s) ? v : (v > s ? v : s)) : s

def maxM(a, b):
    return isUndefined(a) ? b : (isUndefined(b) ? a : (a > b ? a : b))

def undefI():
    return undefined
`
	sum := mustAggregate(t, two, scriptagg.Spec{Init: "zeroI", Add: "sumA", Merge: "sumM", Result: "ident"})
	largest := mustAggregate(t, two, scriptagg.Spec{Init: "undefI", Add: "maxA", Merge: "maxM", Result: "ident"})
	if got := db.ValueText(fold(sum, samples()).Result()); got != "21" {
		t.Errorf("sum = %s, want 21 -- the integers 4, 1, 9 and 7", got)
	}
	if got := db.ValueText(fold(largest, samples()).Result()); got != "9" {
		t.Errorf("the largest is %s, want 9", got)
	}
	if err := db.CheckAggregate(largest, samples()); err != nil {
		t.Errorf("CheckAggregate on the largest: %v", err)
	}
}

// TestMergeRefusesAForeignAccumulator covers the engine-bug case. Merging across registrations is
// not an input to handle, and the quiet alternative is a number that is wrong and looks right.
func TestMergeRefusesAForeignAccumulator(t *testing.T) {
	a := mustAggregate(t, avgSrc, scriptagg.Spec{})
	b := mustAggregate(t, avgSrc, scriptagg.Spec{})
	defer func() {
		if recover() == nil {
			t.Fatal("merging an accumulator from another registration did not panic")
		}
	}()
	a.NewAccumulator().Merge(b.NewAccumulator())
}

// TestStatsCountWhatTheQueryDid covers the counters an operator reads to tell a fan-out from a
// serial fold, since a pass that stopped fanning out gives the same answers.
func TestStatsCountWhatTheQueryDid(t *testing.T) {
	a := mustAggregate(t, avgSrc, scriptagg.Spec{})
	acc := a.NewAccumulator()
	acc.Merge(fold(a, samples()))
	acc.Result()
	acc.Result() // cached, so it must not count twice
	s := a.Stats()
	if s.Accumulators != 2 || s.Merges != 1 || s.Results != 1 {
		t.Fatalf("Stats = %+v, want 2 accumulators, 1 merge, 1 result", s)
	}
}

// TestRegisteredAggregateAnswersAQuery is the end-to-end: registered by name, named by a request,
// reduced by the shared GROUP BY core alongside the built-in it imitates.
//
// The built-in AVG is in the same request on purpose. It is the reference, computed by the engine
// over the same rows in the same pass, so the comparison is against what the query layer would have
// answered rather than against a number written down here.
func TestRegisteredAggregateAnswersAQuery(t *testing.T) {
	mod := mustModule(t, avgSrc)
	if _, err := scriptagg.Register("scriptavg", mod, scriptagg.Spec{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// The registry is process-wide and append-only, so a second registration of the same name is
	// the error that says so rather than a silent overwrite.
	if _, err := scriptagg.Register("scriptavg", mod, scriptagg.Spec{}); !errors.Is(err, db.ErrAggregateExists) {
		t.Fatalf("re-registering gave %v, want ErrAggregateExists", err)
	}
	if _, err := scriptagg.Register("scriptsecond", mustModule(t, secondSrc), scriptagg.Spec{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	d, err := db.OpenConfig(db.Config{})
	if err != nil {
		t.Fatalf("OpenConfig: %v", err)
	}
	defer d.Close()

	tx := d.Begin()
	for i, src := range []string{
		`[ Pool = "cm1"; Memory = 1024 ]`,
		`[ Pool = "cm1"; Memory = 2048 ]`,
		`[ Pool = "cm1"; Memory = 4096 ]`,
		`[ Pool = "cm2"; Memory = 8192 ]`,
		`[ Pool = "cm2"; Memory = 512 ]`,
		`[ Pool = "cm2"; Memory = "lots" ]`, // not a number: no aggregate counts it
	} {
		ad, perr := classad.Parse(src)
		if perr != nil {
			t.Fatalf("Parse: %v", perr)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if cerr := tx.Commit(); cerr != nil {
		t.Fatalf("Commit: %v", cerr)
	}

	rows, err := d.AggregateColsStats("true",
		[]db.GroupCol{{Attr: "Pool"}},
		[]db.AggSpec{
			{Func: db.AggAvg, Arg: "Memory"},
			{Func: db.AggUser, UserFunc: "scriptavg", Arg: "Memory"},
			{Func: db.AggUser, UserFunc: "scriptsecond", Arg: "Memory"},
		}, nil, nil)
	if err != nil {
		t.Fatalf("AggregateColsStats: %v", err)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Group[0] < rows[j].Group[0] })
	if len(rows) != 2 {
		t.Fatalf("got %d groups, want 2", len(rows))
	}
	got := map[string][]string{}
	for _, r := range rows {
		got[r.Group[0]] = r.Values
	}

	// cm1 is every row numeric, which is where the script and the built-in have to agree.
	cm1 := got[`"cm1"`]
	builtin, perr := strconv.ParseFloat(cm1[0], 64)
	if perr != nil {
		t.Fatalf("cm1: built-in AVG is %q, not a number", cm1[0])
	}
	script, perr := strconv.ParseFloat(cm1[1], 64)
	if perr != nil {
		t.Fatalf("cm1: the script AVG is %q, not a number", cm1[1])
	}
	if math.Abs(builtin-script) > 1e-9*math.Max(1, math.Abs(builtin)) {
		t.Errorf("cm1: built-in AVG %v, script AVG %v", builtin, script)
	}
	if cm1[2] != "2048" {
		t.Errorf("cm1: second largest is %s, want 2048", cm1[2])
	}

	// cm2 carries a row whose Memory is a string, and the two aggregates part company there.
	// The built-in propagates it; the script skips it, because skipping is a line of script and
	// not something the engine decided. Pinned rather than avoided: an author writing an
	// aggregate here is choosing this, and a test that only used clean data would hide the choice.
	cm2 := got[`"cm2"`]
	if cm2[0] != "error" {
		t.Errorf("cm2: built-in AVG is %s, want error over a string Memory", cm2[0])
	}
	if cm2[1] != "4352" {
		t.Errorf("cm2: script AVG is %s, want 4352 -- the mean of 8192 and 512", cm2[1])
	}
	if cm2[2] != "512" {
		t.Errorf("cm2: second largest is %s, want 512", cm2[2])
	}
}

// TestUnknownAggregateNameFailsTheQuery pins that naming one that was never registered is an error
// rather than a column of undefined, which is the difference between a query that failed and a
// query that was answered wrongly.
func TestUnknownAggregateNameFailsTheQuery(t *testing.T) {
	d, err := db.OpenConfig(db.Config{})
	if err != nil {
		t.Fatalf("OpenConfig: %v", err)
	}
	defer d.Close()
	_, err = d.AggregateColsStats("true", []db.GroupCol{{Attr: "Pool"}},
		[]db.AggSpec{{Func: db.AggUser, UserFunc: "nosuchscriptagg", Arg: "Memory"}}, nil, nil)
	if !errors.Is(err, db.ErrNoSuchAggregate) {
		t.Fatalf("got %v, want ErrNoSuchAggregate", err)
	}
}
