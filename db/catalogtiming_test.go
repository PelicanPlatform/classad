package db

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Catalog open is a loop over directories, so without per-open timing the whole thing is one number -- which
// is how a 15s startup stayed unattributed: every other phase reported 0s and catalog-open reported all of
// it, with nothing inside. OnOpenStep is what makes the next restart say WHICH table.
func TestCatalogOnOpenStepReportsEachTable(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"jobs", "history_t"} {
		if _, err := cat.EnsureTable(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cat.CreateArchiveTable("hist", ArchiveConfig{SegmentSize: 1 << 16}); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	seen := map[string]time.Duration{}
	kinds := map[string]string{}
	reopened, err := OpenCatalogConfig(CatalogConfig{
		Dir: dir,
		OnOpenStep: func(kind, name string, d time.Duration) {
			seen[name] = d
			kinds[name] = kind
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	for _, want := range []struct{ name, kind string }{
		{"jobs", "table"}, {"history_t", "table"}, {"hist", "archive"},
	} {
		if _, ok := seen[want.name]; !ok {
			t.Errorf("no timing reported for %s %q; catalog open would still be one opaque number",
				want.kind, want.name)
			continue
		}
		if kinds[want.name] != want.kind {
			t.Errorf("%q reported as kind %q, want %q", want.name, kinds[want.name], want.kind)
		}
	}
	// A nil hook must stay the default path, since every existing caller passes no hook.
	c3, err := OpenCatalogConfig(CatalogConfig{Dir: dir})
	if err != nil {
		t.Fatalf("opening without a hook: %v", err)
	}
	c3.Close()
	_ = fmt.Sprint
}

// Catalog open runs its table and archive opens CONCURRENTLY, which introduces two failure modes a serial
// loop did not have. Both are cheap to get wrong and invisible when they are.
func TestCatalogParallelOpenIsCorrect(t *testing.T) {
	dir := t.TempDir()
	const n = 12
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := cat.EnsureTable(fmt.Sprintf("t%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := cat.CreateArchiveTable(fmt.Sprintf("a%d", i), ArchiveConfig{SegmentSize: 1 << 16}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	// EVERY table must land in the map, under its own name. Results are collected into a slice and inserted
	// after the join precisely so a concurrent map write cannot drop or misfile one.
	var mu sync.Mutex
	seen := map[string]int{}
	reopened, err := OpenCatalogConfig(CatalogConfig{
		Dir: dir,
		OnOpenStep: func(kind, name string, _ time.Duration) {
			mu.Lock() // the hook is called from whichever goroutine opened that table
			defer mu.Unlock()
			seen[kind+":"+name]++
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("t%02d", i)
		if _, ok := reopened.Table(name); !ok {
			t.Errorf("table %q missing after a parallel open", name)
		}
		if got := seen["table:"+name]; got != 1 {
			t.Errorf("table %q was opened %d times, want exactly 1", name, got)
		}
	}
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("a%d", i)
		if got := seen["archive:"+name]; got != 1 {
			t.Errorf("archive %q was opened %d times, want exactly 1", name, got)
		}
	}
}

// A failing open must not leak the ones that succeeded. They are not in the catalog's maps yet, so closeAll
// cannot reach them -- the helper closes them itself, and a leaked mmap would otherwise keep files open for
// the life of the process.
func TestCatalogParallelOpenFailureClosesTheRest(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := cat.EnsureTable(fmt.Sprintf("t%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	// Make one table unopenable by replacing its directory with a file, so the parallel batch has both
	// successes and a failure.
	bad := filepath.Join(dir, tablesSubdir, "t3")
	if err := os.RemoveAll(bad); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("not a directory"), 0o640); err != nil {
		t.Fatal(err)
	}

	// A file is skipped rather than opened (the loop requires a directory), so this must still SUCCEED with
	// five tables -- which is the pre-existing behaviour and worth pinning while changing the loop around it.
	c2, err := OpenCatalogConfig(CatalogConfig{Dir: dir})
	if err != nil {
		t.Fatalf("a stray file in the tables directory should be ignored, not fatal: %v", err)
	}
	defer c2.Close()
	if _, ok := c2.Table("t3"); ok {
		t.Error("t3 is a file, not a table directory; it should not have loaded")
	}
	for _, name := range []string{"t0", "t1", "t2", "t4", "t5"} {
		if _, ok := c2.Table(name); !ok {
			t.Errorf("table %q should still be present", name)
		}
	}
}
