//go:build goexperiment.simd

// Requires Go 1.27+ and GOEXPERIMENT=simd; the tag above excludes it from every ordinary build. Run with:
//
//	GOTOOLCHAIN=local GOEXPERIMENT=simd go1.27rc2 test -bench BenchmarkSIMDKernels -run TestSIMDKernelsAgree ./
//
// SPIKE: what would SIMD comparison kernels buy? Measured on Go 1.27rc2, darwin/arm64 (NEON, 128-bit).
//
// The executor's comparison kernel builds mask-plane words -- for each 64 records, compare a column
// against a literal and pack 64 result bits into one uint64 -- deliberately the shape a SIMD version
// replaces. This measures that replacement over a full-sized row group. CONCLUSION FIRST: not yet, and
// the reason is the block layout rather than the instruction set.
//
// WHAT THE PORTABLE PACKAGE CANNOT DO, which constrains the design more than speed does:
//
//	simd.Mask16s   And, Or, ToInt16s, ToArch          -- no ToBits
//	simd.Int16s    Add, Sub, Masked, IfElse, Store    -- no horizontal reduction
//
// So a portable kernel cannot produce a mask-plane word, and cannot reduce a vector to a count. It CAN
// accumulate per lane -- a true mask is -1, so Sub adds one -- and store once at the end, needing
// neither; that serves a terminal COUNT (2.9x) but not the executor's bitplane pipeline.
//
// Bitplanes need archsimd. An AVX-512 mask register already IS a bitmask, so amd64 gets ToBits for free;
// arm64 has no mask register and must emulate movemask (zero a weight vector where the mask is false,
// then reduce-sum). The two architectures do not want the same kernel, so this is two kernels to keep.
//
// LANES COME FROM THE COLUMN'S STORED WIDTH. Storage fits each int column to 1/2/4/6/8 bytes, so the
// natural SIMD type is not int64: 8 lanes as int16 against 2 as int64 on NEON. But narrowing ALONE
// measured 0.93x -- no help at all. Scalar code does not care whether it loads two bytes or eight; the
// whole 5.3x is vectorization, and width matters only because it multiplies lanes.
//
// THE HOT REGION IS STRIDED, AND THAT IS THE ACTUAL BLOCKER. A block's hot region is row-major
// (hotStride bytes per record: escape bitmap, bool bitset and popular numerics together), so a hot column
// cannot be loaded into a vector register without a gather. The executor already pays that gather in
// loadIntBatch, so the fair comparison is pipeline against pipeline rather than SIMD against a contiguous
// baseline the system never has:
//
//	HOT column  (strided, gather required)   9730 ns -> 4906 ns   1.98x
//	COLD column (already columnar)           6207 ns -> 1077 ns   5.76x
//
// The gather is 39% of the current hot pipeline and caps the hot-column win at ~2x. Cold numeric columns
// ARE columnar and reach 5.76x -- but a cold column is by definition one queries do not touch, which is
// what made it cold. SIMD helps most exactly where it matters least.
//
// With the comparison kernel at roughly a third of a scan, ~2x on it is ~1.2x end to end for the columns
// queries actually hit. That is not worth two architecture-specific kernels behind an experiment flag.
//
// REVISIT WHEN either the portable API grows mask-to-bits or a horizontal reduction, or the hot region
// becomes columnar per field within a block. The latter is the larger lever and is a storage change that
// trades against the reason hot is row-major at all: a whole-record read is one contiguous span.

package vm

import (
	"simd"
	"simd/archsimd"
	"testing"
)

const simdSpikeN = 8192 // colGroupMaxRows: the largest row group

// --- baselines, scalar -------------------------------------------------------

// scalarBitplaneInt64 is the executor's kernel as it stands: contiguous int64, compared against a
// literal, packed 64 results to a word.
func scalarBitplaneInt64(src []int64, lit int64, plane []uint64) {
	for w := range plane {
		var word uint64
		base := w * 64
		for i := 0; i < 64; i++ {
			if src[base+i] > lit {
				word |= 1 << uint(i)
			}
		}
		plane[w] = word
	}
}

// scalarBitplaneInt16 is the scalar kernel at the column's STORED width, which separates the two
// independent effects: narrowing (fewer bytes touched) from vectorizing (more lanes per instruction).
func scalarBitplaneInt16(src []int16, lit int16, plane []uint64) {
	for w := range plane {
		var word uint64
		base := w * 64
		for i := 0; i < 64; i++ {
			if src[base+i] > lit {
				word |= 1 << uint(i)
			}
		}
		plane[w] = word
	}
}

func scalarCountInt16(src []int16, lit int16) int {
	c := 0
	for _, v := range src {
		if v > lit {
			c++
		}
	}
	return c
}

// --- portable SIMD, counting without bitplanes -------------------------------

// simdCountPortable is the fully portable kernel: compare, and accumulate per lane because a true mask is
// -1 so Sub adds one. One store at the end, then a scalar sum of Len() lanes -- which is how it avoids
// needing the horizontal reduction the portable package does not have.
func simdCountPortable(src []int16, lit int16, buf []int16) int {
	lanes := simd.BroadcastInt16s(0).Len()
	v := simd.BroadcastInt16s(lit)
	acc := simd.BroadcastInt16s(0)
	i := 0
	for ; i+lanes <= len(src); i += lanes {
		acc = acc.Sub(simd.LoadInt16s(src[i:]).Greater(v).ToInt16s())
	}
	acc.Store(buf[:lanes])
	c := 0
	for _, x := range buf[:lanes] {
		c += int(x)
	}
	for ; i < len(src); i++ { // tail
		if src[i] > lit {
			c++
		}
	}
	return c
}

// --- arch-specific SIMD, producing bitplanes ---------------------------------

// weights16 turns a NEON mask into a byte: zero the weights where the mask is false, reduce-sum. The
// emulation amd64 does not need.
var weights16 = archsimd.LoadInt16x8([]int16{1, 2, 4, 8, 16, 32, 64, 128})

func simdBitplaneInt16Arch(src []int16, lit int16, plane []uint64) {
	v := archsimd.BroadcastInt16x8(lit)
	for w := range plane {
		var word uint64
		base := w * 64
		for g := 0; g < 8; g++ {
			m := archsimd.LoadInt16x8(src[base+g*8:]).Greater(v)
			word |= uint64(uint8(weights16.Masked(m).ReduceSum())) << uint(g*8)
		}
		plane[w] = word
	}
}

// --- the gather a hot column requires ----------------------------------------

// gatherStrided64 is what loadIntBatch does TODAY: the same strided walk, widened to int64. The executor
// already pays a gather, which is the fact that makes the comparison below a pipeline against a pipeline
// rather than SIMD against a contiguous baseline the system never has.
func gatherStrided64(hot []byte, stride, off int, dst []int64) {
	for k := range dst {
		p := k*stride + off
		dst[k] = int64(uint16(hot[p]) | uint16(hot[p+1])<<8)
	}
}

func gatherStrided(hot []byte, stride, off int, dst []int16) {
	for k := range dst {
		p := k*stride + off
		dst[k] = int16(uint16(hot[p]) | uint16(hot[p+1])<<8)
	}
}

func fixtures() ([]int64, []int16, []byte, int, int) {
	i64 := make([]int64, simdSpikeN)
	i16 := make([]int16, simdSpikeN)
	x := uint32(12345)
	for i := range i64 {
		x = x*1664525 + 1013904223
		v := int64(1024 + (x>>16)%32*512) // the RequestMemory distribution: fits two bytes
		i64[i], i16[i] = v, int16(v)
	}
	stride, off := 24, 6
	hot := make([]byte, simdSpikeN*stride)
	for k := 0; k < simdSpikeN; k++ {
		u := uint16(i16[k])
		hot[k*stride+off] = byte(u)
		hot[k*stride+off+1] = byte(u >> 8)
	}
	return i64, i16, hot, stride, off
}

func TestSIMDKernelsAgree(t *testing.T) {
	i64, i16, hot, stride, off := fixtures()
	const lit = 8192
	words := simdSpikeN / 64

	want := make([]uint64, words)
	scalarBitplaneInt64(i64, lit, want)
	wantCount := scalarCountInt16(i16, lit)

	got := make([]uint64, words)
	simdBitplaneInt16Arch(i16, lit, got)
	for w := range want {
		if got[w] != want[w] {
			t.Fatalf("arch bitplane word %d = %#016x, want %#016x", w, got[w], want[w])
		}
	}
	buf := make([]int16, 64)
	if c := simdCountPortable(i16, lit, buf); c != wantCount {
		t.Fatalf("portable count = %d, want %d", c, wantCount)
	}
	g := make([]int16, simdSpikeN)
	gatherStrided(hot, stride, off, g)
	for k := range g {
		if g[k] != i16[k] {
			t.Fatalf("gather record %d = %d, want %d", k, g[k], i16[k])
		}
	}
	t.Logf("all kernels agree (count=%d/%d); portable lanes int16=%d, NEON int16x8 lanes=%d",
		wantCount, simdSpikeN, simd.BroadcastInt16s(0).Len(), archsimd.BroadcastInt16x8(0).Len())
}

func BenchmarkSIMDKernels(b *testing.B) {
	i64, i16, hot, stride, off := fixtures()
	const lit = 8192
	plane := make([]uint64, simdSpikeN/64)
	buf := make([]int16, 64)
	g := make([]int16, simdSpikeN)

	b.Run("1_scalar_bitplane_int64", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			scalarBitplaneInt64(i64, lit, plane)
		}
	})
	b.Run("2_arch_bitplane_int16", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			simdBitplaneInt16Arch(i16, lit, plane)
		}
	})
	b.Run("2b_scalar_bitplane_int16", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			scalarBitplaneInt16(i16, lit, plane)
		}
	})
	b.Run("3_scalar_count_int16", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			scalarCountInt16(i16, lit)
		}
	})
	b.Run("4_portable_count_int16", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			simdCountPortable(i16, lit, buf)
		}
	})
	b.Run("5_gather_strided", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			gatherStrided(hot, stride, off, g)
		}
	})
	// The decision-relevant pair: what the executor does now against what it would do. Both gather,
	// because a hot column is strided either way; they differ in the width gathered and the kernel run.
	g64 := make([]int64, simdSpikeN)
	b.Run("6_pipeline_today_gather64_scalar", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			gatherStrided64(hot, stride, off, g64)
			scalarBitplaneInt64(g64, lit, plane)
		}
	})
	b.Run("7_pipeline_simd_gather16_arch", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			gatherStrided(hot, stride, off, g)
			simdBitplaneInt16Arch(g, lit, plane)
		}
	})
	b.Run("8_gather64_only", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			gatherStrided64(hot, stride, off, g64)
		}
	})
	// And the cold-column case, where the data is ALREADY columnar and no gather is needed at all.
	b.Run("9_cold_contiguous_scalar", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			scalarBitplaneInt64(i64, lit, plane)
		}
	})
	b.Run("A_cold_contiguous_simd", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			simdBitplaneInt16Arch(i16, lit, plane)
		}
	})
}
