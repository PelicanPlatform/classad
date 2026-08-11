//go:build goexperiment.simd && amd64

package vm

import (
	"simd/archsimd"
	"unsafe"
)

const rawSupported = true

// The amd64 engine: AVX2, 256 bits, so 16 lanes at a 2-byte column width and 8 at 4 bytes -- twice arm64's
// NEON, and the mask comes free.
//
// An AVX-512 mask register already IS a bitmask, so ToBits is a register read rather than the weight-vector
// reduction arm64 has to emulate. That extraction was 30% of the arm64 kernel, which is why this engine should
// reach nearer the 8.32x bound the arm64 spike measured with extraction removed than arm64's own 5.82x.
//
// Declines without AVX2 rather than adding a 128-bit path. The shared scalar kernel is still width-native, so
// declining costs the vector width and not the whole representation, and AVX2 is 2013 hardware -- a pool
// without it is not the case worth a third code path.

// simdSupportsWidth reports whether this engine has lanes for a column of this width, on this CPU.
func simdSupportsWidth(width int, _ bool) bool {
	return archsimd.X86.AVX2() && (width == 2 || width == 4)
}

func simdBaseMask(base cmpBase, raw []byte, width int, unsigned bool, lit int64, n int, lo []uint64) bool {
	if !archsimd.X86.AVX2() {
		return false
	}
	switch {
	case width == 2 && !unsigned && fitsInt16(lit):
		return maskInt16x16(base, raw, int16(lit), n, lo)
	case width == 2 && unsigned && fitsUint16(lit):
		return maskUint16x16(base, raw, uint16(lit), n, lo)
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
