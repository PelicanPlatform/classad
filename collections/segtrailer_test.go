package collections

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// sealedSegmentFact is one sealed segment of a store: where it is and how much of it is
// written. A trailer may only go past the written extent, so both are needed.
type sealedSegmentFact struct {
	path string
	used int
}

// aSealedSegment opens dir, picks a sealed (non-active) segment, and closes again.
func aSealedSegment(t *testing.T, dir string) sealedSegmentFact {
	t.Helper()
	return aSealedSegmentOf(t, dir, 1<<14)
}

// standInDict is a body the size of nothing in particular, for the tests that are about
// the trailer rather than about dictionaries. A real trained dictionary is ~112 KB and
// does not fit the shared fixture's 16 KiB segments, which is the ratio rule working.
func standInDict(id byte) []byte {
	return bytes.Repeat([]byte{id, 0xAB, 0xCD, 0xEF}, 128)
}

// buildCarrierStore is a store whose segments are large enough to carry a real trained
// dictionary: ~112 KB against replicaSizeRatio needs close to a megabyte of arena, and a
// sealed segment of that size needs enough records to fill one.
func buildCarrierStore(t *testing.T) string {
	t.Helper()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	sample := loadCorpus(t)
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	put := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			if perr := c.Put([]byte(fmt.Sprintf("job%07d", i)), sample[i%len(sample)]); perr != nil {
				t.Fatal(perr)
			}
		}
	}
	put(0, 20000)
	if _, err := c.RetrainDict(1500); err != nil {
		t.Skipf("dictionary training unavailable: %v", err)
	}
	put(20000, 60000)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func aSealedSegmentOf(t *testing.T, dir string, segSize int) sealedSegmentFact {
	t.Helper()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: segSize})
	if err != nil {
		t.Fatal(err)
	}
	var got sealedSegmentFact
	sh := c.shards[0]
	sh.mu.Lock()
	for _, seg := range sh.segs {
		if seg == nil || seg == sh.act || seg.path == "" || seg.used == 0 {
			continue
		}
		got = sealedSegmentFact{path: seg.path, used: seg.used}
		break
	}
	sh.mu.Unlock()
	if cerr := c.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if got.path == "" {
		t.Skip("fixture produced no sealed segment")
	}
	return got
}

// TestSegTrailerRoundTrip: sections come back in the order they went in, repeated ids and
// all -- a carrier holding two dictionary replicas has two sections with the same id --
// and an empty body is dropped rather than costing a directory entry.
func TestSegTrailerRoundTrip(t *testing.T) {
	in := []segTrailerSection{
		{id: 1, body: []byte("first")},
		{id: 7, body: []byte("unknown to this build")},
		{id: 1, body: []byte("second, same id")},
		{id: 3, body: nil},
	}
	b := buildSegTrailer(in)
	got, ok := parseSegTrailer(b)
	if !ok {
		t.Fatal("a trailer this build wrote does not parse")
	}
	if got.bytes != len(b) {
		t.Errorf("bytes = %d; want %d", got.bytes, len(b))
	}
	if len(got.sections) != 3 {
		t.Fatalf("parsed %d sections; want 3 (the empty one dropped)", len(got.sections))
	}
	for i, want := range []segTrailerSection{in[0], in[1], in[2]} {
		if got.sections[i].id != want.id || !bytes.Equal(got.sections[i].body, want.body) {
			t.Errorf("section %d = (%d, %q); want (%d, %q)",
				i, got.sections[i].id, got.sections[i].body, want.id, want.body)
		}
	}
	// An empty trailer is still a trailer: it says "this file has a tail and the arena
	// ends before it", which is the fact the mapping depends on.
	if e, ok := parseSegTrailer(buildSegTrailer(nil)); !ok || len(e.sections) != 0 {
		t.Errorf("empty trailer: ok=%v sections=%d", ok, len(e.sections))
	}
}

// TestSegTrailerRejectsDamage: the trailer lives in a region that was block-reserved and
// zero-filled and is rewritten in place, so a torn write can leave bytes that end in the
// magic. Anything that does not verify must read as NO trailer, which maps the segment
// whole -- exactly what every segment written before trailers existed does.
func TestSegTrailerRejectsDamage(t *testing.T) {
	good := buildSegTrailer([]segTrailerSection{
		{id: segSecDictReplica, body: encodeDictReplica(3, bytes.Repeat([]byte("dict"), 64))},
		{id: 9, body: []byte("another section")},
	})
	if _, ok := parseSegTrailer(good); !ok {
		t.Fatal("control: the undamaged trailer does not parse")
	}

	// One flipped bit anywhere -- body, directory or foot -- must be caught.
	for _, off := range []int{0, 7, len(good) / 2, len(good) - 24, len(good) - 20, len(good) - 9} {
		damaged := append([]byte(nil), good...)
		damaged[off] ^= 0x01
		if _, ok := parseSegTrailer(damaged); ok {
			t.Errorf("a flipped bit at offset %d was not caught", off)
		}
	}
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"shorter than the foot", good[:segTrailerFootLen-1]},
		{"truncated head", good[1:]},
		{"truncated tail", good[:len(good)-1]},
		{"all zero", make([]byte, len(good))},
	} {
		if _, ok := parseSegTrailer(tc.b); ok {
			t.Errorf("%s: parsed as a valid trailer", tc.name)
		}
	}

	// A foot claiming more sections than the body can hold must be refused before
	// anything is sliced on its word.
	absurd := append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(absurd[len(absurd)-12:], segTrailerMaxSections+1)
	if _, ok := parseSegTrailer(absurd); ok {
		t.Error("a foot claiming too many sections was accepted")
	}
}

// TestCarrierSegmentStillServesEveryRecord is the first thing a trailer must not break:
// the segment it rides on reads exactly as it did.
func TestCarrierSegmentStillServesEveryRecord(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	before := func() int {
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.Len()
	}()
	seg := aSealedSegment(t, dir)

	if err := addDictReplica(seg.path, seg.used, 1, standInDict(1)); err != nil {
		t.Fatalf("addDictReplica: %v", err)
	}

	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("reopen a store with a carrier segment: %v", err)
	}
	defer c.Close()
	if got := c.Len(); got != before {
		t.Errorf("record count changed with the trailer: %d -> %d", before, got)
	}

	arenaEnd, err := segArenaEnd(seg.path)
	if err != nil {
		t.Fatal(err)
	}
	var carried []uint32
	mapped := -1
	sh := c.shards[0]
	sh.mu.Lock()
	for _, s := range sh.segs {
		if s != nil && s.path == seg.path {
			carried = s.carriedDicts
			mapped = len(s.data)
		}
	}
	sh.mu.Unlock()
	if len(carried) != 1 || carried[0] != 1 {
		t.Errorf("carriedDicts = %v; want [1]", carried)
	}
	// The mapping must cover the ARENA, not the file. A mapping that ran over the trailer
	// would hand the segment capacity it does not have: a resumed active segment would
	// append into the tail, and the byte accounting would count the trailer as arena.
	if int64(mapped) != arenaEnd {
		t.Errorf("mapped %d bytes of a %d byte arena", mapped, arenaEnd)
	}
}

// TestCarrierTerminatesAWalkThatIgnoresTheTrailer is the compatibility claim, checked
// rather than asserted. A build that knows nothing about trailers maps the WHOLE file and
// walks records until it reads a zero totalLen. That walk must still stop inside the
// arena, never reaching the tail.
func TestCarrierTerminatesAWalkThatIgnoresTheTrailer(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	seg := aSealedSegment(t, dir)
	if err := addDictReplica(seg.path, seg.used, 1, standInDict(1)); err != nil {
		t.Fatal(err)
	}
	arenaEnd, err := segArenaEnd(seg.path)
	if err != nil {
		t.Fatal(err)
	}

	whole, err := os.ReadFile(seg.path) // as an older build maps it
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(whole)) <= arenaEnd {
		t.Fatal("the trailer did not extend the file")
	}
	off := 0
	for off+recTotalLenOff+4 <= len(whole) {
		total := int(recTotalLen(whole, uint32(off)))
		if total == 0 {
			break
		}
		off += total
	}
	if int64(off) >= arenaEnd {
		t.Fatalf("a trailer-unaware walk ran to %d, at or past the arena end %d", off, arenaEnd)
	}
}

// TestDictReplicaIsReadableAndDecodesItsSegment is the end of the chain: a dictionary
// recovered from a segment's trailer is the dictionary, and it reads the records of the
// segment that carried it. Storing it RAW is what makes that possible -- a dictionary
// compressed under itself is unreadable, which is the shape of the downstream failure when
// dicts/ goes missing.
func TestDictReplicaIsReadableAndDecodesItsSegment(t *testing.T) {
	dir := buildCarrierStore(t)
	seg := aSealedSegmentOf(t, dir, 1<<20)
	path := filepath.Join(dir, "dicts", "1.zst")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if aerr := addDictReplica(seg.path, seg.used, 1, want); aerr != nil {
		t.Fatal(aerr)
	}

	tr, ok := readSegTrailerFile(seg.path)
	if !ok {
		t.Fatal("the segment reports no trailer")
	}
	got, ok := tr.dictReplica(1)
	if !ok {
		t.Fatal("the trailer carries no copy of dictionary 1")
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the copy is not the dictionary")
	}
	if ids := tr.dictReplicaIDs(); len(ids) != 1 || ids[0] != 1 {
		t.Errorf("dictReplicaIDs = %v; want [1]", ids)
	}

	// The proof that matters: the store opens on the copy alone.
	if rmErr := os.Remove(path); rmErr != nil {
		t.Fatal(rmErr)
	}
	if _, oerr := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20}); oerr == nil {
		t.Fatal("control: the store opened without dictionary 1")
	}
	if werr := os.WriteFile(path, got, 0o644); werr != nil {
		t.Fatal(werr)
	}
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatalf("restoring the dictionary from the trailer did not make the store openable: %v", err)
	}
	defer c.Close()
	if n := c.Len(); n == 0 {
		t.Error("no records came back")
	}
}

// TestAddDictReplicaKeepsWhatItDidNotWrite: a second replica joins the first rather than
// replacing it, the same dictionary twice leaves one copy, and a section id this build
// does not know survives the rewrite byte for byte.
func TestAddDictReplicaKeepsWhatItDidNotWrite(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	seg := aSealedSegment(t, dir)
	stranger := []byte("written by a build that knows something this one does not")
	arenaEnd, err := segArenaEnd(seg.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSegTrailer(seg.path, arenaEnd, []segTrailerSection{{id: 4242, body: stranger}}); err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct {
		id   uint32
		dict []byte
	}{{1, standInDict(1)}, {2, standInDict(2)}, {1, standInDict(1)}} {
		if err := addDictReplica(seg.path, seg.used, step.id, step.dict); err != nil {
			t.Fatalf("addDictReplica(%d): %v", step.id, err)
		}
	}

	tr, ok := readSegTrailerFile(seg.path)
	if !ok {
		t.Fatal("no trailer after three writes")
	}
	if ids := tr.dictReplicaIDs(); len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Errorf("dictReplicaIDs = %v; want [1 2]", ids)
	}
	replicas := 0
	kept := false
	for _, s := range tr.sections {
		switch s.id {
		case segSecDictReplica:
			replicas++
		case 4242:
			kept = bytes.Equal(s.body, stranger)
		}
	}
	if replicas != 2 {
		t.Errorf("%d replica sections; want 2 (the repeat replaced, not appended)", replicas)
	}
	if !kept {
		t.Error("a section this build does not understand did not survive the rewrite")
	}
}

// TestAddDictReplicaRefusesWhereItWouldNotFit covers both bounds: a copy that would dwarf
// the segment carrying it, and an arena written so close to its end that a
// trailer-unaware walk would read into the tail.
func TestAddDictReplicaRefusesWhereItWouldNotFit(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	seg := aSealedSegment(t, dir)
	arenaEnd, err := segArenaEnd(seg.path)
	if err != nil {
		t.Fatal(err)
	}

	huge := bytes.Repeat([]byte("x"), int(arenaEnd))
	if err := addDictReplica(seg.path, seg.used, 1, huge); !errors.Is(err, ErrNoRoomForReplica) {
		t.Errorf("an oversized replica gave %v; want ErrNoRoomForReplica", err)
	}
	small := bytes.Repeat([]byte("x"), 64)
	if err := addDictReplica(seg.path, int(arenaEnd)-1, 1, small); !errors.Is(err, ErrNoRoomForReplica) {
		t.Errorf("a full arena gave %v; want ErrNoRoomForReplica", err)
	}
	if _, ok := readSegTrailerFile(seg.path); ok {
		t.Error("a refused write left a trailer behind")
	}
	for _, bad := range []struct {
		name string
		id   uint32
		dict []byte
	}{{"dictionary 0", 0, small}, {"no bytes", 1, nil}} {
		if err := addDictReplica(seg.path, seg.used, bad.id, bad.dict); err == nil {
			t.Errorf("%s: accepted", bad.name)
		}
	}
	// And the control, so the refusals above are not passing for the wrong reason.
	if err := addDictReplica(seg.path, seg.used, 1, small); err != nil {
		t.Errorf("a replica that fits was refused: %v", err)
	}
}

// TestSegArenaEndIsStableAcrossRewrites: the arena end is a property of the segment, not
// of how many times its trailer has been rewritten. If it drifted, each rewrite would eat
// into the records.
func TestSegArenaEndIsStableAcrossRewrites(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	seg := aSealedSegment(t, dir)
	want, err := segArenaEnd(seg.path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		body := bytes.Repeat([]byte("d"), 64*(i+1))
		if aerr := addDictReplica(seg.path, seg.used, uint32(i+1), body); aerr != nil {
			t.Fatal(aerr)
		}
		got, gerr := segArenaEnd(seg.path)
		if gerr != nil {
			t.Fatal(gerr)
		}
		if got != want {
			t.Fatalf("rewrite %d moved the arena end: %d -> %d", i, want, got)
		}
	}
	// And shrinking: a rewrite that leaves the old, longer tail behind would leave the
	// file ending in bytes no directory accounts for.
	if werr := writeSegTrailer(seg.path, want, []segTrailerSection{{id: 1, body: []byte("tiny")}}); werr != nil {
		t.Fatal(werr)
	}
	got, gerr := segArenaEnd(seg.path)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got != want {
		t.Fatalf("shrinking the trailer moved the arena end: %d -> %d", want, got)
	}
	st, err := os.Stat(seg.path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != want+int64(len(buildSegTrailer([]segTrailerSection{{id: 1, body: []byte("tiny")}}))) {
		t.Errorf("the file was not shrunk to the new trailer: size=%d arena=%d", st.Size(), want)
	}
}

// TestSegTrailerToleratesUnaddressedBodyBytes: the directory need not account for every
// byte between the start of the trailer and the directory itself. The CRC has already
// vouched for those bytes, so they are not damage -- they are a writer doing something
// this build does not, such as padding a section for alignment. A section table exists so
// that a newer trailer stays readable; refusing here would defeat it.
//
// A directory pointing PAST the bodies is a different matter and must still be refused.
func TestSegTrailerToleratesUnaddressedBodyBytes(t *testing.T) {
	// Hand-build a trailer whose single section covers only part of the body region, with
	// a correct CRC -- which is the only way to reach this, since any bit flip is caught
	// by the checksum first.
	build := func(bodies []byte, id uint16, declared uint32) []byte {
		out := append([]byte(nil), bodies...)
		out = binary.LittleEndian.AppendUint16(out, id)
		out = binary.LittleEndian.AppendUint16(out, 0)
		out = binary.LittleEndian.AppendUint32(out, declared)
		total := len(bodies) + segTrailerDirEntryLen + segTrailerFootLen
		out = binary.LittleEndian.AppendUint64(out, uint64(total))
		out = binary.LittleEndian.AppendUint32(out, 1)
		out = binary.LittleEndian.AppendUint32(out, crc32.Checksum(out, crcTable))
		return binary.LittleEndian.AppendUint32(out, segTrailerMagic)
	}

	bodies := []byte("SECTIONpadding-a-newer-build-added")
	got, ok := parseSegTrailer(build(bodies, 5, 7))
	if !ok {
		t.Fatal("a trailer with unaddressed body bytes was refused")
	}
	if len(got.sections) != 1 || got.sections[0].id != 5 ||
		!bytes.Equal(got.sections[0].body, []byte("SECTION")) {
		t.Errorf("sections = %+v; want one section 5 holding %q", got.sections, "SECTION")
	}

	if _, ok := parseSegTrailer(build(bodies, 5, uint32(len(bodies)+1))); ok {
		t.Error("a directory reaching past the bodies was accepted")
	}
}
