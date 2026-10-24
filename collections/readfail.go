package collections

import (
	"errors"
	"sync/atomic"
)

// readFail names WHY a stored record could not be read. A read that misses returns a bare
// false, which collapses five unrelated conditions into one answer -- and on a production
// mirror that ambiguity was the whole difficulty: patch writes were being refused because
// the base "could not be read", the refusal counter climbed, and nothing said whether the
// key was invisible at the snapshot, whose segment had gone, whose columnar payload was
// missing, whose delta chain no longer resolved, or whose bytes would not decode. The fixes
// for those are unrelated, so the reason has to travel with the miss.
type readFail uint8

const (
	failNone       readFail = iota // no failure
	failNotVisible                 // no version of the key is live at the read's snapshot
	failSegGone                    // the location resolved, but its segment is no longer mapped
	failReassemble                 // a stripped record whose segment's columnar payload is missing or damaged
	failDecode                     // the bytes were fetched but would not decode
	failRecordCRC                  // the record's own checksum did not match its bytes (see VerifyReads)

	// A delta chain that could not be materialized, split by WHERE the walk gave up. These
	// started as one reason, and production then reported 100% of its refusals under it --
	// which narrowed the cause to this function and no further. They are separate because
	// they mean unrelated things: no base is a chain whose whole record is GONE (the hazard
	// the seal-collapse invariant exists to prevent), while a flag mismatch is a record whose
	// header and payload disagree, and a decompress failure is bad bytes.
	failDeltaNoVersions   // the walk found no versions of the key at all
	failDeltaNoBase       // versions found, but none of them is a whole record to merge onto
	failDeltaReassemble   // a participant's columnar payload could not be reassembled
	failDeltaDecompress   // a participant would not decompress
	failDeltaFlagMismatch // a participant's header and payload disagree about being a delta
	failDeltaBaseDecode   // the chain's WHOLE record would not decode
	failDeltaPatchDecode  // a delta layered on top of the base would not decode
	failDeltaChainBroken  // the version walk truncated at a link into a segment that is gone
	failDeltaIndexPending // a sealed segment could not be probed: its key index is not built yet

	failMax // not a reason; bounds the counter array
)

func (r readFail) String() string {
	switch r {
	case failNone:
		return "none"
	case failNotVisible:
		return "not-visible"
	case failSegGone:
		return "segment-gone"
	case failReassemble:
		return "reassemble"
	case failDecode:
		return "decode"
	case failRecordCRC:
		return "record-crc"
	case failDeltaNoVersions:
		return "delta-no-versions"
	case failDeltaNoBase:
		return "delta-no-base"
	case failDeltaReassemble:
		return "delta-reassemble"
	case failDeltaDecompress:
		return "delta-decompress"
	case failDeltaFlagMismatch:
		return "delta-flag-mismatch"
	case failDeltaBaseDecode:
		return "delta-base-decode"
	case failDeltaPatchDecode:
		return "delta-patch-decode"
	case failDeltaChainBroken:
		return "delta-chain-broken"
	case failDeltaIndexPending:
		return "delta-index-pending"
	}
	return "unknown"
}

// lastDecodeFailure samples the most recent decode error behind a delta-base-decode or
// delta-patch-decode refusal. A count says how often the bytes would not decode; only the
// error says what the decoder objected to, and without it a deployment refusing hundreds of
// writes an hour still cannot tell a truncated record from one in a format the reader does
// not recognise. Sampled, not accumulated: one live example is the diagnostic, and keeping
// every message would be unbounded.
var lastDecodeFailure atomic.Pointer[decodeFailure]

type decodeFailure struct {
	Stage string // "base" or "patch"
	Err   string
}

// LastDeltaDecodeFailure returns the most recently sampled decode error behind a refused
// chain merge, or ("", "") if none has happened.
func LastDeltaDecodeFailure() (stage, msg string) {
	if f := lastDecodeFailure.Load(); f != nil {
		return f.Stage, f.Err
	}
	return "", ""
}

// noteDecodeFailure samples one decode error. Called only on the failure path.
func noteDecodeFailure(stage string, err error) {
	if err == nil {
		return
	}
	lastDecodeFailure.Store(&decodeFailure{Stage: stage, Err: err.Error()})
}

// sealedSkippedNoIndex counts sealed segments a key probe could not look inside because their
// key index has not been built yet. The index is built by a reindex pass rather than at seal
// time, so this is a real window, and a probe that comes up empty during it is not the same as
// a key that is absent.
var sealedSkippedNoIndex atomic.Int64

// SealedProbesSkipped reports how many sealed-segment probes were skipped for want of a key
// index. A rising count alongside delta-index-pending says reads are racing the reindex pass.
func SealedProbesSkipped() int64 { return sealedSkippedNoIndex.Load() }

// lastNoBase samples the shape of the most recent chain that had no whole record. The reason
// name says which of three faults it was; this says how much of the chain was found, which is
// what separates "the base is one link past a dead segment" from "there is nothing here".
var lastNoBase atomic.Pointer[noBaseSample]

type noBaseSample struct {
	Versions      int
	ChainBroken   bool
	SealedSkipped int
}

// LastNoBaseDetail returns the shape of the most recent no-base chain: how many versions the
// walk did find, whether it truncated at a dead link, and how many sealed segments it could not
// probe.
func LastNoBaseDetail() (versions int, chainBroken bool, sealedSkipped int) {
	if s := lastNoBase.Load(); s != nil {
		return s.Versions, s.ChainBroken, s.SealedSkipped
	}
	return 0, false, 0
}

func noteNoBase(versions int, chainBroken bool, sealedSkipped int) {
	lastNoBase.Store(&noBaseSample{Versions: versions, ChainBroken: chainBroken, SealedSkipped: sealedSkipped})
}

// unreadableByReason counts refused patch writes per readFail, indexed by the reason. It is
// the per-reason breakdown of fallbackUnreadableBase, whose total it always sums to.
var unreadableByReason [failMax]atomic.Int64

// UnreadableBaseReasonNames lists every reason name UnreadableBaseReasons can report, including
// the ones currently at zero. A consumer publishing these as metrics needs the whole set up
// front: a reason that appears only once it is non-zero reads as "no such counter" at exactly
// the moment someone is checking whether it is the one firing.
func UnreadableBaseReasonNames() []string {
	out := make([]string, 0, failMax-1)
	for r := failNotVisible; r < failMax; r++ {
		out = append(out, r.String())
	}
	return out
}

// UnreadableBaseReasons reports why patch writes were refused for an unreadable base, keyed
// by reason name. The values sum to UnreadableBaseRefusals.
func UnreadableBaseReasons() map[string]int64 {
	out := make(map[string]int64, len(unreadableByReason))
	for r := failNone; r < failMax; r++ {
		if n := unreadableByReason[r].Load(); n != 0 {
			out[r.String()] = n
		}
	}
	return out
}

// readSkip names why a record read returned nothing. A scan skips such a record rather than fail
// the query, and a point read reports the key as not found; either way the answer is short, and
// this is the only place that says so.
type readSkip uint8

const (
	skipRecordCRC       readSkip = iota // the record's own checksum did not match its bytes
	skipDictUnusable                    // the segment's attribute dictionary failed its checksum or was not recovered
	skipColMissing                      // a remnant whose segment has no columnar payload
	skipColDamaged                      // a record in a segment whose columnar payload was refused
	skipColRowMissing                   // a remnant the segment's columnar payload has no row for
	skipReassemble                      // a remnant and its columnar row would not rejoin
	skipDeltaUnresolved                 // a delta record whose chain could not be merged
	skipDecode                          // the bytes would not decompress or decode
	skipDamaged                         // a record its segment holds as damaged (see DamagedRecordsSkipped)
	skipFraming                         // a walk lost the records' framing at a damaged length (one per walk)
	skipMax                             // not a reason; bounds the counter array
)

func (r readSkip) String() string {
	switch r {
	case skipRecordCRC:
		return "record-crc"
	case skipDictUnusable:
		return "dict-unusable"
	case skipColMissing:
		return "col-missing"
	case skipColDamaged:
		return "col-damaged"
	case skipColRowMissing:
		return "col-row-missing"
	case skipReassemble:
		return "reassemble"
	case skipDeltaUnresolved:
		return "delta-unresolved"
	case skipDecode:
		return "decode"
	case skipDamaged:
		return "damaged"
	case skipFraming:
		return "framing"
	}
	return "unknown"
}

// readSkips counts record reads that returned nothing, indexed by readSkip.
var readSkips [skipMax]atomic.Int64

// noteReadSkip counts one record read that returned nothing because of err. Called only on the
// failure path.
func noteReadSkip(err error) { readSkips[readSkipOf(err)].Add(1) }

// readSkipOf classifies a record-read error.
func readSkipOf(err error) readSkip {
	switch {
	case errors.Is(err, errRecordCRC):
		return skipRecordCRC
	case errors.Is(err, errRecordDamaged):
		return skipDamaged
	case errors.Is(err, errFramingLost):
		return skipFraming
	case errors.Is(err, errSegDictUnusable):
		return skipDictUnusable
	case errors.Is(err, errColMissing):
		return skipColMissing
	case errors.Is(err, errColDamaged):
		return skipColDamaged
	case errors.Is(err, errColRowMissing):
		return skipColRowMissing
	case errors.Is(err, errBadRemnant):
		return skipReassemble
	case errors.Is(err, errDeltaUnresolved):
		return skipDeltaUnresolved
	}
	return skipDecode
}

// RecordReadSkipReasonNames lists every reason RecordReadSkips can report, including those at zero.
func RecordReadSkipReasonNames() []string {
	out := make([]string, 0, skipMax)
	for r := readSkip(0); r < skipMax; r++ {
		out = append(out, r.String())
	}
	return out
}

// RecordReadSkips reports, by reason, how many record reads since the process started returned
// nothing because the stored record could not be turned into a whole ad. Only nonzero reasons are
// present.
//
// Such a record is not an error to the caller: a scan or query skips it and carries on, and a point
// read reports the key as not found. A nonzero count therefore means some answers were short, which
// nothing in the answers themselves shows; Fsck is what says where. Each count is of reads, not
// records: a record two scans skip counts twice. VerifyReadSkips still counts the "record-crc"
// refusals of scans on its own.
func RecordReadSkips() map[string]int64 {
	out := make(map[string]int64, skipMax)
	for r := readSkip(0); r < skipMax; r++ {
		if n := readSkips[r].Load(); n != 0 {
			out[r.String()] = n
		}
	}
	return out
}
