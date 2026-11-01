package wasmudf

import (
	"math"

	"github.com/tetratelabs/wazero/api"

	"github.com/PelicanPlatform/classad/classad"
)

// The guest-facing names. They are prefixed rather than short because a guest may
// be a program with its own exports (a TinyGo or Rust module exports plenty), and
// a collision would be resolved silently in whichever direction the toolchain
// chose.
const (
	exportABIVersion = "classad_udf_abi_version"
	exportCall       = "classad_udf_call"
	exportMemory     = "memory"

	importModule = "classad_udf"
	importFuel   = "fuel"
)

// abiVersion is what a guest must report from exportABIVersion. It is checked at
// load, not per call, and it exists so that a change to the slot layout is a
// refusal naming two numbers rather than a guest reading fields at offsets the
// host no longer writes -- which would not fail, it would return wrong values.
const abiVersion = 1

// Slot geometry. See the package comment for the layout and why it is fixed width.
const (
	slotSize      = 16
	offTag        = 0
	offLen        = 4
	offPayload    = 8
	statusOK      = 0
	resultPoison  = ^uint32(0) // an impossible tag, written before every call
	payloadHiMask = uint64(0xFFFFFFFF00000000)
)

// Value tags. Their numbering is part of the ABI and may only be appended to.
const (
	tagUndefined uint32 = 0
	tagError     uint32 = 1
	tagBoolean   uint32 = 2
	tagInteger   uint32 = 3
	tagReal      uint32 = 4
	tagString    uint32 = 5
)

// frame is where one call's slots live inside the host's region.
type frame struct {
	args   uint32 // argc slots
	result uint32 // one slot
}

// writeArgs lays out one call frame in the host's region and returns the pointers
// to pass to the guest.
//
// It writes the arguments in order and appends any string bytes to an arena after
// the result slot, so an argument's slot and its bytes are both inside the region
// the host owns and the guest can address with a constant offset from argsPtr.
//
// The result slot is poisoned here rather than after the call for the obvious
// reason that after the call is too late, and rather than being left alone because
// an instance is REUSED across records: the bytes sitting in that slot are the
// previous record's answer, and a guest that returns success without writing would
// otherwise produce it again -- the same value, for a different record, with
// nothing anywhere reporting a problem.
func writeArgs(mem api.Memory, base, size uint32, args []classad.Value) (frame, Reason) {
	// In 64 bits, because (argc+1)*16 computed in uint32 WRAPS at 2^28 arguments
	// and a wrapped small total would pass this check and then write slots past
	// the end of the region. Not a shape any expression has, and not a bug worth
	// leaving in a bounds check.
	need := (uint64(len(args)) + 1) * slotSize
	if need > uint64(size) {
		return frame{}, ReasonArgsTooLarge
	}
	f := frame{args: base, result: base + uint32(len(args))*slotSize}

	// The arena grows up from just past the result slot; cursor is an absolute
	// offset in guest memory because that is what a slot payload has to hold.
	cursor := f.result + slotSize
	limit := base + size

	for i, v := range args {
		slot := f.args + uint32(i)*slotSize

		tag, payload, s, ok := encodeValue(v)
		if !ok {
			// A list or a nested ad. Refused here, BEFORE the guest runs, so the
			// reason distinguishes "we would not pass this" from anything the
			// guest did.
			return frame{}, ReasonUnsupportedArg
		}

		var length uint32
		if tag == tagString {
			if uint64(cursor)+uint64(len(s)) > uint64(limit) {
				return frame{}, ReasonArgsTooLarge
			}
			if s != "" && !mem.Write(cursor, []byte(s)) {
				// Unreachable while the region is inside the memory, which the
				// loader established by growing it; a write that fails anyway
				// means the memory shrank under us, and guessing is worse than
				// one ERROR.
				return frame{}, ReasonBadResult
			}
			length, payload = uint32(len(s)), uint64(cursor)
			cursor += length
		}

		if !mem.WriteUint32Le(slot+offTag, tag) ||
			!mem.WriteUint32Le(slot+offLen, length) ||
			!mem.WriteUint64Le(slot+offPayload, payload) {
			return frame{}, ReasonBadResult
		}
	}

	if !mem.WriteUint32Le(f.result+offTag, resultPoison) {
		return frame{}, ReasonBadResult
	}
	return f, ReasonNone
}

// encodeValue maps one ClassAd value onto its tag and payload, returning the
// string separately (the caller owns the arena it goes in, and the payload is the
// pointer into that arena) and ok=false for a value this ABI has no tag for.
//
// The accessors' errors are checked rather than discarded even though the
// predicate immediately above each one makes them impossible. The cost is a
// branch the compiler can see is never taken; the benefit is that if classad's
// value representation ever changes under this file, the result is ok=false --
// a refused argument -- rather than a zero payload encoded under a confident tag,
// which the guest would compute on and nothing would report.
func encodeValue(v classad.Value) (tag uint32, payload uint64, s string, ok bool) {
	switch {
	case v.IsUndefined():
		return tagUndefined, 0, "", true
	case v.IsError():
		return tagError, 0, "", true
	case v.IsBool():
		b, err := v.BoolValue()
		if err != nil {
			return 0, 0, "", false
		}
		if b {
			payload = 1
		}
		return tagBoolean, payload, "", true
	case v.IsInteger():
		n, err := v.IntValue()
		if err != nil {
			return 0, 0, "", false
		}
		return tagInteger, uint64(n), "", true
	case v.IsReal():
		r, err := v.RealValue()
		if err != nil {
			return 0, 0, "", false
		}
		return tagReal, math.Float64bits(r), "", true
	case v.IsString():
		str, err := v.StringValue()
		if err != nil {
			return 0, 0, "", false
		}
		// The payload is filled in by the caller once it has placed the bytes.
		return tagString, 0, str, true
	default:
		return 0, 0, "", false
	}
}

// readResult decodes the result slot. Every field is validated, because the slot
// is written by the guest and a guest is exactly as trustworthy as whoever built
// it: a bad tag, a pointer outside linear memory or a length over the cap each
// become ERROR rather than a panic, a wrong value, or a read of someone else's
// bytes.
func readResult(mem api.Memory, resPtr uint32, maxBytes int) (classad.Value, Reason) {
	tag, ok := mem.ReadUint32Le(resPtr + offTag)
	if !ok {
		return classad.NewErrorValue(), ReasonBadResult
	}
	length, ok := mem.ReadUint32Le(resPtr + offLen)
	if !ok {
		return classad.NewErrorValue(), ReasonBadResult
	}
	payload, ok := mem.ReadUint64Le(resPtr + offPayload)
	if !ok {
		return classad.NewErrorValue(), ReasonBadResult
	}

	switch tag {
	case tagUndefined:
		return classad.NewUndefinedValue(), ReasonNone
	case tagError:
		// The guest's own ERROR. Not a failure of the call: the UDF decided its
		// answer, so no reason is counted and no failure hook fires.
		return classad.NewErrorValue(), ReasonNone
	case tagBoolean:
		if payload > 1 {
			// A boolean is 0 or 1. Reading "nonzero means true" would accept a
			// guest that stored an int in a boolean slot, and the point of a
			// tagged slot is that the tag and the payload agree.
			return classad.NewErrorValue(), ReasonBadResult
		}
		return classad.NewBoolValue(payload == 1), ReasonNone
	case tagInteger:
		return classad.NewIntValue(int64(payload)), ReasonNone
	case tagReal:
		return classad.NewRealValue(math.Float64frombits(payload)), ReasonNone
	case tagString:
		if payload&payloadHiMask != 0 {
			// A wasm32 offset is 32 bits. Truncating a payload with bits above
			// that would read a DIFFERENT, in-bounds region and return a
			// plausible wrong string.
			return classad.NewErrorValue(), ReasonBadResult
		}
		if int(length) > maxBytes {
			return classad.NewErrorValue(), ReasonBadResult
		}
		if length == 0 {
			return classad.NewStringValue(""), ReasonNone
		}
		b, ok := mem.Read(uint32(payload), length)
		if !ok {
			return classad.NewErrorValue(), ReasonBadResult
		}
		// string(b) copies, which it must: b aliases guest memory and the next
		// call to this instance overwrites it.
		return classad.NewStringValue(string(b)), ReasonNone
	default:
		return classad.NewErrorValue(), ReasonBadResult
	}
}
