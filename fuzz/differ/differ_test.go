//go:build libclassad

package differ

import "testing"

// TestCyclicSelfRefNonDivergence pins the invariant for a cyclic self-reference
// (`A0 = A0`, reached here through a ternary's lazy false branch): it must never
// count as a divergence, regardless of which libclassad the harness is built
// against. The Go engine detects the cycle and returns `error`; libclassad
// returns `undefined` on a version that has the cyclic-eval fix (classified as a
// KnownQuirk) or infinite-loops on one that does not (classified as CppTimeout).
// Both are non-divergences. Neither engine may hang the differ.
func TestCyclicSelfRefNonDivergence(t *testing.T) {
	r := Compare(`[A0=0?e:A0]`, DefaultOptions())
	if r.IsDivergence() {
		t.Fatalf("cyclic self-reference reported as a divergence: category=%v go=%q cpp=%q detail=%s",
			r.Category, r.GoRaw, r.CppRaw, r.Detail)
	}
	if r.Category != KnownQuirk && r.Category != CppTimeout {
		t.Errorf("category = %v, want known-quirk (fixed libclassad) or cpp-timeout (unfixed); cpp=%q",
			r.Category, r.CppRaw)
	}
	// The Go engine must parse it and resolve the cycle to error, never hang or panic.
	if !r.GoParsed {
		t.Errorf("Go should parse the input")
	}
}
