package collections

// The segment trailer: a structured tail past the arena.
//
// A segment file is its arena and nothing else. Records start at offset 0, the walk stops
// at the first zero totalLen, the file is block-reserved to SegmentSize up front, and
// recovery sizes the mapping from the file. There is no header and nowhere to put a fact
// about the segment that is not a record.
//
// Some facts do not belong in the record stream. The flag bits in the keyLen word are all
// four spoken for (marker, dict, columnar, delta), and worse, a new marker kind is not
// transparently skipped by an older build: recIsTimeMarker is "marker and not (dict|col)",
// so it would read a new marker's body as a unixMillis, and recDeleteMarkerKey would read
// it as a delete marker with a garbage key. Adding a record kind is a format break.
//
// The trailer sidesteps all of it by living where no record walk reaches. At seal the file
// is extended and the trailer written past the arena; recovery reads it first and then maps
// only the arena, so the mapping covers exactly what it covered before. A build that knows
// nothing about trailers maps the whole file and still stops at the first zero totalLen,
// long before the tail -- so a carrier segment reads correctly on an older build, with no
// version to negotiate.
//
// The layout is a section table rather than a fixed trailer, which is where the sidecar
// container ended up after six versions of minting a new magic per section (see
// sidecar_sections.go). A reader slices by the directory instead of by a layout it has to
// know in advance, and an id it does not recognise is bytes it can carry through a
// read-modify-write untouched.
//
//	[ ...arena..., zero fill to the original segment size ]
//	[ section bodies, in directory order                  ]
//	[ directory: nsec * (id u16 | pad u16 | len u32 | crc32c u32) ]
//	[ foot: trailerBytes u64 | nsec u32 | crc32c u32 | magic u32("SGT1") ]
//
// EVERY SECTION CARRIES ITS OWN CHECKSUM, and the foot's covers the directory and the foot
// alone -- not the bodies. That split is what makes a damaged trailer recoverable in
// pieces rather than lost whole. The directory is the index, so if it does not verify
// nothing can be located and the trailer is gone; but once it does, every (id, length,
// checksum) is known good, each body can be checked on its own, and one bad section costs
// exactly itself. A trailer holding three dictionary replicas does not lose all three
// because one segment got a bad block.
//
// The foot's checksum is also what keeps a zero-filled region that happens to end in the
// magic from reading as a trailer. A trailer whose DIRECTORY does not verify is treated as
// no trailer at all: the segment maps whole, which is exactly what every segment written
// before this did.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"sort"
)

const (
	// segTrailerMagic is the last four bytes of a segment file that has a trailer.
	segTrailerMagic = 0x53475431 // "SGT1"
	// segTrailerFootLen is trailerBytes u64 + nsec u32 + crc32c u32 + magic u32.
	segTrailerFootLen = 20
	// segTrailerDirEntryLen is id u16 + pad u16 (so the length is 4-byte aligned) + len u32
	// + the section's own crc32c u32.
	segTrailerDirEntryLen = 12
	// segTrailerMaxSections bounds what the foot may claim before the directory is read,
	// so a corrupt nsec cannot make the reader address far outside the file.
	segTrailerMaxSections = 64
	// segTrailerMaxBytes bounds the whole trailer for the same reason, before anything is
	// allocated. Generous next to a 112 KB dictionary and small next to a segment.
	segTrailerMaxBytes = 4 << 20
)

// Section ids. This is a namespace of its own and NOT the sidecar container's: the two
// tables are read by different code from different files and nothing is gained by making
// an id mean the same in both. Never renumber one; the id is on disk.
const (
	// segSecDictReplica holds a copy of a compression dictionary, raw. The body is the
	// dictionary id (u32, little-endian) followed by the dictionary's bytes.
	//
	// Raw is the whole point. The dictionary cannot be stored compressed under itself,
	// which is also why a store that loses dicts/ reports its interned attribute
	// dictionaries as damaged rather than as missing: those ARE compressed, by the zstd
	// dictionary that is gone.
	segSecDictReplica uint16 = 1
	// segSecDictFingerprint records WHICH compression dictionary a segment's records were
	// written under, and what that dictionary was: the id, its length, and a CRC-32C of
	// its bytes.
	//
	// The id alone is already on disk -- it is in the file name, and since stamping it is
	// in every frame header too -- so what this adds is the third case. A dictionary file
	// that is stamped with the right id but holds DIFFERENT BYTES passes every check
	// there was: loadDicts accepts it, fsck's file check accepts it, and then every frame
	// fails its content checksum, is classified as an ordinary decode skip, and the
	// records quietly stop appearing in results. Bitrot in a dictionary file reaches that
	// state on its own.
	//
	// Body: dictID u32 | dictLen u32 | dictCRC u32.
	segSecDictFingerprint uint16 = 2
)

// segTrailerSection is one typed piece of a trailer.
type segTrailerSection struct {
	id   uint16
	body []byte
}

// segTrailer is a parsed trailer. Sections keep the order they were written in, and ids
// may repeat: a carrier holding two dictionary replicas has two sections with the same id.
type segTrailer struct {
	sections []segTrailerSection
	// damaged are the ids of sections whose body failed its own checksum. The directory
	// verified, so each of these is known to have been written and known to be unusable
	// now -- which is a different and more useful fact than the section simply being
	// absent. Nothing else in the trailer is affected by them.
	damaged []uint16
	// bytes is the trailer's whole on-disk length, which is what a reader subtracts from
	// the file size to get the arena.
	bytes int
}

// buildSegTrailer packs sections into the on-disk layout. A zero-length body is dropped:
// absence and emptiness are the same thing to every reader, and keeping the distinction
// would mean a section that says nothing still costs a directory entry.
func buildSegTrailer(sections []segTrailerSection) []byte {
	kept := make([]segTrailerSection, 0, len(sections))
	body := 0
	for _, s := range sections {
		if len(s.body) > 0 {
			kept = append(kept, s)
			body += len(s.body)
		}
	}
	total := body + len(kept)*segTrailerDirEntryLen + segTrailerFootLen
	out := make([]byte, 0, total)
	for _, s := range kept {
		out = append(out, s.body...)
	}
	dirAt := len(out)
	for _, s := range kept {
		out = binary.LittleEndian.AppendUint16(out, s.id)
		out = binary.LittleEndian.AppendUint16(out, 0) // pad
		out = binary.LittleEndian.AppendUint32(out, uint32(len(s.body)))
		out = binary.LittleEndian.AppendUint32(out, crc32.Checksum(s.body, crcTable))
	}
	out = binary.LittleEndian.AppendUint64(out, uint64(total))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(kept)))
	// Over the directory and the foot's own fields, NOT over the bodies: each body is
	// checked against its directory entry instead, so one bad block costs one section.
	out = binary.LittleEndian.AppendUint32(out, crc32.Checksum(out[dirAt:], crcTable))
	return binary.LittleEndian.AppendUint32(out, segTrailerMagic)
}

// parseSegTrailer reads a trailer out of b, which must be exactly the trailer region (its
// last byte is the file's last byte). ok is false for anything that is not a well-formed,
// checksummed trailer -- including a file that simply has none, which is the common case
// and not an error.
func parseSegTrailer(b []byte) (segTrailer, bool) {
	if len(b) < segTrailerFootLen {
		return segTrailer{}, false
	}
	foot := b[len(b)-segTrailerFootLen:]
	if binary.LittleEndian.Uint32(foot[16:]) != segTrailerMagic {
		return segTrailer{}, false
	}
	total := binary.LittleEndian.Uint64(foot[0:])
	nsec := binary.LittleEndian.Uint32(foot[8:])
	if total != uint64(len(b)) || nsec > segTrailerMaxSections {
		return segTrailer{}, false
	}
	dirLen := int(nsec) * segTrailerDirEntryLen
	if len(b) < dirLen+segTrailerFootLen {
		return segTrailer{}, false
	}
	// The directory is the index: nothing can be located without it, so it is checked
	// first and as a whole. Its checksum covers the foot's own fields too, which is what
	// stops a zero-filled tail that happens to end in the magic from reading as a trailer.
	if crc32.Checksum(b[len(b)-segTrailerFootLen-dirLen:len(b)-8], crcTable) !=
		binary.LittleEndian.Uint32(foot[12:]) {
		return segTrailer{}, false
	}
	dir := b[len(b)-segTrailerFootLen-dirLen : len(b)-segTrailerFootLen]
	bodies := b[:len(b)-segTrailerFootLen-dirLen]
	t := segTrailer{bytes: len(b)}
	at := 0
	for i := 0; i < int(nsec); i++ {
		e := dir[i*segTrailerDirEntryLen:]
		id := binary.LittleEndian.Uint16(e)
		n := int(binary.LittleEndian.Uint32(e[4:]))
		if n < 0 || at+n > len(bodies) {
			return segTrailer{}, false
		}
		body := bodies[at : at+n]
		at += n
		// The directory verified, so this entry's length and checksum are known good and
		// the slice above is in the right place. A body that fails its own checksum
		// therefore costs exactly itself: it is named as damaged and every other section
		// is handed back untouched.
		if crc32.Checksum(body, crcTable) != binary.LittleEndian.Uint32(e[8:]) {
			t.damaged = append(t.damaged, id)
			continue
		}
		t.sections = append(t.sections, segTrailerSection{id: id, body: body})
	}
	// Body bytes the directory does not address are left alone rather than refused. The
	// CRC has already vouched for them, so they are not damage; they are a writer doing
	// something this build does not do -- padding a section for alignment, say. Refusing
	// would make a newer trailer unreadable for no gain, which is the opposite of what a
	// section table is for. A directory pointing PAST the bodies is still rejected above.
	return t, true
}

// segTrailerState says what a segment file's tail turned out to be. "No trailer" and
// "a trailer that will not read" answer the same question for a reader -- neither gives
// anything usable -- but they are opposite facts for anyone asking after the store's
// health, and a reader that cannot tell them apart reports damage as absence.
type segTrailerState int

const (
	// segTrailerNone: the file ends in records and zero fill, as every segment written
	// before trailers existed does, and as every non-carrier does now.
	segTrailerNone segTrailerState = iota
	// segTrailerDamaged: the magic is there, so a trailer was written, and the directory
	// does not verify. Nothing in it can be located. This is the one state in which the
	// arena's end is unknowable, because the length that says where it is lives in the
	// foot that just failed its own checksum.
	segTrailerDamaged
	// segTrailerOK: the directory verified. Individual sections may still be damaged;
	// segTrailer.damaged names them.
	segTrailerOK
)

// readSegTrailerAt reads the trailer of a segment file of the given size. It is two short
// ReadAts -- the foot, then the region it describes -- rather than a mapping, because it
// runs before the segment is mapped and decides how much of the file to map.
func readSegTrailerAt(f *os.File, size int64) (segTrailer, segTrailerState) {
	if size < segTrailerFootLen {
		return segTrailer{}, segTrailerNone
	}
	var foot [segTrailerFootLen]byte
	if _, err := f.ReadAt(foot[:], size-segTrailerFootLen); err != nil {
		return segTrailer{}, segTrailerNone
	}
	// The magic is the discriminator. Past here a trailer was written, so anything that
	// does not add up is damage rather than absence.
	if binary.LittleEndian.Uint32(foot[16:]) != segTrailerMagic {
		return segTrailer{}, segTrailerNone
	}
	total := binary.LittleEndian.Uint64(foot[0:])
	if total < segTrailerFootLen || total > segTrailerMaxBytes || total > uint64(size) {
		return segTrailer{}, segTrailerDamaged
	}
	b := make([]byte, total)
	if _, err := f.ReadAt(b, size-int64(total)); err != nil {
		return segTrailer{}, segTrailerDamaged
	}
	t, ok := parseSegTrailer(b)
	if !ok {
		return segTrailer{}, segTrailerDamaged
	}
	return t, segTrailerOK
}

// readSegTrailerState opens path and reads its trailer, saying which of the three states
// it is in.
func readSegTrailerState(path string) (segTrailer, segTrailerState) {
	f, err := os.Open(path)
	if err != nil {
		return segTrailer{}, segTrailerNone
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return segTrailer{}, segTrailerNone
	}
	return readSegTrailerAt(f, st.Size())
}

// readSegTrailerFile is readSegTrailerState for the readers that only want what they can
// use. A damaged trailer gives them nothing, same as none.
func readSegTrailerFile(path string) (segTrailer, bool) {
	t, state := readSegTrailerState(path)
	return t, state == segTrailerOK
}

// writeSegTrailer replaces a segment file's trailer with sections, in place, and makes it
// durable. arenaEnd is where the trailer starts: the file's size with any existing trailer
// taken off, which is the segment's original reserved size.
//
// It is NOT atomic, and does not need to be. A torn write leaves a trailer whose CRC does
// not verify, which every reader treats as no trailer at all -- the segment maps whole and
// reads exactly as it did before anything was written here. The replicas the trailer held
// are then restored by the next census, which is the safe direction and the reason the
// census has to exist whatever the trailer does.
//
// Nothing here touches a mapped byte: the trailer lives past the mapping, so this runs
// safely against a sealed segment that readers are using right now.
func writeSegTrailer(path string, arenaEnd int64, sections []segTrailerSection) error {
	t := buildSegTrailer(sections)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(t, arenaEnd); err != nil {
		f.Close()
		return err
	}
	// After the write, not before: shrinking first would expose a file that is neither the
	// old trailer nor the new one for the length of the write.
	if err := f.Truncate(arenaEnd + int64(len(t))); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ErrDamagedTrailer is returned for a segment whose trailer was written and will not
// read. Its arena's end is genuinely unknowable: the length that says where the trailer
// begins lives in the foot that just failed its own checksum.
//
// Nothing may be written to such a segment's tail. Treating the damage as "no trailer"
// would put the arena's end at the file's end -- past the damaged trailer -- and append a
// second one after it, so every pass that tried would grow the file by another trailer and
// fold the last one into what recovery maps as the arena.
var ErrDamagedTrailer = errors.New("collections: segment trailer is damaged and its extent cannot be determined")

// segArenaEnd returns where a segment file's arena ends -- its size, less any trailer.
func segArenaEnd(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	switch t, state := readSegTrailerState(path); state {
	case segTrailerOK:
		return st.Size() - int64(t.bytes), nil
	case segTrailerDamaged:
		return 0, fmt.Errorf("%w: %s", ErrDamagedTrailer, path)
	}
	return st.Size(), nil
}

// encodeDictFingerprint builds a segSecDictFingerprint body.
func encodeDictFingerprint(id uint32, dictLen int, crc uint32) []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint32(b[0:], id)
	binary.LittleEndian.PutUint32(b[4:], uint32(dictLen))
	binary.LittleEndian.PutUint32(b[8:], crc)
	return b
}

// dictFingerprint returns what a trailer records about the dictionary its segment was
// written under. ok is false when it records none, which is every segment sealed before
// fingerprints existed and every one with no room for a trailer.
func (t segTrailer) dictFingerprint() (id, dictLen, crc uint32, ok bool) {
	for _, s := range t.sections {
		if s.id == segSecDictFingerprint && len(s.body) >= 12 {
			return binary.LittleEndian.Uint32(s.body[0:]),
				binary.LittleEndian.Uint32(s.body[4:]),
				binary.LittleEndian.Uint32(s.body[8:]), true
		}
	}
	return 0, 0, 0, false
}

// encodeDictReplica builds a segSecDictReplica body: the dictionary id, then the
// dictionary's own bytes, uncompressed.
func encodeDictReplica(id uint32, dict []byte) []byte {
	b := make([]byte, 4, 4+len(dict))
	binary.LittleEndian.PutUint32(b, id)
	return append(b, dict...)
}

// decodeDictReplica splits a segSecDictReplica body. The returned slice aliases b.
func decodeDictReplica(b []byte) (uint32, []byte, bool) {
	if len(b) < 5 { // an id and at least one byte of dictionary
		return 0, nil, false
	}
	return binary.LittleEndian.Uint32(b), b[4:], true
}

// dictReplicaIDs lists the dictionary ids a trailer carries copies of, ascending and
// deduplicated. It reads only the four-byte id of each replica section, not the
// dictionaries themselves.
func (t segTrailer) dictReplicaIDs() []uint32 {
	seen := map[uint32]bool{}
	for _, s := range t.sections {
		if s.id != segSecDictReplica {
			continue
		}
		if id, _, ok := decodeDictReplica(s.body); ok {
			seen[id] = true
		}
	}
	out := make([]uint32, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// dictReplica returns the copy of dictionary id a trailer carries, if it has one.
func (t segTrailer) dictReplica(id uint32) ([]byte, bool) {
	for _, s := range t.sections {
		if s.id != segSecDictReplica {
			continue
		}
		if got, dict, ok := decodeDictReplica(s.body); ok && got == id {
			return dict, true
		}
	}
	return nil, false
}

// ErrNoRoomForReplica is returned when a segment file cannot carry a dictionary copy:
// either the copy would dwarf the data it rides along with, or the arena is written so
// close to its end that a trailer would sit where a record walk still looks.
var ErrNoRoomForReplica = errors.New("collections: segment cannot carry a dictionary replica")

// replicaSizeRatio bounds a replica against the segment carrying it: a copy may not be
// more than this fraction of the arena. Segment size is a deployment knob -- fixtures and
// small-segment configurations run at 64 KiB, where a 112 KB dictionary does not fit at
// all -- so the rule has to be a ratio rather than a count of segments.
const replicaSizeRatio = 8

// trailerWalkGuard is how much zero fill must separate the end of the written arena from
// the trailer.
//
// This is what keeps a carrier readable by a build that knows nothing about trailers. Such
// a build maps the whole file and walks records until it reads a zero totalLen, which sits
// at offset used+24. If the arena were written right up to its end, that read would land in
// the trailer and the walk would take trailer bytes for a record header. A record header is
// 32 bytes, so requiring that much zero fill leaves the terminator where it has always
// been, inside the arena, with room to spare.
const trailerWalkGuard = recHeaderSize

// updateSegTrailer rewrites a sealed segment file's trailer, handing revise the sections
// that are there now and writing back whatever it returns. It is the one way anything adds
// to a trailer, so the rules below hold for every section kind.
//
// arenaUsed is the segment's written extent. The caller has it (the segment, or its
// sidecar, records it) and this cannot safely guess: see trailerWalkGuard.
//
// The segment must be SEALED. A trailer sits past the arena and an active segment's appends
// grow into it; recovery maps only the arena, so an active segment that acquired a trailer
// would quietly lose that space.
//
// Sections that failed their own checksum are NOT handed to revise and so are dropped by
// the rewrite rather than carried forward as garbage. That is the right outcome and not a
// silent one: the damage is reported by Fsck, and a damaged replica is replaced by the next
// census.
func updateSegTrailer(path string, arenaUsed int, revise func([]segTrailerSection) []segTrailerSection) error {
	arenaEnd, err := segArenaEnd(path)
	if err != nil {
		return err
	}
	if int64(arenaUsed)+trailerWalkGuard > arenaEnd {
		return fmt.Errorf("%w: %d of %d bytes written leaves no room before the trailer",
			ErrNoRoomForReplica, arenaUsed, arenaEnd)
	}
	var have []segTrailerSection
	if t, ok := readSegTrailerFile(path); ok {
		for _, s := range t.sections {
			// Copy: these alias the buffer the read allocated, and the rebuild appends
			// them into a new one.
			have = append(have, segTrailerSection{id: s.id, body: append([]byte(nil), s.body...)})
		}
	}
	return writeSegTrailer(path, arenaEnd, revise(have))
}

// addDictReplica writes a copy of a compression dictionary into a sealed segment file's
// trailer, replacing any copy of the same dictionary already there and keeping every other
// section, including ones this build does not recognise.
func addDictReplica(path string, arenaUsed int, id uint32, dict []byte) error {
	if id == 0 || len(dict) == 0 {
		return fmt.Errorf("collections: dictionary %d has nothing to replicate", id)
	}
	arenaEnd, err := segArenaEnd(path)
	if err != nil {
		return err
	}
	if int64(len(dict))*replicaSizeRatio > arenaEnd {
		return fmt.Errorf("%w: %d bytes into a %d byte segment", ErrNoRoomForReplica, len(dict), arenaEnd)
	}
	return updateSegTrailer(path, arenaUsed, func(have []segTrailerSection) []segTrailerSection {
		keep := have[:0]
		for _, s := range have {
			if s.id == segSecDictReplica {
				if got, _, ok := decodeDictReplica(s.body); ok && got == id {
					continue // replaced below
				}
			}
			keep = append(keep, s)
		}
		return append(keep, segTrailerSection{id: segSecDictReplica, body: encodeDictReplica(id, dict)})
	})
}

// addDictFingerprint records which compression dictionary a sealed segment was written
// under, and what that dictionary was. Unlike a replica it costs twelve bytes, so it is
// not bounded against the segment's size -- only against the walk guard, which every
// trailer must respect.
func addDictFingerprint(path string, arenaUsed int, id uint32, dict []byte) error {
	if id == 0 || len(dict) == 0 {
		return fmt.Errorf("collections: dictionary %d has nothing to fingerprint", id)
	}
	body := encodeDictFingerprint(id, len(dict), crc32.Checksum(dict, crcTable))
	return updateSegTrailer(path, arenaUsed, func(have []segTrailerSection) []segTrailerSection {
		keep := have[:0]
		for _, s := range have {
			if s.id != segSecDictFingerprint {
				keep = append(keep, s)
			}
		}
		return append(keep, segTrailerSection{id: segSecDictFingerprint, body: body})
	})
}
