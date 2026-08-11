package vm

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// HOW THE ENGINES TURN ON, and the one way that can silently stop working.
//
// There is no runtime switch and nothing to configure. An engine is selected entirely at BUILD time by the
// //go:build goexperiment.simd tag on simdengine_amd64.go and simdengine_arm64.go, and on amd64 by one CPU
// check for AVX2. So the first build on a supported platform picks it up by itself:
//
//	GOEXPERIMENT=simd go build ...   engine on  (Go 1.26+ amd64, Go 1.27+ arm64)
//	go build ...                     engine off, everything takes the scalar path
//
// THE RISK IS THE TAG, not the code. simd/archsimd is behind GOEXPERIMENT today; when it graduates, the
// goexperiment.simd tag stops being set and these files would quietly stop compiling in -- leaving a build
// that CAN vectorize running the scalar kernels, with nothing to say so. That failure is invisible: every
// answer stays correct and every query is just slower.
//
// So this is a tripwire. It fails on a toolchain newer than the one the tag was last verified against, on an
// architecture that has an engine, when no engine is active -- which is exactly the moment to check whether
// the build tag needs to become a version tag. Bump verifiedThroughGoMinor when you have checked.
const verifiedThroughGoMinor = 27

func TestSIMDEngineTurnsOnByItself(t *testing.T) {
	arch := runtime.GOARCH
	engineArch := arch == "amd64" || arch == "arm64"
	t.Logf("GOARCH=%s %s: engine compiled in = %v", arch, runtime.Version(), rawSupported)

	if rawSupported {
		// The engine is in. Confirm it actually answers for a width the store produces, so "compiled in" and
		// "usable" cannot drift apart.
		if !simdSupportsWidth(2, false) && !simdSupportsWidth(4, false) {
			t.Errorf("engine compiled in but supports neither a 2- nor a 4-byte column; on amd64 this is the "+
				"AVX2 check failing, which means this CPU (%s) falls back to scalar", arch)
		}
		return
	}

	minor, ok := goMinor(runtime.Version())
	if !ok {
		t.Skipf("cannot parse a Go minor version from %q", runtime.Version())
	}
	if engineArch && minor > verifiedThroughGoMinor {
		t.Errorf("Go 1.%d on %s has no SIMD engine compiled in, and the tag was only verified through Go 1.%d.\n"+
			"If simd/archsimd is no longer behind GOEXPERIMENT in this release, the //go:build goexperiment.simd "+
			"tag on simdengine_%s.go no longer matches and the engine is silently off. Check, then either add the "+
			"version tag or bump verifiedThroughGoMinor.",
			minor, arch, verifiedThroughGoMinor, arch)
	}
}

// goMinor parses the minor version out of a runtime version string: "go1.27", "go1.27rc2", "go1.28.3" all
// yield 27, 27, 28. Returns false for a devel or otherwise unparseable build.
func goMinor(v string) (int, bool) {
	v = strings.TrimPrefix(v, "go")
	if !strings.HasPrefix(v, "1.") {
		return 0, false
	}
	rest := v[2:]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}
