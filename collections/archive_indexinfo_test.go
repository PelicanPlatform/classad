package collections

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TestArchiveIndexIntrospection covers what `.indexes <archive>` reports: the configured
// index attributes and the zone-mapped ones. Zone maps cover the explicit ZoneAttrs plus
// every value-indexed attribute, which is what makes a range query prune whole segments.
func TestArchiveIndexIntrospection(t *testing.T) {
	t.Parallel()
	a, _ := buildArchive(t, t.TempDir(), 400, ArchiveOptions{
		CategoricalAttrs: []string{"Owner", "JobStatus"},
		ValueAttrs:       []string{"Memory"},
		ZoneAttrs:        []string{"CompletionDate"},
	})
	defer a.Close()

	cat, val := a.IndexedAttrs()
	if !slices.Contains(cat, "Owner") || !slices.Contains(cat, "JobStatus") {
		t.Errorf("categorical indexes = %v, want Owner and JobStatus", cat)
	}
	if !slices.Contains(val, "Memory") {
		t.Errorf("value indexes = %v, want Memory", val)
	}
	zones := a.ZoneAttrs()
	for _, want := range []string{"CompletionDate", "Memory"} {
		if !slices.Contains(zones, want) {
			t.Errorf("zone attrs = %v, want %s (explicit zone attrs plus value-indexed ones)", zones, want)
		}
	}
	if slices.Contains(zones, "Owner") {
		t.Errorf("zone attrs = %v: a categorical index is not zone-mapped", zones)
	}
}

// TestArchiveAddIndexReindexesInPlace is the point of a re-index as distinct from a
// rewrite: adding an index to an existing archive must reach every already-sealed segment
// by rebuilding only their index sidecars, leaving all segment DATA byte-for-byte untouched.
// It also checks that a value index added at runtime becomes zone-mapped, as one configured
// at creation would be.
func TestArchiveAddIndexReindexesInPlace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, src := buildArchive(t, dir, 400, ArchiveOptions{CategoricalAttrs: []string{"Owner"}})
	defer a.Close()
	if got := a.ZoneAttrs(); len(got) != 0 {
		t.Fatalf("zone attrs = %v, want none (no value or zone attrs configured)", got)
	}
	if _, sealed := a.StaleIndexSegments(); sealed == 0 {
		t.Fatal("expected several sealed segments")
	}
	before := hashArchiveData(t, dir)

	if !a.AddIndex(nil, []string{"CompletionDate"}) {
		t.Fatal("AddIndex reported no change")
	}
	if got := a.ZoneAttrs(); !slices.Contains(got, "CompletionDate") {
		t.Errorf("zone attrs = %v, want CompletionDate: a runtime value index should be zoned too", got)
	}
	// Every sealed segment now carries an index built under the current configuration.
	if stale, sealed := a.StaleIndexSegments(); stale != 0 {
		t.Errorf("stale/sealed = %d/%d, want 0 stale: the sidecars are rebuilt in place", stale, sealed)
	}
	// ... and not one byte of segment data was rewritten to get there.
	after := hashArchiveData(t, dir)
	if len(before) == 0 {
		t.Fatal("no segment data files found")
	}
	if !maps.Equal(before, after) {
		t.Errorf("segment data changed: a re-index must not rewrite data\n before=%v\n after=%v", before, after)
	}

	// Correctness is unchanged throughout: the new index is a pruning aid, not a filter.
	for _, qs := range []string{
		`CompletionDate > 1700000100`,
		`CompletionDate > 1700000100 && CompletionDate < 1700000200`,
		`Owner == "carol" && CompletionDate >= 1700000300`,
		`CompletionDate > 1700000900`, // above every value: fully pruned
	} {
		got := archiveQueryIDs(t, a, qs)
		want := bruteIDs(src, mustQuery(t, qs))
		if !equalInts(got, want) {
			t.Errorf("%s: got %d ads, want %d", qs, len(got), len(want))
		}
	}
}

// TestArchiveReindexSurvivesReopen checks that an in-place re-indexed sidecar is the one
// found on reopen -- i.e. the rebuild really replaced the on-disk sidecar, rather than only
// the in-process mapping.
func TestArchiveReindexSurvivesReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	opts := ArchiveOptions{CategoricalAttrs: []string{"Owner"}}
	a, src := buildArchive(t, dir, 400, opts)
	if !a.AddIndex(nil, []string{"CompletionDate"}) {
		t.Fatal("AddIndex reported no change")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	opts.Dir = dir
	opts.ValueAttrs = []string{"CompletionDate"} // the config a restart would replay
	b, err := OpenArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if stale, sealed := b.StaleIndexSegments(); stale != 0 {
		t.Errorf("after reopen stale/sealed = %d/%d, want the rebuilt sidecars to load", stale, sealed)
	}
	for _, qs := range []string{`CompletionDate > 1700000100`, `CompletionDate > 1700000100 && CompletionDate < 1700000200`} {
		got := archiveQueryIDs(t, b, qs)
		want := bruteIDs(src, mustQuery(t, qs))
		if !equalInts(got, want) {
			t.Errorf("after reopen %s: got %d ads, want %d", qs, len(got), len(want))
		}
	}
}

// hashArchiveData fingerprints every file under dir that is NOT an index sidecar, so a test
// can assert that a re-index left the segment data alone.
func hashArchiveData(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".idx") {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(dir, path)
		out[rel] = fmt.Sprintf("%x", sha256.Sum256(b))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// TestArchiveReindexUnderConcurrentScans is the safety anchor for swapping a sealed
// segment's sidecar while it is being read. A scan's candidate bitmaps are zero-copy views
// into the mapping, so releasing a displaced mapping one pin too early is a use-after-munmap
// (a crash, not a wrong answer). Readers hammer the archive while an indexer forces repeated
// sidecar swaps; every query must keep returning the brute-force answer.
func TestArchiveReindexUnderConcurrentScans(t *testing.T) {
	t.Parallel()
	a, src := buildArchive(t, t.TempDir(), 1200, ArchiveOptions{
		CategoricalAttrs: []string{"Owner"},
		ValueAttrs:       []string{"Memory"},
	})
	defer a.Close()

	queries := []string{
		`Memory > 2048`,
		`Memory > 2048 && Memory < 6144`,
		`Owner == "carol"`,
		`CompletionDate > 1700000600`,
		`Owner == "bob" && Memory >= 4096`,
	}
	want := make([][]int, len(queries))
	for i, qs := range queries {
		want[i] = bruteIDs(src, mustQuery(t, qs))
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 6; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				i := (r + n) % len(queries)
				if got := archiveQueryIDs(t, a, queries[i]); !equalInts(got, want[i]) {
					t.Errorf("%s: got %d ads, want %d", queries[i], len(got), len(want[i]))
					return
				}
			}
		}(r)
	}
	// Each add/drop bumps the spec generation, so every sealed sidecar is rebuilt and its
	// predecessor queued for release -- exactly the window under test.
	for i := 0; i < 6; i++ {
		a.AddIndex(nil, []string{"CompletionDate"})
		a.DropIndex("CompletionDate")
	}
	close(stop)
	wg.Wait()
}
