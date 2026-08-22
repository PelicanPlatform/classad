package collections

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestOpenTiming verifies the per-phase open timing is populated on a persistent reopen, and that
// a dir-snapshot miss (which forces a full record-walk rebuild) is both flagged and reflected in
// DirRestore -- the signal that separates a segment-count reopen from a record-count one.
func TestOpenTiming(t *testing.T) {
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 15, ValueAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		ad := classad.New()
		_ = ad.Set("ClusterId", int64(i))
		_ = ad.Set("Owner", "alice")
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	var got OpenIndexDiag
	OpenIndexDiagHook = func(d OpenIndexDiag) { got = d }
	defer func() { OpenIndexDiagHook = nil }()

	reopen := func() OpenTiming {
		got = OpenIndexDiag{}
		aa, err := OpenArchive(ArchiveOptions{Dir: dir, ValueAttrs: []string{"ClusterId"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = aa.Close()
		return got.Timing
	}

	// (1) Clean reopen: a dir snapshot was written by the clean Close above, so the directory is
	// restored (not rebuilt), and mapping the segments is always timed.
	t1 := reopen()
	if t1.MapSegments <= 0 {
		t.Errorf("MapSegments not timed: %+v", t1)
	}
	if t1.DirRebuilt {
		t.Errorf("clean reopen rebuilt the directory instead of restoring the snapshot: %+v", t1)
	}

	// (2) Remove the dir snapshot to simulate an unclean shutdown: the reopen must fall back to a
	// full rebuild, which the timing flags (DirRebuilt) and attributes to DirRestore.
	subs, _ := os.ReadDir(dir)
	removed := 0
	for _, e := range subs {
		if !e.IsDir() {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name(), dirSnapName)) == nil {
			removed++
		}
	}
	if removed == 0 {
		t.Fatal("no dir snapshot found to remove (expected one from the clean Close)")
	}
	t2 := reopen()
	if !t2.DirRebuilt {
		t.Errorf("snapshot-miss reopen did not flag DirRebuilt: %+v", t2)
	}
	if t2.DirRestore <= 0 {
		t.Errorf("DirRestore not timed on the rebuild path: %+v", t2)
	}
}
