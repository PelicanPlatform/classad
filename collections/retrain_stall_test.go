package collections

import (
	"fmt"
	"testing"
	"time"
)

// TestRetrainDoesNotBlockConcurrentAccess asserts that a dictionary retrain holds no lock
// that a concurrent transaction (Begin/Put/Commit) or read (Get) needs. RetrainDict swaps
// the codec atomically and recompacts shard-by-shard under only brief per-shard locks, so
// writers and readers must proceed throughout -- the property the store's design relies on.
//
// The retrainStallHook makes one retrain observably long (a full second, holding NO
// collection lock) so a wide, deterministic window overlaps it. If retrain blocked any of
// these operations via a shared lock, their worst-case latency would approach the stall
// duration; the assertion is that they stay orders of magnitude below it.
//
// This is the regression record for the collector "slow batch-flush during retrain"
// investigation: the flush stall was NOT a retrain lock (proven here), which redirected the
// hunt to the RPC round-trip's send/wait phases.
func TestRetrainDoesNotBlockConcurrentAccess(t *testing.T) {
	const stall = 1 * time.Second

	sample := loadCorpus(t)
	c := populate(t, sample, 3000)

	started := make(chan struct{})
	retrainStallHook = func() {
		close(started)
		time.Sleep(stall)
	}
	defer func() { retrainStallHook = nil }()

	retrainDone := make(chan struct{})
	go func() {
		if _, err := c.RetrainDict(2000); err != nil {
			t.Errorf("RetrainDict: %v", err)
		}
		close(retrainDone)
	}()

	<-started // retrain is now inside the stalled section

	var maxBegin, maxCommit, maxGet time.Duration
	probes := 0
	deadline := time.Now().Add(stall - 100*time.Millisecond)
	for time.Now().Before(deadline) {
		t0 := time.Now()
		tx := c.Begin()
		if d := time.Since(t0); d > maxBegin {
			maxBegin = d
		}
		tx.Put([]byte(fmt.Sprintf("probe.%d", probes)), mustAd(t, `[ Name = "probe@host"; State = "Claimed" ]`))
		t1 := time.Now()
		if res := tx.Commit(); res.Conflicted() {
			t.Fatalf("probe commit conflicted: %v", res.Conflicts)
		}
		if d := time.Since(t1); d > maxCommit {
			maxCommit = d
		}

		t2 := time.Now()
		_, _ = c.Get([]byte("probe.0"))
		if d := time.Since(t2); d > maxGet {
			maxGet = d
		}
		probes++
	}

	<-retrainDone
	t.Logf("during a %v retrain stall: %d probes; max Begin=%v  max Commit=%v  max Get=%v",
		stall, probes, maxBegin, maxCommit, maxGet)

	limit := stall / 5
	if maxBegin > limit {
		t.Errorf("Begin blocked %v during retrain (limit %v) -- retrain holds a lock writers need", maxBegin, limit)
	}
	if maxCommit > limit {
		t.Errorf("Commit blocked %v during retrain (limit %v)", maxCommit, limit)
	}
	if maxGet > limit {
		t.Errorf("Get blocked %v during retrain (limit %v)", maxGet, limit)
	}
}
