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

	if got, ok := NewClassAdValue(New()).Unparse(); ok {
		t.Errorf("nested classad: Unparse() = %q, true; want ok=false", got)
	}
}
