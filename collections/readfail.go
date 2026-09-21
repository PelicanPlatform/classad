package collections

import "sync/atomic"

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
	failDeltaDecode       // a participant would not decode

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
	case failDeltaDecode:
		return "delta-decode"
	}
	return "unknown"
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
