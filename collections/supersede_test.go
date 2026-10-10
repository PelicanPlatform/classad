package collections

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// patchSupersede rewrites the supersededBySeq of the first record whose key matches,
// and reports the record's own sequence. It edits the file directly, which is the
// point: this field is not covered by the record checksum, so nothing notices.
func patchSupersede(t *testing.T, path, key string, to uint64) (seq uint64, found bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off+recHeaderSize <= len(b); {
		total := int(binary.LittleEndian.Uint32(b[off+recTotalLenOff:]))
		if total <= 0 || off+total > len(b) {
			break
		}
		kl := int(binary.LittleEndian.Uint32(b[off+recKeyLenOff:]) & keyLenMask)
		if string(b[off+recKeyOff:off+recKeyOff+kl]) == key {
			seq = binary.LittleEndian.Uint64(b[off+recSeqOff:])
			binary.LittleEndian.PutUint64(b[off+recSupOff:], to)
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
			return seq, true
		}
		off += total
	}
	return 0, false
}

// findKeySegment returns the segment file holding key.
func findKeySegment(t *testing.T, s *corruptStore, key string) string {
	t.Helper()
	for _, p := range s.segments() {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for off := 0; off+recHeaderSize <= len(b); {
			total := int(binary.LittleEndian.Uint32(b[off+recTotalLenOff:]))
			if total <= 0 || off+total > len(b) {
				break
			}
			kl := int(binary.LittleEndian.Uint32(b[off+recKeyLenOff:]) & keyLenMask)
			if string(b[off+recKeyOff:off+recKeyOff+kl]) == key {
				return p
			}
			off += total
		}
	}
	t.Fatalf("key %q not found in any segment", key)
	return ""
}

// TestFsckFindsImpossibleSupersede: a live record whose supersededBySeq is corrupted
// simply stops being returned -- the key goes missing with nothing saying why, and no
// other check can see it because the checksum does not cover that field. Fsck must
// name it.
func TestFsckFindsImpossibleSupersede(t *testing.T) {
	s := buildCorruptStore(t, 400)
	const key = "job00100"
	seg := findKeySegment(t, s, key)

	// seqMax is all ones; clearing the top bit is a single flip and lands far above
	// any real commit sequence.
	seq, ok := patchSupersede(t, seg, key, seqMax>>1)
	if !ok {
		t.Fatalf("could not find %s to patch", key)
	}
	t.Logf("%s: seq=%d, supersededBySeq set to %d", key, seq, seqMax>>1)

	// The damage is real: the key stops coming back.
	found, missing, _, err := s.survivors()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after the patch: %d found, %d missing", len(found), len(missing))
	if len(missing) != 1 || missing[0] != key {
		t.Fatalf("expected exactly %s to go missing, got %v", key, missing)
	}

	r, err := Fsck(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + r.String())
	if len(r.Supersede) != 1 {
		t.Fatalf("want 1 impossible supersede reported, got %d", len(r.Supersede))
	}
	f := r.Supersede[0]
	if f.Key != key {
		t.Errorf("reported key %q, want %q", f.Key, key)
	}
	if !strings.Contains(f.Why, "never happened") {
		t.Errorf("reason should say the commit never happened, got %q", f.Why)
	}
	// The record walk is otherwise clean: this is NOT reported as byte damage.
	if _, runs, _ := r.Totals(); runs != 0 {
		t.Errorf("a supersede fault should not be reported as %d damaged run(s)", runs)
	}
	if !strings.Contains(r.String(), "JUDGEMENT CALL") {
		t.Error("the report should say fsck will not repair this automatically")
	}
}

// TestFsckFindsSupersededBeforeWritten covers the other impossible shape, which needs
// no knowledge of the rest of the store.
func TestFsckFindsSupersededBeforeWritten(t *testing.T) {
	s := buildCorruptStore(t, 400)
	const key = "job00150"
	seg := findKeySegment(t, s, key)
	seq, ok := patchSupersede(t, seg, key, 1) // superseded by commit 1, long before it existed
	if !ok {
		t.Fatalf("could not find %s", key)
	}
	t.Logf("%s: seq=%d, supersededBySeq set to 1", key, seq)

	r, err := Fsck(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Supersede) != 1 {
		t.Fatalf("want 1 finding, got %d\n%s", len(r.Supersede), r.String())
	}
	if !strings.Contains(r.Supersede[0].Why, "at or before") {
		t.Errorf("wrong reason: %q", r.Supersede[0].Why)
	}
}

// TestFsckAcceptsLegitimateSupersession is the control, and the one that matters most:
// ordinary superseded records -- every key written twice, and every delete -- must NOT
// be reported. A check that cries wolf on normal history is worse than no check.
func TestFsckAcceptsLegitimateSupersession(t *testing.T) {
	s := buildCorruptStore(t, 400)

	// Rewrite and delete a good number of keys, producing real superseded records.
	c, err := Open(Options{Dir: s.dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		ad, ok := c.Get([]byte(s.keys[i]))
		if !ok {
			t.Fatalf("%s missing before rewrite", s.keys[i])
		}
		if err := c.Put([]byte(s.keys[i]), ad); err != nil {
			t.Fatal(err)
		}
	}
	for i := 100; i < 150; i++ {
		if !c.Delete([]byte(s.keys[i])) {
			t.Fatalf("delete of %s did nothing", s.keys[i])
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := Fsck(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Supersede) != 0 {
		t.Errorf("100 rewrites and 50 deletes produced %d false finding(s):\n%s",
			len(r.Supersede), r.String())
	}
	t.Logf("100 rewrites + 50 deletes: 0 false findings, highest commit seen %d", r.MaxSeq)
}

// TestFsckSkipsTheBoundCheckAfterACrash: without a directory snapshot there is no
// true commit sequence, and the highest record sequence is only a floor -- a delete
// advances the commit sequence without writing a record. Reporting healthy deletes as
// corruption would make the whole check untrustworthy, so it is skipped and said to be.
func TestFsckSkipsTheBoundCheckAfterACrash(t *testing.T) {
	s := buildCorruptStore(t, 400)

	c, err := Open(Options{Dir: s.dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	for i := 300; i < 350; i++ { // trailing deletes: tombstone seqs above every record's
		if !c.Delete([]byte(s.keys[i])) {
			t.Fatalf("delete of %s did nothing", s.keys[i])
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash: the snapshot a clean Close wrote is not there.
	for _, seg := range s.segments() {
		_ = os.Remove(filepath.Dir(seg) + "/" + dirSnapName)
	}

	r, err := Fsck(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.CommitSeqKnown {
		t.Fatal("no snapshot remains, so no commit sequence should be claimed")
	}
	if len(r.Supersede) != 0 {
		t.Errorf("with no commit sequence available, %d healthy delete(s) were reported:\n%s",
			len(r.Supersede), r.String())
	}

	// The invariant that needs no global state still applies.
	const key = "job00200"
	if _, ok := patchSupersede(t, findKeySegment(t, s, key), key, 1); !ok {
		t.Fatalf("could not patch %s", key)
	}
	r2, err := Fsck(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Supersede) != 1 {
		t.Fatalf("the state-free check should still fire; got %d finding(s)", len(r2.Supersede))
	}
	t.Logf("no snapshot: bound check skipped, state-free check still caught %s", r2.Supersede[0].Key)
}
