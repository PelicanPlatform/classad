//go:build goexperiment.simd && arm64

package vm

import (
	"simd/archsimd"
	"unsafe"
)

const rawSupported = true

// The arm64 engine: NEON, 128 bits, so 8 lanes at a 2-byte column width and 4 at 4 bytes.
//
// arm64 has no mask register -- a comparison yields a vector of all-ones lanes -- so a bitmask must be
// emulated: zero a weight vector where the mask is false, then reduce-sum it. Two extra operations per vector
// and the reduction is cross-lane, which is the expensive kind, and it measured at 30% of the kernel. amd64
// gets the same bits for free from a mask register, which is why the two engines are separate files rather
// than one with a switch.
//
// Measured against the scalar bitplane kernel over 8192 records: 5.82x with this extraction, 8.32x with the
// extraction removed, which is the bound amd64 approaches.

var (
	weights16 = archsimd.LoadInt16x8([]int16{1, 2, 4, 8, 16, 32, 64, 128})
	weights32 = archsimd.LoadInt32x4([]int32{1, 2, 4, 8})
)

func movemask16(m archsimd.Mask16x8) uint64 {
	return uint64(uint8(weights16.Masked(m).ReduceSum()))
}

func movemask32(m archsimd.Mask32x4) uint64 {
	return uint64(uint8(weights32.Masked(m).ReduceSum()))
}

// simdBaseMask fills lo with the bitmask of base(value, lit) per record, or declines.
//
// Declines for a width it has no vector type for (1, 6 and 8 bytes) and for a literal that does not fit the
// column's type, since the comparison would have to happen at a wider type than the lanes.
// simdSupportsWidth reports whether this engine has lanes for a column of this width. NEON is baseline on
// arm64, so there is no CPU check to make.
func simdSupportsWidth(width int, _ bool) bool { return width == 2 || width == 4 }

func simdBaseMask(base cmpBase, raw []byte, width int, unsigned bool, lit int64, n int, lo []uint64) bool {
	switch {
	case width == 2 && !unsigned && fitsInt16(lit):
		return maskInt16x8(base, raw, int16(lit), n, lo)
	case width == 2 && unsigned && fitsUint16(lit):
		return maskUint16x8(base, raw, uint16(lit), n, lo)
	case width == 4 && !unsigned && fitsInt32(lit):
		return maskInt32x4(base, raw, int32(lit), n, lo)
	case width == 4 && unsigned && fitsUint32(lit):
		return maskUint32x4(base, raw, uint32(lit), n, lo)
	}
	return false
}

func maskInt16x8(base cmpBase, raw []byte, lit int16, n int, lo []uint64) bool {
	v := archsimd.BroadcastInt16x8(lit)
	const lanes = 8
	for w := range lo {
		var word uint64
		for g := 0; g*lanes < 64; g++ {
			i := w*64 + g*lanes
			if i+lanes > n {
				word |= scalarTailInt(base, raw, 2, false, int64(lit), i, n, w*64)
				break
			}
			x := archsimd.LoadInt16x8(unsafe.Slice((*int16)(unsafe.Pointer(&raw[i*2])), lanes))
			var m archsimd.Mask16x8
			switch base {
			case baseEQ:
				m = x.Equal(v)
			case baseGT:
				m = x.Greater(v)
			default:
				m = x.Less(v)
			}
			word |= movemask16(m) << uint(g*lanes)
		}
		lo[w] = word
	}
	return true
}

func maskUint16x8(base cmpBase, raw []byte, lit uint16, n int, lo []uint64) bool {
	v := archsimd.BroadcastUint16x8(lit)
	const lanes = 8
	for w := range lo {
		var word uint64
		for g := 0; g*lanes < 64; g++ {
			i := w*64 + g*lanes
			if i+lanes > n {
				word |= scalarTailInt(base, raw, 2, true, int64(lit), i, n, w*64)
				break
			}
			x := archsimd.LoadUint16x8(unsafe.Slice((*uint16)(unsafe.Pointer(&raw[i*2])), lanes))
			var m archsimd.Mask16x8
			switch base {
			case baseEQ:
				m = x.Equal(v)
			case baseGT:
				m = x.Greater(v)
			default:
				m = x.Less(v)
			}
			word |= movemask16(m) << uint(g*lanes)
		}
		lo[w] = word
	}
	return true
}

func maskInt32x4(base cmpBase, raw []byte, lit int32, n int, lo []uint64) bool {
	v := archsimd.BroadcastInt32x4(lit)
	const lanes = 4
	for w := range lo {
		var word uint64
		for g := 0; g*lanes < 64; g++ {
			i := w*64 + g*lanes
			if i+lanes > n {
				word |= scalarTailInt(base, raw, 4, false, int64(lit), i, n, w*64)
				break
			}
			x := archsimd.LoadInt32x4(unsafe.Slice((*int32)(unsafe.Pointer(&raw[i*4])), lanes))
			var m archsimd.Mask32x4
			switch base {
			case baseEQ:
				m = x.Equal(v)
			case baseGT:
				m = x.Greater(v)
			default:
				m = x.Less(v)
			}
			word |= movemask32(m) << uint(g*lanes)
		}
		lo[w] = word
	}
	return true
}

func maskUint32x4(base cmpBase, raw []byte, lit uint32, n int, lo []uint64) bool {
	v := archsimd.BroadcastUint32x4(lit)
	const lanes = 4
	for w := range lo {
		var word uint64
		for g := 0; g*lanes < 64; g++ {
			i := w*64 + g*lanes
			if i+lanes > n {
				word |= scalarTailInt(base, raw, 4, true, int64(lit), i, n, w*64)
				break
			}
			x := archsimd.LoadUint32x4(unsafe.Slice((*uint32)(unsafe.Pointer(&raw[i*4])), lanes))
			var m archsimd.Mask32x4
			switch base {
			case baseEQ:
				m = x.Equal(v)
			case baseGT:
				m = x.Greater(v)
			default:
				m = x.Less(v)
			}
			word |= movemask32(m) << uint(g*lanes)
		}
		lo[w] = word
	}
	return true
}
