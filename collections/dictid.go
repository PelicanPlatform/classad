package collections

// A trained ZSTD dictionary names itself, and until now every dictionary this package
// wrote gave the same name.
//
// RFC 8878 §5 puts a 4-byte Dictionary_ID immediately after the dictionary's magic, and
// the encoder copies that id into the header of every frame it compresses. TrainDictSize
// passed the constant 1, so every dictionary and every frame said "1": the only thing
// that ever distinguished one dictionary from another was the NAME of the file it was
// stored in, dicts/<id>.zst. A file renamed, restored into the wrong slot, or swapped
// with another was undetectable from its own bytes.
//
// The store did not notice, either. A wrong dictionary IS caught -- the zstd frame
// content checksum fails essentially every time, rather than yielding plausible garbage --
// but the failure arrives as an ordinary decode error, is classified as a decode skip, and
// the record is dropped. A relabelled dictionary therefore removed every record of the
// segments naming it from every query result, with a generic counter as the only evidence.
//
// Stamping the registered id into the dictionary ends that for everything written from
// here on. The slot and the file agree or Open refuses (loadDicts), and anything that got
// past that meets a decoder answering ErrUnknownDictionary on the first record instead of
// quietly returning fewer rows.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
)

const (
	// dictMagic is the ZSTD dictionary magic (RFC 8878 §5), written little-endian, so a
	// dictionary file begins 37 A4 30 EC.
	dictMagic = 0xEC30A437
	// dictIDOff is where the 4-byte little-endian Dictionary_ID sits.
	dictIDOff = 4
	// legacyDictID is the Dictionary_ID carried by every dictionary written before
	// stamping existed. It is also what BuildDict is handed before register stamps the
	// real id over it, because the builder needs a non-zero id: zero means "no dictionary
	// id" in the format and would leave a decoder unable to find the dictionary a frame
	// asks for.
	//
	// A dictionary still carrying it is UNVERIFIABLE, not wrong. Nothing records which
	// slot it belonged to, so Open accepts it and counts it instead. Two such dictionaries
	// that swap names stay indistinguishable by construction; this change ends that for
	// new dictionaries, not retroactively.
	legacyDictID = 1
)

// dictsUnverifiable counts dictionaries loaded that still carry legacyDictID, and so
// cannot be checked against the slot they were found in. It answers "how much of this
// store predates stamping", which is the question an operator has after this ships; it
// falls to zero as dictionaries are retrained.
var dictsUnverifiable atomic.Int64

// DictsUnverifiable reports how many loaded dictionaries carry no distinguishing id of
// their own, process-wide since start. See legacyDictID.
func DictsUnverifiable() int64 { return dictsUnverifiable.Load() }

// dictIDOf reads a dictionary's own Dictionary_ID. ok is false when b is not a ZSTD
// dictionary at all -- a separate question, which fsckDictFile asks directly and
// NewZSTDCodec answers by failing.
func dictIDOf(b []byte) (uint32, bool) {
	if len(b) < dictIDOff+4 || binary.LittleEndian.Uint32(b) != dictMagic {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b[dictIDOff:]), true
}

// stampDictID returns a copy of dict carrying id as its Dictionary_ID, so the dictionary
// names the slot it is about to be stored in and every frame compressed under it names
// the same.
//
// Registry ids are small, which RFC 8878 reserves against only for dictionaries shared
// between unrelated programs; a dictionary that never leaves its own store is the private
// environment the specification exempts. (The constant it replaces, 1, is in the same
// range.)
//
// It copies rather than writing through, because the caller still holds the unstamped
// bytes and a failed register must leave them as they were. 112 KB once per retrain does
// not buy enough to be worth the aliasing hazard.
func stampDictID(dict []byte, id uint32) ([]byte, error) {
	if id == 0 {
		return nil, errors.New("collections: dictionary id 0 is the base codec, which has no dictionary")
	}
	if _, ok := dictIDOf(dict); !ok {
		return nil, errors.New("collections: not a ZSTD dictionary")
	}
	out := make([]byte, len(dict))
	copy(out, dict)
	binary.LittleEndian.PutUint32(out[dictIDOff:], id)
	return out, nil
}

// ErrDictIDMismatch is returned by Open when a dictionary file's own id does not match the
// slot it is stored under -- dicts/3.zst holding the dictionary stamped 7.
//
// It is distinct from ErrMissingDictionaries because the operator action is distinct:
// nothing is damaged and nothing is lost, the files are in the wrong places, and the fix
// is to work out which belongs where. Opening anyway would decode the segments naming 3
// with a dictionary that is not theirs, and every one of their records would be skipped.
var ErrDictIDMismatch = errors.New("collections: a dictionary file does not match the id it is stored under")

// DictIDMismatchError names the slot, what was found in it, and the file.
type DictIDMismatchError struct {
	Slot  uint32 // the id taken from the file name
	Found uint32 // the Dictionary_ID the file itself carries
	Path  string
}

func (e *DictIDMismatchError) Error() string {
	return fmt.Sprintf("%v: %s carries dictionary id %d, not %d; every record in the segments naming dictionary %d would be skipped",
		ErrDictIDMismatch, e.Path, e.Found, e.Slot, e.Slot)
}

func (e *DictIDMismatchError) Unwrap() error { return ErrDictIDMismatch }
