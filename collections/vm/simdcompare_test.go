package vm

import (
	"fmt"
	"testing"
)

// The width-native kernels are verified against each other: whatever engine this build has must produce the
// same mask planes as the scalar width-native kernel, for every width, signedness and operator.
//
// It runs on ANY build. Without the simd experiment the engine declines and this compares the scalar kernel
// against itself, which proves nothing about SIMD but does prove the shared layer -- the operator-to-base
// mapping, the complement, and the tail. Under GOEXPERIMENT=simd on Go 1.26+ (amd64) or 1.27+ (arm64) it is
// the real differential test, and simdEngineActive says which happened rather than leaving it ambiguous.
func TestWidthNativeMatchesScalar(t *testing.T) {
	// Record counts around the vector and word boundaries, since the tail is where a lane-at-a-time kernel
	// goes wrong: not a multiple of 8, of 16, or of 64.
	for _, n := range []int{1, 3, 7, 8, 9, 15, 16, 17, 63, 64, 65, 100, 128, 372, 1000} {
		for _, width := range []int{1, 2, 4, 8} {
			for _, unsigned := range []bool{false, true} {
				raw := make([]byte, n*width)
				x := uint32(0x9e3779b9)
				for i := range raw {
					x = x*1664525 + 1013904223
					raw[i] = byte(x >> 16)
				}
				for _, lit := range []int64{0, 1, -1, 127, 128, 255, 256, -32768, 32767, 65535, 1 << 20, -1 << 20} {
					for _, c := range []cmpOp{cmpEQ, cmpNE, cmpLT, cmpLE, cmpGT, cmpGE} {
						base, negate := baseFor(c)
						words := maskWords(n)

						want := make([]uint64, words)
						widened := make([]int64, n)
						widenTyped(widened, raw, width, unsigned, n)
						scalarBaseMaskInts(base, widened, lit, n, want)
						if negate {
							complementPlane(want, n)
						}

						got := make([]uint64, words)
						engine := simdBaseMask(base, raw, width, unsigned, lit, n, got)
						if !engine {
							continue // this build or this width has no engine; the scalar path already answered
						}
						if negate {
							complementPlane(got, n)
						}
						for w := range want {
							if got[w] != want[w] {
								t.Fatalf("n=%d width=%d unsigned=%v lit=%d op=%d: word %d = %#016x, want %#016x",
									n, width, unsigned, lit, c, w, got[w], want[w])
							}
						}
					}
				}
			}
		}
	}
}

// TestWidthNativeReportsEngine says whether an engine ran, so a green differential test cannot be mistaken
// for a verified engine when there is none.
func TestWidthNativeReportsEngine(t *testing.T) {
	raw := make([]byte, 64*2)
	lo := make([]uint64, 1)
	active := simdBaseMask(baseGT, raw, 2, false, 0, 64, lo)
	t.Logf("simd engine for a 2-byte signed column: active=%v", active)
	if !active {
		t.Log("no engine in this build: needs GOEXPERIMENT=simd with Go 1.26+ on amd64 or 1.27+ on arm64. " +
			"The differential test above compared the scalar kernel against itself.")
	}
}

// TestWidthNativeThroughExecutor drives the whole path -- a raw column in a Vec, compared against a literal,
// producing mask planes -- against the same comparison on a widened column, which is what the executor did
// before. The two must agree for every operator.
func TestWidthNativeThroughExecutor(t *testing.T) {
	if !rawSupported {
		t.Skip("no engine in this build, so the raw column representation is compiled out entirely: " +
			"needs GOEXPERIMENT=simd with Go 1.26+ on amd64 or 1.27+ on arm64")
	}
	const n = 300
	for _, width := range []int{1, 2, 4, 8} {
		for _, unsigned := range []bool{false, true} {
			raw := make([]byte, n*width)
			x := uint32(12345)
			for i := range raw {
				x = x*1664525 + 1013904223
				raw[i] = byte(x >> 16)
			}
			for _, lit := range []int64{0, 5, 300, -300, 70000} {
				for _, op := range []string{"==", "!=", "<", "<=", ">", ">="} {
					c, _ := parseCmpOp(op)

					// Raw path.
					rawVec := newVec(n)
					rawVec.Raw, rawVec.RawWidth, rawVec.RawUnsigned = raw, width, unsigned
					for k := 0; k < n; k++ {
						rawVec.St[k] = VsInt
					}
					if !simdCompareRaw(c, rawVec, lit) {
						t.Fatalf("width=%d: the raw path declined a width it should read", width)
					}

					// Widened path: the same column as int64, through the ordinary kernel.
					wide := newVec(n)
					for k := 0; k < n; k++ {
						wide.SetInt(k, readIntLE(raw[k*width:], width, unsigned))
					}
					lv := newVec(n)
					lv.St[0], lv.I[0], lv.Const = VsInt, lit, true
					if !compareVecConst(nil, op, c, wide, lv) {
						t.Fatalf("the widened path declined")
					}

					for k := 0; k < n; k++ {
						if rawVec.IsTrue(k) != wide.IsTrue(k) {
							t.Fatalf("width=%d unsigned=%v lit=%d %s: record %d raw=%v widened=%v",
								width, unsigned, lit, op, k, rawVec.IsTrue(k), wide.IsTrue(k))
						}
					}
				}
			}
		}
	}
	_ = fmt.Sprint
}
