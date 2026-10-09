package collections

// A fault-injection harness for the on-disk format: build a real persistent
// collection, close it, damage specific bytes or files, reopen, and report exactly
// which keys survived.
//
// The point is to make corruption behavior OBSERVABLE rather than inferred. Every
// claim about recovery -- "a torn tail costs only the tail", "a wrong dictionary is
// rejected rather than silently decoded" -- is a statement about what this harness
// prints, not about what the format comment says.

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// corruptStore is a closed, on-disk collection plus the keys it should contain.
type corruptStore struct {
	t    *testing.T
	dir  string
	keys []string
}

// buildCorruptStore writes n records across enough segments that sealing happens,
// then closes the collection so every byte is on disk and reopenable.
func buildCorruptStore(t *testing.T, n int) *corruptStore {
	t.Helper()
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	keys := make([]string, 0, n)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("job%05d", i)
		ad := classad.New()
		ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%7))
		ad.InsertAttrString("Cmd", "/bin/sleep")
		ad.InsertAttr("RequestCpus", int64(1+i%4))
		ad.InsertAttr("QDate", int64(1790000000+i))
		if err := c.Put([]byte(key), ad); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		keys = append(keys, key)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return &corruptStore{t: t, dir: dir, keys: keys}
}

// segments lists the collection's segment data files, shard directory order then
// file order, as recovery would see them.
func (s *corruptStore) segments() []string {
	s.t.Helper()
	var out []string
	shards, err := os.ReadDir(s.dir)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, sh := range shards {
		if !sh.IsDir() || sh.Name() == "dicts" {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.dir, sh.Name()))
		if err != nil {
			s.t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".dat") {
				out = append(out, filepath.Join(s.dir, sh.Name(), f.Name()))
			}
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		s.t.Fatal("no segment files were written")
	}
	return out
}

// recordOffsets walks a segment file the way recovery does and returns the start
// offset of every record, so a test can damage record k rather than a blind offset.
func recordOffsets(t *testing.T, path string) []int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var offs []int
	for off := 0; off+recHeaderSize <= len(b); {
		total := int(binary.LittleEndian.Uint32(b[off+recTotalLenOff:]))
		if total <= 0 || off+total > len(b) {
			break
		}
		offs = append(offs, off)
		off += total
	}
	return offs
}

// flipBit flips one bit at an absolute file offset: the smallest damage that is
// still damage, so a test cannot pass by accident of magnitude.
func flipBit(t *testing.T, path string, off int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var one [1]byte
	if _, err := f.ReadAt(one[:], int64(off)); err != nil {
		t.Fatalf("read at %d of %s: %v", off, path, err)
	}
	one[0] ^= 0x01
	if _, err := f.WriteAt(one[:], int64(off)); err != nil {
		t.Fatal(err)
	}
}

// survivors reopens the collection and reports which of the expected keys are
// readable, plus any error from Open itself. A key counts as surviving only if its
// ad comes back and its QDate is the one written -- a key that reads back wrong is
// worse than one that is missing, and must never be counted as recovered.
func (s *corruptStore) survivors() (found []string, missing []string, wrong []string, openErr error) {
	c, err := Open(Options{Dir: s.dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		return nil, nil, nil, err
	}
	defer c.Close()
	for i, k := range s.keys {
		ad, ok := c.Get([]byte(k))
		if !ok {
			missing = append(missing, k)
			continue
		}
		want := int64(1790000000 + i)
		if got, ok := ad.EvaluateAttrInt("QDate"); !ok || got != want {
			wrong = append(wrong, fmt.Sprintf("%s(QDate=%d want %d)", k, got, want))
			continue
		}
		found = append(found, k)
	}
	return found, missing, wrong, nil
}

// report prints a one-line census; every corruption test ends with one so a failure
// (or a future behavior change) is readable from the log without a debugger.
func (s *corruptStore) report(label string) (found, missing, wrong int) {
	s.t.Helper()
	f, m, w, err := s.survivors()
	if err != nil {
		s.t.Logf("%-28s open FAILED: %v", label, err)
		return 0, len(s.keys), 0
	}
	s.t.Logf("%-28s of %d keys: %d intact, %d missing, %d WRONG", label, len(s.keys), len(f), len(m), len(w))
	if len(w) > 0 {
		s.t.Errorf("%s: %d keys read back with wrong contents: %v", label, len(w), w[:min(len(w), 5)])
	}
	return len(f), len(m), len(w)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestHarnessBaselineIsClean is the control. Without it, every other test in this
// file could pass by measuring a store that was already broken.
func TestHarnessBaselineIsClean(t *testing.T) {
	s := buildCorruptStore(t, 400)
	segs := s.segments()
	t.Logf("baseline: %d segment file(s)", len(segs))
	for _, p := range segs {
		t.Logf("  %s (%d records)", filepath.Base(p), len(recordOffsets(t, p)))
	}
	found, missing, wrong := s.report("baseline (undamaged)")
	if found != len(s.keys) || missing != 0 || wrong != 0 {
		t.Fatalf("an undamaged store did not read back whole: %d/%d", found, len(s.keys))
	}
}
