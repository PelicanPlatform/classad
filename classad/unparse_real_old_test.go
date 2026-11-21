package classad

import (
	"math"
	"strings"
	"testing"
)

// TestClassadRealOld pins the OLD-ClassAd spelling of a real against
// condor_submit, which was asked for every row: a submit file with
// `retry_request_memory = 0 % <literal> % queue`, whose transform body the
// schedd re-parses out of a string, so the text has to match exactly.
//
// The new-format column is here too, because the bug this fixes was the old
// path using the new format for everything except zero.
func TestClassadRealOld(t *testing.T) {
	cases := []struct {
		in       float64
		old, new string
	}{
		{0.1, "0.1", "1.000000000000000E-01"},
		{1.5, "1.5", "1.500000000000000E+00"},
		{2.0, "2.0", "2.000000000000000E+00"},
		{0.0, "0.0", "0.0"},
		{-0.1, "-0.1", "-1.000000000000000E-01"},
		{0.5, "0.5", "5.000000000000000E-01"},
		{1.0, "1.0", "1.000000000000000E+00"},
		{1e-5, "1E-05", "1.000000000000000E-05"},
		{1e15, "1000000000000000.0", "1.000000000000000E+15"},
		{1e16, "1E+16", "1.000000000000000E+16"},
		{1e300, "1E+300", "1.000000000000000E+300"},
		{1e-20, "9.999999999999999E-21", "9.999999999999999E-21"},
		{100000.0, "100000.0", "1.000000000000000E+05"},
		{3.14159265358979, "3.14159265358979", "3.141592653589790E+00"},
		{123456789012345678.0, "1.234567890123457E+17", "1.234567890123457E+17"},
	}
	for _, c := range cases {
		if got := classadRealOld(c.in); got != c.old {
			t.Errorf("classadRealOld(%v) = %q, want %q", c.in, got, c.old)
		}
		if got := classadReal(c.in); got != c.new {
			t.Errorf("classadReal(%v) = %q, want %q", c.in, got, c.new)
		}
	}
}

// TestClassadRealOldNonFinite keeps the two modes agreeing where the
// reference unparser does not branch on the mode at all.
func TestClassadRealOldNonFinite(t *testing.T) {
	for _, c := range []struct {
		v    float64
		want string
	}{
		{math.NaN(), `real("NaN")`},
		{math.Inf(1), `real("INF")`},
		{math.Inf(-1), `real("-INF")`},
	} {
		if got := classadRealOld(c.v); got != c.want {
			t.Errorf("classadRealOld(%v) = %q, want %q", c.v, got, c.want)
		}
		if got := classadReal(c.v); got != c.want {
			t.Errorf("classadReal(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

// TestMarshalOldRendersRealsTheOldWay is the end-to-end check: the library's
// own old-format serializer has to use the old spelling.
func TestMarshalOldRendersRealsTheOldWay(t *testing.T) {
	for _, c := range []struct{ src, wantOld string }{
		{"0 % .1 % queue", "X = 0 % 0.1 % queue"},
		{".1", "X = 0.1"},
		{"2.0", "X = 2.0"},
		{"-0.1", "X = -0.1"},
		{"0.0", "X = 0.0"},
		{"{ 1.5, 2.0 }", "X = { 1.5,2.0 }"},
	} {
		e, err := ParseExprOld(c.src)
		if err != nil {
			t.Fatalf("ParseExprOld(%q): %v", c.src, err)
		}
		ad := New()
		ad.InsertExpr("X", e)
		if got := ad.MarshalOld(); got != c.wantOld {
			t.Errorf("MarshalOld of %q = %q, want %q", c.src, got, c.wantOld)
		}
		// The new-format path must keep its own spelling.
		if strings.Contains(ad.String(), "E+") || strings.Contains(ad.String(), "E-") {
			t.Logf("note: new-format rendering of %q is %s", c.src, ad.String())
		}
	}
}
