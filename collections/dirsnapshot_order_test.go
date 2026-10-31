package collections

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// readDirSnapEntries returns a dir.snap's entry array: the offset where it starts, and
// the decoded entries in the order the file holds them.
func readDirSnapEntries(t *testing.T, path string) (entryStart int, body []byte, ents []dirSnapEnt) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body = data[:len(data)-4]
	r := snapReader{b: body}
	if got := r.u32(); got != dirSnapMagic {
		t.Fatalf("magic %#x", got)
	}
	version := r.u32()
	r.u64() // commit sequence
	r.u64() // live count
	nSegs := int(r.u32())
	for j := 0; j < nSegs; j++ {
		r.u64()   // durable length
		r.str16() // basename
		if version >= 2 {
			r.u64() // minSeq
			r.u64() // maxSeq
			r.u64() // maxSup
			r.u64() // dead
		}
	}
	n := int(r.u64())
	entryStart = r.pos
	for i := 0; i < n; i++ {
		h := r.u64()
		slot := r.u32()
		off := r.u32()
		ents = append(ents, dirSnapEnt{hash: h, slot: slot, seg: slot, off: off})
	}
	if r.err {
		t.Fatal("short dir.snap")
	}
	return entryStart, body, ents
}

// fillAndClose writes n keys into a fresh collection at dir and closes it cleanly, so
// each shard leaves a directory snapshot behind.
func fillAndClose(t *testing.T, dir string, n int) {
	t.Helper()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ Owner="u%d"; Seq=%d ]`, i%5, i))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%04d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestDirSnapshotEntriesInRecordOrder pins that a snapshot's entries are ordered by the
// record they name, not by directory map order.
//
// Validating an entry reads the record it points at, so map order means one random page
// touch per live key on both the Close that writes the file and the Open that reads it
// back -- which on a store past the size of the page cache is the dominant cost of the
// fast path, and can be worse than the full walk it replaces. Nothing about the restored
// directory changes with the order, so no other test here would notice it regressing.
func TestDirSnapshotEntriesInRecordOrder(t *testing.T) {
	dir := t.TempDir()
	fillAndClose(t, dir, 600)

	_, _, ents := readDirSnapEntries(t, filepath.Join(dir, "0", dirSnapName))
	if len(ents) < 100 {
		t.Fatalf("expected a populated snapshot, got %d entries", len(ents))
	}
	segs := map[uint32]bool{}
	for i, e := range ents {
		segs[e.slot] = true
		if i == 0 {
			continue
		}
		p := ents[i-1]
		if e.slot < p.slot || (e.slot == p.slot && e.off < p.off) {
			t.Fatalf("entry %d (slot %d, off %d) precedes (slot %d, off %d): entries are not in record order",
				i, e.slot, e.off, p.slot, p.off)
		}
	}
	if len(segs) < 2 {
		t.Fatalf("want entries spanning several segments to make the ordering meaningful, got %d", len(segs))
	}
}

// TestDirSnapshotLoadsUnorderedEntries proves the reader does not DEPEND on that order:
// a snapshot written by an older build is still restored correctly. The reader sorts for
// the same locality reason, and sorting before building the directory -- rather than
// after -- is the part that could silently drop entries.
func TestDirSnapshotLoadsUnorderedEntries(t *testing.T) {
	dir := t.TempDir()
	fillAndClose(t, dir, 600)

	path := filepath.Join(dir, "0", dirSnapName)
	entryStart, body, ents := readDirSnapEntries(t, path)

	// Rewrite the file with its entries reversed -- the worst case for a reader that
	// assumed sorted input, and a shape an older writer could legitimately produce.
	buf := append([]byte{}, body[:entryStart]...)
	for i := len(ents) - 1; i >= 0; i-- {
		buf = binary.LittleEndian.AppendUint64(buf, ents[i].hash)
		buf = binary.LittleEndian.AppendUint32(buf, ents[i].slot)
		buf = binary.LittleEndian.AppendUint32(buf, ents[i].off)
	}
	buf = binary.LittleEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}

	var loaded, scanned int
	onDirSnapLoad = func(ok bool) {
		if ok {
			loaded++
		} else {
			scanned++
		}
	}
	defer func() { onDirSnapLoad = nil }()

	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if loaded != 1 || scanned != 0 {
		t.Fatalf("reversed snapshot should still take the fast path: loaded=%d scanned=%d", loaded, scanned)
	}
	for i := 0; i < 600; i++ {
		ad, ok := c.Get([]byte(fmt.Sprintf("k%04d", i)))
		if !ok {
			t.Fatalf("key k%04d missing after restoring a reversed snapshot", i)
		}
		v, _ := ad.EvaluateAttrInt("Seq")
		if v != int64(i) {
			t.Fatalf("k%04d: Seq=%d, want %d", i, v, i)
		}
	}
}
