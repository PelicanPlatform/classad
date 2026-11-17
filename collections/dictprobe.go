package collections

// Checking a dictionary against the records that need it, two independent ways.
//
// The file name says which dictionary a segment needs, and since stamping, so does every
// frame inside it -- but both say only WHICH, never WHICH BYTES. A dictionary file carrying
// the right id and the wrong contents passes every check there is: loadDicts accepts it,
// the file check accepts it, and then every frame fails its own content checksum, is
// classified as an ordinary decode skip, and the records quietly stop appearing. Bitrot
// gets a dictionary file into that state with nobody's help.
//
// So fsck asks twice, and the value is in the disagreements.
//
//   - The FINGERPRINT says what the dictionary was when the segment was sealed: the id,
//     the length and a CRC-32C, recorded in the segment's own trailer (segtrailer.go).
//   - The PROBE says what is true now: take a few records and decode them with the
//     dictionary presently in dicts/.
//
// Either alone is ambiguous. A fingerprint that disagrees might be the dictionary having
// changed or the fingerprint being wrong; records that will not decode might be a wrong
// dictionary or damaged records. Together they separate those:
//
//	fingerprint   probe      what it means
//	differs       fails      the dictionary in dicts/ is not the one these records used
//	differs       decodes    the records are fine; the FINGERPRINT is the untrustworthy one
//	matches       fails      the dictionary is right, so the RECORDS are damaged
//	not recorded  fails      no second opinion; sealed before fingerprints existed
//
// The probe has one measured trap. An INCOMPRESSIBLE record decodes correctly under any
// dictionary, because it never back-references one, so a single sample that decodes proves
// nothing. Several are taken, largest first, since a record that compressed well is one
// that leaned on the dictionary.

import (
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
)

// dictFingerprintMismatches counts segments whose recorded fingerprint does not match the
// dictionary the store loaded for them, process-wide since start.
var dictFingerprintMismatches atomic.Int64

// DictFingerprintMismatches reports how many segments have been found to name a dictionary
// that is not the one their records were written with. Any count at all means records are
// being skipped.
func DictFingerprintMismatches() int64 { return dictFingerprintMismatches.Load() }

// dictMismatchTally counts, for one dictionary, the segments recovery found to disagree
// with it.
type dictMismatchTally struct {
	segments int
	example  string
}

// noteDictMismatch records one segment whose fingerprint disagrees with the dictionary
// loaded for it. Recovery runs a shard at a time and nothing else touches this.
func (c *Collection) noteDictMismatch(id uint32, segPath string) {
	if c.dictMismatch == nil {
		c.dictMismatch = map[uint32]*dictMismatchTally{}
	}
	t := c.dictMismatch[id]
	if t == nil {
		t = &dictMismatchTally{example: segPath}
		c.dictMismatch[id] = t
	}
	t.segments++
}

// reportDictMismatches says what recovery found, ONCE per dictionary rather than once per
// segment. A dictionary that is not the one its records were written with disagrees with
// EVERY segment naming it, so a line apiece would bury the one fact an operator needs
// under hundreds of repetitions of it -- and the count is itself the useful part, because
// it says how much of the store is affected.
func (c *Collection) reportDictMismatches() {
	ids := make([]uint32, 0, len(c.dictMismatch))
	for id := range c.dictMismatch {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		t := c.dictMismatch[id]
		c.reportDictFault(&DictFault{
			ID: id, Kind: DictFaultMismatch, Segments: t.segments, From: t.example,
			Path: filepath.Join(c.dir, "dicts", fmt.Sprintf("%d.zst", id)),
		})
	}
	c.dictMismatch = nil
}

// checkSegFingerprint compares what a segment recorded about its dictionary against the
// dictionary the store just loaded for it. ok is false when they disagree.
//
// Recovery already reads the trailer -- it is where the replica list and this fingerprint
// come from -- so this costs a comparison and no I/O at all, which is why Open can afford
// to ask on every segment.
func checkSegFingerprint(t segTrailer, dictID uint32, pin *dictPin) (mismatch bool) {
	id, length, crc, ok := t.dictFingerprint()
	if !ok || pin == nil {
		return false // nothing recorded, or no file to compare against: fsck says so
	}
	return id != dictID || length != uint32(pin.size) || crc != pin.crc
}

// dictProbeSamples is how many records a probe decodes per segment. More than one because
// an incompressible record decodes under any dictionary; a handful because each is
// microseconds and the point is to find one that actually used the dictionary.
const dictProbeSamples = 4

// dictProbeKit is what a probe needs for one dictionary: a codec built from the file that
// is in dicts/ now, and that file's shape, to compare a fingerprint against.
type dictProbeKit struct {
	codec  Codec
	length uint32
	crc    uint32
}

// newDictProbeKits builds a kit for every dictionary with a usable file. A dictionary with
// no file, or one that will not load, gets none: fsck already reports that, and probing
// with nothing would only restate it.
func newDictProbeKits(dictsDir string, ids []uint32) map[uint32]*dictProbeKit {
	kits := map[uint32]*dictProbeKit{}
	for _, id := range ids {
		path := fmt.Sprintf("%s/%d.zst", dictsDir, id)
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		codec, cerr := NewZSTDCodec(b)
		if cerr != nil {
			continue
		}
		kits[id] = &dictProbeKit{codec: codec, length: uint32(len(b)), crc: crc32.Checksum(b, crcTable)}
	}
	return kits
}

// FsckDictCheck is one segment's dictionary checked both ways. Only the segments where
// something disagreed are reported; a segment whose fingerprint matches and whose records
// decode says nothing worth saying.
type FsckDictCheck struct {
	Path   string
	DictID uint32
	// Recorded is whether the segment's trailer records which dictionary it was written
	// under. False for every segment sealed before fingerprints existed, and for one with
	// no room for a trailer.
	Recorded bool
	// Matches is whether that record agrees with the dictionary file in dicts/ now.
	// Meaningless when Recorded is false.
	Matches bool
	// Probed is how many records were decoded with that dictionary and ProbeFailures how
	// many of them refused. Probed is 0 when there was no usable dictionary to probe with,
	// or no record to probe.
	Probed, ProbeFailures int
}

// agrees reports whether both checks are content: nothing to say about this segment.
func (c FsckDictCheck) agrees() bool {
	return c.ProbeFailures == 0 && (!c.Recorded || c.Matches)
}

// Verdict is the one line an operator needs, and it is written from BOTH signals because
// neither alone distinguishes a wrong dictionary from damaged records.
func (c FsckDictCheck) Verdict() string {
	switch {
	case c.Recorded && !c.Matches && c.ProbeFailures > 0:
		return fmt.Sprintf("the dictionary in dicts/%d.zst is not the one these records were written with "+
			"(the fingerprint differs and %d of %d sample records will not decode)", c.DictID, c.ProbeFailures, c.Probed)
	case c.Recorded && !c.Matches && c.Probed > 0:
		return fmt.Sprintf("the records decode but the recorded fingerprint of dictionary %d does not match "+
			"the file; the FINGERPRINT is the untrustworthy one here, not the dictionary", c.DictID)
	case c.Recorded && !c.Matches:
		return fmt.Sprintf("the recorded fingerprint of dictionary %d does not match the file, "+
			"and no record could be sampled to settle it", c.DictID)
	case c.ProbeFailures > 0 && c.Recorded:
		return fmt.Sprintf("dictionary %d is the one recorded, so the RECORDS are damaged, not the dictionary "+
			"(%d of %d sample records will not decode)", c.DictID, c.ProbeFailures, c.Probed)
	case c.ProbeFailures > 0:
		return fmt.Sprintf("%d of %d sample records will not decode with dictionary %d, and this segment "+
			"records no fingerprint to settle which of the two is wrong", c.ProbeFailures, c.Probed, c.DictID)
	}
	return ""
}

// checkSegDict compares a segment's recorded fingerprint against the dictionary in dicts/
// and folds in what the probe found while the segment was walked.
func checkSegDict(path string, dictID uint32, kit *dictProbeKit, probed, failures int) FsckDictCheck {
	c := FsckDictCheck{Path: path, DictID: dictID, Probed: probed, ProbeFailures: failures}
	t, state := readSegTrailerState(path)
	if state != segTrailerOK {
		return c
	}
	id, length, crc, ok := t.dictFingerprint()
	if !ok {
		return c
	}
	c.Recorded = true
	c.Matches = id == dictID && kit != nil && length == kit.length && crc == kit.crc
	return c
}

// keepLargestProbe keeps the dictProbeSamples largest ads seen so far, smallest last so
// the one it displaces is cheap to find. The slices alias the segment's bytes, which the
// caller holds for the whole walk.
func keepLargestProbe(have [][]byte, ad []byte) [][]byte {
	if len(ad) == 0 {
		return have
	}
	if len(have) < dictProbeSamples {
		have = append(have, ad)
	} else {
		smallest := 0
		for i := 1; i < len(have); i++ {
			if len(have[i]) < len(have[smallest]) {
				smallest = i
			}
		}
		if len(ad) <= len(have[smallest]) {
			return have
		}
		have[smallest] = ad
	}
	return have
}

// runDictProbe decodes the samples with the dictionary that is in dicts/ now, and reports
// how many were tried and how many refused.
//
// A refusal is the zstd frame's own content checksum failing, which a wrong dictionary
// provokes essentially every time -- measured at 1200 out of 1200 -- so one failure is
// enough to say the dictionary and the records disagree. Successes are weaker evidence
// individually, which is why several are tried.
func runDictProbe(kit *dictProbeKit, samples [][]byte) (probed, failures int) {
	if kit == nil {
		return 0, 0
	}
	for _, ad := range samples {
		probed++
		if _, err := kit.codec.Decompress(nil, ad); err != nil {
			failures++
		}
	}
	return probed, failures
}
