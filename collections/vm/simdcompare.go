package vm

// Width-native comparison of a stored column against a literal.
//
// LANES COME FROM WIDTH. Storage fits each int column to 1/2/4/6/8 bytes, so a column fitted to two bytes is
// 8 lanes of a 128-bit vector where int64 is 2 -- and widening at load throws that away before any kernel
// sees it. This is the path that keeps the stored width all the way into the compare.
//
// The architecture-specific engines below it implement exactly three BASE operations -- equal, greater, less
// -- and this file derives the other three by complementing the extracted bits:
//
//	==  equal        !=  not equal
//	>   greater      <=  not greater
//	<   less         >=  not less
//
// That is what keeps two engines to a tractable size: three operations per (width, signedness) rather than
// six, and the complement is one XOR on the assembled word.
//
// A scalar fallback runs whenever an engine declines -- an unsupported width, no SIMD in the build, a CPU
// without the vector extension. It widens the column first, with the width decision hoisted out of the loop,
// and compares int64s: exactly what the executor did before the raw representation existed.
//
// So WITHOUT an engine this representation is neutral, not a win. Reading the column at its stored width per
// element instead -- the obvious way to write a "width-native" scalar kernel -- measured 0.69x to 0.84x of a
// scan, because a byte-at-a-time read with the width branch inside the loop is what a hoisted load replaced.
// The win is the lanes, and lanes need an engine.

// RawColumnUseful reports whether handing a column of this width to the executor in its STORED form can pay
// off -- that is, whether an engine on this build and this CPU has lanes for it.
//
// A source should gate on this rather than always supplying a raw column. Without an engine the raw form is
// not free: every non-comparison path has to widen it, and doing that measured 0.87x to 0.89x of a scan on the
// arithmetic shapes, where both operands widen. There is no point paying that for lanes nobody has.
func RawColumnUseful(width int, unsigned bool) bool { return simdSupportsWidth(width, unsigned) }

// cmpBase is the subset of comparisons an engine implements.
type cmpBase uint8

const (
	baseEQ cmpBase = iota
	baseGT
	baseLT
)

// baseFor maps an operator to the base operation and whether the result is its complement.
func baseFor(c cmpOp) (cmpBase, bool) {
	switch c {
	case cmpEQ:
		return baseEQ, false
	case cmpNE:
		return baseEQ, true
	case cmpGT:
		return baseGT, false
	case cmpLE:
		return baseGT, true
	case cmpLT:
		return baseLT, false
	default: // cmpGE
		return baseLT, true
	}
}

// simdCompareRaw compares l's raw column against an integer literal, writing l's mask planes.
//
// Always answers when the column is a width it can read, whether or not an engine took it: the scalar
// fallback is width-native too. ok=false leaves the caller to widen and use the ordinary path.
func simdCompareRaw(c cmpOp, l *Vec, lit int64) bool {
	if !rawSupported {
		return false
	}
	w := l.RawWidth
	if w < 1 || w > 8 || len(l.Raw) < l.n*w {
		return false
	}
	base, negate := baseFor(c)
	if !simdBaseMask(base, l.Raw, w, l.RawUnsigned, lit, l.n, l.Lo) {
		// No engine took it: widen with the hoisted loop, then compare int64s.
		n, lo := l.n, l.Lo
		l.ensureInts()
		scalarBaseMaskInts(base, l.I, lit, n, lo)
		l.Mask = true
		if negate {
			complementPlane(lo, n)
		}
		for i := range l.Hi {
			l.Hi[i] = 0
		}
		return true
	}
	if negate {
		complementPlane(l.Lo, l.n)
	}
	// A comparison yields only TRUE or FALSE -- a stored value is never UNDEFINED, that is what the escape
	// bitmap is for, and the caller only supplies Raw for an escape-free column -- so the high plane is zero.
	for i := range l.Hi {
		l.Hi[i] = 0
	}
	l.Mask, l.Raw = true, nil
	return true
}

// complementPlane flips the value bits, leaving the tail past n clear so a popcount cannot count records
// that do not exist.
func complementPlane(lo []uint64, n int) {
	for w := range lo {
		lo[w] = ^lo[w]
	}
	if tail := uint(n % 64); tail != 0 && len(lo) > 0 {
		lo[len(lo)-1] &= (uint64(1) << tail) - 1
	}
}

// scalarBaseMaskInts assembles result words from an already-widened column.
func scalarBaseMaskInts(base cmpBase, vals []int64, lit int64, n int, lo []uint64) {
	for w := range lo {
		start := w * 64
		end := start + 64
		if end > n {
			end = n
		}
		var word uint64
		for i := start; i < end; i++ {
			v := vals[i]
			var res bool
			switch base {
			case baseEQ:
				res = v == lit
			case baseGT:
				res = v > lit
			default:
				res = v < lit
			}
			if res {
				word |= 1 << uint(i-start)
			}
		}
		lo[w] = word
	}
}

// scalarTailInt fills the bits for records [from, n) of the word starting at wordStart, for a group a vector
// load cannot cover.
//
// Row groups are sealed on multiples of eight (see colGroupAlign), so only a segment's final group has a tail
// at all -- but it does have one, and reading a vector past the end of the column would be a fault rather
// than a wrong answer.
func scalarTailInt(base cmpBase, raw []byte, width int, unsigned bool, lit int64, from, n, wordStart int) uint64 {
	var word uint64
	for i := from; i < n && i < wordStart+64; i++ {
		v := readIntLE(raw[i*width:], width, unsigned)
		var res bool
		switch base {
		case baseEQ:
			res = v == lit
		case baseGT:
			res = v > lit
		default:
			res = v < lit
		}
		if res {
			word |= 1 << uint(i-wordStart)
		}
	}
	return word
}

func fitsInt16(v int64) bool  { return v >= -1<<15 && v < 1<<15 }
func fitsUint16(v int64) bool { return v >= 0 && v < 1<<16 }
func fitsInt32(v int64) bool  { return v >= -1<<31 && v < 1<<31 }
func fitsUint32(v int64) bool { return v >= 0 && v < 1<<32 }
