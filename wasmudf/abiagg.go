package wasmudf

import (
	"bytes"

	"github.com/tetratelabs/wazero/api"

	"github.com/PelicanPlatform/classad/classad"
)

// The guest-facing names of the AGGREGATE ABI.
//
// They are a separate set from the scalar ones, under their own version, and one
// module exports one or the other. A function and an aggregate have nothing in
// common at the boundary except the value slot, and giving them one version
// number would mean a change to either forcing a rebuild of every module of both
// kinds -- for a layout half of them do not read.
const (
	exportAggVersion = "classad_udaf_abi_version"
	exportAggInit    = "classad_udaf_init"
	exportAggFold    = "classad_udaf_fold"
	exportAggFoldNum = "classad_udaf_fold_num"
	exportAggMerge   = "classad_udaf_merge"
	exportAggResult  = "classad_udaf_result"
)

// The aggregate ABI versions this host speaks, checked once at load like the
// scalar one and for the same reason: a layout read at offsets the host no longer
// writes does not fail, it computes.
const (
	aggABIVersion1 = 1

	aggABIVersionMin = aggABIVersion1
	aggABIVersionMax = aggABIVersion1
)

// THE STATE IS A BYTE RANGE THE HOST OWNS, which is the one decision everything
// else in this file follows from.
//
// The alternative is an opaque HANDLE: the guest allocates an accumulator in its
// own memory and the host passes an integer around. That is faster -- no state
// crosses the boundary per fold -- and it is what a guest author would write by
// hand. It was rejected because of what it does to lifetimes.
//
// A handle is only meaningful inside the instance that minted it, so an
// accumulator holding one PINS that instance for as long as the accumulator
// lives. The engine's accumulator interface is Add/Merge/Result: there is no
// close, so nothing tells the host when an accumulator is finished. Worse, there
// is no moment that could be inferred either -- when the columnar pass DECLINES,
// every worker's accumulator is dropped without Merge or Result ever being
// called, which is the common case rather than an edge one. So a handle's guest
// memory could only be reclaimed by a finalizer, and the instances it pins would
// be unavailable to other queries until the collector got to them.
//
// With the state in the host, an accumulator is an ordinary Go value holding a
// []byte. Instances are borrowed for the length of one guest call exactly as the
// scalar path borrows them, so MaxInstances keeps meaning "calls in flight"
// rather than "accumulators alive"; merging two accumulators is a pure function
// of two byte ranges, so it works whichever instances they were folded in; and
// the host can CAP the state, which it cannot do for memory the guest allocates
// behind a handle.
//
// What it costs is a copy in and a copy out per fold call, which is why a fold
// takes a BATCH of values rather than one: the copy is amortized over AggBatch
// values, and for the state sizes an aggregate actually has -- counters, a
// sketch -- it is a few hundred bytes against a thousand values.

// aggFrame is the fixed layout of one aggregate instance's region. It is computed
// once per instance rather than per call: nothing in it depends on the call.
//
//	stateA    one input state
//	stateB    the other input state, for merge
//	stateOut  where the guest writes the state it produced
//	result    one value slot, for finalize
//	vals      valCount slots, then the string arena
//
// Three state buffers rather than two, so that a guest never has to tolerate its
// output overlapping either input. Aliasing is the kind of thing a guest gets
// right on the fixture that was tested and wrong on a memcpy-shaped state. That
// one IS load-bearing: merge is handed both inputs at once.
//
// The result slot sitting apart from the values is not. No operation uses both --
// a fold has no result slot and a finalize has no values -- so they could share
// the same bytes and nothing would notice. They are kept apart because a layout
// whose correctness depends on which fields an operation happens to touch is a
// layout that breaks the first time an operation touches both.
type aggFrame struct {
	stateA, stateB, stateOut uint32
	result                   uint32
	vals                     uint32

	// stateCap is what each of the three state buffers holds; valsCap covers the
	// value slots AND the string arena after them.
	stateCap uint32
	valsCap  uint32
}

// aggRegionBytes is what one aggregate instance needs.
func aggRegionBytes(maxState, batch, strBytes int) int {
	return 3*maxState + slotSize + batch*slotSize + strBytes
}

func aggFrameAt(base uint32, maxState, batch, strBytes int) aggFrame {
	ms := uint32(maxState)
	f := aggFrame{
		stateA:   base,
		stateB:   base + ms,
		stateOut: base + 2*ms,
		result:   base + 3*ms,
		stateCap: ms,
		valsCap:  uint32(batch*slotSize + strBytes),
	}
	f.vals = f.result + slotSize
	return f
}

// aggVal is one buffered value in the ABI's own terms: the tag and payload that
// will be written into a slot, plus the string bytes if it is one.
//
// The accumulator buffers THESE rather than classad.Values, so that the numeric
// column path -- which has no classad.Value and would have to make one -- does
// not box, and so that the one value this ABI cannot carry is refused where it
// arrives rather than at the next flush.
type aggVal struct {
	tag     uint32
	payload uint64
	s       string
}

// encodeAggValue maps a ClassAd value onto a buffered value, ok=false for a list
// or a nested ad, which this ABI has no tag for.
func encodeAggValue(v classad.Value) (aggVal, bool) {
	tag, payload, s, ok := encodeValue(v)
	if !ok {
		return aggVal{}, false
	}
	return aggVal{tag: tag, payload: payload, s: s}, true
}

// writeAggValues lays out the value slots and their string bytes.
//
// The caller has already kept the buffer inside valsCap -- it flushes when the
// slots or the arena would overflow -- so a refusal here means a single value
// that cannot fit at all, which is a failure and not a reason to fold a prefix
// and lose the rest.
func writeAggValues(mem api.Memory, f aggFrame, vals []aggVal) Reason {
	need := uint64(len(vals)) * slotSize
	if need > uint64(f.valsCap) {
		return ReasonArgsTooLarge
	}
	cursor := f.vals + uint32(need)
	limit := f.vals + f.valsCap

	for i, v := range vals {
		slot := f.vals + uint32(i)*slotSize
		length, payload := uint32(0), v.payload
		if v.tag == tagString {
			if uint64(cursor)+uint64(len(v.s)) > uint64(limit) {
				return ReasonArgsTooLarge
			}
			if v.s != "" && !mem.Write(cursor, []byte(v.s)) {
				return ReasonBadResult
			}
			length, payload = uint32(len(v.s)), uint64(cursor)
			cursor += length
		}
		if !mem.WriteUint32Le(slot+offTag, v.tag) ||
			!mem.WriteUint32Le(slot+offLen, length) ||
			!mem.WriteUint64Le(slot+offPayload, payload) {
			return ReasonBadResult
		}
	}
	return ReasonNone
}

// writeAggState copies one state into the guest.
func writeAggState(mem api.Memory, at, capacity uint32, state []byte) Reason {
	if uint64(len(state)) > uint64(capacity) {
		return ReasonAggState
	}
	if len(state) > 0 && !mem.Write(at, state) {
		return ReasonBadResult
	}
	return ReasonNone
}

// readAggState copies back the state a guest reported writing, validating the
// length it claims.
//
// A NEGATIVE length is the guest declining or failing, and is kept apart from an
// oversized one: the first is the guest's own report, the second is the guest
// having written past a buffer whose size it was given. Collapsing them would
// leave an operator unable to tell a module that refuses its input from one that
// is scribbling.
func readAggState(mem api.Memory, at, capacity uint32, n int32) ([]byte, Reason) {
	if n < 0 {
		return nil, ReasonStatus
	}
	if uint64(n) > uint64(capacity) {
		return nil, ReasonAggState
	}
	if n == 0 {
		// A stateless aggregate -- one whose answer is a constant, or whose
		// state is empty until it meets a value it cares about -- is legal and
		// is not a failure.
		return nil, ReasonNone
	}
	b, ok := mem.Read(at, uint32(n))
	if !ok {
		return nil, ReasonBadResult
	}
	// Cloned, because b aliases guest memory and the next call to this instance
	// overwrites it -- and this state outlives the instance it was read from.
	return bytes.Clone(b), ReasonNone
}
