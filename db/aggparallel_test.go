package db

import (
	"fmt"
	"math/rand"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// The columnar value pass fans out across segments, so these tests exist to show the two paths
// answer alike.
//
// EVERY ONE OF THEM CHECKS THE FAN-OUT COUNTER FIRST, and the reason is specific rather than
// fastidious: declining to fan out is silent by design -- the pass runs the same windows on the
// calling goroutine and gives the same answer -- so a change that disabled the dispatch, or a
// fixture that never grew past the size threshold, would leave every comparison below passing while
// comparing the serial path with itself. The counter is the only thing that can tell those apart.

// parPad makes each record wide enough that a modest record count still clears the fan-out
// threshold, which is a byte count. Padding the records rather than multiplying them keeps these
// tests to tens of thousands of records instead of nearly a million, and the gate under test is a
// byte gate either way. The content varies per record so the segment does not compress to nothing
// and fall back under the threshold.
//
// The width is set with margin rather than to just clear the gate: at 480 bytes the fixtures landed
// either side of it depending on which attributes a given test inserted, and a test that stops
// exercising the fan-out does not fail loudly on its own -- the guards here are what caught it.
func parPad(i int) string {
	// RANDOM, not a generated pattern. The first version of this cycled through the alphabet, which
	// zstd crushed to nothing: the records were wide and the SEGMENTS were not, so the fixture sat
	// under the gate while looking like it cleared it by a wide margin. The gate counts stored
	// bytes, so the padding has to survive compression.
	rng := rand.New(rand.NewSource(int64(i)))
	b := make([]byte, 600)
	for j := range b {
		b[j] = byte('!' + rng.Intn(90))
	}
	return string(b)
}

// parTable builds a table big enough to clear the fan-out threshold, with par as its
// QueryParallelism (1 serial, 0 auto).
func parTable(t *testing.T, n, par int) *DB {
	t.Helper()
	d, err := OpenConfig(Config{SegmentSize: 1 << 16, QueryParallelism: par})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	tx := d.Begin()
	for i := 0; i < n; i++ {
		ad := classad.New()
		ad.InsertAttr("JobStatus", int64(i%5))
		ad.InsertAttr("Cpus", int64(i%64))
		// A real column as well, because the integer fields are exact under any fold order and
		// would not notice a merge that mishandled the float accumulation.
		ad.InsertAttrFloat("Load", float64(i%97)/7.0)
		ad.InsertAttrString("Pad", parPad(i))
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
		if i%20000 == 19999 {
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			tx = d.Begin()
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
	return d
}

// fannedOut runs fn and reports whether the aggregate pass fanned out while it ran.
func fannedOut(t *testing.T, fn func()) bool {
	t.Helper()
	before := collections.AggFanouts()
	fn()
	return collections.AggFanouts() > before
}

// TestBuiltinAggregatesAgreeAcrossTheFanout: every built-in the columnar rung serves gives the same
// text on both paths. Text, not a number, because a type promotion that came out differently would
// be as wrong as an arithmetic one and much easier to miss.
func TestBuiltinAggregatesAgreeAcrossTheFanout(t *testing.T) {
	const n = 60000
	serial := parTable(t, n, 1)
	par := parTable(t, n, 0)

	for _, c := range []struct {
		what string
		aggs []AggSpec
	}{
		{"count", []AggSpec{{Func: AggCount, Arg: "Cpus"}}},
		{"sum-int", []AggSpec{{Func: AggSum, Arg: "Cpus"}}},
		{"min-int", []AggSpec{{Func: AggMin, Arg: "Cpus"}}},
		{"max-int", []AggSpec{{Func: AggMax, Arg: "Cpus"}}},
		{"avg-int", []AggSpec{{Func: AggAvg, Arg: "Cpus"}}},
		{"min-real", []AggSpec{{Func: AggMin, Arg: "Load"}}},
		{"max-real", []AggSpec{{Func: AggMax, Arg: "Load"}}},
		{"sum-int-where", []AggSpec{{Func: AggSum, Arg: "Cpus"}}},
	} {
		t.Run(c.what, func(t *testing.T) {
			where := "true"
			if strings.HasSuffix(c.what, "-where") {
				where = "Cpus > 10"
			}
			var want, got []AggRow
			var err error
			if fannedOut(t, func() { want, err = serial.AggregateColsStats(where, nil, c.aggs, nil, nil) }) {
				t.Fatal("the serial table fanned out, so this compares two parallel runs")
			}
			if err != nil {
				t.Fatal(err)
			}
			if !fannedOut(t, func() { got, err = par.AggregateColsStats(where, nil, c.aggs, nil, nil) }) {
				t.Fatal("the pass never fanned out, so this compares the serial path with itself")
			}
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(want) != fmt.Sprint(got) {
				t.Fatalf("serial %v, fanned out %v", want, got)
			}
		})
	}
}

// TestRealSumAgreesToItsErrorBound is the one built-in that cannot be compared as text. Fanning the
// pass out changes the order the float additions happen in, and float addition is not associative,
// so the last bits may move. The guarantee is an error bound, not equality, and this states it --
// quietly comparing the text would either flake or, worse, pass only because the fixture happened to
// sum without rounding.
func TestRealSumAgreesToItsErrorBound(t *testing.T) {
	const n = 60000
	serial := parTable(t, n, 1)
	par := parTable(t, n, 0)

	want, ok := serial.NumStats("true", "Load")
	if !ok {
		t.Fatal("the serial columnar pass declined")
	}
	var got NumStats
	if !fannedOut(t, func() {
		var ok bool
		got, ok = par.NumStats("true", "Load")
		if !ok {
			t.Error("the parallel columnar pass declined")
		}
	}) {
		t.Fatal("the pass never fanned out, so this compares the serial path with itself")
	}
	if got.N != want.N {
		t.Fatalf("N: serial %d, fanned out %d", want.N, got.N)
	}
	// Every value is non-negative here, so the sum of the magnitudes IS the sum, and the bound is
	// a relative one on the answer itself.
	if d := got.Sum - want.Sum; d > 1e-9*want.Sum || d < -1e-9*want.Sum {
		t.Fatalf("Sum: serial %v, fanned out %v, difference %v exceeds the summation error bound", want.Sum, got.Sum, d)
	}
	if got.Min != want.Min || got.Max != want.Max {
		t.Fatalf("extremes moved: serial [%v,%v], fanned out [%v,%v]", want.Min, want.Max, got.Min, got.Max)
	}
}

// parSum is a user-defined aggregate that takes column values and counts how many accumulators were
// made and how many merges folded them, so a test can see the fan-out reach the aggregate itself
// rather than infer it.
type parSum struct {
	news   *atomic.Int64
	merges *atomic.Int64
}

func (p parSum) NewAccumulator() Accumulator {
	p.news.Add(1)
	return &parSumAcc{merges: p.merges}
}

type parSumAcc struct {
	n      int64
	merges *atomic.Int64
}

func (a *parSumAcc) Add(v classad.Value) {
	if i, err := v.IntValue(); err == nil {
		a.n += i
	}
}
func (a *parSumAcc) AddNum(v NumValue) {
	if v.Kind == NumInt {
		a.n += v.I
	}
}
func (a *parSumAcc) Merge(other Accumulator) {
	a.merges.Add(1)
	a.n += other.(*parSumAcc).n
}
func (a *parSumAcc) Result() classad.Value { return classad.NewIntValue(a.n) }

// TestUserAggregateIsFoldedFromPerWorkerAccumulators: the fan-out gives each worker its own
// accumulator and folds them with the aggregate's own Merge.
//
// This is what Merge was put on the interface FOR. It carried the argument that a parallel pass
// would need it long before anything parallel existed, and until now its only real caller folded two
// storage groups that rendered to one label.
func TestUserAggregateIsFoldedFromPerWorkerAccumulators(t *testing.T) {
	var news, merges atomic.Int64
	name := registerOnce(t, "par_fold_sum", parSum{news: &news, merges: &merges})
	const n = 60000
	par := parTable(t, n, 0)

	var want int64
	for i := 0; i < n; i++ {
		want += int64(i % 64)
	}
	news.Store(0)
	merges.Store(0)
	var rows []AggRow
	var err error
	if !fannedOut(t, func() {
		rows, err = par.AggregateColsStats("true", nil,
			[]AggSpec{{Func: AggUser, UserFunc: name, Arg: "Cpus"}}, nil, nil)
	}) {
		t.Fatal("the pass never fanned out, so nothing here exercises the fold")
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Values[0] != fmt.Sprint(want) {
		t.Fatalf("got %v, want sum %d", rows, want)
	}
	// A fan-out runs at least two workers -- one is the serial path -- so the aggregate must have
	// been asked for at least two accumulators and folded at least one of them away.
	if got := news.Load(); got < 2 {
		t.Fatalf("the fold ran on %d accumulator(s); a fan-out must make one per worker", got)
	}
	if got := merges.Load(); got < 1 {
		t.Fatalf("%d merges: the per-worker accumulators were never folded", got)
	}
	if got, want := merges.Load(), news.Load()-1; got != want {
		t.Fatalf("%d merges for %d accumulators; folding n accumulators takes exactly n-1", got, news.Load())
	}
}

// colPanicsOnNum is a ColumnAccumulator that panics once it has been fed, to show a panic raised on
// a WORKER goroutine still fails just this query.
type colPanicsOnNum struct{}

func (colPanicsOnNum) NewAccumulator() Accumulator { return &colPanicsOnNumAcc{} }

type colPanicsOnNumAcc struct{}

func (a *colPanicsOnNumAcc) Add(classad.Value)     {}
func (a *colPanicsOnNumAcc) AddNum(NumValue)       { panic("aggregate exploded on a worker") }
func (a *colPanicsOnNumAcc) Merge(Accumulator)     {}
func (a *colPanicsOnNumAcc) Result() classad.Value { return classad.NewIntValue(0) }

// TestUserAggregatePanicOnAWorkerFailsTheQuery: containment survives the fan-out.
//
// A panic on a goroutine the pass started cannot be recovered by anything the caller defers, so
// without the workers catching and re-raising it this would take the process down instead of failing
// one query. The error must still name the aggregate, which is the only thing that tells an operator
// whose code did it.
func TestUserAggregatePanicOnAWorkerFailsTheQuery(t *testing.T) {
	name := registerOnce(t, "par_panic_col", colPanicsOnNum{})
	par := parTable(t, 60000, 0)
	rows, err := par.AggregateColsStats("true", nil,
		[]AggSpec{{Func: AggUser, UserFunc: name, Arg: "Cpus"}}, nil, nil)
	if err == nil {
		t.Fatalf("a panicking AddNum should fail the query, got %v", rows)
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("the error should name the aggregate that panicked, got %v", err)
	}
}

// TestADecliningWindowDeclinesTheWholeFanout is the highest-stakes test here.
//
// A worker that finds a window the pass cannot read must abort the WHOLE pass, not drop that window
// and let the others answer. Dropping it would produce a sum over a subset of the records and return
// it as the sum over all of them -- a wrong answer with nothing about it that looks wrong, which is
// the failure mode every rung of this ladder is built to avoid. The fold must also not run, so a
// caller cannot read a partial accumulator by mistake.
//
// An expression-valued cell in the aggregated column is the decline that is reachable from data: the
// column has no numeric reading of it at all.
func TestADecliningWindowDeclinesTheWholeFanout(t *testing.T) {
	const n = 60000
	d, err := OpenConfig(Config{SegmentSize: 1 << 16, QueryParallelism: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	for i := 0; i < n; i++ {
		ad := classad.New()
		ad.InsertAttr("JobStatus", int64(i%5))
		ad.InsertAttrString("Pad", parPad(i))
		if i == n-1 {
			// The LAST record, so a pass that declines only after reading almost everything is
			// the case under test -- the partial accumulator is at its most complete and most
			// plausible exactly here.
			ad.InsertExpr("Cpus", mustExpr(t, "JobStatus + 1"))
		} else {
			ad.InsertAttr("Cpus", int64(i%64))
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
		if i%20000 == 19999 {
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			tx = d.Begin()
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}

	var merges atomic.Int64
	var news atomic.Int64
	name := registerOnce(t, "par_decline_col", parSum{news: &news, merges: &merges})

	for _, c := range []struct {
		what string
		aggs []AggSpec
		rung string
	}{
		{"built-in", []AggSpec{{Func: AggSum, Arg: "Cpus"}}, "columnar-aggregate"},
		{"user-defined", []AggSpec{{Func: AggUser, UserFunc: name, Arg: "Cpus"}}, "columnar-user-aggregate"},
	} {
		t.Run(c.what, func(t *testing.T) {
			merges.Store(0)
			var rows []AggRow
			var ok bool
			var err error
			if !fannedOut(t, func() {
				rows, ok, err = d.aggLadderAnswer(t, c.rung, AggRequest{Constraint: "true", Aggs: c.aggs})
			}) {
				t.Fatal("the pass never fanned out, so this tests the serial decline, not the fan-out")
			}
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Fatalf("the rung answered %v over a column it cannot read; that answer covers only "+
					"the records read before the pass gave up", rows)
			}
			if rows != nil {
				t.Fatalf("a declining rung must return no rows, got %v", rows)
			}
			if got := merges.Load(); got != 0 {
				t.Fatalf("the fold ran %d time(s) on a pass that declined", got)
			}
		})
	}

	// And the query as a whole still gets the right answer, from the rung below.
	var want int64
	for i := 0; i < n-1; i++ {
		want += int64(i % 64)
	}
	want += int64((n-1)%5) + 1 // the expression record, evaluated
	rows, err := d.AggregateColsStats("true", nil, []AggSpec{{Func: AggSum, Arg: "Cpus"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Values[0] != fmt.Sprint(want) {
		t.Fatalf("after the decline the scan should answer %d, got %v", want, rows)
	}
}

// TestASmallPassDoesNotFanOut pins the gate from the other side.
//
// Every other test here asserts the fan-out HAPPENED, so all of them would still pass if the
// threshold were removed entirely and every aggregate fanned out. Measured, a fan-out below the gate
// is 25-40% SLOWER than running the pass on the calling goroutine, and it spends worker tokens that
// concurrent queries draw from the same budget -- so fanning out a small pass is a regression that
// no correctness test can see.
//
// Same records as the fixtures above, without the padding that makes them wide.
func TestASmallPassDoesNotFanOut(t *testing.T) {
	d, err := OpenConfig(Config{SegmentSize: 1 << 16, QueryParallelism: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	for i := 0; i < 20000; i++ {
		ad := classad.New()
		ad.InsertAttr("Cpus", int64(i%64))
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
	var rows []AggRow
	col0, _ := collections.AggSegmentPaths()
	if fannedOut(t, func() {
		rows, err = d.AggregateColsStats("true", nil, []AggSpec{{Func: AggSum, Arg: "Cpus"}}, nil, nil)
	}) {
		t.Fatal("a pass this small fanned out; below the gate that is slower than staying serial")
	}
	if err != nil {
		t.Fatal(err)
	}
	// And it really did run the columnar pass -- otherwise this would pass just as well against a
	// request the columnar rung never served at all.
	if col, _ := collections.AggSegmentPaths(); col == col0 {
		t.Fatal("no segment took the columnar path, so this says nothing about the gate")
	}
	var want int64
	for i := 0; i < 20000; i++ {
		want += int64(i % 64)
	}
	if len(rows) != 1 || rows[0].Values[0] != fmt.Sprint(want) {
		t.Fatalf("got %v, want %d", rows, want)
	}
}
