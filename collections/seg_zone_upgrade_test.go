package collections

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestSegZoneUpgradePersistsAcrossReopen is the regression for the every-open zone recompute: a
// sealed segment whose sidecar has no zone section must, on the reopen that recomputes the map, also
// PERSIST it -- so the following reopen adopts it instead of decoding every record again. Without
// the upgrade the recompute recurs on every start (sealSegmentIndex never revisits a sealed segment).
func TestSegZoneUpgradePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 15,
		ValueAttrs: []string{"ClusterId"}, ZoneAttrs: []string{"CompletionDate"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		ad := classad.New()
		_ = ad.Set("ClusterId", int64(i))
		_ = ad.Set("CompletionDate", int64(1700000000+i))
		_ = ad.Set("Owner", "alice")
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate legacy sidecars: strip the zone section from every sealed segment's .idx, so the
	// next open cannot adopt a zone map and must recompute (the state a pre-zone archive is in).
	stripped := stripZoneSections(t, dir)
	if stripped == 0 {
		t.Fatal("no sidecars found to strip")
	}

	var timing OpenTiming
	OpenIndexDiagHook = func(d OpenIndexDiag) { timing = d.Timing }
	defer func() { OpenIndexDiagHook = nil }()
	reopen := func() OpenTiming {
		timing = OpenTiming{}
		aa, err := OpenArchive(ArchiveOptions{Dir: dir, ValueAttrs: []string{"ClusterId"},
			ZoneAttrs: []string{"CompletionDate"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = aa.Close()
		return timing
	}

	// First reopen after stripping: the fallback recomputes the zone map (and, with the fix, writes
	// it back into the sidecar).
	t1 := reopen()
	if t1.ZoneRecompute == 0 {
		t.Fatal("expected a zone recompute on the first reopen after stripping the sidecars")
	}

	// Second reopen: the sidecar now carries the zone section, so it is adopted -- no recompute.
	t2 := reopen()
	if t2.ZoneRecompute != 0 {
		t.Errorf("zone map was not persisted: the second reopen recomputed again (%v). "+
			"The recompute recurs on every open.", t2.ZoneRecompute)
	}
}

// stripZoneSections rewrites every sealed-segment sidecar (*.dat.idx) under an archive dir to
// remove its zone section, reproducing a legacy pre-zone-persistence sidecar. Returns the count.
func stripZoneSections(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".dat.idx") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		attr, key, col, zone, used, dictRec, ok := splitSegmentSidecarV4(data)
		if !ok || len(zone) == 0 {
			return nil // already zone-less or unreadable
		}
		rebuilt := buildSegmentSidecar(
			append([]byte(nil), attr...), append([]byte(nil), key...), append([]byte(nil), col...),
			nil, used, dictRec)
		if os.WriteFile(path, rebuilt, 0o644) == nil {
			n++
		}
		return nil
	})
	return n
}
