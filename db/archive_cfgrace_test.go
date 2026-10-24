package db

import (
	"fmt"
	"sync"
	"testing"
)

// TestArchiveConfigConcurrentPersist covers the config struct an ArchiveTable folds live state
// back into. Several methods write parts of it and then marshal the whole thing to
// archiveconfig.json, and callers drive them from different goroutines: a federated hub
// backfills an archive's indexes in the background while the foreground asserts the required
// attribute set on the same table.
//
// Unguarded that is both a data race on cfg and a torn write -- the marshalled snapshot can hold
// half of one update -- and two savers also collide on the shared .tmp path. Run under -race;
// without the lock the race detector reports AddIndex's write against saveConfig's read.
func TestArchiveConfigConcurrentPersist(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{
		ValueAttrs: []string{"ClusterId"},
		ZoneAttrs:  []string{"CompletionDate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := hist.AppendOld(fmt.Sprintf("ClusterId = %d\nCompletionDate = %d\nOwner = \"u\"\nRequestMemory = %d", i, 1000+i, i)); err != nil {
			t.Fatal(err)
		}
	}

	// Every pair below writes cfg and then persists it; each goroutine exercises a different
	// writer so the test covers the set rather than one method against itself.
	var wg sync.WaitGroup
	start := make(chan struct{})
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fn()
		}()
	}
	run(func() { hist.AddIndex([]string{"Owner"}, nil) })
	run(func() { hist.AddIndex(nil, []string{"RequestMemory"}) })
	run(func() {
		if _, err := hist.SetSchemaRequireAttrs([]string{"RequestMemory", "CompletionDate"}); err != nil {
			t.Errorf("SetSchemaRequireAttrs: %v", err)
		}
	})
	run(func() { hist.AddHotAttrs("Owner") })
	run(func() {
		if err := hist.SetRowGroupBytes(1 << 16); err != nil {
			t.Errorf("SetRowGroupBytes: %v", err)
		}
	})
	close(start)
	wg.Wait()

	// The file left behind must be readable and must carry every update, not whichever one
	// happened to marshal last: a torn write loses an earlier writer's field silently.
	cat2, err := OpenCatalog(cat.dir)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	h2, ok := cat2.ArchiveTable("history")
	if !ok {
		t.Fatal("history archive missing after reopen")
	}
	cats, vals := h2.IndexedAttrs()
	if !containsFold(cats, "Owner") {
		t.Errorf("categorical index on Owner lost across reopen: %v", cats)
	}
	if !containsFold(vals, "RequestMemory") {
		t.Errorf("value index on RequestMemory lost across reopen: %v", vals)
	}
	if got := h2.SchemaRequireAttrs(); !containsFold(got, "RequestMemory") {
		t.Errorf("required attrs lost across reopen: %v", got)
	}
	if got := h2.HotAttrs(); !containsFold(got, "Owner") {
		t.Errorf("hot attrs lost across reopen: %v", got)
	}
	if got := h2.RowGroupBytes(); got != 1<<16 {
		t.Errorf("RowGroupBytes = %d, want %d", got, 1<<16)
	}
}

func containsFold(hay []string, needle string) bool {
	for _, h := range hay {
		if len(h) == len(needle) && equalFold(h, needle) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	for i := range a {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
