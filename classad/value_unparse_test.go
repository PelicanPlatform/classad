package classad

import (
	"math"
	"testing"
)

func TestValueUnparse(t *testing.T) {
	list, err := ParseExpr("{1, 1+1, \"a\"}")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}

	tests := []struct {
		name string
		v    Value
		want string
	}{
		{"zero", NewRealValue(0), "0.0"},
		{"negative zero", NewRealValue(math.Copysign(0, -1)), "-0.0"},
		{"real", NewRealValue(1.5), "1.500000000000000E+00"},
		{"nan", NewRealValue(math.NaN()), `real("NaN")`},
		{"inf", NewRealValue(math.Inf(1)), `real("INF")`},
		{"int", NewIntValue(-7), "-7"},
		{"bool", NewBoolValue(true), "true"},
		{"string", NewStringValue("a\"b\n"), `"a\"b\n"`},
		{"undefined", NewUndefinedValue(), "undefined"},
		{"error", NewErrorValue(), "error"},
		{"list literal", list.Eval(nil), `{ 1,1 + 1,"a" }`},
	}
	for _, tt := range tests {
		got, ok := tt.v.Unparse()
		if !ok || got != tt.want {
			t.Errorf("%s: Unparse() = %q, %v; want %q, true", tt.name, got, ok, tt.want)
		}
	}

}

// TestValueUnparseNestedAd pins the sink form of a classad value.
//
// The reference unparser writes "[ " and " ]" whether or not anything
// goes between them, so an EMPTY ad is "[  ]", with two spaces -- which
// is what HTCondor's own $EVAL([]) expands to. Rendering it as "[]" made
// every config macro that evaluates to an ad differ from condor_config.
func TestValueUnparseNestedAd(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"empty", "[]", "[  ]"},
		{"one attribute", "[a = 1]", "[ a = 1 ]"},
		{"two attributes", "[a = 1; b = 2]", "[ a = 1; b = 2 ]"},
		// Attributes are unparsed as written, not evaluated.
		{"an expression", "[a = 1 + 1]", "[ a = 1 + 1 ]"},
		{"a nested list", "[a = {}]", "[ a = {  } ]"},
		{"a nested ad", "[a = []]", "[ a = [  ] ]"},
		{"a quoted name", `[ 'a b' = 1 ]`, `[ 'a b' = 1 ]`},
	}
	for _, tt := range tests {
		expr, err := ParseExpr(tt.src)
		if err != nil {
			t.Fatalf("%s: ParseExpr(%q): %v", tt.name, tt.src, err)
		}
		got, ok := expr.Eval(New()).Unparse()
		if !ok || got != tt.want {
			t.Errorf("%s: Unparse() = %q, %v; want %q, true", tt.name, got, ok, tt.want)
		}
	}

	// A list holding an ad renders its element the same way.
	expr, err := ParseExpr("{[a = 1]}")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	if got, ok := expr.Eval(New()).Unparse(); !ok || got != "{ [ a = 1 ] }" {
		t.Errorf("a list holding an ad: Unparse() = %q, %v", got, ok)
	}
}
