package wasmudf

import (
	"math"

	"github.com/PelicanPlatform/classad/classad"
)

// Batch is one column of values in the vector engine's own representation.
//
// Its fields are named and shaped to match collections/vm's Vec field for field,
// so an adapter on that side is an assignment and not a conversion:
//
//	Batch{N: v.Len(), Mask: v.Mask, Const: v.Const, St: v.St, I: v.I, Hi: v.Hi, Lo: v.Lo}
//
// The slices ALIAS the engine's vectors; nothing here copies them, and nothing
// here retains them past the call. That is the point. The engine pools its
// vectors across batches and queries precisely so a scan does not allocate per
// block, and an ABI that took a copy of every column on the way in would hand
// that back.
//
// It is declared here rather than imported from collections/vm because the
// dependency can only run one way: collections imports classad, this module
// imports classad, and the engine is the CALLER. An import of collections/vm
// here would be a cycle the moment the engine called into this package. The cost
// is that the two declarations have to agree, which TestVectorStatesMatchEngine
// turns from a convention into a test.
//
// NOT REPRESENTED, deliberately:
//
//   - Raw (a column in its stored width). It is a transient load-time
//     optimization whose shape -- 2, 4 or 8 bytes, signed or unsigned, integer
//     bits or float bits -- is a property of the block on disk. Exposing it would
//     put four code paths in every guest and break them all the next time the
//     block format changes. The engine already collapses Raw to I on the first
//     use that is not specialized for it, and a call into a guest is one; the
//     cost is the widening pass loadIntBatch used to do unconditionally, paid now
//     only by columns that reach a UDF.
//
//   - Dict (dictionary codes plus a block-wide entry list). Carrying it would mean
//     serializing a dictionary into guest memory per batch to keep the codes
//     meaningful; materializing it instead means a string per record, which is
//     what the dictionary exists to avoid. Both are real costs and neither is
//     obviously right, so this increment carries neither: a dictionary-coded or
//     plain-string argument DECLINES the batch and the engine's elementwise path
//     serves it. Declining up front rather than per record is not a shortcut --
//     a string column is strings for every record, so escalating them one at a
//     time would be the elementwise path with a vector call in front of it.
//
// There is no "absent" or "not present" state, here or in the engine. An
// attribute a record does not have arrives as UNDEFINED -- that is what
// LoadColumn writes for a non-member of a group schema, and what the mask form's
// mUndef means. Adding a third bitplane to say "absent" would be inventing a
// distinction the evaluator does not make.
type Batch struct {
	// N is the element count. For a Const batch only element 0 is meaningful,
	// but N is still the record count, because that is what the result has to be.
	N int

	// Mask selects the form. False: St and I hold the values, one element each.
	// True: Hi and Lo are the two bitplanes, two bits per record.
	Mask bool

	// Const marks a batch every record of which holds the same value, kept once
	// in element 0 with the rest left as garbage.
	//
	// It is passed THROUGH to the guest as a flag rather than materialized. A
	// literal argument is the dominant case, and writing nine identical bytes per
	// record to say the same thing n times is the cost Vec.Const was introduced
	// to remove -- 9.8% of a scan in the profile that found it. Re-materializing
	// it at the ABI boundary would put that straight back.
	Const bool

	// St and I are the DATA form: a state code per element and an int64 payload
	// per element. A real's payload is its IEEE-754 bits, which is how both
	// classad.Value and the columnar block already store one.
	St []uint8
	I  []int64

	// Hi and Lo are the MASK form: three-valued logic as two bitplanes.
	Hi, Lo []uint64

	// Uniform asserts that every one of the N elements is in state State, so the
	// host does not have to look. It is a DECLARATION, not a hint: nothing
	// verifies it, and a batch that sets it wrongly hands the guest a column
	// described as something it is not.
	//
	// It is here because for the columns that matter the answer is already known
	// rather than discoverable. A numeric column loaded from a columnar block with
	// no escaped values has its state written across it by the loader, from the
	// field's declared kind; a boolean one arrives as a mask. Proving afterwards
	// what the loader just did would be the only pass over the column the whole
	// call makes.
	//
	// Left false, the host scans, which is a pass it was making anyway on the
	// version 1 path to find strings. So this buys the scan back, it does not
	// unlock the form.
	Uniform bool

	// State is the state every element is in when Uniform is set, using the same
	// codes as St. Ignored otherwise.
	State uint8
}

// Vector element states. These numbers are the ABI, and they are the engine's
// numbers: collections/vm's VsInt..VsDictCode, in that order, from the same iota.
//
// Mirroring them rather than converting is what makes the data form pass through
// without a translation pass over n elements. It also means a renumbering on
// either side is a silent wrong answer -- a real read as an integer -- which is
// why TestVectorStatesMatchEngine reads the engine's source and checks.
const (
	vsInt      uint8 = 0 // I is the integer
	vsReal     uint8 = 1 // I is math.Float64bits of the real
	vsBool     uint8 = 2 // I is 0 or 1
	vsUndef    uint8 = 3 // UNDEFINED
	vsError    uint8 = 4 // ERROR
	vsString   uint8 = 5 // the engine keeps the string beside I; not carried here
	vsDictCode uint8 = 6 // I is a dictionary code; not carried here

	// statePoison is written over every result element before a vector call. It
	// is not a state the engine has, so a guest that leaves it there has not
	// written that element -- see readBatchResult.
	statePoison uint8 = 0xFF
)

// Mask states, as (hi, lo) bit pairs. The engine's mFalse/mTrue/mUndef/mError.
const (
	mkFalse = 0 // 00
	mkTrue  = 1 // 01
	mkUndef = 2 // 10
	mkError = 3 // 11
)

func maskWords(n int) int { return (n + 63) / 64 }

// value boxes element i, from whichever form the batch holds.
//
// This is the host-side read, used to build the scalar frame for a record the
// guest declined. It is deliberately the ONLY way a declined record's arguments
// are obtained: they come from the caller's batch, not from the copy in guest
// memory, so an escalation cannot be fed values a guest edited after the host
// wrote them.
func (b *Batch) value(i int) classad.Value {
	if b.Const {
		i = 0
	}
	if b.Mask {
		switch maskStateAt(b.Hi, b.Lo, i) {
		case mkTrue:
			return classad.NewBoolValue(true)
		case mkFalse:
			return classad.NewBoolValue(false)
		case mkUndef:
			return classad.NewUndefinedValue()
		default:
			return classad.NewErrorValue()
		}
	}
	switch b.St[i] {
	case vsInt:
		return classad.NewIntValue(b.I[i])
	case vsReal:
		return classad.NewRealValue(math.Float64frombits(uint64(b.I[i])))
	case vsBool:
		return classad.NewBoolValue(b.I[i] != 0)
	case vsUndef:
		return classad.NewUndefinedValue()
	default:
		// vsError, and the states this ABI does not carry. A string argument has
		// already declined the batch by the time anything reads an element, so
		// reaching here with vsString means the caller lied about its batch; ERROR
		// is the safe reading, and it is a value rather than a crash.
		return classad.NewErrorValue()
	}
}

// setValue writes a scalar result into element i of a data-form batch, which is
// how an escalated record's answer gets back into the output column.
//
// ok=false for a value a vector cannot hold -- a string, a list, a nested ad --
// which makes the whole batch decline rather than carry a truncated element. That
// is the engine's own rule for a call result (setValueData), kept identical here
// so a UDF cannot produce a batch the engine would have refused.
func (b *Batch) setValue(i int, v classad.Value) bool {
	switch {
	case v.IsInteger():
		n, err := v.IntValue()
		if err != nil {
			return false
		}
		b.St[i], b.I[i] = vsInt, n
	case v.IsReal():
		f, err := v.RealValue()
		if err != nil {
			return false
		}
		b.St[i], b.I[i] = vsReal, int64(math.Float64bits(f))
	case v.IsBool():
		x, err := v.BoolValue()
		if err != nil {
			return false
		}
		b.St[i] = vsBool
		if x {
			b.I[i] = 1
		} else {
			b.I[i] = 0
		}
	case v.IsUndefined():
		b.St[i], b.I[i] = vsUndef, 0
	case v.IsError():
		b.St[i], b.I[i] = vsError, 0
	default:
		return false
	}
	return true
}

// maskStateAt reads one record's two bits, the same way the engine's function of
// the same name does.
func maskStateAt(hi, lo []uint64, i int) int {
	w, bit := i/64, uint(i%64)
	return int((hi[w]>>bit)&1)<<1 | int((lo[w]>>bit)&1)
}

// valid reports whether a batch is self-consistent enough to read N elements
// from, so a malformed one is refused at the boundary instead of panicking
// somewhere inside a loop.
//
// A Const batch needs only element 0, which is why its length requirement is 1
// and not N: that is the whole saving, and demanding full-length backing arrays
// for it would quietly undo it.
func (b *Batch) valid() bool {
	if b.N < 0 {
		return false
	}
	need := b.N
	if b.Const {
		need = 1
	}
	if b.Mask {
		// A mask batch still needs whole words, Const or not: the bits for
		// element 0 live in word 0 either way.
		w := maskWords(need)
		return len(b.Hi) >= w && len(b.Lo) >= w
	}
	return len(b.St) >= need && len(b.I) >= need
}

// carriesStrings reports whether any element is in a state this ABI does not
// carry into a guest, which declines the batch.
//
// It scans, because a column's state is per element: a dictionary column is
// uniformly coded but a mixed one -- a block with escaped values -- is strings
// for some records and UNDEFINED for others, so the only honest test is to look.
// The scan is one byte per element with no branch mispredicts to speak of, and it
// happens once per batch per argument rather than once per record.
func (b *Batch) carriesStrings() bool {
	if b.Mask {
		return false // the mask form has no string state
	}
	n := b.N
	if b.Const {
		n = 1
	}
	for i := range n {
		if b.St[i] == vsString || b.St[i] == vsDictCode {
			return true
		}
	}
	return false
}
