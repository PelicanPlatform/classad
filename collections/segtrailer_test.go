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
	return buildCarrierStoreShards(t, 1)
}

func buildCarrierStoreShards(t *testing.T, shards int) string {
	t.Helper()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	sample := loadCorpus(t)
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: shards, SegmentSize: 1 << 20})
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
	put(0, 900*shards)
	if _, err := c.RetrainDict(1500); err != nil {
		t.Skipf("dictionary training unavailable: %v", err)
	}
	put(900*shards, 3000*shards)
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

// TestSegTrailerRejectsDirectoryDamage: the directory is the index. Nothing in the
// trailer can be located without it, so damage there loses the trailer whole -- and that
// has to read as NO trailer, which maps the segment exactly as every segment written
// before trailers existed.
//
// The foot's checksum is also what keeps a zero-filled region that happens to end in the
// magic from being taken for one.
func TestSegTrailerRejectsDirectoryDamage(t *testing.T) {
	bodyLen := 256
	good := buildSegTrailer([]segTrailerSection{
		{id: segSecDictReplica, body: encodeDictReplica(3, bytes.Repeat([]byte("dict"), 63))},
		{id: 9, body: []byte("another section")},
	})
	if _, ok := parseSegTrailer(good); !ok {
		t.Fatal("control: the undamaged trailer does not parse")
	}
	dirAt := len(good) - segTrailerFootLen - 2*segTrailerDirEntryLen
	if dirAt != bodyLen+len("another section") {
		t.Fatalf("directory starts at %d; the test's arithmetic is wrong", dirAt)
	}

	// One flipped bit anywhere in the directory or the foot.
	for _, off := range []int{dirAt, dirAt + 5, dirAt + segTrailerDirEntryLen,
		len(good) - segTrailerFootLen, len(good) - 9, len(good) - 1} {
		damaged := append([]byte(nil), good...)
		damaged[off] ^= 0x01
		if _, ok := parseSegTrailer(damaged); ok {
			t.Errorf("a flipped bit at offset %d (directory/foot) was not caught", off)
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

// TestSegTrailerLosesOnlyTheDamagedSection is why each section carries its own checksum.
// A trailer holding three dictionary replicas must not lose all three because one segment
// got a bad block: the directory says where each body is and what it should hash to, so a
// damaged body costs exactly itself and is NAMED, which is a more useful fact than the
// section simply being absent.
func TestSegTrailerLosesOnlyTheDamagedSection(t *testing.T) {
	first := encodeDictReplica(1, bytes.Repeat([]byte("one"), 40))
	middle := encodeDictReplica(2, bytes.Repeat([]byte("two"), 40))
	last := []byte("a section this build does not know")
	good := buildSegTrailer([]segTrailerSection{
		{id: segSecDictReplica, body: first},
		{id: segSecDictReplica, body: middle},
		{id: 4242, body: last},
	})

	// Damage the middle body, which is the hardest case: the sections around it are
	// located by lengths that sit in the (undamaged) directory, not by scanning.
	damaged := append([]byte(nil), good...)
	damaged[len(first)+7] ^= 0x01

	tr, ok := parseSegTrailer(damaged)
	if !ok {
		t.Fatal("a trailer with one damaged body was thrown away whole")
	}
	if len(tr.damaged) != 1 || tr.damaged[0] != segSecDictReplica {
		t.Errorf("damaged = %v; want one section %d", tr.damaged, segSecDictReplica)
	}
	if len(tr.sections) != 2 {
		t.Fatalf("%d sections survived; want 2", len(tr.sections))
	}
	if !bytes.Equal(tr.sections[0].body, first) {
		t.Error("the section before the damage did not survive intact")
	}
	if tr.sections[1].id != 4242 || !bytes.Equal(tr.sections[1].body, last) {
		t.Error("the section after the damage did not survive intact")
	}
	// And the point of it: the replica that was not damaged is still usable.
	if got, has := tr.dictReplica(1); !has || !bytes.Equal(got, first[4:]) {
		t.Error("the undamaged dictionary replica is no longer readable")
	}
	if _, has := tr.dictReplica(2); has {
		t.Error("the damaged replica was handed back as if it were good")
	}
	if ids := tr.dictReplicaIDs(); len(ids) != 1 || ids[0] != 1 {
		t.Errorf("dictReplicaIDs = %v; want [1]", ids)
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

	// The proof that matters: with the file gone the store still opens, because Open
	// restores the copy itself (see dictrecover.go), and the file it writes back is the
	// dictionary byte for byte.
	if rmErr := os.Remove(path); rmErr != nil {
		t.Fatal(rmErr)
	}
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatalf("the store did not open on the copy carried in its own segment: %v", err)
	}
	defer c.Close()
	if n := c.Len(); n == 0 {
		t.Error("no records came back")
	}
	back, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the dictionary was not written back: %v", err)
	}
	if !bytes.Equal(back, want) {
		t.Error("what was written back is not the dictionary that was lost")
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
		dirAt := len(out)
		out = binary.LittleEndian.AppendUint16(out, id)
		out = binary.LittleEndian.AppendUint16(out, 0)
		out = binary.LittleEndian.AppendUint32(out, declared)
		var sum uint32
		if int(declared) <= len(bodies) {
			sum = crc32.Checksum(bodies[:declared], crcTable)
		}
		out = binary.LittleEndian.AppendUint32(out, sum)
		total := len(bodies) + segTrailerDirEntryLen + segTrailerFootLen
		out = binary.LittleEndian.AppendUint64(out, uint64(total))
		out = binary.LittleEndian.AppendUint32(out, 1)
		out = binary.LittleEndian.AppendUint32(out, crc32.Checksum(out[dirAt:], crcTable))
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

// TestSegTrailerRefusesNonsense covers the guards rather than the happy path. Every one
// of these is a caller handing over something that is not a trailer, or a file that is
// not there, and each must come back as "no trailer" rather than as a slice out of range
// or a half-built result -- a segment with no trailer is the overwhelmingly common case
// and shares every one of these paths with a damaged one.
func TestSegTrailerRefusesNonsense(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "not-a-segment.dat")

	// parseSegTrailer wants EXACTLY the trailer region. A longer slice is a caller
	// mistake, not a trailer that happens to start late.
	good := buildSegTrailer([]segTrailerSection{{id: 1, body: []byte("body")}})
	if _, ok := parseSegTrailer(append([]byte("leading junk"), good...)); ok {
		t.Error("parsed a slice longer than the trailer it describes")
	}

	// The file-level readers answer "no trailer" for anything they cannot read.
	if _, ok := readSegTrailerFile(absent); ok {
		t.Error("read a trailer out of a file that does not exist")
	}
	short := filepath.Join(t.TempDir(), "short.dat")
	if err := os.WriteFile(short, []byte("tiny"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSegTrailerFile(short); ok {
		t.Error("read a trailer out of a file smaller than one")
	}
	// A foot whose claimed length is longer than the file is refused before the read it
	// would otherwise size.
	absurd := append([]byte(nil), good...)
	binary.LittleEndian.PutUint64(absurd[len(absurd)-segTrailerFootLen:], 1<<40)
	huge := filepath.Join(t.TempDir(), "huge.dat")
	if err := os.WriteFile(huge, absurd, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSegTrailerFile(huge); ok {
		t.Error("accepted a foot claiming a trailer larger than the file")
	}

	// The writers report rather than create: a path that is not a segment must not become
	// a file consisting only of a trailer.
	if _, err := segArenaEnd(absent); err == nil {
		t.Error("segArenaEnd invented an arena for a file that does not exist")
	}
	if err := writeSegTrailer(absent, 0, nil); err == nil {
		t.Error("writeSegTrailer created a file that was not a segment")
	}
	if err := addDictReplica(absent, 0, 1, []byte("dictionary")); err == nil {
		t.Error("addDictReplica created a file that was not a segment")
	}
	if _, err := os.Stat(absent); err == nil {
		t.Error("a refused write left a file behind")
	}

	// And the replica accessors, which read bodies a newer or damaged writer may have
	// left in any shape.
	if _, _, ok := decodeDictReplica([]byte{1, 2, 3}); ok {
		t.Error("decoded a replica body too short to hold an id and a dictionary")
	}
	tr, ok := parseSegTrailer(buildSegTrailer([]segTrailerSection{
		{id: segSecDictReplica, body: encodeDictReplica(4, []byte("four"))},
		{id: segSecDictReplica, body: []byte{1, 2}}, // malformed: skipped, not fatal
	}))
	if !ok {
		t.Fatal("a trailer holding a malformed replica section did not parse")
	}
	if _, has := tr.dictReplica(9); has {
		t.Error("found a copy of a dictionary nothing carries")
	}
	if got, has := tr.dictReplica(4); !has || string(got) != "four" {
		t.Errorf("dictReplica(4) = (%q, %v); want (\"four\", true)", got, has)
	}
	if ids := tr.dictReplicaIDs(); len(ids) != 1 || ids[0] != 4 {
		t.Errorf("dictReplicaIDs = %v; want [4] (the malformed section ignored)", ids)
	}
}
