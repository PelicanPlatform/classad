package collections

import (
	"encoding/binary"
	"math"
	"math/bits"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// The tier-1 candidate set: one BIT per record, not one byte.
//
// Four scans share the same skeleton -- count, grouped stats, ungrouped stats, top-K. Each establishes
// which of a block's records a snapshot can see, narrows that set one predicate column at a time, and
// then does its own work for whatever survives. All four carried the set as a []bool, which put the
// representation at odds with the only kernels that could speed the middle step up: the comparison
// kernels deal in packed uint64 planes (see vm/simdcompare.go), so reaching one from a []bool meant a
// conversion at both ends of every pass, around a kernel that is a handful of instructions per 64
// records.
//
// Making the SET the bitplane removes that conversion, and it pays in all three steps rather than only
// in the one it was for:
//
//	visibility   one store per 64 records instead of 64 byte stores, and `live` is a popcount rather
//	             than an increment per record
//	narrowing    a comparison kernel writes its plane and the set intersects it with one AND per 64
//	             records -- no per-record read-modify-write of the set at all
//	survivors    a zero word skips 64 records in one test, and the loop visits set bits directly
//	             instead of testing every byte to find them
//
// None of that needs a SIMD build, and that is the point of doing it in this order. `rawSupported ==
// false` is the production build today, so a representation that only paid off under GOEXPERIMENT=simd
// would be a regression everywhere it actually runs. Measured on a 120k-record history fixture with no
// engine compiled in, the grouped query is ~22% faster and one narrowing pass over a four-byte column
// goes from 3.55 to 2.29 ns/record; WITH the engine that same pass is 0.48 ns/record, because the plane
// the kernel writes is now the set's own form and the engine drops into the hole left for it.

// candSet is one block's candidate records as a bitplane. Record k is a candidate iff bit k&63 of
// w[k>>6] is set; live is the number of set bits, maintained by the operations that change them so no
// caller has to recount.
type candSet struct {
	w    []uint64
	n    int
	live int
}

// reset sizes the set for a block of n records and clears it, reusing the backing array across blocks
// the way the []bool it replaces was reused.
func (c *candSet) reset(n int) {
	nw := (n + 63) / 64
	if cap(c.w) < nw {
		c.w = make([]uint64, nw)
	}
	c.w = c.w[:nw]
	clear(c.w)
	c.n, c.live = n, 0
}

// test reports whether record k is still a candidate.
func (c *candSet) test(k int) bool { return c.w[k>>6]&(1<<uint(k&63)) != 0 }

// and intersects the set with a plane, recounting as it goes. The plane must be at least as long as
// the set's words; bits past n are clear in both, so the tail cannot resurrect a record.
func (c *candSet) and(plane []uint64) {
	live := 0
	for i := range c.w {
		x := c.w[i] & plane[i]
		c.w[i] = x
		live += bits.OnesCount64(x)
	}
	c.live = live
}

// setVisible fills the set with the records of a block that snapshot s0 can see.
//
// This pass is never pruned: it runs over every record of every candidate block before any predicate
// has narrowed anything, so unlike the narrowing passes it cannot be made cheaper by a selective
// constraint. And it is the one pass whose reads are SCATTERED -- an offset table lookup, then two
// fields of that record's header, which sits a record-length away from the last one -- so it touches a
// cache line per record across the whole segment rather than walking a column.
//
// Nothing vectorizes a gather like that on the architectures here, so what this does instead is stop
// paying per record for things that are constant across the block: the offset table's length, the
// damage set's existence, the window's data slice, and the six-word copy of segWindow that calling its
// visible method made once per record. What is left in the body is the offset load, the two header
// loads and the bit.
func (c *candSet) setVisible(cs *colSegment, w segWindow, s0 uint64, base, n int) {
	c.reset(n)
	// A block can outrun the offset table after a partial reindex, and a record with no offset has no
	// header to read. Bounding the loop here is the hoisted form of the per-record `gk < cs.offsLen()`
	// the four scans each wrote.
	avail := cs.offsLen() - base
	if avail > n {
		avail = n
	}
	if avail <= 0 {
		return
	}
	offs := cs.offsB[base*4 : base*4+avail*4]
	data, dmg := w.data, w.dmg
	live := 0
	for i := range c.w {
		lo := i * 64
		if lo >= avail {
			break
		}
		hi := lo + 64
		if hi > avail {
			hi = avail
		}
		var word uint64
		for k := lo; k < hi; k++ {
			o := binary.LittleEndian.Uint32(offs[k*4:])
			// A damaged record is never visible: its seq and supersededBySeq are not to be believed.
			// The nil test is hoisted because a damage set is the exception, not the rule.
			if dmg != nil && dmg.covers(o) {
				continue
			}
			if recSeq(data, o) <= s0 && recSuperseded(data, o) > s0 {
				word |= 1 << uint(k-lo)
			}
		}
		c.w[i] = word
		live += bits.OnesCount64(word)
	}
	c.live = live
}

// predScratch is the per-query buffer a narrowing pass reuses from block to block: the widened column
// and two result planes. Two, because a field carrying more than one comparison intersects them, and
// allocating either per block would make a segment's worth of garbage out of buffers that are
// overwritten anyway.
type predScratch struct {
	ints  []int64
	plane []uint64
	alt   []uint64
}

// prepare sizes the scratch for a block of n records.
func (s *predScratch) prepare(n int) {
	if cap(s.ints) < n {
		s.ints = make([]int64, n)
	}
	s.ints = s.ints[:n]
	nw := (n + 63) / 64
	if cap(s.plane) < nw {
		s.plane = make([]uint64, nw)
		s.alt = make([]uint64, nw)
	}
	s.plane, s.alt = s.plane[:nw], s.alt[:nw]
}

// intTest is one comparison lowered into the column's own integer domain: the operator and the literal
// as an int64, ready for a kernel that never sees a float64.
type intTest struct {
	op  string
	lit int64
}

// lowerIntTests turns a field's comparisons into integer ones, or reports that they cannot be lowered.
//
// The scan compares float64s because that is what a ClassAd comparison is defined over, and the
// predicate arrives as a closure over float64. Lowering is sound only where the float64 comparison and
// the integer one cannot disagree, which needs two things.
//
// The VALUES must be exactly representable. An int column fitted to six bytes or fewer holds nothing
// past 2^47, well inside the integers float64 identifies individually, so float64(v) OP t orders the
// same way v does. An eight-byte column does not: two values past 2^53 can share a float64, and the
// existing path counts them as equal to a literal that an integer compare would reject. Lowering those
// would CHANGE answers, so it declines and they keep the float64 loop.
//
// The LITERAL must land on an integer boundary. An integral one lowers as itself; a fractional one
// lowers by moving the boundary to the nearest integer the column can actually hold, which is exact
// for the four ordering operators -- `v > 4.5` is `v > 4` over integers. Equality against a fractional
// literal has no such form (it is simply never true), and rather than carry a constant-false plane
// through the kernels it declines and lets the float64 loop answer it.
func lowerIntTests(tests []zoneTest) ([]intTest, bool) {
	out := make([]intTest, 0, len(tests))
	for _, t := range tests {
		if len(t.vals) != 1 {
			return nil, false
		}
		v := t.vals[0]
		// NaN compares false against everything, which no integer comparison expresses.
		if math.IsNaN(v) || v <= -(1<<62) || v >= 1<<62 {
			return nil, false
		}
		op, fl := t.op, math.Floor(v)
		if fl != v {
			switch op {
			case "<", "<=":
				op = "<=" // v < t and v <= t are both v <= floor(t) when t is not an integer
			case ">", ">=":
				op = ">" // and both of those are v > floor(t)
			default:
				return nil, false
			}
		}
		out = append(out, intTest{op: op, lit: int64(fl)})
	}
	return out, true
}

// comparePlane writes the plane of "record k's stored value satisfies test" for one numeric column,
// preferring the column's STORED width over a widened copy of it.
//
// Lanes come from width: a four-byte column is eight lanes of an AVX2 vector where int64 is four, and
// the two predicate columns a history dashboard asks about are four bytes and one. Widening to int64
// before the kernel sees anything throws those lanes away, which is why the raw form is tried first
// and the widened one is a fallback rather than the plan.
//
// Unlike the executor's own raw path this does NOT require the field to be escape-free. An escaped
// record's slot holds undefined bytes, so its plane bit is meaningless -- but it is also fixed up
// afterwards by the one caller, from the cold tail, and only for records still in the candidate set.
// Gating on escape-freedom instead would turn the raw form off for any field with a handful of odd
// records in the block, which at ~1000-record row groups and a 1% escape rate is nearly all of them.
//
// ok=false when the column will not load at all, which gives the field back to the per-record path.
func comparePlane(blk *columnarBlock, idx int, f adField, tests []intTest, bc *blockCache,
	sc *predScratch) ([]uint64, bool) {
	n := blk.n
	raw, rawOK := []byte(nil), false
	if vm.RawColumnUseful(f.width, f.unsigned) {
		raw, rawOK = blk.rawColumn(idx, bc)
	}
	widened := false
	for i, t := range tests {
		dst := sc.plane
		if i > 0 {
			dst = sc.alt
		}
		if !rawOK || !vm.CompareRawPlane(t.op, raw, f.width, f.unsigned, t.lit, n, dst) {
			// No engine for this width, or a literal that does not fit the column's type: widen
			// once and run the width-native scalar kernel over the int64s.
			if !widened {
				if !blk.loadIntBatch(idx, bc, sc.ints) {
					return nil, false
				}
				widened = true
			}
			if !vm.CompareIntsPlane(t.op, sc.ints, t.lit, n, dst) {
				return nil, false
			}
		}
		if i > 0 {
			for w := range sc.plane {
				sc.plane[w] &= sc.alt[w]
			}
		}
	}
	return sc.plane, true
}

// clear removes record k from the set, keeping live correct. For the one path that still decides
// record by record; everything else changes the set a word at a time.
func (c *candSet) clear(k int) {
	m := uint64(1) << uint(k&63)
	if c.w[k>>6]&m != 0 {
		c.w[k>>6] &^= m
		c.live--
	}
}
