package collections

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// openWalkSegmentBudget is how many times over an open may read the records of its ONE unsealed
// segment. That segment has no sidecar, so its extent, its two trailer records and its attribute
// index all have to be derived from the records themselves; every SEALED segment states those
// facts in its sidecar and is not walked at all.
//
// The budget is deliberately generous -- the number being defended is not 4 or 6 but "bounded by
// a segment", as against "bounded by the store". A phase that walks the whole store overshoots
// this by the segment count, which on any store worth opening is a factor of tens.
const openWalkSegmentBudget = 8

// openScaleArchive builds an archive of n small ads at segSize and returns its directory. With
// columnar set, its sealed segments are rewritten into columnar-native form, which gives each one
// the two trailer records (a payload and a dictionary) whose offsets the sidecar must carry -- the
// other half of the bound, and the half a plain archive does not exercise at all.
func openScaleArchive(t *testing.T, n, segSize int, columnar bool) string {
	t.Helper()
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: segSize, ValueAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ad := classad.New()
		_ = ad.Set("ClusterId", int64(i))
		_ = ad.Set("Owner", "alice")
		_ = ad.Set("CompletionDate", int64(1766900000+i*7))
		if err := a.AppendDeferred(ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	if columnar {
		if !a.BuildAndEnableSchemaScan(4096, 8) {
			t.Fatal("schema scan did not enable")
		}
		for a.c.ColumnarizeSealed() > 0 { // bounded per pass; converge
		}
		if !anySegmentColumnarized(a) {
			t.Fatal("no segment was columnarized")
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// anySegmentColumnarized reports whether any of the archive's segments carries a columnar payload.
func anySegmentColumnarized(a *Archive) bool {
	for _, seg := range a.c.shards[0].segs {
		if seg.columnarized() {
			return true
		}
	}
	return false
}

// openScaleReopen reopens the archive at dir and reports its open timing, its segment count, and
// the most records any one of its segments holds.
func openScaleReopen(t *testing.T, dir string) (tm OpenTiming, segs, maxSegRecs int) {
	t.Helper()
	var got OpenIndexDiag
	prev := OpenIndexDiagHook
	OpenIndexDiagHook = func(d OpenIndexDiag) { got = d }
	defer func() { OpenIndexDiagHook = prev }()
	a, err := OpenArchive(ArchiveOptions{Dir: dir, ValueAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, seg := range a.c.shards[0].segs {
		if seg == nil {
			continue
		}
		segs++
		if n := countSegRecords(seg); n > maxSegRecs {
			maxSegRecs = n
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	return got.Timing, segs, maxSegRecs
}

// countSegRecords frames a segment's written extent and returns how many records it holds.
func countSegRecords(seg *segment) int {
	n := 0
	for off := 0; off < seg.used; {
		total := recTotalLen(seg.data, uint32(off))
		if total == 0 || off+int(total) > seg.used {
			break
		}
		n++
		off += int(total)
	}
	return n
}

// TestOpenDoesNotScaleWithRecordCount holds the invariant that opening a persistent store costs
// O(segments), not O(records): every sealed segment states in its sidecar what open needs to know
// about it (its extent, which trailer records it carries, its index), so only the one unsealed
// segment is ever read record by record.
//
// It is checked against OpenTiming.RecordsWalked rather than elapsed time, because the regression
// it guards reads only record HEADERS -- cheap per record, and so it shows up as an open that is
// merely slow rather than one that is obviously wrong, which is how it survived for as long as it
// did. Quadrupling the records is the discriminator: a per-record phase quadruples with them,
// a per-segment one does not move.
func TestOpenDoesNotScaleWithRecordCount(t *testing.T) {
	for _, columnar := range []bool{false, true} {
		name := "plain"
		if columnar {
			name = "columnar"
		}
		t.Run(name, func(t *testing.T) { openScaleCheck(t, columnar) })
	}
}

func openScaleCheck(t *testing.T, columnar bool) {
	const segSize = 1 << 15
	small := openScaleArchive(t, 5000, segSize, columnar)
	large := openScaleArchive(t, 20000, segSize, columnar)

	tmS, segsS, maxS := openScaleReopen(t, small)
	tmL, segsL, maxL := openScaleReopen(t, large)
	t.Logf("small: %d segments, %d records in the largest, %d walked", segsS, maxS, tmS.RecordsWalked)
	t.Logf("large: %d segments, %d records in the largest, %d walked", segsL, maxL, tmL.RecordsWalked)

	// The premise of the comparison: the larger store really does hold ~4x the records in ~4x the
	// segments of the same size. Without this the test could pass because nothing was measured.
	if segsL < 3*segsS {
		t.Fatalf("the two stores are not far enough apart to discriminate: %d vs %d segments", segsS, segsL)
	}
	if maxS <= 0 || maxL <= 0 {
		t.Fatalf("no records found in either store's segments (%d, %d)", maxS, maxL)
	}

	cases := []struct {
		name   string
		tm     *OpenTiming
		maxSeg int
		segs   int
	}{
		{"small", &tmS, maxS, segsS},
		{"large", &tmL, maxL, segsL},
	}
	for _, c := range cases {
		if budget := openWalkSegmentBudget * c.maxSeg; c.tm.RecordsWalked > budget {
			t.Errorf("%s: open walked %d records, over the %d (= %d segments' worth) a store of %d segments may: "+
				"some phase is reading the whole store, not its metadata (%+v)",
				c.name, c.tm.RecordsWalked, budget, openWalkSegmentBudget, c.segs, *c.tm)
		}
	}

	// And the curve itself: 4x the records must not cost 4x the walking. The slack is one
	// segment's worth, because the unsealed segment's fill is whatever the record count left over.
	if grew := tmL.RecordsWalked - tmS.RecordsWalked; grew > openWalkSegmentBudget*maxL {
		t.Errorf("open scales with the record count: 4x the records walked %d more (max %d), small=%+v large=%+v",
			grew, openWalkSegmentBudget*maxL, tmS, tmL)
	}
}

// TestSealedSidecarRecordsTrailerAbsence checks the two halves of how that bound is achieved for
// the trailer-record searches: a segment sealed with neither an attribute dictionary nor a
// columnar payload says so in its sidecar, and an open that finds a sidecar not saying it writes
// it back, so the search runs once rather than at every start for the life of the store.
func TestSealedSidecarRecordsTrailerAbsence(t *testing.T) {
	dir := openScaleArchive(t, 5000, 1<<15, false)

	// (1) Sealed by this build: the absence is already on disk.
	sidecars := sealedSidecarPaths(t, dir)
	if len(sidecars) == 0 {
		t.Fatal("no sealed-segment sidecars found")
	}
	for _, p := range sidecars {
		if flags := readSidecarTrailer(p).flags; flags&(kidxNoDictRec|kidxNoColRec) != (kidxNoDictRec | kidxNoColRec) {
			t.Fatalf("%s: sidecar does not record that the segment has no trailer records (flags %#x)", p, flags)
		}
	}
	tm, _, maxSeg := openScaleReopen(t, dir)
	adopted := tm.RecordsWalked
	if adopted > openWalkSegmentBudget*maxSeg {
		t.Fatalf("reopen with the absences recorded still walked %d records (max %d): %+v", adopted, openWalkSegmentBudget*maxSeg, tm)
	}

	// (2) Strip the bits, as a sidecar written before they existed would have them: the next open
	// must search (walking far more), and must write the answer back.
	for _, p := range sidecars {
		stripSidecarKIDXFlags(t, p)
	}
	tmStripped, _, _ := openScaleReopen(t, dir)
	if tmStripped.RecordsWalked <= adopted {
		t.Fatalf("stripping the recorded absences did not make open search: %d walked vs %d with them",
			tmStripped.RecordsWalked, adopted)
	}
	if tmStripped.TrailerFlagsUpgraded != len(sidecars) || tmStripped.TrailerFlagsUpgradeFailed != 0 {
		t.Fatalf("open did not write the absences back into all %d sidecars: upgraded=%d failed=%d",
			len(sidecars), tmStripped.TrailerFlagsUpgraded, tmStripped.TrailerFlagsUpgradeFailed)
	}

	// (3) And the write-back sticks: the open after it is back to not searching.
	tmAfter, _, _ := openScaleReopen(t, dir)
	if tmAfter.RecordsWalked > openWalkSegmentBudget*maxSeg {
		t.Fatalf("the write-back did not take: open still walked %d records (max %d): %+v",
			tmAfter.RecordsWalked, openWalkSegmentBudget*maxSeg, tmAfter)
	}
	if tmAfter.TrailerFlagsUpgraded != 0 {
		t.Fatalf("open rewrote %d sidecars that already carried the absences", tmAfter.TrailerFlagsUpgraded)
	}
}

// sealedSidecarPaths returns the .idx sidecars under an archive directory.
func sealedSidecarPaths(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	shards, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, sh := range shards {
		if !sh.IsDir() {
			continue
		}
		ents, err := os.ReadDir(filepath.Join(dir, sh.Name()))
		if err != nil {
			continue
		}
		for _, e := range ents {
			if filepath.Ext(e.Name()) == ".idx" {
				out = append(out, filepath.Join(dir, sh.Name(), e.Name()))
			}
		}
	}
	return out
}

// stripSidecarKIDXFlags rewrites a sidecar with its key index's flags cleared, which is what a
// container written before those bits existed looks like.
func stripSidecarKIDXFlags(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	attr, key, col, zone, ok := splitSegmentSidecar(data)
	if !ok {
		t.Fatalf("%s: not a sidecar container", path)
	}
	facts := readSidecarTrailer(path)
	cleared := reflagKeyIndex(key, 0)
	if cleared == nil {
		t.Fatalf("%s: key index would not reflag", path)
	}
	container := buildSegmentSidecar(attr, cleared, col, zone, facts.used, facts.dictRec, facts.colRec, encodeDamage(facts.damage))
	if err := os.WriteFile(path, container, 0o644); err != nil {
		t.Fatal(err)
	}
}
