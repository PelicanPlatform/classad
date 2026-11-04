package wasmudf

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

// A WASM AGGREGATE UNDER THE ENGINE'S COLUMNAR PASS, which is the rung it was
// built to reach and the one that makes merge load-bearing.
//
// The pass fans out across segment windows: one accumulator per worker, built on
// the calling goroutine before any worker starts, each fed by exactly one
// goroutine, folded with the aggregate's own Merge on the calling goroutine once
// every worker has returned. Which worker draws which segment is a race, so the
// fold order is not fixed between two runs of the same query.
//
// EVERY TEST HERE CHECKS THAT THE PASS ACTUALLY FANNED OUT, before it checks the
// answer. Declining to fan out is silent by design -- the same windows run on the
// calling goroutine and give the same answer -- so a fixture that stopped
// clearing the size threshold, or a dispatch that was turned off, would leave
// these comparisons passing while comparing the serial path with itself.

// testAggGate is the fan-out gate these tests run with. The production gate is
// tens of megabytes, which is the right value on a real workload and the wrong
// one to build a test fixture at: db's own suite found that clearing it needs
// tens of megabytes per table, which under -race put that module over its CI
// timeout for data whose size is incidental. The gate's own value is pinned by a
// test in db that runs at the default.
const testAggGate = 1 << 12

// aggTable builds a table that fans out at testAggGate. Cpus is an exact integer
// column; Load is a real one, because an integer-only fixture cannot tell a merge
// that mishandled float accumulation from one that did not.
func aggTable(t *testing.T, n, par int) *db.DB {
	t.Helper()
	d, err := db.OpenConfig(db.Config{SegmentSize: 1 << 16, QueryParallelism: par, AggMinBytes: testAggGate})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	tx := d.Begin()
	for i := range n {
		ad := classad.New()
		ad.InsertAttr("JobStatus", int64(i%5))
		ad.InsertAttr("Cpus", int64(i%64))
		ad.InsertAttrFloat("Load", float64(i%97)/7.0)
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

// fannedOut runs fn and reports whether the aggregate pass fanned out while it
// ran.
func fannedOut(t *testing.T, fn func()) bool {
	t.Helper()
	before := collections.AggFanouts()
	fn()
	return collections.AggFanouts() > before
}

// aggSeq makes every registration in this file unique.
//
// db's aggregate registry is APPEND-ONLY -- there is no unregister, by design --
// so a suite run with -count=2 would fail its second pass on "already registered"
// if the names were literals. Worse, the second pass would be re-registering
// against a Loader the first pass's cleanup had closed, so the surviving
// registration would name a module that can no longer be instantiated.
var aggSeq atomic.Int64

// registerWasmAgg loads a fixture and registers it under a name of its own,
// returning both the loaded aggregate and the name a query has to use.
func registerWasmAgg(t *testing.T, fixtureName, as string, opts Options) (*UDAF, string) {
	t.Helper()
	name := fmt.Sprintf("%s_%d", as, aggSeq.Add(1))
	l := newLoader(t, opts)
	a, err := RegisterAggregate(context.Background(), l, DBRegistry(), name, fixture(t, fixtureName))
	if err != nil {
		t.Fatalf("registering %s as %s: %v", fixtureName, name, err)
	}
	return a, name
}

// TestWasmAggregateIsFoldedFromPerWorkerAccumulators: the fan-out gives each worker its own
// state and the host merges them.
//
// The guest sees one state per worker and the host merges them, so this is the
// first test in which merge does work that nothing else could do: the partial
// states live in different guest instances, folded on different goroutines, and
// the only thing connecting them is that the state crosses the boundary as bytes.
func TestWasmAggregateIsFoldedFromPerWorkerAccumulators(t *testing.T) {
	a, name := registerWasmAgg(t, "aggsum", "wasm_par_sum", Options{})
	const n = 20000
	par := aggTable(t, n, 0)

	var want int64
	for i := range n {
		want += int64(i % 64)
	}

	before := a.Stats()
	var rows []db.AggRow
	var err error
	if !fannedOut(t, func() {
		rows, err = par.AggregateColsStats("true", nil,
			[]db.AggSpec{{Func: db.AggUser, UserFunc: name, Arg: "Cpus"}}, nil, nil)
	}) {
		t.Fatal("the pass never fanned out, so nothing here exercises the fold")
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Values[0] != fmt.Sprint(want) {
		t.Fatalf("got %v, want sum %d", rows, want)
	}

	after := a.Stats()
	accs := after.Accumulators - before.Accumulators
	merges := after.Merges - before.Merges
	// A fan-out runs at least two workers -- one of them is the calling
	// goroutine -- so the aggregate must have been asked for at least two states
	// and folded at least one of them away.
	if accs < 2 {
		t.Fatalf("the fold ran on %d accumulator(s); a fan-out must make one per worker", accs)
	}
	if merges != accs-1 {
		t.Fatalf("%d merges for %d accumulators; folding n states takes exactly n-1", merges, accs)
	}
	if after.Failures != before.Failures {
		t.Fatalf("failures during a successful query: %v", after.ByReason)
	}
}

// TestTheFannedOutAnswerMatchesTheSerialOne over both an exact column and a float
// one.
//
// The integer sum is compared EXACTLY, because it is accumulated in i64 in the
// guest and no fold order can move it. The float sum is compared to an ERROR
// BOUND, because float addition is not associative and the fan-out changes the
// grouping: demanding equality there would either flake or pass only because the
// fixture happened to sum without rounding. Stating the bound rather than
// widening the comparison is what keeps the integer half honest.
func TestTheFannedOutAnswerMatchesTheSerialOne(t *testing.T) {
	_, name := registerWasmAgg(t, "aggsum", "wasm_cmp_sum", Options{})
	const n = 20000
	serial := aggTable(t, n, 1)
	par := aggTable(t, n, 0)

	run := func(d *db.DB, attr string) string {
		t.Helper()
		rows, err := d.AggregateColsStats("true", nil,
			[]db.AggSpec{{Func: db.AggUser, UserFunc: name, Arg: attr}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("%d rows, want 1", len(rows))
		}
		return rows[0].Values[0]
	}

	var wantInt, gotInt string
	if fannedOut(t, func() { wantInt = run(serial, "Cpus") }) {
		t.Fatal("the serial table fanned out, so this compares two parallel runs")
	}
	if !fannedOut(t, func() { gotInt = run(par, "Cpus") }) {
		t.Fatal("the pass never fanned out, so this compares the serial path with itself")
	}
	if wantInt != gotInt {
		t.Fatalf("integer sum: serial %s, fanned out %s -- an i64 sum cannot move under reordering",
			wantInt, gotInt)
	}

	var wantReal, gotReal string
	if fannedOut(t, func() { wantReal = run(serial, "Load") }) {
		t.Fatal("the serial table fanned out")
	}
	if !fannedOut(t, func() { gotReal = run(par, "Load") }) {
		t.Fatal("the pass never fanned out for the real column")
	}
	x, y := mustFloat(t, wantReal), mustFloat(t, gotReal)
	// The bound float summation actually obeys: a relative epsilon against the
	// magnitudes involved, not an absolute slack chosen until it passed.
	if diff, tol := abs(x-y), 1e-12*(abs(x)+abs(y)); diff > tol {
		t.Fatalf("real sum: serial %v, fanned out %v, differ by %v > %v", x, y, diff, tol)
	}
}

// TestAWasmAggregateWithoutTheNumericFoldIsServedByTheScan: the opt-in is a gate on which rung
// runs, not a label.
//
// aggcount counts everything it is handed, which is exactly what the columnar
// pass cannot honour -- that pass offers only a column's numeric values. It
// withholds the numeric fold export, so the engine must refuse it the column
// rung and run the record scan, which is serial over one accumulator.
//
// The observable is the MERGE count: the scan folds nothing, where the same query
// over the opted-in aggregate on the same table folds one state per worker. The
// accumulator count cannot serve here, because the columnar rung builds one
// accumulator to decide whether the aggregate opted in and drops it again when
// the type assertion fails -- so a scan-served query shows two accumulators and
// one merge would still be a bug.
func TestAWasmAggregateWithoutTheNumericFoldIsServedByTheScan(t *testing.T) {
	scanOnly, countName := registerWasmAgg(t, "aggcount", "wasm_scan_count", Options{})
	columnar, sumName := registerWasmAgg(t, "aggsum", "wasm_scan_sum", Options{})
	const n = 20000
	par := aggTable(t, n, 0)

	run := func(a *UDAF, name string) (string, uint64) {
		t.Helper()
		before := a.Stats().Merges
		rows, err := par.AggregateColsStats("true", nil,
			[]db.AggSpec{{Func: db.AggUser, UserFunc: name, Arg: "Cpus"}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return rows[0].Values[0], a.Stats().Merges - before
	}

	gotCount, scanMerges := run(scanOnly, countName)
	if gotCount != fmt.Sprint(n) {
		t.Fatalf("count = %s, want %d", gotCount, n)
	}
	if scanMerges != 0 {
		t.Fatalf("the query folded %d states; an aggregate that did not opt in must not be fanned out", scanMerges)
	}

	_, colMerges := run(columnar, sumName)
	if colMerges < 1 {
		t.Fatalf("the opted-in aggregate folded %d states on the same table; "+
			"the fixture is not reaching the columnar rung, so the comparison above proves nothing", colMerges)
	}
}

// TestAWasmTrapOnAWorkerFailsTheQueryAndNamesTheAggregate: containment survives the fan-out.
//
// A panic on a goroutine the pass started cannot be recovered by anything the
// caller defers, so without the workers catching and re-raising it this would
// take the process down instead of failing one query. The error has to name the
// aggregate, which is the only thing that tells an operator whose module did it.
func TestAWasmTrapOnAWorkerFailsTheQueryAndNamesTheAggregate(t *testing.T) {
	_, name := registerWasmAgg(t, "aggtrap", "wasm_par_trap", Options{})
	par := aggTable(t, 20000, 0)
	rows, err := par.AggregateColsStats("true", nil,
		[]db.AggSpec{{Func: db.AggUser, UserFunc: name, Arg: "Cpus"}}, nil, nil)
	if err == nil {
		t.Fatalf("a trapping fold should fail the query, got %v", rows)
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("the error should name the aggregate that trapped, got %v", err)
	}
	if !strings.Contains(err.Error(), "trap") {
		t.Fatalf("the error should say the guest trapped, got %v", err)
	}
}

// declineTable builds the fixture both halves of TestADecliningWindowDeclinesTheWholeFanout
// run on: a table whose aggregated column, Cpus, holds an expression in one record of every
// segment. It returns the table and the sum of the column over every record, the expression
// ones evaluated -- the answer the row path gives, and so the answer whatever serves the
// query has to give.
//
// budget is Config.ColumnarSegmentBudget, which is what picks the tier.
//
// There has to be an expression in EVERY segment, not just the last. A segment the columnar
// rewrite has not reached is served by its sidecar, so a fixture with a single one tests
// whichever tier that one segment happened to land in -- it passes in isolation and, about
// one run in four, reports a correct answer having exercised nothing it names.
func declineTable(t *testing.T, n, budget int) (*db.DB, int64) {
	t.Helper()
	d, err := db.OpenConfig(db.Config{SegmentSize: 1 << 16, QueryParallelism: 0,
		AggMinBytes: testAggGate, ColumnarSegmentBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	expr, err := classad.ParseExpr("JobStatus + 1")
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	var want int64
	for i := range n {
		ad := classad.New()
		ad.InsertAttr("JobStatus", int64(i%5))
		if i%97 == 96 || i == n-1 {
			// The last record among them, so a pass that gives up only after
			// reading almost everything is covered: the partial state is at its
			// most complete and most plausible exactly there.
			ad.InsertExpr("Cpus", expr)
			want += int64(i%5) + 1
		} else {
			ad.InsertAttr("Cpus", int64(i%64))
			want += int64(i % 64)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if cerr := tx.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
	return d, want
}

// sumWith runs the registered aggregate over one attribute and reports the answer with the
// number of states the fold consumed.
func sumWith(t *testing.T, d *db.DB, a *UDAF, name, attr string) (string, uint64) {
	t.Helper()
	before := a.Stats().Merges
	rows, err := d.AggregateColsStats("true", nil,
		[]db.AggSpec{{Func: db.AggUser, UserFunc: name, Arg: attr}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	return rows[0].Values[0], a.Stats().Merges - before
}

// TestADecliningWindowDeclinesTheWholeFanout, with the state in the guest.
//
// A worker meeting a window the pass cannot read must abort the WHOLE pass and
// the fold must not run, so no caller can read a partial accumulator. The record
// scan then answers the same query, and its answer is the one to compare against
// -- a pass that dropped the unreadable window and let the others answer would
// return a sum over a subset of the records as though it covered all of them.
//
// WHICH TIER THE DECLINE LIVES IN. An expression-valued cell in the aggregated
// column is a decline reachable from data in one of the two columnar tiers, not
// both. A SIDECAR block is built beside the records it describes and its columns
// hold literals, so such a cell escapes into the block's cold tail and the
// column has no numeric reading of it at all: the pass declines. A COLUMNARIZED
// segment stores the value the expression evaluates to beside the column, so its
// columns answer and there is nothing to decline.
//
// The fixture is in-memory, which used to settle that by accident -- an
// in-memory table kept its sealed segments row-native, so it always got the
// sidecar tier and never said so. It columnarizes now, so each tier is asked for
// by name and gets the assertion that belongs to it: the decline where the
// decline exists, and the row path's own answer where it does not. Both halves
// must produce that answer, because which tier served the query is a storage
// decision and no caller of an aggregate can see it.
func TestADecliningWindowDeclinesTheWholeFanout(t *testing.T) {
	const n = 20000

	// SIDECAR TIER, deliberately: ColumnarSegmentBudget -1 keeps the sealed segments
	// row-native with a columnar copy beside them, which is the tier whose columns cannot
	// read an expression-valued cell.
	t.Run("sidecar", func(t *testing.T) {
		a, name := registerWasmAgg(t, "aggsum", "wasm_decline_sum", Options{})
		d, want := declineTable(t, n, -1)

		// Over JobStatus first -- same table, same query shape, a column with no
		// expression in it. It shows the pass reaches the columnar rung here and folds
		// when it gets there, so the zero below is this column's doing and not a fixture
		// that never reached a worker at all.
		if _, merges := sumWith(t, d, a, name, "JobStatus"); merges < 1 {
			t.Fatal("the pass folded nothing over a plain column; this fixture never reached the rung, " +
				"so a zero over the expression column proves nothing")
		}

		got, merges := sumWith(t, d, a, name, "Cpus")
		if got != fmt.Sprint(want) {
			t.Fatalf("got %v, want %d -- a declined window must not be dropped from the answer", got, want)
		}
		// The scan serves it, so exactly one state exists and nothing is folded. A
		// merge here would mean the declining pass had folded its partial states
		// anyway, which is how a subset gets reported as a whole.
		if merges != 0 {
			t.Fatalf("%d merges on a query the columnar pass declined; the fold must not run", merges)
		}
	})

	// COLUMNAR-NATIVE TIER, the one an in-memory table now gets by default and the one every
	// persistent table already had. The expression's value is in the column, so the pass is
	// entitled to serve the query -- and the requirement that applies is not a refusal but
	// the row path's answer, reached over the fan-out rather than around it.
	t.Run("columnar-native", func(t *testing.T) {
		a, name := registerWasmAgg(t, "aggsum", "wasm_computed_sum", Options{})
		d, want := declineTable(t, n, 0)

		got, merges := sumWith(t, d, a, name, "Cpus")
		if merges < 1 {
			t.Fatal("the pass folded nothing, so it declined or ran serially; this asserts the " +
				"answer of a fan-out that did NOT decline, and there was none")
		}
		if got != fmt.Sprint(want) {
			t.Fatalf("got %v, want %d -- a column holding an expression's value must sum to what "+
				"the row path sums the expressions to", got, want)
		}
	})
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func mustFloat(t *testing.T, s string) float64 {
	t.Helper()
	var f float64
	if _, err := fmt.Sscan(s, &f); err != nil {
		t.Fatalf("parsing %q as a float: %v", s, err)
	}
	return f
}
