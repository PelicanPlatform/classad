package classad

import "testing"

// TestExprUnparseIsTheReferenceForm pins the two ways String differs from the
// reference unparser. Both were found by a differential fuzzer comparing Go
// job ads against HTCondor's own: a whole-number real printed by String comes
// back as an integer, and String's parentheses do not match what HTCondor
// writes.
func TestExprUnparseIsTheReferenceForm(t *testing.T) {
	for _, c := range []struct {
		src         string
		wantUnparse string
		wantString  string
	}{
		// A real that happens to be whole. %g drops the decimal point, so
		// String's output re-parses as an integer -- a different ClassAd type.
		{"8.0", "8.000000000000000E+00", "8"},
		{"8.5", "8.500000000000000E+00", "8.5"},
		// Zero is the reference unparser's one special case.
		{"0.0", "0.0", "0"},
		// An integer stays an integer either way.
		{"8", "8", "8"},
		// String parenthesises every binary operation; the reference does not.
		{"1 + 2", "1 + 2", "(1 + 2)"},
	} {
		e, err := ParseExpr(c.src)
		if err != nil {
			t.Errorf("ParseExpr(%q): %v", c.src, err)
			continue
		}
		if got := e.Unparse(); got != c.wantUnparse {
			t.Errorf("ParseExpr(%q).Unparse() = %q, want %q", c.src, got, c.wantUnparse)
		}
		if got := e.String(); got != c.wantString {
			t.Errorf("ParseExpr(%q).String() = %q, want %q", c.src, got, c.wantString)
		}
	}
}

// TestExprUnparseRoundTripsAsTheSameType is the consequence that matters: what
// Unparse writes must parse back as the type it started as.
func TestExprUnparseRoundTripsAsTheSameType(t *testing.T) {
	for _, src := range []string{"8.0", "0.0", "-3.0", "1e3"} {
		e, err := ParseExpr(src)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", src, err)
		}
		back, err := ParseExpr(e.Unparse())
		if err != nil {
			t.Errorf("reparsing %q from %q: %v", e.Unparse(), src, err)
			continue
		}
		if !back.Eval(New()).IsReal() {
			t.Errorf("%q unparsed to %q, which parses back as a non-real", src, e.Unparse())
		}
	}
}

// TestExprUnparseNilIsUndefined: a nil expression is undefined, not a panic.
func TestExprUnparseNilIsUndefined(t *testing.T) {
	var e *Expr
	if got := e.Unparse(); got != "undefined" {
		t.Errorf("(*Expr)(nil).Unparse() = %q, want %q", got, "undefined")
	}
	if got := e.UnparseOld(); got != "undefined" {
		t.Errorf("(*Expr)(nil).UnparseOld() = %q, want %q", got, "undefined")
	}
}
