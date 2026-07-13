package classad

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// TestNormalizedNameCached verifies NewAttributeReference precomputes the folded
// name and that a struct-literal node falls back to on-demand folding.
func TestNormalizedNameCached(t *testing.T) {
	if got := ast.NewAttributeReference("FooBar", ast.NoScope).NormalizedName(); got != "foobar" {
		t.Errorf("NewAttributeReference(%q).NormalizedName() = %q, want %q", "FooBar", got, "foobar")
	}
	// A node built with a struct literal has no precomputed norm; NormalizedName
	// must still fold on demand.
	if got := (&ast.AttributeReference{Name: "MixEd"}).NormalizedName(); got != "mixed" {
		t.Errorf("struct-literal NormalizedName() = %q, want %q", got, "mixed")
	}
}

// TestEvaluateAttrCaseFoldNoAlloc guards the matchmaking read hot path: resolving
// an attribute by a mixed-case name must (a) be case-insensitive and (b) not
// allocate — lookupInternal folds into a stack buffer and indexes the map with
// string(buf) instead of calling strings.ToLower per lookup. A regression here
// (e.g. reverting to normalizeName) reintroduces ~one allocation per attribute
// reference per candidate, which at 100k slots is hundreds of thousands of
// allocations per scan.
func TestEvaluateAttrCaseFoldNoAlloc(t *testing.T) {
	ad := New()
	ad.InsertAttr("RequestCpus", 4)

	// Case-insensitive resolution through the folded lookup.
	if v := ad.EvaluateAttr("requestcpus"); !v.IsInteger() {
		t.Fatalf("EvaluateAttr(lowercased) did not resolve; got %v", v)
	}
	if v, ok := ad.EvaluateAttrInt("REQUESTCPUS"); !ok || v != 4 {
		t.Fatalf("EvaluateAttrInt(uppercased) = %d, ok=%v; want 4, true", v, ok)
	}

	// Warm the attribute index, then assert the lookup+eval allocates nothing.
	_ = ad.EvaluateAttr("RequestCpus")
	if allocs := testing.AllocsPerRun(200, func() {
		_ = ad.EvaluateAttr("RequestCpus")
	}); allocs > 0 {
		t.Errorf("EvaluateAttr allocated %v/run, want 0 (lookupInternal must fold without strings.ToLower)", allocs)
	}
}
