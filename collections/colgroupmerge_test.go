package collections

import (
	"fmt"
	"sync/atomic"
	"testing"
)

// parGroupFixture is a multi-shard, multi-segment collection with a STRING group column and a
// numeric aggregate column, large enough to fan out at the gate set below.
func parGroupFixture(tb testing.TB, n int) *Collection {
	tb.Helper()
	cd, err := NewZSTDCodec(nil)
	if err != nil {
		tb.Fatal(err)
	}
	c := New(Options{Shards: 8, SegmentSize: 1 << 16, Codec: cd, QueryParallelism: 0, AggMinBytes: 1 << 12})
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

// TestOneDecliningWindowDeclinesTheWholeGroupedFanout.
//
// A worker that finds a window the pass cannot read must abort the WHOLE pass. If it merely stops,
// the other workers' group maps are intact and get folded and returned -- a GROUP BY over a SUBSET
// of the records, with every group label right and every count plausible. There is nothing about
// the shape of that answer to assert against, which is why it needs a test of its own: a mutation
// turning the decline into a `continue` survived every other test in this package and in db.
//
// The decline is injected rather than provoked from data, deliberately. The ungrouped pass declines
// on an expression-valued cell, which a fixture can store; the grouped pass has a scoped tier that
// EVALUATES such a cell and answers correctly, so whether a given run declines is a property of
// which tier took the block -- measured at 10 declines in 12 runs over an identical fixture. A test
// built on that premise fails one run in six by reporting a correct answer. What is under test is
// the fan-out's handling of a decline, not the storage condition that provokes one.
func TestOneDecliningWindowDeclinesTheWholeGroupedFanout(t *testing.T) {
	c := parGroupFixture(t, 20000)

	groups := []GroupSpec{{Attr: "Owner"}}
	aggs := []GroupAggCol{{Attr: "Cpus"}}

	// First, undisturbed, to show the pass serves this request at all -- otherwise the assertion
	// below would hold against a request that declines for some unrelated reason.
	before := AggFanouts()
	base, ok := c.GroupStatsAllAggs(groups, aggs, nil)
	if !ok {
		t.Fatal("the grouped pass declined the undisturbed request; this fixture tests nothing")
	}
	if AggFanouts() == before {
		t.Fatal("the pass never fanned out, so a decline could not reach a second worker")
	}
	if len(base) == 0 {
		t.Fatal("no groups")
	}

	// Now decline exactly ONE window. Every other worker still has groups to fold, which is the
	// case that must not produce an answer.
	var declines atomic.Int64
	groupPassDeclineHook = func(segWindow) bool { return declines.Add(1) == 3 }
	defer func() { groupPassDeclineHook = nil }()

	rows, ok := c.GroupStatsAllAggs(groups, aggs, nil)
	if ok {
		t.Fatalf("one window declined and the pass still answered %d group(s); those groups cover "+
			"only the records the other workers reached", len(rows))
	}
	if rows != nil {
		t.Fatalf("a declining pass must return no groups, got %d", len(rows))
	}
	if declines.Load() < 3 {
		t.Fatalf("the hook ran %d times; it never reached the window it was set to decline", declines.Load())
	}
}

// TestDecliningTheFirstWindowDeclinesTheGroupedPass covers the other end: the decline arrives before
// any worker has accumulated anything, which is the case a guard written around "we already have
// groups" would miss.
func TestDecliningTheFirstWindowDeclinesTheGroupedPass(t *testing.T) {
	c := parGroupFixture(t, 20000)
	var n atomic.Int64
	groupPassDeclineHook = func(segWindow) bool { return n.Add(1) == 1 }
	defer func() { groupPassDeclineHook = nil }()

	rows, ok := c.GroupStatsAllAggs([]GroupSpec{{Attr: "Owner"}}, []GroupAggCol{{Attr: "Cpus"}}, nil)
	if ok {
		t.Fatalf("the first window declined and the pass still answered %d group(s)", len(rows))
	}
}
