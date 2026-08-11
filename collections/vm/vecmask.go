package vm

import (
	"math"
	"math/bits"
)

// The MASK form of a vector: three-valued logic as two bitplanes, two bits per record.
//
//	hi lo
//	 0  0  FALSE      0  1  TRUE      1  0  UNDEFINED      1  1  ERROR
//
// A boolean element cost nine bytes in the data form (a state byte plus an eight-byte payload) and every
// logical operator walked it with a branch per record, which measured as expensive as a comparison that
// loads and compares int64s -- for an operation that only merges booleans. In this form a combine is a
// fixed sequence of word-wise ANDs and ORs over 64 records at a time, with no branch, and its cost no
// longer depends on how many records were missing an attribute.
//
// SHAPED FOR SIMD, SCALAR INSIDE. These are the operations AVX-512 performs natively -- a comparison
// RETURNS a mask (Int64x8.Greater gives Mask64x8), Mask64x8.And is the combine, ToBits feeds a popcount,
// and Int64x8.Compress is the selection-pushdown primitive -- so widening these loops later replaces
// their bodies and nothing above them. Everything here is ordinary uint64 arithmetic and runs on every
// architecture today; simd/archsimd in Go 1.26 is amd64-only and behind GOEXPERIMENT.
//
// The tables are LEFT-BIASED, not symmetric: `ERROR && FALSE` is ERROR while `FALSE && ERROR` is FALSE.
// That is short-circuit semantics made total -- the left operand decides where it can -- and asymmetry
// costs a few word operations and nothing else. Every table here is verified against the tree-walking
// evaluator over all sixteen state pairs, because a combine that quietly symmetrised them would still
// pass any corpus that never put an ERROR on the left.

// Mask state encodings, as (hi, lo) bit pairs.
const (
	mFalse = 0 // 00
	mTrue  = 1 // 01
	mUndef = 2 // 10
	mError = 3 // 11
)

func maskWords(n int) int { return (n + 63) / 64 }

// planeAnd is ClassAd &&:
//
//	ERROR && anything = ERROR      FALSE && anything = FALSE
//	TRUE  && y        = y          UNDEF && FALSE    = FALSE
//	                               UNDEF && ERROR    = ERROR, else UNDEFINED
//
// FALSE is 00, so it needs no term: whatever is none of ERROR, TRUE or UNDEFINED falls out as zero in
// both planes.
func planeAnd(xhi, xlo, yhi, ylo, ohi, olo []uint64) {
	for w := range ohi {
		xh, xl, yh, yl := xhi[w], xlo[w], yhi[w], ylo[w]
		if xh|yh == 0 {
			// Neither operand has an UNDEFINED or ERROR anywhere in these 64 records, so the whole
			// three-valued apparatus collapses to one AND. One branch per 64 records, and it is the
			// common case: a comparison over an escape-free column yields only TRUE or FALSE.
			ohi[w], olo[w] = 0, xl&yl
			continue
		}
		isTx, isUx, isEx := ^xh&xl, xh&^xl, xh&xl
		isTy, isUy, isEy := ^yh&yl, yh&^yl, yh&yl
		hands := isTx | isUx // the left operand does not settle it alone
		resE := isEx | (hands & isEy)
		resT := isTx & isTy
		resU := (isTx & isUy) | (isUx & (isTy | isUy))
		ohi[w], olo[w] = resU|resE, resT|resE
	}
}

// planeOr is ClassAd ||, left-biased the same way:
//
//	ERROR || anything = ERROR      TRUE  || anything = TRUE
//	FALSE || y        = y          UNDEF || TRUE     = TRUE
//	                               UNDEF || ERROR    = ERROR, else UNDEFINED
func planeOr(xhi, xlo, yhi, ylo, ohi, olo []uint64) {
	for w := range ohi {
		xh, xl, yh, yl := xhi[w], xlo[w], yhi[w], ylo[w]
		if xh|yh == 0 {
			ohi[w], olo[w] = 0, xl|yl
			continue
		}
		isFx, isTx, isUx, isEx := ^xh&^xl, ^xh&xl, xh&^xl, xh&xl
		isFy, isTy, isUy, isEy := ^yh&^yl, ^yh&yl, yh&^yl, yh&yl
		hands := isFx | isUx
		resE := isEx | (hands & isEy)
		resT := isTx | (hands & isTy)
		resU := (isFx & isUy) | (isUx & (isFy | isUy))
		ohi[w], olo[w] = resU|resE, resT|resE
	}
}

// planeNot is ClassAd !: TRUE and FALSE swap, UNDEFINED and ERROR pass through. The hi plane is
// unchanged, which is what makes it two operations per word.
func planeNot(hi, lo, ohi, olo []uint64) {
	for w := range ohi {
		h, l := hi[w], lo[w]
		ohi[w], olo[w] = h, (^h&^l)|(h&l)
	}
}

// planeCountTrue counts records whose result is TRUE and that live has set: one popcount per 64 records.
//
// math/bits.OnesCount64 already lowers to a single instruction on both amd64 and arm64 and measures about
// half a nanosecond per WORD, so there is nothing here for a vectorized popcount to improve at row-group
// scale.
func planeCountTrue(hi, lo, live []uint64) int {
	n := 0
	for w := range hi {
		n += bits.OnesCount64(^hi[w] & lo[w] & live[w])
	}
	return n
}

// maskStateAt reads one record's two bits.
func maskStateAt(hi, lo []uint64, i int) int {
	w, b := i/64, uint(i%64)
	return int((hi[w]>>b)&1)<<1 | int((lo[w]>>b)&1)
}

// setMaskStateAt writes one record's two bits, clearing whatever was there.
func setMaskStateAt(hi, lo []uint64, i, st int) {
	w, b := i/64, uint(i%64)
	m := uint64(1) << b
	if st&2 != 0 {
		hi[w] |= m
	} else {
		hi[w] &^= m
	}
	if st&1 != 0 {
		lo[w] |= m
	} else {
		lo[w] &^= m
	}
}

// dataStateToMask maps one DATA element to its mask state as a logical operand.
//
// Numbers are TRUTHY in ClassAd's logical operators -- `1 && true` is TRUE, `0 && true` is FALSE, `0.0` is
// FALSE, `!42` is FALSE -- while a STRING is an ERROR. That asymmetry is the whole reason this function
// exists rather than a rule like "not a boolean means error", which is what it looks like it should be and
// which TestMaskConversionMatchesReference would have caught.
func dataStateToMask(st uint8, payload int64) int {
	switch st {
	case VsBool:
		if payload != 0 {
			return mTrue
		}
		return mFalse
	case VsInt:
		if payload != 0 {
			return mTrue
		}
		return mFalse
	case VsReal:
		f := math.Float64frombits(uint64(payload))
		if f != 0 {
			return mTrue
		}
		return mFalse
	case VsUndef:
		return mUndef
	default:
		// A string, and anything a vector cannot hold, is an error in a logical position.
		return mError
	}
}

// MaskWords returns the number of uint64 words a mask plane needs for n records. Exported so a caller
// that builds its own visibility mask sizes it the same way the executor does.
func MaskWords(n int) int { return maskWords(n) }

// PopCountMasked counts set bits of a & b, for a caller combining its own bitmaps.
func PopCountMasked(a, b []uint64) int {
	n := 0
	for w := range a {
		n += bits.OnesCount64(a[w] & b[w])
	}
	return n
}
