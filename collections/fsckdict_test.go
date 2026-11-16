package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildDictStore builds a persistent collection whose segments are compressed under a
// TRAINED dictionary (id 1), not the base codec. Dictionary id 0 needs no file, so a
// store that never retrained cannot exercise any of this.
func buildDictStore(t *testing.T, n int) string {
	t.Helper()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	sample := loadCorpus(t)
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	put := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			if err := c.Put([]byte(fmt.Sprintf("job%05d", i)), sample[i%len(sample)]); err != nil {
				t.Fatalf("put: %v", err)
			}
		}
	}
	// Retrain in the MIDDLE. A retrain installs a codec for new writes and does not
	// rewrite what is already sealed, so this is what produces a store holding segments
	// under both the base codec (id 0, which needs no file) and the trained one (id 1).
	// That mix is the realistic shape and the one the assertions below partition on.
	put(0, n/2)
	if _, err := c.RetrainDict(1500); err != nil {
		t.Fatalf("retrain: %v", err)
	}
	put(n/2, n)
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dicts", "1.zst")); err != nil {
		t.Fatalf("expected a trained dictionary 1: %v", err)
	}
	return dir
}

// recordsByDict sums a report's per-segment record counts for segments naming dictID.
func recordsByDict(r *FsckReport, dictID uint32) (segments, records int) {
	for i := range r.Segments {
		if r.Segments[i].DictID == dictID {
			segments++
			records += r.Segments[i].Records
		}
	}
	return
}

// dictPath is <dir>/dicts/<id>.zst.
func dictPath(dir string, id uint32) string {
	return filepath.Join(dir, "dicts", fmt.Sprintf("%d.zst", id))
}

// TestFsckCountsDictlessRecordsAsUndecodable is the headline defect: record framing and
// checksums say nothing about whether the bytes can be decompressed, so a segment whose
// dictionary is gone used to contribute every record to "N record(s) readable" and
// nothing to the unreadable count -- while the same report printed MISSING dictionary 1.
func TestFsckCountsDictlessRecordsAsUndecodable(t *testing.T) {
	dir := buildDictStore(t, 2000)

	before, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	healthy, _, _ := before.Totals()
	baseSegs, baseRecs := recordsByDict(before, 0)
	trainedSegs, trainedRecs := recordsByDict(before, 1)
	// Both halves must be non-empty or the partition below proves nothing.
	if baseRecs == 0 || trainedRecs == 0 {
		t.Fatalf("fixture did not straddle the retrain: dict0=%d records in %d segments, "+
			"dict1=%d in %d", baseRecs, baseSegs, trainedRecs, trainedSegs)
	}
	if healthy != baseRecs+trainedRecs {
		t.Fatalf("control: Totals %d != %d+%d", healthy, baseRecs, trainedRecs)
	}

	if rmErr := os.Remove(dictPath(dir, 1)); rmErr != nil {
		t.Fatal(rmErr)
	}
	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + r.String())

	rec, _, _ := r.Totals()
	segs, undec := r.Undecodable()
	if rec != baseRecs {
		t.Errorf("Totals reported %d readable records; want %d, the records still decodable "+
			"under dictionary 0", rec, baseRecs)
	}
	if segs != trainedSegs || undec != trainedRecs {
		t.Errorf("Undecodable = (%d segments, %d records); want (%d, %d)",
			segs, undec, trainedSegs, trainedRecs)
	}
	for i := range r.Segments {
		if got, want := r.Segments[i].DictUnusable, r.Segments[i].DictID == 1; got != want {
			t.Errorf("segment %s (dict %d): DictUnusable = %v, want %v",
				r.Segments[i].Path, r.Segments[i].DictID, got, want)
		}
	}
	if out := r.String(); !strings.Contains(out, "UNDECODABLE") {
		t.Error("report does not say the records are undecodable")
	}
}

// TestFsckRejectsUnloadableDictFile covers the other half: a dictionary file that exists
// but does not load is not a dictionary the store has. Reporting it present let fsck
// pronounce a store healthy that Open then refused with "rebuild codec for dict 1".
func TestFsckRejectsUnloadableDictFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []byte
		why     string
	}{
		{"empty", nil, "empty file"},
		{"garbage", []byte("this is not a dictionary"), "not a zstd dictionary"},
		{"truncated magic", []byte{0x37, 0xA4}, "not a zstd dictionary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := buildDictStore(t, 2000)
			if err := os.WriteFile(dictPath(dir, 1), tc.content, 0o644); err != nil {
				t.Fatal(err)
			}
			r, err := Fsck(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Log("\n" + r.String())

			for _, id := range r.DictsPresent {
				if id == 1 {
					t.Error("an unloadable dictionary was reported present")
				}
			}
			var found *FsckDictFile
			for i := range r.DictsDamaged {
				if r.DictsDamaged[i].ID == 1 {
					found = &r.DictsDamaged[i]
				}
			}
			if found == nil {
				t.Fatalf("dictionary 1 not reported damaged; damaged=%v", r.DictsDamaged)
			}
			if !strings.Contains(found.Why, tc.why) {
				t.Errorf("Why = %q; want it to mention %q", found.Why, tc.why)
			}
			_, trainedRecs := recordsByDict(r, 1)
			if trainedRecs == 0 {
				t.Fatal("fixture wrote no segment under the trained dictionary")
			}
			if _, undec := r.Undecodable(); undec != trainedRecs {
				t.Errorf("Undecodable reported %d records over an unloadable dictionary; want %d",
					undec, trainedRecs)
			}
		})
	}
}

// TestFsckAtticDictMustLoadToBeRecoverable: the RECOVERABLE line tells an operator to move
// a file back into dicts/. Saying that about a file that will not open is worse than
// saying nothing, because it costs a daemon restart to find out.
func TestFsckAtticDictMustLoadToBeRecoverable(t *testing.T) {
	dir := buildDictStore(t, 2000)
	attic := filepath.Join(dir, "dicts", "attic")
	if err := os.MkdirAll(attic, 0o755); err != nil {
		t.Fatal(err)
	}

	// A genuine retirement: the dictionary moves intact. That IS recoverable.
	if rerr := os.Rename(dictPath(dir, 1), filepath.Join(attic, "1.zst")); rerr != nil {
		t.Fatal(rerr)
	}
	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + r.String())
	if len(r.DictsInAttic) != 1 || r.DictsInAttic[0] != 1 {
		t.Fatalf("an intact attic dictionary was not reported recoverable: %v", r.DictsInAttic)
	}
	if !strings.Contains(r.String(), "RECOVERABLE") {
		t.Error("report does not offer the attic restore")
	}

	// Now damage the attic copy. There is nothing to restore any more.
	if werr := os.WriteFile(filepath.Join(attic, "1.zst"), []byte("junk"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	r, err = Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + r.String())
	if len(r.DictsInAttic) != 0 {
		t.Errorf("an unloadable attic dictionary was offered as recoverable: %v", r.DictsInAttic)
	}
	if out := r.String(); strings.Contains(out, "RECOVERABLE") {
		t.Error("report still offers a restore that would not load")
	}
	if len(r.DictsDamaged) == 0 {
		t.Error("the damaged attic file was not reported")
	}
}

// TestFsckCleanStoreWithDictionary is the control for all of the above: none of these
// checks may fire on a healthy store that happens to use a trained dictionary.
func TestFsckCleanStoreWithDictionary(t *testing.T) {
	dir := buildDictStore(t, 2000)
	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + r.String())
	rec, runs, lost := r.Totals()
	segs, undec := r.Undecodable()
	if runs != 0 || lost != 0 {
		t.Errorf("clean store reported %d damaged runs / %d lost records", runs, lost)
	}
	if segs != 0 || undec != 0 {
		t.Errorf("clean store reported %d undecodable segments / %d records", segs, undec)
	}
	if len(r.DictsDamaged) != 0 {
		t.Errorf("clean store reported damaged dictionaries: %v", r.DictsDamaged)
	}
	if rec == 0 {
		t.Error("clean store reported no readable records")
	}
	if out := r.String(); strings.Contains(out, "UNDECODABLE") || strings.Contains(out, "DAMAGED dictionary") {
		t.Errorf("clean store report mentions dictionary damage:\n%s", out)
	}
}
