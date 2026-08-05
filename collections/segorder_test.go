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

// TestSegmentOrderIsContentNotFilename pins that recovery orders segments by what they
// contain, not by the number in their file name.
//
// This is the hazard a segment merge introduces: the merged file is allocated last and so
// gets the highest number, while holding the OLDEST records. Ordered by name it would come
// back as the newest segment, and a newest-first query with a limit -- "the last K jobs" --
// would return ancient records and stop. The failure is silent: every record is present and
// correct, only their order is wrong.
//
// Renaming the oldest segment's file to the highest number simulates that without needing a
// merge, so the invariant is protected before anything depends on it.
func TestSegmentOrderIsContentNotFilename(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	const n = 400
	for i := 0; i < n; i++ {
		ad, _ := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nPad = %q", i, strings.Repeat("z", 60)))
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	newestBefore := newestClusterID(t, a)
	if newestBefore != n-1 {
		t.Fatalf("newest before reopen = %d, want %d", newestBefore, n-1)
	}
	a.Close()

	// Find the shard dir and renumber the OLDEST segment file to the highest number.
	shardDir := findShardDir(t, dir)
	names := segFileNames(t, shardDir)
	if len(names) < 3 {
		t.Fatalf("need >=3 segments, got %d", len(names))
	}
	oldest, newest := names[0], names[len(names)-1]
	var on, nn uint64
	var od, nd uint32
	fmt.Sscanf(oldest, "seg-%d.d%d.dat", &on, &od)
	fmt.Sscanf(newest, "seg-%d.d%d.dat", &nn, &nd)
	renamed := fmt.Sprintf("seg-%d.d%d.dat", nn+1000, od)
	for _, suffix := range []string{"", ".idx"} {
		src := filepath.Join(shardDir, oldest+suffix)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Rename(src, filepath.Join(shardDir, renamed+suffix)); err != nil {
			t.Fatal(err)
		}
	}

	a2, err := OpenArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	if got := a2.Count(); got != n {
		t.Errorf("recovered %d records, want %d", got, n)
	}
	if got := newestClusterID(t, a2); got != n-1 {
		t.Errorf("newest after reopen = %d, want %d -- segments were ordered by file name, "+
			"so the oldest segment came back as the newest", got, n-1)
	}
}

func newestClusterID(t *testing.T, a *Archive) int64 {
	t.Helper()
	q, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	for ad := range a.QueryLimit(q, 1) {
		v, _ := ad.EvaluateAttrInt("ClusterId")
		return v
	}
	t.Fatal("no records")
	return -1
}

func findShardDir(t *testing.T, dir string) string {
	t.Helper()
	var found string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || found != "" {
			return nil
		}
		if strings.HasPrefix(fi.Name(), "seg-") && strings.HasSuffix(fi.Name(), ".dat") {
			found = filepath.Dir(p)
		}
		return nil
	})
	if found == "" {
		t.Fatal("no segment files found")
	}
	return found
}

func segFileNames(t *testing.T, shardDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(shardDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "seg-") && strings.HasSuffix(e.Name(), ".dat") {
			out = append(out, e.Name())
		}
	}
	return out
}
