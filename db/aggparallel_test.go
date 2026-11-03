package db

import (
	"fmt"
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

// testAggGate is the fan-out gate these tests run with. The real gate is tens of megabytes, which
// is the right value on a real workload and the wrong one to build in a test: clearing it needs a
// fixture per table large enough that the suite stopped fitting its CI timeout, under -race, for
// data whose size is incidental to everything being tested here. The gate's own value is pinned by
// TestASmallPassDoesNotFanOut, which runs at the DEFAULT and is cheap because it asserts the pass
// does NOT fan out.
const testAggGate = 1 << 12

// parTable builds a table that fans out at testAggGate, with par as its QueryParallelism (1 serial,
// 0 auto).
func parTable(t *testing.T, n, par int) *DB {
	t.Helper()
	d, err := OpenConfig(Config{SegmentSize: 1 << 16, QueryParallelism: par, AggMinBytes: testAggGate})
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
	const n = 20000
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
	const n = 20000
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
	const n = 20000
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
	par := parTable(t, 20000, 0)
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
// An expression-valued cell in a columnar block is the decline that is reachable from data: the
// column has no numeric reading of it at all. It has to be IN a columnar block, though. A segment
// with no block -- the shard's write target above all -- is read record by record, and that
// fallback evaluates the expression and answers correctly, so a fixture whose expression lands
// there tests nothing and fails by reporting the right sum.
//
// Where it lands is decided by the commit, not by insertion order: a transaction applies its
// writes in map order, so the record written last is anywhere in its shard. Written last in one
// big transaction, it reached the write target about one run in four. It is committed ALONE and
// FIRST instead, which makes it the first record of its shard, in a segment that seals long before
// the fixture is done.
func TestADecliningWindowDeclinesTheWholeFanout(t *testing.T) {
	const n = 20000
	d, err := OpenConfig(Config{SegmentSize: 1 << 16, QueryParallelism: 0, AggMinBytes: testAggGate})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	exprKey := n - 1
	tx := d.Begin()
	exprAd := classad.New()
	exprAd.InsertAttr("JobStatus", int64(exprKey%5))
	exprAd.InsertExpr("Cpus", mustExpr(t, "JobStatus + 1"))
	tx.NewClassAd(fmt.Sprintf("k%d", exprKey), exprAd)
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx = d.Begin()
	for i := 0; i < n; i++ {
		if i == exprKey {
			continue
		}
		ad := classad.New()
		ad.InsertAttr("JobStatus", int64(i%5))
		ad.InsertAttr("Cpus", int64(i%64))
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err = tx.Commit(); err != nil {
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

// TestSegmentPathCountersTellTheTwoPathsApart.
//
// The counters are what every other test and the benchmark use to assert the columnar path ran, so
// a counter that mislabels the row fallback as columnar makes all of those guards vacuous -- they
// would keep passing over a pass that never touched a column. A mutation swapping the two increments
// survived the whole suite until this test existed.
//
// Two fixtures over the same records, differing only in segment size. With small segments the data
// seals and columnarizes; with segments large enough that nothing seals, every segment falls to the
// row fallback while the rung still reports success, which is the case that made the first scaling
// numbers here nonsense.
func TestSegmentPathCountersTellTheTwoPathsApart(t *testing.T) {
	build := func(segSize int) (cols, rows int64) {
		d, err := OpenConfig(Config{SegmentSize: segSize, QueryParallelism: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		tx := d.Begin()
		for i := 0; i < 50000; i++ {
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
		c0, r0 := collections.AggSegmentPaths()
		got, err := d.AggregateColsStats("true", nil, []AggSpec{{Func: AggSum, Arg: "Cpus"}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Both fixtures must produce the right answer -- the point is that the counters tell apart
		// two paths that are indistinguishable from the result.
		var want int64
		for i := 0; i < 50000; i++ {
			want += int64(i % 64)
		}
		if len(got) != 1 || got[0].Values[0] != fmt.Sprint(want) {
			t.Fatalf("segSize=%d: got %v, want %d", segSize, got, want)
		}
		c1, r1 := collections.AggSegmentPaths()
		return c1 - c0, r1 - r0
	}

	smallCols, smallRows := build(1 << 16)
	if smallCols == 0 {
		t.Fatalf("small segments: %d columnar, %d row; this fixture is meant to columnarize", smallCols, smallRows)
	}
	bigCols, bigRows := build(1 << 20)
	if bigRows == 0 {
		t.Fatalf("large segments: %d columnar, %d row; this fixture is meant to seal nothing", bigCols, bigRows)
	}
	if bigCols != 0 {
		t.Fatalf("large segments: %d segments counted as columnar, but none of them has a block", bigCols)
	}
	// Both fixtures keep one unsealed live segment per shard, so the row count does NOT separate
	// them -- it is the same on both sides. What separates them is that the small fixture has
	// columnar segments and the large one has none, over identical records giving an identical
	// answer.
	if smallCols <= smallRows {
		t.Fatalf("small segments: %d columnar against %d row; the columnar path should dominate here",
			smallCols, smallRows)
	}
	t.Logf("small=(%d columnar, %d row)  large=(%d columnar, %d row)", smallCols, smallRows, bigCols, bigRows)
}

// TestTheDefaultGateStillLetsARealPassFanOut is the one test here that pays for a fixture at the
// real gate's scale, and it buys the one property the cheap tests cannot.
//
// Every other fan-out test runs at testAggGate, so all of them would keep passing if the DEFAULT
// gate were raised to something nothing ever reaches -- the fan-out would simply never happen in
// production and no test would say so. TestASmallPassDoesNotFanOut catches a default that is too
// LOW; this catches one that is too high. Together they bracket it.
//
// Plain records, no padding: the gate counts stored bytes, and reaching tens of megabytes with
// ordinary records is a few hundred thousand of them, which is cheaper than it sounds and much
// cheaper than making each record wide.
func TestTheDefaultGateStillLetsARealPassFanOut(t *testing.T) {
	if raceEnabled {
		// Minutes, under -race, to build a fixture whose only job is to be large. The race
		// detector cannot say anything about whether a gate constant permits a fan-out, so the
		// ordinary build carries this one.
		t.Skip("builds a fixture at the real fan-out gate; too slow under -race for nothing gained")
	}
	d, err := OpenConfig(Config{SegmentSize: 1 << 16, QueryParallelism: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var want int64
	tx := d.Begin()
	for i := 0; i < 700000; i++ {
		ad := classad.New()
		ad.InsertAttr("Cpus", int64(i%64))
		ad.InsertAttr("JobStatus", int64(i%5))
		ad.InsertAttr("Mem", int64(i%4096))
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
		want += int64(i % 64)
		if i%50000 == 49999 {
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
	var rows []AggRow
	if !fannedOut(t, func() {
		rows, err = d.AggregateColsStats("true", nil, []AggSpec{{Func: AggSum, Arg: "Cpus"}}, nil, nil)
	}) {
		t.Fatal("a pass this size did not fan out at the default gate; the gate is set above what " +
			"any real aggregate reaches, so the fan-out never happens in production")
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Values[0] != fmt.Sprint(want) {
		t.Fatalf("got %v, want %d", rows, want)
	}
}
