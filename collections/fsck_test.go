package collections

import (
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFsckCleanStore is the control: fsck must find nothing wrong with a healthy
// store. Without this, "fsck reports damage" tests could pass on noise.
func TestFsckCleanStore(t *testing.T) {
	s := buildCorruptStore(t, 400)
	r, err := Fsck(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, runs, lost := r.Totals()
	t.Log("\n" + r.String())
	if runs != 0 || lost != 0 {
		t.Errorf("clean store reported %d damaged runs / %d lost records", runs, lost)
	}
	if len(r.Strays) != 0 {
		t.Errorf("clean store reported strays: %v", r.Strays)
	}
	if rec < len(s.keys) {
		t.Errorf("fsck counted %d records, fewer than the %d keys written", rec, len(s.keys))
	}
}

// TestFsckSeesPastDamage is the point of the tool: Open's walk stops at the first bad
// record, so a mid-file bit flip in a segment with no sidecar silently costs every
// record after it. Fsck must report ONE damaged run and still count the rest.
func TestFsckSeesPastDamage(t *testing.T) {
	s := buildCorruptStore(t, 400)
	seg := s.segments()[0]
	if err := os.Remove(seg + ".idx"); err != nil { // force the walk path
		t.Fatal(err)
	}
	offs := recordOffsets(t, seg)
	total := len(offs)
	mid := offs[total/2]
	flipBit(t, seg, mid+recKeyOff+2)

	// What a normal open recovers, for comparison.
	found, _, _, err := s.survivors()
	if err != nil {
		t.Fatal(err)
	}

	r, ferr := Fsck(s.dir)
	if ferr != nil {
		t.Fatal(ferr)
	}
	t.Log("\n" + r.String())
	rec, runs, _ := r.Totals()

	if runs != 1 {
		t.Errorf("want exactly 1 damaged run, got %d", runs)
	}
	// The whole value of resync: fsck still sees essentially every record, while the
	// open path lost the tail of that segment.
	if rec <= len(found) {
		t.Errorf("fsck counted %d records but a plain open recovered %d keys; "+
			"resync should see MORE than the open path", rec, len(found))
	}
	t.Logf("open recovered %d keys; fsck can still read %d records (%d more)",
		len(found), rec, rec-len(found))
}

// TestFsckNamesStrayFiles: a renamed segment is invisible to recovery AND its
// dictionary becomes a pruning candidate, so silence here is how a recoverable
// rename becomes permanent loss. Fsck must name the file.
func TestFsckNamesStrayFiles(t *testing.T) {
	s := buildCorruptStore(t, 400)
	seg := s.segments()[0]
	renamed := filepath.Join(filepath.Dir(seg), "segment-1.d0.dat")
	if err := os.Rename(seg, renamed); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(seg + ".idx")

	r, err := Fsck(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + r.String())
	if len(r.Strays) != 1 {
		t.Fatalf("want 1 stray file reported, got %d: %v", len(r.Strays), r.Strays)
	}
	if !strings.Contains(r.Strays[0].Path, "segment-1.d0.dat") {
		t.Errorf("stray names the wrong file: %s", r.Strays[0].Path)
	}
	if !strings.Contains(r.Strays[0].Why, "pruned") {
		t.Errorf("stray reason should warn about dictionary pruning, got: %s", r.Strays[0].Why)
	}
}

// TestFsckFlagsDamageHiddenByASidecar: when a sidecar supplies the extent, Open does
// not walk the segment at all, so damage inside it is invisible until a query trips
// over it. Fsck must both find it and say that a normal open will not.
func TestFsckFlagsDamageHiddenByASidecar(t *testing.T) {
	s := buildCorruptStore(t, 400)
	seg := s.segments()[0]
	if _, err := os.Stat(seg + ".idx"); err != nil {
		t.Skipf("no sidecar to test with: %v", err)
	}
	offs := recordOffsets(t, seg)
	flipBit(t, seg, offs[len(offs)/2]+recKeyOff+2)

	r, err := Fsck(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + r.String())
	var hidden bool
	for _, sg := range r.Segments {
		if sg.ExtentTrusted && len(sg.Damage) > 0 {
			hidden = true
		}
	}
	if !hidden {
		t.Error("damage inside a trusted extent was not flagged as hidden from a normal open")
	}
	if !strings.Contains(r.String(), "will not notice") {
		t.Error("the report should say a normal open will not notice this damage")
	}
}

// TestFsckReportsMissingDictionary: a segment whose dictionary file is gone cannot be
// decoded at all. That is the one unrecoverable loss in the format, so it must be
// called out rather than surfacing as a decode error later.
func TestFsckReportsMissingDictionary(t *testing.T) {
	dir := t.TempDir()
	// A dictionary id in the name with no file behind it.
	shard := filepath.Join(dir, "0")
	if err := os.MkdirAll(shard, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shard, "seg-1.d7.dat"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + r.String())
	if len(r.DictsMissing) != 1 || r.DictsMissing[0] != 7 {
		t.Fatalf("want dictionary 7 reported missing, got %v", r.DictsMissing)
	}
}

// TestFsckDoesNotWrite pins the contract an operator depends on: running fsck on a
// damaged store must not change it. Anything else and the first diagnostic step is
// also the step that destroys evidence.
func TestFsckDoesNotWrite(t *testing.T) {
	s := buildCorruptStore(t, 400)
	seg := s.segments()[0]
	flipBit(t, seg, recordOffsets(t, seg)[5]+recKeyOff+2)

	before := snapshotTree(t, s.dir)
	if _, err := Fsck(s.dir); err != nil {
		t.Fatal(err)
	}
	after := snapshotTree(t, s.dir)

	if len(before) != len(after) {
		t.Fatalf("fsck changed the file set: %d files before, %d after", len(before), len(after))
	}
	for path, sum := range before {
		if after[path] != sum {
			t.Errorf("fsck modified %s", path)
		}
	}
}

// snapshotTree records every file's size and modification time, which is enough to
// catch a rewrite, a truncation, a creation or a deletion.
func snapshotTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(dir, p)
		out[rel] = string(crcOf(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func crcOf(b []byte) []byte {
	c := crc32.Checksum(b, crcTable)
	return []byte{byte(c), byte(c >> 8), byte(c >> 16), byte(c >> 24)}
}

// TestFsckCountsDamagedRecordsExactly: when a record's framing survives, fsck knows
// exactly how many records are bad, and must say so rather than estimating from the
// byte length. An operator deciding whether to restore from backup needs "2 records"
// to mean 2.
func TestFsckCountsDamagedRecordsExactly(t *testing.T) {
	for _, n := range []int{1, 2, 5} {
		s := buildCorruptStore(t, 400)
		seg := s.segments()[0]
		offs := recordOffsets(t, seg)
		for i := 0; i < n; i++ {
			flipBit(t, seg, offs[10+i]+recKeyOff+2) // consecutive records
		}
		r, err := Fsck(s.dir)
		if err != nil {
			t.Fatal(err)
		}
		_, runs, lost := r.Totals()
		if lost != n {
			t.Errorf("%d damaged records: fsck reported %d (runs=%d)\n%s", n, lost, runs, r.String())
		}
		if runs != 1 {
			t.Errorf("%d consecutive damaged records should be ONE run, got %d", n, runs)
		}
	}
}

// TestOpenReportsIgnoredFiles: recovery used to skip an unparseable segment name in
// total silence, which is how intact data goes missing without a trace. Open must at
// least say it saw the file.
func TestOpenReportsIgnoredFiles(t *testing.T) {
	s := buildCorruptStore(t, 400)
	seg := s.segments()[0]
	renamed := filepath.Join(filepath.Dir(seg), "segment-1.d0.dat")
	if err := os.Rename(seg, renamed); err != nil {
		t.Fatal(err)
	}

	var diag OpenIndexDiag
	prev := OpenIndexDiagHook
	OpenIndexDiagHook = func(d OpenIndexDiag) { diag = d }
	defer func() { OpenIndexDiagHook = prev }()

	c, err := Open(Options{Dir: s.dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if len(diag.IgnoredFiles) != 1 {
		t.Fatalf("Open reported %d ignored files, want 1: %v", len(diag.IgnoredFiles), diag.IgnoredFiles)
	}
	if !strings.Contains(diag.IgnoredFiles[0], "segment-1.d0.dat") {
		t.Errorf("wrong file reported: %s", diag.IgnoredFiles[0])
	}
	t.Logf("Open reported the mangled segment: %s", diag.IgnoredFiles[0])
}
