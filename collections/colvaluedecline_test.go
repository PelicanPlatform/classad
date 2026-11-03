package collections

import (
	"fmt"
	"sync/atomic"
	"testing"
)

// The ungrouped value pass must handle a declining worker the way the grouped one does, and for the
// same reason: a worker that stops reading must abort the WHOLE pass rather than leave the others to
// answer over the records they happened to reach. See TestOneDecliningWindowDeclinesTheWholeGroupedFanout.
//
// These tests inject the decline through valuePassDeclineHook. They used to provoke it from data --
// an expression-valued cell in the aggregated column, which the pass had no numeric reading of --
// and that premise died when a computed cell's value moved into the column: the same fixture now
// gets a correct answer. The storage condition was never what these tests were about.
//
// The fixture is the grouped one; it carries a numeric Cpus column, which is all that is needed here.

// TestOneDecliningWindowDeclinesTheWholeValueFanout declines a window with work already folded into
// the other workers' accumulators -- the partial answer at its most complete and most plausible.
func TestOneDecliningWindowDeclinesTheWholeValueFanout(t *testing.T) {
	c := parGroupFixture(t, 20000)

	// Undisturbed first, to show the pass serves this request at all and reaches a second worker.
	// Without this the assertion below would hold just as well against a request that declines for
	// some unrelated reason, which is exactly how these tests go quietly vacuous.
	before := AggFanouts()
	base, ok := c.NumStatsQuery(nil, "Cpus")
	if !ok {
		t.Fatal("the value pass declined the undisturbed request; this fixture tests nothing")
	}
	if AggFanouts() == before {
		t.Fatal("the pass never fanned out, so a decline could not reach a second worker")
	}
	if base.N == 0 {
		t.Fatal("the undisturbed pass aggregated no values")
	}

	var declines atomic.Int64
	valuePassDeclineHook = func(segWindow) bool { return declines.Add(1) == 3 }
	defer func() { valuePassDeclineHook = nil }()

	got, ok := c.NumStatsQuery(nil, "Cpus")
	if ok {
		t.Fatalf("one window declined and the pass still answered N=%d sum=%v; that answer covers "+
			"only the records the other workers reached", got.N, got.IntSum)
	}
	if got.N != 0 {
		t.Fatalf("a declining pass must return no accumulation, got N=%d", got.N)
	}
	if declines.Load() < 3 {
		t.Fatalf("the hook ran %d times; it never reached the window it was set to decline", declines.Load())
	}
}

// TestDecliningTheFirstWindowDeclinesTheValuePass covers the other end: the decline arrives before
// anything has been accumulated, which a guard written around "we already have values" would miss.
func TestDecliningTheFirstWindowDeclinesTheValuePass(t *testing.T) {
	c := parGroupFixture(t, 20000)
	var n atomic.Int64
	valuePassDeclineHook = func(segWindow) bool { return n.Add(1) == 1 }
	defer func() { valuePassDeclineHook = nil }()

	if got, ok := c.NumStatsQuery(nil, "Cpus"); ok {
		t.Fatalf("the first window declined and the pass still answered N=%d", got.N)
	}
}

// TestADecliningValuePassNeverFoldsItsSinks is the caller-visible half. A caller supplies one sink
// per worker and a merge that folds them; if merge runs on a declined pass, the caller is holding an
// accumulator built from a subset of the records with nothing to mark it as partial -- and a caller
// that then reads it has the wrong answer whatever the pass returned.
func TestADecliningValuePassNeverFoldsItsSinks(t *testing.T) {
	c := parGroupFixture(t, 20000)

	count := func() (int64, int64, bool) {
		var news, merges atomic.Int64
		ok := c.NumValuesQuery(nil, "Cpus",
			func() func(NumValue) { news.Add(1); return func(NumValue) {} },
			func() { merges.Add(1) })
		return news.Load(), merges.Load(), ok
	}

	if news, merges, ok := count(); !ok || news < 2 || merges != 1 {
		t.Fatalf("undisturbed: ok=%v sinks=%d merges=%d, want a served pass over several sinks "+
			"folded exactly once", ok, news, merges)
	}

	var declines atomic.Int64
	valuePassDeclineHook = func(segWindow) bool { return declines.Add(1) == 3 }
	defer func() { valuePassDeclineHook = nil }()

	news, merges, ok := count()
	if ok {
		t.Fatalf("one window declined and the pass still reported success over %d sink(s)", news)
	}
	if merges != 0 {
		t.Fatalf("the fold ran %d time(s) on a pass that declined", merges)
	}
}

// TestTheSerialValuePassDeclinesToo covers the path the three tests above cannot reach. All of them
// assert the pass fanned out, which is what makes them worth having -- and it means the serial
// branch, which walks the same windows on the calling goroutine, is not exercised by any of them.
// It has its own copy of the decline, so it can lose it on its own: dropping a window there and
// carrying on returns a partial aggregate as a complete one, below the gate, where most passes run.
// A mutation turning that return into a `continue` survived the whole package before this test.
func TestTheSerialValuePassDeclinesToo(t *testing.T) {
	c := parValueFixtureSerial(t, 20000)

	before := AggFanouts()
	base, ok := c.NumStatsQuery(nil, "Cpus")
	if !ok {
		t.Fatal("the serial value pass declined the undisturbed request; this fixture tests nothing")
	}
	if AggFanouts() != before {
		t.Fatal("the pass fanned out, so this is not testing the serial branch")
	}
	if base.N == 0 {
		t.Fatal("the undisturbed pass aggregated no values")
	}

	// The SECOND window, so the walk has already accumulated a window's worth when it stops.
	var n atomic.Int64
	valuePassDeclineHook = func(segWindow) bool { return n.Add(1) == 2 }
	defer func() { valuePassDeclineHook = nil }()

	got, ok := c.NumStatsQuery(nil, "Cpus")
	if ok {
		t.Fatalf("a window declined and the serial pass still answered N=%d sum=%v", got.N, got.IntSum)
	}
	if n.Load() < 2 {
		t.Fatalf("the hook ran %d time(s); it never reached the window it was set to decline", n.Load())
	}
}

// parValueFixtureSerial is parGroupFixture with the fan-out off, so the pass walks its windows on
// the calling goroutine.
func parValueFixtureSerial(tb testing.TB, n int) *Collection {
	tb.Helper()
	cd, err := NewZSTDCodec(nil)
	if err != nil {
		tb.Fatal(err)
	}
	c := New(Options{Shards: 8, SegmentSize: 1 << 16, Codec: cd, QueryParallelism: 1, AggMinBytes: 1 << 12})
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("Owner = \"user%d\"\nCpus = %d\n", i%37, i%64)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(tb, src)); err != nil {
			tb.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		tb.Skip("no sealed segments")
	}
	return c
}
