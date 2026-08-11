//go:build !goexperiment.simd

package vm

// rawSupported is false, so the compiler deletes every raw-column branch in the executor: without an engine
// the representation has no use, and the checks alone measured 0.91x to 0.94x on the arithmetic shapes.
const rawSupported = false

// simdBaseMask declines without the simd experiment, leaving the width-native scalar kernel to answer.
//
// This is the build every ordinary toolchain takes: simd/archsimd exists only under GOEXPERIMENT=simd, and
// only from Go 1.26 on amd64 and Go 1.27 on arm64.
func simdBaseMask(cmpBase, []byte, int, bool, int64, int, []uint64) bool { return false }

func simdSupportsWidth(int, bool) bool { return false }
