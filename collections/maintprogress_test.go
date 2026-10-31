package collections

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// The diagnostic an operator needs most while a maintenance pass is running must not be one
// the pass itself can block. This is the library half of that: MaintProgress reads an atomic
// pointer and takes no lock, so it answers in the middle of a pass that holds maintMu -- which
// is precisely when someone is asking whether the daemon is hung or working.
//
// The test is deliberately two assertions, because the first one is what keeps the second
// honest: it proves the pass really is holding maintMu at the moment of the read (by showing
// that a call which DOES take maintMu does not come back), and only then that MaintProgress
// does come back. Without the first, a fixture that quietly stopped holding the lock would let
// the second pass while proving nothing.
func TestMaintProgressAnswersWhileMaintenanceLockHeld(t *testing.T) {
	c := progressFixture(t, 12000, 4)
	defer c.Close()
	if !c.BuildAndEnableSchemaScan(2000, 32) {
		t.Skip("schema scan did not enable; nothing to columnarize")
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	colCommitStallHook = func() {
		// Stall the FIRST segment commit only: the pass is then inside columnarizeSealed, has
		// held maintMu since it started, and has published its progress record.
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	defer func() { colCommitStallHook = nil }()

	passDone := make(chan int, 1)
	go func() { passDone <- c.ColumnarizeSealed() }()
	// The pass must be let go and drained before the deferred Close, however this test exits:
	// Close unmaps the segments a rewrite is still reading, so abandoning a stalled pass on a
	// failure path faults instead of reporting. (Observed while mutation-testing this test.)
	drain := sync.OnceValue(func() int {
		close(release)
		return <-passDone
	})
	defer drain()
	select {
	case <-entered:
	case <-time.After(60 * time.Second):
		t.Fatal("the columnarize pass never reached its commit seam; the fixture produced no backlog")
	}

	// Non-vacuity gate: Retention() takes maintMu and nothing else, so it stands in for every
	// caller the pass blocks. If it answers here, the pass is not holding the lock and the
	// assertion below would be meaningless.
	lockedOut := make(chan struct{})
	go func() { c.Retention(); close(lockedOut) }()
	select {
	case <-lockedOut:
		t.Fatal("Retention() answered while a maintenance pass was in flight: the pass is not holding maintMu, so this test cannot prove MaintProgress is lock-free")
	case <-time.After(250 * time.Millisecond):
	}

	// The actual property: progress is readable, and meaningful, mid-pass.
	got := make(chan MaintProgress, 1)
	go func() { got <- c.MaintProgress() }()
	var p MaintProgress
	select {
	case p = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("MaintProgress() did not answer while a maintenance pass held maintMu")
	}
	rewritten := drain()

	if !p.Active {
		t.Error("MaintProgress reports no pass in flight while one was stalled mid-commit")
	}
	if p.Phase != MaintColumnarize {
		t.Errorf("MaintProgress phase = %q, want %q", p.Phase, MaintColumnarize)
	}
	if p.Total <= 0 {
		t.Errorf("MaintProgress Total = %d, want the pass's segment budget: without it there is no \"remaining\" to report", p.Total)
	}
	if p.Elapsed() <= 0 {
		t.Error("MaintProgress Elapsed = 0 for a pass that has been running: an operator cannot tell a long pass from a new one")
	}

	// After the pass: nothing in flight, and the phase's cumulative total moved. The counter is
	// what a Prometheus rate() is derived from, so a progress record that never folds into it
	// would leave the pass invisible between scrapes.
	if after := c.MaintProgress(); after.Active {
		t.Errorf("MaintProgress still reports phase %q active after the pass returned", after.Phase)
	}
	if n := c.OpStats().Columnarize.Count; n == 0 {
		t.Errorf("OpStats().Columnarize.Count = 0 after %d segments were rewritten: the pass's wall time was not accumulated", rewritten)
	}
}

// Compact's abort point is between SHARDS, not between segments, because compactShard is
// all-or-nothing for its shard: every one of its internal failure paths reaps the unpublished
// destination segments and leaves the shard's sources in place, and the install (ids,
// supersession transfer, directory rebuild, segment swap) happens in one critical section
// under the shard write lock. Stopping between shards is therefore indistinguishable from a
// pass whose remaining shards were simply under the compaction threshold.
//
// The stop arrives MID-pass, via the phase-2 seam, which is how it arrives in production: a
// SIGTERM lands while a pass is already rewriting. A stop set before the call would be caught
// by the check at the top of the loop and would leave the in-loop check unproven.
func TestCompactStopsBetweenShards(t *testing.T) {
	run := func(stopOnFirst bool) int {
		c := compactBacklogFixture(t, 4)
		defer c.Close()
		if stopOnFirst {
			var once sync.Once
			compactPhase2Hook = func([]*segment) {
				once.Do(func() { c.StopMaintenance() })
			}
			defer func() { compactPhase2Hook = nil }()
		}
		return c.Compact()
	}

	full := run(false)
	if full < 2 {
		t.Fatalf("uninterrupted Compact reclaimed %d shards; need at least 2 for a between-shards stop to be distinguishable", full)
	}
	if got := run(true); got != 1 {
		t.Errorf("Compact stopped mid-pass reclaimed %d shards, want 1 (the in-flight one, then stop); uninterrupted reclaims %d", got, full)
	}
}

// progressFixture builds a persistent collection holding n ads over the given shard count,
// with a columnarize budget small enough that enabling the schema scan cannot consume the
// whole backlog -- so the pass under test has work to do.
func progressFixture(t *testing.T, n, budget int) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16,
		GroupSchemaCount: -1, ColumnarSegmentBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		ad, perr := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep" ]`,
			i, i%10, i%7, i%6, (i%16)*1024))
		if perr != nil {
			t.Fatal(perr)
		}
		if perr := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); perr != nil {
			t.Fatal(perr)
		}
	}
	return c
}

// compactBacklogFixture builds a collection whose every shard is over the compaction threshold
// with garbage compactShard actually has to REWRITE.
//
// The shape matters. Overwriting every key leaves whole segments fully dead, and Compact
// unlinks those cheaply without a rewrite -- which drops the shard back under the threshold, so
// compactShard is never called and the phase-2 seam never fires. So each segment is built to
// hold a mixture: three churn records per keeper, interleaved, and then every churn key is
// superseded once more. Each old segment ends ~75% dead and so is never fully dead, which is
// exactly the fragmented garbage only a rewrite reclaims.
func compactBacklogFixture(t *testing.T, shards int) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: shards, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	put := func(key string, gen int) {
		ad, perr := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; ProcId=0; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep" ]`,
			gen, gen%7, gen%6, (gen%16)*1024))
		if perr != nil {
			t.Fatal(perr)
		}
		if perr := c.Put([]byte(key), ad); perr != nil {
			t.Fatal(perr)
		}
	}
	const keys = 4000
	for i := range keys {
		for gen := range 3 {
			put(fmt.Sprintf("churn-%d.0", i), gen)
		}
		put(fmt.Sprintf("keep-%d.0", i), 0)
	}
	for i := range keys {
		put(fmt.Sprintf("churn-%d.0", i), 9) // every churn record in the mixed segments is now dead
	}
	return c
}
