package collections

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestOpenIndexDiagHook verifies the diagnostic distinguishes the healthy adopt path from the
// two rebuild causes it exists to explain: a present-but-removed sidecar (rejected/absent) and
// a segment with no sidecar file at all.
func TestOpenIndexDiagHook(t *testing.T) {
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 15, ValueAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		ad := classad.New()
		_ = ad.Set("ClusterId", int64(i%500))
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

	reopen := func() OpenIndexDiag {
		got = OpenIndexDiag{}
		aa, err := OpenArchive(ArchiveOptions{Dir: dir, ValueAttrs: []string{"ClusterId"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = aa.Close()
		return got
	}

	// (1) Clean reopen: every sealed segment's attribute index should be adopted, nothing rebuilt.
	d1 := reopen()
	if d1.SealedSegments == 0 {
		t.Fatalf("expected sealed segments, got 0 (diag=%+v)", d1)
	}
	if d1.AttrIndexAdopted != d1.SealedSegments {
		t.Errorf("clean reopen: adopted %d of %d sealed; reasons=%v (nothing should rebuild)",
			d1.AttrIndexAdopted, d1.SealedSegments, d1.Reasons)
	}

	// (2) Remove all sidecar files: every segment should report "no-sidecar-file" and none adopt.
	removed := 0
	subs, _ := os.ReadDir(dir)
	for _, e := range subs {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		files, _ := os.ReadDir(sub)
		for _, f := range files {
			n := f.Name()
			if len(n) > 4 && (n[len(n)-4:] == ".idx" || (len(n) > 5 && n[len(n)-5:] == ".kidx")) {
				if os.Remove(filepath.Join(sub, n)) == nil {
					removed++
				}
			}
		}
	}
	if removed == 0 {
		t.Fatal("no sidecar files found to remove")
	}
	d2 := reopen()
	if d2.AttrIndexAdopted != 0 {
		t.Errorf("after removing sidecars: adopted %d, want 0", d2.AttrIndexAdopted)
	}
	if d2.Reasons["no-sidecar-file"] != d2.SealedSegments {
		t.Errorf("after removing sidecars: no-sidecar-file=%d, want %d (reasons=%v)",
			d2.Reasons["no-sidecar-file"], d2.SealedSegments, d2.Reasons)
	}

	// (3) After (2)'s reopen rebuilt+persisted sidecars, a further reopen adopts again.
	d3 := reopen()
	if d3.AttrIndexAdopted != d3.SealedSegments {
		t.Errorf("reopen after rebuild: adopted %d of %d; reasons=%v", d3.AttrIndexAdopted, d3.SealedSegments, d3.Reasons)
	}
}
