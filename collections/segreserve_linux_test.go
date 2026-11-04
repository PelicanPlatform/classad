//go:build linux

package collections

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestSegmentFallocateReservesBlocks confirms reserve() commits real disk blocks up front on Linux
// (fallocate), rather than leaving the segment file sparse (Truncate). A dense file means a
// disk-full is caught at allocation -- not later at mmap writeback, where it could silently lose
// data or SIGBUS.
func TestSegmentFallocateReservesBlocks(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// A record forces the active segment file to exist at full size.
	if err := c.Put([]byte("k"), mustAd(t, `[Id=1]`)); err != nil {
		t.Fatal(err)
	}

	segPath := ""
	entries, _ := os.ReadDir(filepath.Join(dir, "0"))
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".dat" {
			segPath = filepath.Join(dir, "0", e.Name())
			break
		}
	}
	if segPath == "" {
		t.Fatalf("no segment .dat file under %s", filepath.Join(dir, "0"))
	}

	var st unix.Stat_t
	if err := unix.Stat(segPath, &st); err != nil {
		t.Fatal(err)
	}
	allocated := st.Blocks * 512 // st_blocks is in 512-byte units
	// The segment was created 1 MiB. fallocate reserves (approximately) all of it; a sparse Truncate
	// would leave only the touched pages allocated (a few KiB). Require most of it to be committed.
	if allocated < st.Size*9/10 {
		t.Errorf("segment %s: %d bytes allocated of %d (sparse); fallocate did not reserve blocks",
			filepath.Base(segPath), allocated, st.Size)
	}
	t.Logf("segment size=%d allocated=%d (%.0f%% reserved)", st.Size, allocated, 100*float64(allocated)/float64(st.Size))
}
