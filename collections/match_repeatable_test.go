package collections

import (
	"fmt"
	"strings"
	"testing"
)

// Matchmaking has to give the same answer twice.
//
// This is NOT a claim about what the order is. MatchSorted documents ties in Rank as broken in
// an unspecified order and that stands: a caller needing a particular order must impose it.
// What a caller is entitled to is that the same job against the same unchanged store produces
// the same sequence -- without which "this change did not alter matchmaking" cannot be tested
// at all, and an assignment built on the result varies for no reason anyone can see.
//
// It did not. The parallel paths collected results per WORKER while workers STOLE tasks, so
// which worker held which slots varied between runs, and the number of workers varied too
// (tryAcquire can come back with none, in which case the serial path runs instead). The
// stable sort over that then faithfully preserved a different input order each time.
//
// So the assertion is repetition, not order, and the fixture is built to make a regression
// show: every slot ranks identically, which leaves the sort nothing to order by and the
// underlying sequence completely exposed.
func TestMatchOrderIsRepeatable(t *testing.T) {
	const slots = 600
	c := New(Options{Shards: 4, QueryParallelism: 8, SegmentSize: 1 << 12})

	for i := 0; i < slots; i++ {
		// Rank is a constant, so every match ties with every other.
		text := fmt.Sprintf(`[ Id=%d; Memory=8192; Arch="X86_64"; Cpus=8; Pad=%q; Requirements = true ]`,
			i, strings.Repeat("x", 200))
		if err := c.Put([]byte(fmt.Sprintf("slot%06d", i)), mustAd(t, text)); err != nil {
			t.Fatal(err)
		}
	}
	job := mustAd(t, `[ RequestMemory = 4096;
		Requirements = TARGET.Memory >= RequestMemory && TARGET.Arch == "X86_64";
		Rank = 1 ]`)

	// The parallel fan-out is where the order moved, so a run that never reaches it proves
	// nothing. It needs at least two tasks and a byte budget the default puts at 1 MiB;
	// drop the budget rather than build a megabyte of fixture, and check the task count.
	c.parallelMinBytes = 0
	tasks, _, release := c.gatherTasks()
	ntasks := len(tasks)
	release()
	if ntasks < 2 {
		t.Fatalf("fixture yields %d scan task(s); the parallel path needs at least 2, so this "+
			"test would pass without ever running the code it is about", ntasks)
	}

	render := func() string {
		ms := c.MatchSortedRanked(job, 0)
		if len(ms) != slots {
			t.Fatalf("matched %d slots, want %d", len(ms), slots)
		}
		var b strings.Builder
		for _, m := range ms {
			id, _ := m.Ad.EvaluateAttrInt("Id")
			fmt.Fprintf(&b, "%d,", id)
		}
		return b.String()
	}

	first := render()
	for i := 1; i < 25; i++ {
		if got := render(); got != first {
			t.Fatalf("run %d returned a different order from run 0 over an unchanged store "+
				"(%d scan tasks, so work was split across workers).\nfirst: %.120s\nrun %d: %.120s",
				i, ntasks, first, i, got)
		}
	}
}
