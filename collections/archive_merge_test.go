package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

func buildMergeArchive(t *testing.T, dir string, n int) *Archive {
	t.Helper()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ad, _ := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nPad = %q", i, strings.Repeat("m", 60)))
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// allClusterIDs reads every record newest-first, which is the order that matters: a merge
// that lands in the wrong position keeps every record but reorders them.
func allClusterIDs(t *testing.T, a *Archive) []int64 {
	t.Helper()
	q, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for ad := range a.Query(q) {
		v, _ := ad.EvaluateAttrInt("ClusterId")
		out = append(out, v)
	}
	return out
}

func sealedRun(t *testing.T, a *Archive, k int) []*segment {
	t.Helper()
	sh := a.c.shards[0]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	var run []*segment
	for _, s := range sh.segs {
		if s != nil && s != sh.act && s.used > 0 && len(run) < k {
			run = append(run, s)
		}
	}
	return run
}

// TestMergeSegmentsPreservesEverything is the core correctness check: after merging an
// adjacent run, the archive holds the same records in the same order -- in memory and again
// after a reopen, which is where a merge landing in the wrong position would show up.
func TestMergeSegmentsPreservesEverything(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	a := buildMergeArchive(t, dir, 500)
	before := allClusterIDs(t, a)
	segsBefore := a.c.Stats().Segments
	run := sealedRun(t, a, 3)
	if len(run) < 3 {
		t.Fatalf("need >=3 sealed segments, got %d", len(run))
	}

	a.c.maintMu.Lock()
	ok := a.c.mergeSegments(a.c.shards[0], run)
	a.c.maintMu.Unlock()
	if !ok {
		t.Fatal("mergeSegments reported failure")
	}
	if got := allClusterIDs(t, a); !equalIDs(got, before) {
		t.Fatalf("after merge: %d records, want %d (order/content changed)", len(got), len(before))
	}
	if after := a.c.Stats().Segments; after != segsBefore-2 {
		t.Errorf("segments = %d, want %d (3 merged into 1)", after, segsBefore-2)
	}
	a.Close()

	a2, err := OpenArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	if got := allClusterIDs(t, a2); !equalIDs(got, before) {
		t.Errorf("after reopen: %d records, want %d -- the merged segment did not land in "+
			"append order", len(got), len(before))
	}
}

// TestMergeCrashRecovery drives each window the intent marker exists to cover. The invariant
// is one-directional: a crash may lose the MERGE, never a RECORD.
func TestMergeCrashRecovery(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	t.Run("marker present, target still staged", func(t *testing.T) {
		dir := t.TempDir()
		a := buildMergeArchive(t, dir, 500)
		want := allClusterIDs(t, a)
		a.Close()
		shardDir := findShardDir(t, dir)
		names := segFileNames(t, shardDir)
		// Simulate: merged output staged and fsynced, marker written, crash before rename.
		staged := mergeTmpPrefix + "-9999.d0.dat"
		srcs := names[:2]
		copyFile(t, filepath.Join(shardDir, srcs[0]), filepath.Join(shardDir, staged))
		if err := writeFileSync(filepath.Join(shardDir, staged+mergeMarkerSuffix),
			[]byte(strings.Join(srcs, "\n"))); err != nil {
			t.Fatal(err)
		}
		finishPendingMerges(shardDir)
		if _, err := os.Stat(filepath.Join(shardDir, mergedFinalName(staged))); err != nil {
			t.Error("recovery did not rename the staged target into place")
		}
		for _, s := range srcs {
			if _, err := os.Stat(filepath.Join(shardDir, s)); err == nil {
				t.Errorf("source %s survived a completed merge", s)
			}
		}
		if leftover, _ := filepath.Glob(filepath.Join(shardDir, "*"+mergeMarkerSuffix)); len(leftover) != 0 {
			t.Errorf("marker not cleared: %v", leftover)
		}
		_ = want
	})

	t.Run("marker present, target lost: sources untouched", func(t *testing.T) {
		dir := t.TempDir()
		a := buildMergeArchive(t, dir, 500)
		want := allClusterIDs(t, a)
		a.Close()
		shardDir := findShardDir(t, dir)
		srcs := segFileNames(t, shardDir)[:2]
		staged := mergeTmpPrefix + "-8888.d0.dat" // never created
		if err := writeFileSync(filepath.Join(shardDir, staged+mergeMarkerSuffix),
			[]byte(strings.Join(srcs, "\n"))); err != nil {
			t.Fatal(err)
		}
		finishPendingMerges(shardDir)
		for _, s := range srcs {
			if _, err := os.Stat(filepath.Join(shardDir, s)); err != nil {
				t.Fatalf("source %s was removed though the merge never became durable", s)
			}
		}
		a2, err := OpenArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 12})
		if err != nil {
			t.Fatal(err)
		}
		defer a2.Close()
		if got := allClusterIDs(t, a2); !equalIDs(got, want) {
			t.Errorf("records changed: got %d, want %d", len(got), len(want))
		}
	})

	t.Run("staged output with no marker is discarded", func(t *testing.T) {
		dir := t.TempDir()
		a := buildMergeArchive(t, dir, 300)
		want := allClusterIDs(t, a)
		a.Close()
		shardDir := findShardDir(t, dir)
		staged := mergeTmpPrefix + "-7777.d0.dat"
		copyFile(t, filepath.Join(shardDir, segFileNames(t, shardDir)[0]), filepath.Join(shardDir, staged))
		finishPendingMerges(shardDir)
		if _, err := os.Stat(filepath.Join(shardDir, staged)); err == nil {
			t.Error("orphan staged output was not discarded")
		}
		a2, err := OpenArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 12})
		if err != nil {
			t.Fatal(err)
		}
		defer a2.Close()
		if got := allClusterIDs(t, a2); !equalIDs(got, want) {
			t.Errorf("records changed: got %d, want %d", len(got), len(want))
		}
	})
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
