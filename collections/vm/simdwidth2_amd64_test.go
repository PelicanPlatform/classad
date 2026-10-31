//go:build goexperiment.simd && amd64

package vm

import (
	"simd/archsimd"
	"testing"
)

// The width-2 AVX2 kernels, tested WITHOUT depending on what the runner's CPU happens to be.
//
// TestWidthNativeMatchesScalar covers width 2 through simdBaseMask, which picks a kernel by CPU feature.
// On an AVX-512 runner that is the KMOVW kernel and the AVX2 one is never executed; on a runner without
// it, the reverse. Either way half of the width-2 surface would go untested, and which half would depend
// on the machine the job landed on -- so these call the kernels directly.

// width2Cases is the grid the differential test uses, narrowed to width 2: counts around the 16-lane and
// 64-record boundaries, where a lane-at-a-time kernel goes wrong.
var width2Counts = []int{1, 3, 7, 8, 9, 15, 16, 17, 31, 32, 33, 63, 64, 65, 100, 128, 372, 1000}

var width2Lits = []int64{0, 1, -1, 127, 128, 255, 256, -32768, 32767, 65535}

func width2Raw(n int) []byte {
	raw := make([]byte, n*2)
	x := uint32(0x9e3779b9)
	for i := range raw {
		x = x*1664525 + 1013904223
		raw[i] = byte(x >> 16)
	}
	return raw
}

// TestWidth2AVX2MatchesScalar runs the AVX2 kernels against the scalar reference on every CPU that can
// execute them at all, AVX-512 or not. This is the test that would have caught the KMOVW the AVX-512 gate
// was added to work around, had the kernel existed then.
func TestWidth2AVX2MatchesScalar(t *testing.T) {
	if !archsimd.X86.AVX2() {
		t.Skip("no AVX2 on this CPU, so neither width-2 kernel can run here")
	}
	for _, n := range width2Counts {
		raw := width2Raw(n)
		widened := make([]int64, n)
		for _, unsigned := range []bool{false, true} {
			widenTyped(widened, raw, 2, unsigned, n)
			for _, lit := range width2Lits {
				if unsigned && !fitsUint16(lit) || !unsigned && !fitsInt16(lit) {
					continue // the kernel declines these, and simdBaseMask never calls it with one
				}
				for _, base := range []cmpBase{baseEQ, baseGT, baseLT} {
					words := maskWords(n)
					want := make([]uint64, words)
					scalarBaseMaskInts(base, widened, lit, n, want)

					got := make([]uint64, words)
					var ok bool
					if unsigned {
						ok = maskUint16x16AVX2(base, raw, uint16(lit), n, got)
					} else {
						ok = maskInt16x16AVX2(base, raw, int16(lit), n, got)
					}
					if !ok {
						t.Fatalf("n=%d unsigned=%v lit=%d base=%d: the AVX2 kernel declined", n, unsigned, lit, base)
					}
					for w := range want {
						if got[w] != want[w] {
							t.Fatalf("n=%d unsigned=%v lit=%d base=%d: word %d = %#016x, want %#016x",
								n, unsigned, lit, base, w, got[w], want[w])
						}
					}
				}
			}
		}
	}
}

// TestWidth2KernelsAgree cross-checks the two extractions against each other where both can run. It is
// strictly weaker than the scalar comparison above and exists for a different reason: it is the one that
// fails if the AVX-512 kernel is the one that drifts.
func TestWidth2KernelsAgree(t *testing.T) {
	if !archsimd.X86.AVX2() {
		t.Skip("no AVX2 on this CPU")
	}
	if !archsimd.X86.AVX512() {
		t.Skip("no AVX-512 on this CPU, so the KMOVW kernel cannot be executed to compare against")
	}
	for _, n := range width2Counts {
		raw := width2Raw(n)
		for _, unsigned := range []bool{false, true} {
			for _, lit := range width2Lits {
				if unsigned && !fitsUint16(lit) || !unsigned && !fitsInt16(lit) {
					continue
				}
				for _, base := range []cmpBase{baseEQ, baseGT, baseLT} {
					words := maskWords(n)
					avx2, avx512 := make([]uint64, words), make([]uint64, words)
					if unsigned {
						maskUint16x16AVX2(base, raw, uint16(lit), n, avx2)
						maskUint16x16(base, raw, uint16(lit), n, avx512)
					} else {
						maskInt16x16AVX2(base, raw, int16(lit), n, avx2)
						maskInt16x16(base, raw, int16(lit), n, avx512)
					}
					for w := range avx2 {
						if avx2[w] != avx512[w] {
							t.Fatalf("n=%d unsigned=%v lit=%d base=%d: word %d AVX2 %#016x, AVX-512 %#016x",
								n, unsigned, lit, base, w, avx2[w], avx512[w])
						}
					}
				}
			}
		}
	}
}

// TestCompressEvenBits checks the bit fold over EVERY sixteen-lane pattern. It needs no vector unit at
// all, so it runs on any amd64 runner and covers the one part of the extraction that is arithmetic rather
// than an instruction.
func TestCompressEvenBits(t *testing.T) {
	for lanes := 0; lanes < 1<<16; lanes++ {
		// What VPMOVMSKB produces for this lane pattern: each lane's bit in both of its bytes.
		var dup uint32
		for i := 0; i < 16; i++ {
			if lanes&(1<<i) != 0 {
				dup |= 3 << uint(2*i)
			}
		}
		if got := compressEvenBits(dup); got != uint64(lanes) {
			t.Fatalf("compressEvenBits(%#08x) = %#x, want %#x", dup, got, lanes)
		}
	}
}

// TestCompressEvenBitsIgnoresOddBits pins that only the even bit of each pair is read. The two are always
// equal coming out of VPMOVMSKB, so nothing downstream would notice if the fold started reading the other
// one -- until a future extraction produced a vector where they differ.
func TestCompressEvenBitsIgnoresOddBits(t *testing.T) {
	for _, lanes := range []uint32{0, 1, 0xffff, 0xaaaa, 0x5555, 0x1234} {
		var even uint32
		for i := 0; i < 16; i++ {
			if lanes&(1<<uint(i)) != 0 {
				even |= 1 << uint(2*i)
			}
		}
		if got := compressEvenBits(even); got != uint64(lanes) {
			t.Fatalf("even-only %#08x folded to %#x, want %#x", even, got, lanes)
		}
		if got := compressEvenBits(even | 0xaaaaaaaa); got != uint64(lanes) {
			t.Fatalf("odd bits changed the answer: %#x, want %#x", got, lanes)
		}
	}
}
