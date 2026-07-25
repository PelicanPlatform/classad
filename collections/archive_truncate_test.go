package collections

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countFilesWithSuffix walks dir and counts files whose name ends in suffix -- used to
// assert that Truncate actually unlinks segment data (.dat) and sidecar (.idx) files, not
// just resets in-memory state.
func countFilesWithSuffix(t *testing.T, dir, suffix string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() && strings.HasSuffix(fi.Name(), suffix) {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestArchiveTruncate verifies Truncate empties the archive in place: records gone, on-disk
// segment + sidecar files unlinked, the store still usable (append + query) afterward, and
// the emptiness durable across a reopen. The archive is indexed (categorical + value) and
// uses a tiny segment size so several sealed segments with sidecar indexes exist -- the case
// that exercises the reapAndHook sidecar cleanup, not just the active segment.
func TestArchiveTruncate(t *testing.T) {
	dir := t.TempDir()
	opts := ArchiveOptions{
		SegmentSize:      8 << 10, // many small sealed segments (each with an .idx sidecar)
		CategoricalAttrs: []string{"Owner"},
		ValueAttrs:       []string{"Memory"},
		ZoneAttrs:        []string{"CompletionDate"},
	}
	a, _ := buildArchive(t, dir, 300, opts)

	if a.Count() != 300 {
		t.Fatalf("pre-truncate Count = %d, want 300", a.Count())
	}
	if archiveLiveSegs(a) < 2 {
		t.Fatalf("expected several sealed segments, got %d", archiveLiveSegs(a))
	}
	if dats := countFilesWithSuffix(t, dir, ".dat"); dats < 2 {
		t.Fatalf("expected >=2 .dat files before truncate, got %d", dats)
	}
	if idxs := countFilesWithSuffix(t, dir, ".idx"); idxs < 1 {
		t.Fatalf("expected >=1 sidecar .idx file before truncate, got %d", idxs)
	}

	a.Truncate()

	if a.Count() != 0 {
		t.Fatalf("post-truncate Count = %d, want 0", a.Count())
	}
	if got := archiveQueryIDs(t, a, "true"); len(got) != 0 {
		t.Fatalf("post-truncate query returned %d ads, want 0", len(got))
	}
	// The data + sidecar files must be unlinked, not merely forgotten -- otherwise recovery
	// would resurrect the truncated records and the disk would leak.
	if dats := countFilesWithSuffix(t, dir, ".dat"); dats != 0 {
		t.Errorf("post-truncate .dat files = %d, want 0 (segments not unlinked)", dats)
	}
	if idxs := countFilesWithSuffix(t, dir, ".idx"); idxs != 0 {
		t.Errorf("post-truncate .idx sidecar files = %d, want 0 (sidecars not unlinked)", idxs)
	}

	// Still usable: appends after a truncate are retained and queryable.
	appendJob := func(id int, owner string) {
		ad := mustAd(t, `[ ID=`+itoa(id)+`; Owner="`+owner+`"; Memory=1024; CompletionDate=1800000000 ]`)
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	appendJob(1000, "frank")
	appendJob(1001, "frank")
	if a.Count() != 2 {
		t.Fatalf("post-truncate re-append Count = %d, want 2", a.Count())
	}
	if got := archiveQueryIDs(t, a, `Owner == "frank"`); len(got) != 2 {
		t.Fatalf(`query Owner=="frank" returned %d, want 2`, len(got))
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	// Durability: reopen sees exactly the two post-truncate records, never the 300 dropped.
	b, err := OpenArchive(func() ArchiveOptions { o := opts; o.Dir = dir; return o }())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Count() != 2 {
		t.Errorf("reopened Count = %d, want 2 (truncate not durable)", b.Count())
	}
}
