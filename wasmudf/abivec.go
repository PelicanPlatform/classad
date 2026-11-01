package wasmudf

import (
	"encoding/binary"

	"github.com/tetratelabs/wazero/api"
)

// The VECTOR calling convention: one call per batch instead of one per record.
//
// A guest that exports classad_udf_call_vec is handed whole columns and writes a
// whole column back:
//
//	classad_udf_call_vec(n, argsPtr, argc, resPtr, declinePtr) -> status
//
// argsPtr points at argc BATCH DESCRIPTORS, resPtr at one more, each 16 bytes:
//
//	offset  0  u32  form   0 = data (state bytes + i64 payloads), 1 = mask (bitplanes)
//	offset  4  u32  flags  bit 0 = const: only element 0 is meaningful
//	offset  8  u32  aPtr   data: n state bytes        mask: ceil(n/64) u64, the hi plane
//	offset 12  u32  bPtr   data: n i64 payloads       mask: ceil(n/64) u64, the lo plane
//
// The two pointers' meaning depends on form, which keeps a descriptor the same
// size as a scalar value slot and lets a guest index both the same way.
//
// WHY THESE AND NOT A NEW ENCODING. The engine already holds a column in exactly
// one of these shapes, and both are the shapes that vectorize: the data form is
// int64 lanes with a parallel byte lane of states, and the mask form is the two
// bitplanes a comparison already produces -- 2 bits per record, which a logical
// combine reads 64 records at a time. Translating either into something of this
// package's own invention would mean a pass over n elements before the guest got
// to do anything, for a batch the engine is handing over for free. See Batch for
// what is NOT carried (Raw, Dict, strings) and what that costs.
//
// Const is a FLAG, not n copies. A literal argument is the common case and the
// engine deliberately keeps it unmaterialized; expanding it here would pay the
// cost it exists to avoid, once per batch, to tell the guest the same thing n
// times.
//
// THE RESULT is always the data form. The engine's own call result is a data-form
// vector (callFuncVec writes one through setValueData), so a mask-form result
// would be converted immediately on arrival, and one form means one output path
// in every guest.
//
// THE DECLINE MASK is the escape hatch, and the reason a vectorized UDF does not
// have to be a complete UDF. declinePtr points at ceil(n/64) zeroed u64s; the
// guest sets bit i to say "I did not handle record i", and the host then runs
// that record through the scalar entry point. So a guest can implement the one
// typed path its data actually takes -- two integers, say -- and decline the rows
// with an UNDEFINED operand, a mixed type or anything else it would rather not
// express in branch-free form, without those rows getting a different answer than
// the scalar implementation would give them. They get the scalar implementation's
// answer, because they ARE run through it.
//
// What makes that safe is the poison. Every result element's state byte is
// statePoison before the call, and the host validates the state of every element
// the guest did NOT decline. So a guest that declines a record and also leaves
// garbage in its slot is fine (the slot is never read), and a guest that declines
// nothing and writes nothing is caught rather than returning the previous batch's
// column.
//
// The decline mask is trusted ONLY when the call returns status 0. A guest that
// trapped or ran out of steps was stopped at an arbitrary instruction, so the
// mask it left behind says nothing; the whole batch becomes ERROR and nothing is
// escalated. Escalating on the strength of a mask written by a guest that died
// would mean the same input produced a scalar answer or an ERROR depending on
// where it happened to die.
const (
	descSize    = 16
	offDescForm = 0
	offDescFlag = 4
	offDescA    = 8
	offDescB    = 12

	formData uint32 = 0
	formMask uint32 = 1

	flagConst uint32 = 1
)

// vecFrame is where one vector call's arrays live inside the host's region.
type vecFrame struct {
	args    uint32 // argc descriptors
	result  uint32 // one descriptor
	decline uint32 // ceil(n/64) u64 words

	// resSt and resI are the result arrays the host allocated and poisoned.
	//
	// They are kept HERE rather than re-read from the result descriptor after the
	// call, and that is not a micro-optimization. The descriptor lives in guest
	// memory, so a guest can rewrite it; a host that re-read it would follow the
	// new pointers to bytes it never poisoned, and a guest that wrote nothing
	// could have its output taken from any region of its own memory that happened
	// to hold legal state codes. The poison only guarantees anything if the host
	// reads the slots it poisoned.
	resSt, resI uint32
}

// align8 rounds up to the next multiple of 8, so every i64 array the guest reads
// is 8-byte aligned. wasm permits an unaligned i64.load, so this is a performance
// measure and not a correctness one -- removing it fails no test, deliberately
// noted here rather than left for someone to discover. It costs at most seven
// bytes per array and it keeps the hot loops off the unaligned path every engine
// has a slower route for.
func align8(x uint32) uint32 { return (x + 7) &^ 7 }

// vectorRegionBytes is what one vector call needs for n elements and argc
// arguments, assuming the worst case for every argument: the data form, not
// const, which is nine bytes per element against the mask form's quarter of one.
//
// It is computed rather than guessed because a region too small by one element
// turns the vector path off silently -- the batch declines, the engine falls back,
// and the only symptom is that it is slow.
func vectorRegionBytes(n, argc int) int {
	per := align8(uint32(n)) + align8(uint32(n)*8) // states, then payloads
	total := uint32(argc+1)*descSize +             // descriptors
		align8(uint32(maskWords(n))*8) + // decline mask
		uint32(argc+1)*per // arguments, then the result
	return int(total)
}

// writeBatchArgs lays out one vector call's frame and copies the columns in.
//
// It returns ok=false when the frame does not fit the region, which DECLINES the
// batch rather than failing it: a batch too large for the window is not a broken
// UDF, it is a configuration the engine can route around by using the scalar
// path. The caller counts it so that "the vector path is off" is visible rather
// than merely slow.
func writeBatchArgs(mem api.Memory, base, size uint32, n int, args []Batch) (vecFrame, bool) {
	var f vecFrame
	cursor := base
	limit := base + size

	// Descriptors first, at a known offset, so the guest's indexing is
	// argsPtr + k*16 with no table to walk.
	f.args = cursor
	cursor += uint32(len(args)) * descSize
	f.result = cursor
	cursor += descSize
	f.decline = align8(cursor)
	cursor = f.decline + align8(uint32(maskWords(n))*8)
	if cursor > limit {
		return f, false
	}

	// The decline mask must start clear. A bit left set from a previous batch
	// escalates a record the guest handled, which is not a wrong answer -- the
	// scalar path computes the same thing -- but it is a DIFFERENT path taken on
	// the strength of history, and a stale full mask turns the vector path into
	// the scalar path with a vector call in front of it.
	if !zeroRange(mem, f.decline, align8(uint32(maskWords(n))*8)) {
		return f, false
	}

	for i := range args {
		a := &args[i]
		desc := f.args + uint32(i)*descSize
		var err bool
		cursor, err = writeOneBatch(mem, desc, cursor, limit, n, a)
		if err {
			return f, false
		}
	}

	// The result: data form, host-allocated, poisoned.
	stPtr := cursor
	cursor = align8(stPtr + uint32(n))
	iPtr := cursor
	cursor += uint32(n) * 8
	if cursor > limit {
		return f, false
	}
	if !fillBytes(mem, stPtr, uint32(n), statePoison) {
		return f, false
	}
	if !writeDesc(mem, f.result, formData, 0, stPtr, iPtr) {
		return f, false
	}
	f.resSt, f.resI = stPtr, iPtr
	return f, true
}

// writeOneBatch copies one column into the region and writes its descriptor,
// returning the new cursor.
func writeOneBatch(mem api.Memory, desc, cursor, limit uint32, n int, b *Batch) (uint32, bool) {
	count := n
	flags := uint32(0)
	if b.Const {
		// One element, and the guest is told to read only that one. This is the
		// whole saving; writing n copies here would be the cost Vec.Const exists
		// to avoid, paid at the boundary instead of in the engine.
		count, flags = 1, flagConst
	}

	if b.Mask {
		words := uint32(maskWords(count)) * 8
		hiPtr := align8(cursor)
		loPtr := hiPtr + words
		end := loPtr + words
		if end > limit {
			return cursor, true
		}
		if !writeWords(mem, hiPtr, b.Hi[:maskWords(count)]) ||
			!writeWords(mem, loPtr, b.Lo[:maskWords(count)]) {
			return cursor, true
		}
		return end, !writeDesc(mem, desc, formMask, flags, hiPtr, loPtr)
	}

	stPtr := cursor
	iPtr := align8(stPtr + uint32(count))
	end := iPtr + uint32(count)*8
	if end > limit {
		return cursor, true
	}
	if !mem.Write(stPtr, b.St[:count]) || !writeInts(mem, iPtr, b.I[:count]) {
		return cursor, true
	}
	return end, !writeDesc(mem, desc, formData, flags, stPtr, iPtr)
}

func writeDesc(mem api.Memory, at, form, flags, aPtr, bPtr uint32) bool {
	return mem.WriteUint32Le(at+offDescForm, form) &&
		mem.WriteUint32Le(at+offDescFlag, flags) &&
		mem.WriteUint32Le(at+offDescA, aPtr) &&
		mem.WriteUint32Le(at+offDescB, bPtr)
}

// writeInts and writeWords copy a slice of 64-bit values into guest memory.
//
// They resolve the destination with one mem.Read, so the bounds check and the
// reslice happen once rather than per element, and then hand the copy to putInts
// -- which on a little-endian host is a memmove, because guest memory and a Go
// int64 slice are then the same bytes in the same order. See bulk.go.
func writeInts(mem api.Memory, at uint32, xs []int64) bool {
	dst, ok := mem.Read(at, uint32(len(xs))*8)
	if !ok {
		return false
	}
	putInts(dst, xs)
	return true
}

func writeWords(mem api.Memory, at uint32, xs []uint64) bool {
	dst, ok := mem.Read(at, uint32(len(xs))*8)
	if !ok {
		return false
	}
	putWords(dst, xs)
	return true
}

func zeroRange(mem api.Memory, at, size uint32) bool { return fillBytes(mem, at, size, 0) }

func fillBytes(mem api.Memory, at, size uint32, v byte) bool {
	dst, ok := mem.Read(at, size)
	if !ok {
		return false
	}
	for i := range dst {
		dst[i] = v
	}
	return true
}

// readDeclineMask copies the guest's decline bits out.
//
// Bits at or above n are MASKED OFF. Be honest about what that buys today:
// nothing observable. The escalation loop is bounded by n independently, so a
// guest setting bits in the last word that correspond to no record is already
// harmless, and a mutation removing this line fails no test.
//
// It stays because it makes in.declined mean exactly "the records this guest
// declined", which is the precondition for anything that ever reads the mask as a
// whole -- a popcount to report the escalation rate, or a caller handed the mask
// to make its own decision. The line is cheap; the invariant is the point.
func readDeclineMask(mem api.Memory, at uint32, n int, dst []uint64) bool {
	words := maskWords(n)
	src, ok := mem.Read(at, uint32(words)*8)
	if !ok {
		return false
	}
	for w := range words {
		dst[w] = binary.LittleEndian.Uint64(src[w*8:])
	}
	if rem := uint(n % 64); rem != 0 {
		dst[words-1] &= (uint64(1) << rem) - 1
	}
	return true
}

// readBatchResult copies the guest's result column into out, skipping every
// record the guest declined.
//
// Every element it does read is validated. A state the engine does not have --
// the poison it was given, or a number a guest invented -- is a malformed result
// for the whole batch rather than one bad element, because a guest that got one
// element's state wrong has no claim to the others.
//
// vsString and vsDictCode are refused even though they are real engine states:
// this ABI carries no string storage in either direction yet, so a guest claiming
// one has produced a value with nothing behind it. A call result is never
// dictionary-coded in the engine either (callFuncVec clears Dict unconditionally).
func readBatchResult(mem api.Memory, f vecFrame, n int, declined []uint64, out *Batch) bool {
	// f.resSt and f.resI, not the descriptor: see vecFrame. The result descriptor
	// is the host telling the guest where to write, not the guest telling the
	// host where to read.
	st, ok := mem.Read(f.resSt, uint32(n))
	if !ok {
		return false
	}
	payload, ok := mem.Read(f.resI, uint32(n)*8)
	if !ok {
		return false
	}

	for i := range n {
		if declined[i/64]&(uint64(1)<<uint(i%64)) != 0 {
			continue
		}
		s := st[i]
		if s > vsError {
			// Covers the poison (0xFF), vsString, vsDictCode and anything else.
			return false
		}
		v := int64(binary.LittleEndian.Uint64(payload[i*8:]))
		if s == vsBool && uint64(v) > 1 {
			// The same rule the scalar path applies to a boolean slot, and it has
			// to be the same: a vectorized module is two implementations of one
			// function, and the differential test between them is only worth
			// anything if the host holds both to the same contract. The engine
			// would read a nonzero payload as TRUE and the scalar path refuses it,
			// so without this a guest could get different answers from its own two
			// entry points for the same input.
			return false
		}
		out.St[i] = s
		out.I[i] = v
	}
	return true
}
