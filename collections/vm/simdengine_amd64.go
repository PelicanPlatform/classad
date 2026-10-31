//go:build goexperiment.simd && amd64

package vm

import (
	"simd/archsimd"
	"unsafe"
)

const rawSupported = true

// The amd64 engine: AVX2, 256 bits, so 32 lanes at a 1-byte column width, 16 at 2 bytes and 8 at 4 bytes --
// twice arm64's NEON.
//
// At widths 1 and 4 the mask comes free: VPMOVMSKB and VMOVMSKPS read it straight out of the comparison
// result, where arm64 has to emulate the extraction with a weight vector and a cross-lane reduction worth
// 30% of its kernel. That is why this engine should reach nearer the 8.32x bound the arm64 spike measured
// with extraction removed than arm64's own 5.82x -- on an AVX-512 CPU at every width, and at widths 1 and
// 4 on any AVX2 one. Width 2 without AVX-512 pays an extraction of its own (movemask16x16), cheaper than
// arm64's but not free.
//
// None of those amd64 numbers has been measured on amd64 hardware; they are what the arm64 spike predicts.
//
// Declines without AVX2 rather than adding a 128-bit path. The shared scalar kernel is still width-native, so
// declining costs the vector width and not the whole representation, and AVX2 is 2013 hardware -- a pool
// without it is not the case worth a third code path.
//
// EVERY WIDTH IS AVX2. What differs between them is how the bitmask is EXTRACTED, not how the comparison
// is made: the comparison is VPCMPGTW/VPCMPGTB/VPCMPGTD whatever the extraction.
//
// Mask8x32.ToBits is VPMOVMSKB and Mask32x8.ToBits is VMOVMSKPS, both AVX2 or older. Mask16x16.ToBits is
// not: it is VPMOVW2M + KMOVW, two AVX-512 instructions. Gating the engine on AVX2 alone therefore put an
// AVX-512 instruction on every AVX2-without-AVX512 CPU, which is most of them -- SIGILL on the first
// 2-byte column compared. Width 2 was then gated on AVX512 separately, which fixed the crash by giving up
// the vector path for an entire column width on those CPUs, and a 2-byte column is not rare: it is what
// chooseIntWidth picks for any value that needs 256..65535, RequestMemory among them.
//
// movemask16x16 is the AVX2 extraction that gives it back. See its own comment for how.

// simdSupportsWidth reports whether this engine has lanes for a column of this width, on this CPU.
func simdSupportsWidth(width int, _ bool) bool {
	if !archsimd.X86.AVX2() {
		return false
	}
	return width == 1 || width == 2 || width == 4
}

func simdBaseMask(base cmpBase, raw []byte, width int, unsigned bool, lit int64, n int, lo []uint64) bool {
	if !archsimd.X86.AVX2() {
		return false
	}
	switch {
	case width == 1 && !unsigned && fitsInt8(lit):
		return maskInt8x32(base, raw, int8(lit), n, lo)
	case width == 1 && unsigned && fitsUint8(lit):
		return maskUint8x32(base, raw, uint8(lit), n, lo)
	// Width 2 has two kernels for one comparison: they differ only in the extraction, and the choice is
	// made ONCE here rather than per vector, so neither loop carries a branch on a CPU feature that
	// cannot change under it.
	case width == 2 && !unsigned && fitsInt16(lit):
		if archsimd.X86.AVX512() {
			return maskInt16x16(base, raw, int16(lit), n, lo)
		}
		return maskInt16x16AVX2(base, raw, int16(lit), n, lo)
	case width == 2 && unsigned && fitsUint16(lit):
		if archsimd.X86.AVX512() {
			return maskUint16x16(base, raw, uint16(lit), n, lo)
		}
		return maskUint16x16AVX2(base, raw, uint16(lit), n, lo)
	case width == 4 && !unsigned && fitsInt32(lit):
		return maskInt32x8(base, raw, int32(lit), n, lo)
	case width == 4 && unsigned && fitsUint32(lit):
		return maskUint32x8(base, raw, uint32(lit), n, lo)
	}
	return false
}

func maskInt16x16(base cmpBase, raw []byte, lit int16, n int, lo []uint64) bool {
	v := archsimd.BroadcastInt16x16(lit)
	const lanes = 16
	for w := range lo {
		var word uint64
		for g := 0; g*lanes < 64; g++ {
			i := w*64 + g*lanes
			if i+lanes > n {
				word |= scalarTailInt(base, raw, 2, false, int64(lit), i, n, w*64)
				break
			}
			x := archsimd.LoadInt16x16(unsafe.Slice((*int16)(unsafe.Pointer(&raw[i*2])), lanes))
			var m archsimd.Mask16x16
			switch base {
			case baseEQ:
				m = x.Equal(v)
			case baseGT:
				m = x.Greater(v)
			default:
				m = x.Less(v)
			}
			word |= uint64(m.ToBits()) << uint(g*lanes)
		}
		lo[w] = word
	}
	return true
}

func maskUint16x16(base cmpBase, raw []byte, lit uint16, n int, lo []uint64) bool {
	v := archsimd.BroadcastUint16x16(lit)
	const lanes = 16
	for w := range lo {
		var word uint64
		for g := 0; g*lanes < 64; g++ {
			i := w*64 + g*lanes
			if i+lanes > n {
				word |= scalarTailInt(base, raw, 2, true, int64(lit), i, n, w*64)
				break
			}
			x := archsimd.LoadUint16x16(unsafe.Slice((*uint16)(unsafe.Pointer(&raw[i*2])), lanes))
			var m archsimd.Mask16x16
			switch base {
			case baseEQ:
				m = x.Equal(v)
			case baseGT:
				m = x.Greater(v)
			default:
				m = x.Less(v)
			}
			word |= uint64(m.ToBits()) << uint(g*lanes)
		}
		lo[w] = word
	}
	return true
}

func maskInt32x8(base cmpBase, raw []byte, lit int32, n int, lo []uint64) bool {
	v := archsimd.BroadcastInt32x8(lit)
	const lanes = 8
	for w := range lo {
		var word uint64
		for g := 0; g*lanes < 64; g++ {
			i := w*64 + g*lanes
			if i+lanes > n {
				word |= scalarTailInt(base, raw, 4, false, int64(lit), i, n, w*64)
				break
			}
			x := archsimd.LoadInt32x8(unsafe.Slice((*int32)(unsafe.Pointer(&raw[i*4])), lanes))
			var m archsimd.Mask32x8
			switch base {
			case baseEQ:
				m = x.Equal(v)
			case baseGT:
				m = x.Greater(v)
			default:
				m = x.Less(v)
			}
			word |= uint64(m.ToBits()) << uint(g*lanes)
		}
		lo[w] = word
	}
	return true
}

func maskUint32x8(base cmpBase, raw []byte, lit uint32, n int, lo []uint64) bool {
	v := archsimd.BroadcastUint32x8(lit)
	const lanes = 8
	for w := range lo {
		var word uint64
		for g := 0; g*lanes < 64; g++ {
			i := w*64 + g*lanes
			if i+lanes > n {
				word |= scalarTailInt(base, raw, 4, true, int64(lit), i, n, w*64)
				break
			}
			x := archsimd.LoadUint32x8(unsafe.Slice((*uint32)(unsafe.Pointer(&raw[i*4])), lanes))
			var m archsimd.Mask32x8
			switch base {
			case baseEQ:
				m = x.Equal(v)
			case baseGT:
				m = x.Greater(v)
			default:
				m = x.Less(v)
			}
			word |= uint64(m.ToBits()) << uint(g*lanes)
		}
		lo[w] = word
	}
	return true
}

func maskInt8x32(base cmpBase, raw []byte, lit int8, n int, lo []uint64) bool {
	v := archsimd.BroadcastInt8x32(lit)
	const lanes = 32
	for w := range lo {
		var word uint64
		for g := 0; g*lanes < 64; g++ {
			i := w*64 + g*lanes
			if i+lanes > n {
				word |= scalarTailInt(base, raw, 1, false, int64(lit), i, n, w*64)
				break
			}
			x := archsimd.LoadInt8x32(unsafe.Slice((*int8)(unsafe.Pointer(&raw[i])), lanes))
			var m archsimd.Mask8x32
			switch base {
			case baseEQ:
				m = x.Equal(v)
			case baseGT:
				m = x.Greater(v)
			default:
				m = x.Less(v)
			}
			word |= uint64(m.ToBits()) << uint(g*lanes)
		}
		lo[w] = word
	}
	return true
}

func maskUint8x32(base cmpBase, raw []byte, lit uint8, n int, lo []uint64) bool {
	v := archsimd.BroadcastUint8x32(lit)
	const lanes = 32
	for w := range lo {
		var word uint64
		for g := 0; g*lanes < 64; g++ {
			i := w*64 + g*lanes
			if i+lanes > n {
				word |= scalarTailInt(base, raw, 1, true, int64(lit), i, n, w*64)
				break
			}
			x := archsimd.LoadUint8x32(unsafe.Slice((*uint8)(unsafe.Pointer(&raw[i])), lanes))
			var m archsimd.Mask8x32
			switch base {
			case baseEQ:
				m = x.Equal(v)
			case baseGT:
				m = x.Greater(v)
			default:
				m = x.Less(v)
			}
			word |= uint64(m.ToBits()) << uint(g*lanes)
		}
		lo[w] = word
	}
	return true
}

// movemask16x16 extracts the sixteen-bit mask of a 16-lane comparison WITHOUT AVX-512.
//
// A Mask16x16 is already a vector of 0 or -1 per 16-bit lane -- that is how AVX2 represents it, with no
// mask register to hold -- so ToInt16x16 and AsInt8x32 are reinterpretations that emit no instruction at
// all. What they buy is a view of the same 256 bits as 32 BYTES, each 0x00 or 0xFF, in pairs: both bytes
// of lane i carry lane i's answer.
//
// VPMOVMSKB then gives 32 bits with that same duplication, bit 2i == bit 2i+1 == lane i, and the rest is
// discarding every odd bit. The five-step fold below is the standard compress for mask 0x55555555: each
// step halves the distance between the kept bits until the sixteen are contiguous. It is scalar, so it
// is the price of not having a mask register -- the same price arm64 pays, and for the same reason,
// though it pays it with a cross-lane reduction instead.
//
// The getting-to-bytes is one VPCMPGTB, because an Int8x32 cannot be turned into a Mask8x32 without a
// comparison: 0xFF is -1 as an int8, so "< 0" selects exactly the set lanes. zero is that operand, built
// by the caller once per column rather than once per vector -- and built THERE and not in a package-level
// var, because a broadcast is itself an AVX2 instruction and a package-level one would run at init on
// every CPU, including the pre-AVX2 ones this engine is careful never to issue an instruction on.
func movemask16x16(m archsimd.Mask16x16, zero archsimd.Int8x32) uint64 {
	return compressEvenBits(uint32(m.ToInt16x16().AsInt8x32().Less(zero).ToBits()))
}

// compressEvenBits discards every odd bit of x, moving the sixteen even ones down to a contiguous
// sixteen. It is the standard five-step compress for mask 0x55555555: each step halves the distance
// between the bits that are kept, so after the last one they are adjacent.
//
// Its input is a VPMOVMSKB of a byte view of 16-bit lanes, where bit 2i and bit 2i+1 are both lane i, so
// which of each pair survives does not matter -- only that exactly one does.
func compressEvenBits(x uint32) uint64 {
	x &= 0x55555555
	x = (x | (x >> 1)) & 0x33333333
	x = (x | (x >> 2)) & 0x0f0f0f0f
	x = (x | (x >> 4)) & 0x00ff00ff
	x = (x | (x >> 8)) & 0x0000ffff
	return uint64(x)
}

// maskInt16x16AVX2 is maskInt16x16 extracting without AVX-512. Only the ToBits line differs; the two are
// separate functions so that neither loop tests a CPU feature per vector (see simdBaseMask).
func maskInt16x16AVX2(base cmpBase, raw []byte, lit int16, n int, lo []uint64) bool {
	v := archsimd.BroadcastInt16x16(lit)
	zero := archsimd.BroadcastInt8x32(0)
	const lanes = 16
	for w := range lo {
		var word uint64
		for g := 0; g*lanes < 64; g++ {
			i := w*64 + g*lanes
			if i+lanes > n {
				word |= scalarTailInt(base, raw, 2, false, int64(lit), i, n, w*64)
				break
			}
			x := archsimd.LoadInt16x16(unsafe.Slice((*int16)(unsafe.Pointer(&raw[i*2])), lanes))
			var m archsimd.Mask16x16
			switch base {
			case baseEQ:
				m = x.Equal(v)
			case baseGT:
				m = x.Greater(v)
			default:
				m = x.Less(v)
			}
			word |= movemask16x16(m, zero) << uint(g*lanes)
		}
		lo[w] = word
	}
	return true
}

// maskUint16x16AVX2 is maskUint16x16 extracting without AVX-512. See maskInt16x16AVX2.
func maskUint16x16AVX2(base cmpBase, raw []byte, lit uint16, n int, lo []uint64) bool {
	v := archsimd.BroadcastUint16x16(lit)
	zero := archsimd.BroadcastInt8x32(0)
	const lanes = 16
	for w := range lo {
		var word uint64
		for g := 0; g*lanes < 64; g++ {
			i := w*64 + g*lanes
			if i+lanes > n {
				word |= scalarTailInt(base, raw, 2, true, int64(lit), i, n, w*64)
				break
			}
			x := archsimd.LoadUint16x16(unsafe.Slice((*uint16)(unsafe.Pointer(&raw[i*2])), lanes))
			var m archsimd.Mask16x16
			switch base {
			case baseEQ:
				m = x.Equal(v)
			case baseGT:
				m = x.Greater(v)
			default:
				m = x.Less(v)
			}
			word |= movemask16x16(m, zero) << uint(g*lanes)
		}
		lo[w] = word
	}
	return true
}
