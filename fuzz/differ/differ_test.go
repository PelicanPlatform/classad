package differ

import "testing"

// TestCppTimeoutOnCyclicHang guards that a cyclic self-reference that makes
// libclassad infinite-loop (a known C++ bug; the Go engine resolves it to
// error) is reported as CppTimeout and treated as a non-divergence, rather than
// hanging the differ. This test takes ~cppEvalTimeout to run.
func TestCppTimeoutOnCyclicHang(t *testing.T) {
	r := Compare(`[A0=0?e:A0]`, DefaultOptions())
	if r.Category != CppTimeout {
		t.Fatalf("category = %v, want cpp-timeout (cpp=%q)", r.Category, r.CppRaw)
	}
	if r.IsDivergence() {
		t.Errorf("CppTimeout must not count as a divergence")
	}
	// The Go engine should have resolved the cycle to error, not hung.
	if !r.GoParsed {
		t.Errorf("Go should parse the input")
	}
}
