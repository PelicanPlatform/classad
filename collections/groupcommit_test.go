package collections

import (
	"fmt"
	"sync"
	"testing"
)

// TestGroupCommitCoalescesSyncs drives many concurrent durable transactions into a
// single-shard persistent collection and asserts (a) every commit's data survives a
// reopen -- each commit waited for a covering msync pass, closing the old race where a
// commit could observe an empty dirty list and ack before the covering msync finished --
// and (b) the msync passes were shared: far fewer passes ran than commits (group commit).
func TestGroupCommitCoalescesSyncs(t *testing.T) {
	const n = 64
	dir := t.TempDir()
	c, err := Open(Options{Shards: 1, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	base := c.OpStats().Sync.Count
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // maximize commit overlap
			tx := c.Begin()
			tx.Put([]byte(fmt.Sprintf("job.%d", i)), mustAd(t, fmt.Sprintf(`[ ClusterId = %d; Owner = "grp" ]`, i)))
			if res := tx.Commit(); res.Conflicted() {
				t.Errorf("commit %d conflicted: %v", i, res.Conflicts)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	passes := c.OpStats().Sync.Count - base
	t.Logf("%d concurrent durable commits ran %d msync passes", n, passes)
	if passes >= n {
		t.Errorf("no coalescing: %d msync passes for %d concurrent commits", passes, n)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Durability: every acked commit must be present after reopen.
	c2, err := Open(Options{Shards: 1, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	for i := 0; i < n; i++ {
		if _, ok := c2.Get([]byte(fmt.Sprintf("job.%d", i))); !ok {
			t.Errorf("job.%d missing after reopen (commit acked but not durable)", i)
		}
	}
}

// TestGroupCommitSequentialUnchanged pins that WITHOUT concurrency the coalescing is
// inert: each sequential durable commit still runs its own msync pass (its sequence is
// always past the last completed pass), so single-writer durability behavior -- and its
// metrics -- are unchanged.
func TestGroupCommitSequentialUnchanged(t *testing.T) {
	const n = 8
	c, err := Open(Options{Shards: 1, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	base := c.OpStats().Sync.Count
	for i := 0; i < n; i++ {
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("seq.%d", i)), mustAd(t, `[ Owner = "grp" ]`))
		if res := tx.Commit(); res.Conflicted() {
			t.Fatalf("commit %d conflicted", i)
		}
	}
	if passes := c.OpStats().Sync.Count - base; passes != n {
		t.Errorf("sequential commits ran %d msync passes, want %d (one each)", passes, n)
	}
}
