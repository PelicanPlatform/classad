package wasmudf

import (
	"encoding/binary"
	"math"
	"math/bits"

	"github.com/tetratelabs/wazero/api"
)

// The COLUMNAR form, ABI version 2: one state for the whole column.
//
// Version 1's data form carries a state byte beside every payload, so a column
// can hold an integer, a real, an UNDEFINED and a boolean in four consecutive
// records. That is honest about what a ClassAd column is, and it costs the guest
// a branch per element -- which is a branch that cannot be unrolled, cannot be
// hoisted and cannot be a vector lane. The measurement that matters is not the
// branch's own cost: it is that a loop with a per-element branch retires ONE
// record per back-edge, and a back-edge is what an interruptible runtime charges
// for. On an Apple M2 that charge is around 20ns, against roughly 0.6ns for the
// work. So the per-element state byte, which exists to describe the rare record,
// sets the price of every record.
//
// The columnar form moves that decision to the host. A column is given ONE state;
// the records whose value is not of that state are not represented at all -- the
// host sets their bit in the decline mask before the call, and they take the
// per-record scalar path they would have taken anyway. The guest reads the state
// once, outside its loop, and the loop that remains is branch-free over int64
// lanes: unrollable, and expressible as v128 if the guest wants to.
//
//	offset  0  u32  form   2
//	offset  4  u32  flags  bit 0 = const
//	offset  8  u32  aPtr   ONE state byte
//	offset 12  u32  bPtr   n i64 payloads (one, if const)
//
// It is the same descriptor as the data form with the state plane shortened to a
// single element, which is why it needs no new field and why a guest's two
// readers differ by where the load sits rather than by what it loads.
//
// WHO GETS IT. A guest reporting ABI version 2 is handed the columnar form for
// every data-form argument and never the per-element one; a guest reporting
// version 1 is handed exactly what it was handed before. The version decides,
// not the host and not the batch, because reading the wrong one of these two is
// undetectable: a version 1 guest handed a columnar column would read n state
// bytes from a one-byte state plane, in bounds, and return plausible numbers.
//
// WHAT IT COSTS. The host has to know the column is uniform before it can say so.
// That is not a new pass: version 1 already scans every state byte of every
// argument to find strings (carriesStrings), and this scan answers both questions
// at once. It is also a scan the CALLER can skip entirely by setting Batch.Uniform,
// which the query engine can do for free for the columns that arrive from a
// columnar block -- their states are written by the loader, not discovered.
//
// WHAT IT GIVES UP. Version 1 poisons n result state bytes, so a guest that fills
// half the column and declines nothing is caught at the first unwritten element.
// The columnar result has ONE state byte to poison, so that guest is caught only
// if it also forgot the state. In exchange the host zeroes the result payloads
// before every call, which makes the un-written tail deterministic zeros rather
// than the previous batch's answers. A smaller net, and a stale-value leak closed
// instead.
const (
	formCol uint32 = 2

	// colStatePoison is written over the result's single state byte. Same role as
	// statePoison in the data form and the same value, kept distinct in name
	// because there is one of it rather than n.
	colStatePoison = statePoison
)

// colPlan is the host's decision about one argument column for a version 2 call.
type colPlan struct {
	mask  bool  // the column goes as form 1, untouched
	state uint8 // the single state the columnar form declares
}

// planColumns decides the form of every argument and fills in.hostDeclined with
// the records that have to be escalated for the columns not to be a lie.
//
// It returns false when a column holds a string or a dictionary code, which
// declines the batch exactly as carriesStrings does on the version 1 path: a
// string column is strings for every record, so there is no uniform state to give
// it and escalating record by record would be the per-record path with a vector
// call in front of it. It also declines a column holding a state code that is not
// a state at all, which is a malformed batch rather than an awkward one and is
// counted as one -- an operator sent looking for strings by a column that has
// none would be looking for the wrong thing.
//
// It counts the failure itself, because the reason depends on what the scan
// found and the caller cannot see that.
func (u *UDF) planColumns(in *instance, n int, args []Batch) bool {
	words := maskWords(n)
	clear(in.hostDeclined[:words])
	if cap(in.plan) < len(args) {
		in.plan = make([]colPlan, len(args))
	}
	in.plan = in.plan[:len(args)]

	for i := range args {
		a := &args[i]
		if a.Mask {
			// Already uniform, and already two bits per record. A mask column has
			// no state plane to collapse and nothing to escalate: its four states
			// ARE its values.
			in.plan[i] = colPlan{mask: true}
			continue
		}
		state, r := a.columnState(n, in.colExceptions)
		if r != ReasonNone {
			u.fail(r, nil)
			return false
		}
		in.plan[i] = colPlan{state: state}
		if a.Const {
			continue
		}
		for w := range words {
			in.hostDeclined[w] |= in.colExceptions[w]
		}
	}
	// No bits past the last record have to be cleared here: scanExceptions walks
	// the n state bytes and sets nothing outside them, so the last word's spare
	// bits are the zeros clear() left. Masking them again would be a line that
	// cannot change the answer.
	return true
}

// columnState picks the one state a data-form column will be given and writes the
// bits of the records that do not have it into exc.
//
// Both passes here are eight records at a time, because the host paying per
// element to spare the guest paying per element would be moving the cost rather
// than removing it. The first compares the column against a broadcast of the
// first element's state and stops at the first word that differs; a uniform
// column -- which is what a numeric column off a columnar block with no escaped
// values is -- never leaves that loop. A mixed one falls into the same loop
// recording exceptions, and only a column whose first element is in a MINORITY
// state needs the histogram at the bottom.
func (b *Batch) columnState(n int, exc []uint64) (uint8, Reason) {
	words := maskWords(n)
	clear(exc[:words])
	if n == 0 {
		return vsUndef, ReasonNone
	}
	if b.Const {
		// One element and every record has it, so there is nothing to scan and
		// nothing can be an exception.
		return b.St[0], stateCarried(b.St[0])
	}
	if b.Uniform {
		// The caller asserts it. Nothing is scanned, which is the point: a column
		// that came off a columnar block had its states WRITTEN by the loader
		// rather than discovered, so proving what the loader just did would be the
		// only pass over the column the whole call makes.
		return b.State, stateCarried(b.State)
	}

	st := b.St[:n]
	count, r := scanExceptions(st, st[0], exc)
	if r != ReasonNone {
		return 0, r
	}
	if count*2 <= n {
		// The first element's state covers at least half the column, so nothing
		// else can cover more of it. The exceptions are already recorded.
		return st[0], ReasonNone
	}

	// The first element was in a minority state, so the mask just built would
	// escalate most of the column. Count what is actually there and build it
	// again against the state that covers the most records. A single UNDEFINED at
	// the head of a column of integers is the case this exists for, and it is not
	// hypothetical: it is a record whose attribute is missing.
	// Indexed by the state byte, so it is as wide as a byte can be: a state this
	// package has no name for cannot reach here, because scanExceptions refused
	// the column, but an array sized to the states it knows about would turn that
	// reasoning into a panic the day it stopped holding. It also removes the
	// bounds check from the loop.
	var counts [256]int
	for _, s := range st {
		counts[s]++
	}
	best := uint8(0)
	for s := uint8(1); s <= vsError; s++ {
		if counts[s] > counts[best] {
			best = s
		}
	}
	clear(exc[:words])
	if _, r := scanExceptions(st, best, exc); r != ReasonNone {
		return 0, r
	}
	return best, ReasonNone
}

// stateCarried says whether a state can cross into a guest, and why not when it
// cannot. A string or a dictionary code declines the batch; anything else is a
// number that is not a state, which is a caller that built the column wrong.
func stateCarried(s uint8) Reason {
	switch {
	case s <= vsError:
		return ReasonNone
	case s <= vsDictCode:
		return ReasonBatchStrings
	default:
		return ReasonBatchShape
	}
}

// scanExceptions records the elements of st that are not in state s, and reports
// how many there were.
//
// ok=false when the column holds a state this ABI does not carry -- a string, a
// dictionary code, or a number that is not a state at all. Every byte is covered
// by that check without being tested individually: a byte either equals s, which
// is checked once before the loop, or it is an exception and is checked as one.
//
// Eight at a time: an XOR against a broadcast of s is zero exactly when all eight
// match, so a column that is uniform, or uniform in stretches, is one load and
// one compare per eight records. Only a word that differs is taken apart.
func scanExceptions(st []uint8, s uint8, exc []uint64) (int, Reason) {
	if r := stateCarried(s); r != ReasonNone {
		return 0, r
	}
	broadcast := uint64(s) * 0x0101010101010101
	count := 0
	i := 0
	for ; i+8 <= len(st); i += 8 {
		if binary.LittleEndian.Uint64(st[i:])^broadcast == 0 {
			continue
		}
		for k := range 8 {
			c, r := recordException(st, i+k, s, exc)
			if r != ReasonNone {
				return 0, r
			}
			count += c
		}
	}
	for ; i < len(st); i++ {
		c, r := recordException(st, i, s, exc)
		if r != ReasonNone {
			return 0, r
		}
		count += c
	}
	return count, ReasonNone
}

func recordException(st []uint8, i int, s uint8, exc []uint64) (int, Reason) {
	c := st[i]
	if c == s {
		return 0, ReasonNone
	}
	if r := stateCarried(c); r != ReasonNone {
		return 0, r
	}
	exc[i/64] |= uint64(1) << uint(i%64)
	return 1, ReasonNone
}

// colRegionBytes is what one version 2 vector call needs for n elements and argc
// arguments, assuming the worst case for every argument: the columnar form, not
// const, which is eight bytes per element against the data form's nine.
//
// It is never larger than vectorRegionBytes, so a region sized at load for a
// version 1 guest holds a version 2 frame as well. Written out rather than left
// implicit because the load-time sizing happens before the guest has reported its
// version, and the direction of that inequality is what makes that safe.
func colRegionBytes(n, argc int) int {
	// One state byte, padded so the payloads that follow it are 8-byte aligned,
	// then the payloads. The result has the same shape as an argument.
	per := 8 + align8(uint32(n)*8)
	total := uint32(argc+1)*descSize +
		align8(uint32(maskWords(n))*8) + // decline mask
		uint32(argc+1)*per // the arguments, then the result
	return int(total)
}

// writeColBatchArgs lays out one version 2 vector call's frame.
//
// It differs from writeBatchArgs in three ways, all of them the columnar form:
// a data argument crosses as payloads alone, the decline mask starts at the
// host's own declines rather than at zero, and the result has one state byte
// instead of n.
func writeColBatchArgs(mem api.Memory, base, size uint32, n int, args []Batch, plan []colPlan, hostDeclined []uint64) (vecFrame, bool) {
	var f vecFrame
	cursor := base
	limit := base + size
	words := maskWords(n)

	f.args = cursor
	cursor += uint32(len(args)) * descSize
	f.result = cursor
	cursor += descSize
	f.decline = align8(cursor)
	cursor = f.decline + align8(uint32(words)*8)
	if cursor > limit {
		return f, false
	}
	// The mask the guest is given is the host's declines, not zero. A guest may
	// ignore it and compute every lane -- the records in it are never read back --
	// but a guest that reads it can skip work, and one that would trap on a value
	// it was told nothing about can avoid it.
	if !writeWordsAt(mem, f.decline, hostDeclined[:words]) {
		return f, false
	}

	for i := range args {
		a := &args[i]
		desc := f.args + uint32(i)*descSize
		var bad bool
		if plan[i].mask {
			cursor, bad = writeOneBatch(mem, desc, cursor, limit, n, a)
		} else {
			cursor, bad = writeColumn(mem, desc, cursor, limit, n, a, plan[i].state, hostDeclined)
		}
		if bad {
			return f, false
		}
	}

	// The result: one state byte, poisoned, then n payloads, zeroed.
	stPtr := cursor
	cursor = align8(stPtr + 1)
	iPtr := cursor
	cursor += uint32(n) * 8
	if cursor > limit {
		return f, false
	}
	if !mem.WriteByte(stPtr, colStatePoison) {
		return f, false
	}
	// Zeroing, where the data form poisons. There is one state byte to poison
	// here, so a guest that writes the state and then fills only part of the
	// column is not caught by it; zeroing at least makes what it did not write
	// deterministic rather than the previous batch's answers for other records.
	if dst, ok := mem.Read(iPtr, uint32(n)*8); ok {
		clear(dst)
	} else {
		return f, false
	}
	if !writeDesc(mem, f.result, formCol, 0, stPtr, iPtr) {
		return f, false
	}
	f.resSt, f.resI = stPtr, iPtr
	return f, true
}

// writeColumn copies one data column's payloads across and writes its descriptor.
//
// Declined lanes are overwritten with a benign value of the column's declared
// state. They hold whatever the engine left there -- a real's bit pattern in an
// integer column, say -- and the host never reads them back, so the value is
// arbitrary; what it must not be is one a branch-free guest can trap on. An
// integer lane of 1 divides, a real lane of 1.0 divides, and that is the whole
// requirement. Without it, "ignore the mask and compute every lane" would be
// advice that works until a guest divides.
func writeColumn(mem api.Memory, desc, cursor, limit uint32, n int, b *Batch, state uint8, declined []uint64) (uint32, bool) {
	count := n
	flags := uint32(0)
	if b.Const {
		count, flags = 1, flagConst
	}
	stPtr := cursor
	iPtr := align8(stPtr + 1)
	end := iPtr + uint32(count)*8
	if end > limit {
		return cursor, true
	}
	if !mem.WriteByte(stPtr, state) || !writeInts(mem, iPtr, b.I[:count]) {
		return cursor, true
	}
	if !b.Const {
		if !fillDeclinedLanes(mem, iPtr, n, state, declined) {
			return cursor, true
		}
	}
	return end, !writeDesc(mem, desc, formCol, flags, stPtr, iPtr)
}

// fillDeclinedLanes writes the benign filler into the lanes the host has already
// declined. It walks set bits rather than all n records, because the point of the
// columnar form is that there are few of them.
func fillDeclinedLanes(mem api.Memory, at uint32, n int, state uint8, declined []uint64) bool {
	var filler uint64
	switch state {
	case vsInt:
		filler = 1
	case vsReal:
		filler = math.Float64bits(1)
	}
	dst, ok := mem.Read(at, uint32(n)*8)
	if !ok {
		return false
	}
	for w, w64 := range declined[:maskWords(n)] {
		for w64 != 0 {
			i := w*64 + bits.TrailingZeros64(w64)
			w64 &= w64 - 1
			if i >= n {
				break
			}
			binary.LittleEndian.PutUint64(dst[i*8:], filler)
		}
	}
	return true
}

// readColBatchResult copies a columnar result column out.
//
// The validation that version 1 does per element is done once: one state byte,
// read from the host's own pointer rather than the descriptor's, checked against
// the states a value can have. The payloads are then a copy, which is the saving.
func readColBatchResult(mem api.Memory, f vecFrame, n int, declined []uint64, out *Batch) bool {
	if allDeclined(declined, n) {
		// Nothing was handled, so there is no column and no state to describe it.
		// Requiring one here would make "I handled none of these" -- which the
		// version 1 path accepts, since it reads no element -- a malformed result
		// instead, and turn a batch every record of which has a correct scalar
		// answer into ERROR.
		return true
	}
	raw, ok := mem.Read(f.resSt, 1)
	if !ok {
		return false
	}
	s := raw[0]
	if s > vsError {
		// The poison, a string state, or a number the guest invented.
		return false
	}
	payload, ok := mem.Read(f.resI, uint32(n)*8)
	if !ok {
		return false
	}
	if s == vsBool && !boolLanesValid(payload, n, declined) {
		return false
	}
	getInts(out.I, payload, n)
	fillStates(out.St, n, s)
	return true
}

// boolLanesValid holds a boolean column to the same 0-or-1 rule the scalar slot
// is held to, because a vectorized module is two implementations of one function
// and the engine would read any nonzero payload as TRUE while the scalar path
// calls it malformed.
//
// The fast pass ORs every lane together, which needs no branch and no mask: the
// accumulator exceeds 1 exactly when some lane does. Only then does it matter
// which lane, and only then is the declined set consulted -- the host zeroed the
// column before the call, so a lane the guest never wrote contributes nothing.
func boolLanesValid(payload []byte, n int, declined []uint64) bool {
	var acc uint64
	for i := range n {
		acc |= binary.LittleEndian.Uint64(payload[i*8:])
	}
	if acc <= 1 {
		return true
	}
	for i := range n {
		if declined[i/64]&(uint64(1)<<uint(i%64)) != 0 {
			continue
		}
		if binary.LittleEndian.Uint64(payload[i*8:]) > 1 {
			return false
		}
	}
	return true
}

func writeWordsAt(mem api.Memory, at uint32, xs []uint64) bool {
	dst, ok := mem.Read(at, uint32(len(xs))*8)
	if !ok {
		return false
	}
	putWords(dst, xs)
	return true
}

// allDeclined reports whether every one of the n records is in the mask.
func allDeclined(declined []uint64, n int) bool {
	full := n / 64
	for w := range full {
		if declined[w] != ^uint64(0) {
			return false
		}
	}
	if rem := uint(n % 64); rem != 0 {
		return declined[full] == (uint64(1)<<rem)-1
	}
	return true
}
