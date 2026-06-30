package classad

import "testing"

func TestEvaluatorComparisonOperators(t *testing.T) {
	cases := []struct {
		expr      string
		expectErr bool
		expectVal bool
	}{
		{"3 > 2", false, true},
		{"2 <= 2", false, true},
		{"1 >= 2", false, false},
		{"\"b\" <= \"a\"", false, false},
		{"\"b\" >= \"a\"", false, true},
		// Booleans are numeric (true=1, false=0) in the reference engine, so
		// this is 1 > 0 == true, not an error.
		{"true > false", false, true},
		{"1 >= \"a\"", true, false},
		{"\"c\" > \"b\"", false, true},
	}

	for _, tc := range cases {
		val := evalBuiltin(t, tc.expr)
		if tc.expectErr {
			if !val.IsError() {
				t.Fatalf("expected error for %s", tc.expr)
			}
			continue
		}

		if val.IsError() || val.IsUndefined() {
			t.Fatalf("unexpected non-boolean result for %s: %v", tc.expr, val.Type())
		}
		b, _ := val.BoolValue()
		if b != tc.expectVal {
			t.Fatalf("unexpected value for %s: got %v want %v", tc.expr, b, tc.expectVal)
		}
	}
}

func TestEvaluatorEqualityUndefined(t *testing.T) {
	val := evalBuiltin(t, `1 == undefined`)
	if !val.IsUndefined() {
		t.Fatalf("expected undefined result when comparing to undefined, got %v", val.Type())
	}
}

func TestEvaluatorEqualityNonScalar(t *testing.T) {
	val := evalBuiltin(t, `{1,2} == {1,2}`)
	if !val.IsError() {
		t.Fatalf("expected error when comparing non-scalar values, got %v", val.Type())
	}
}

func TestEvaluatorEqualityVariants(t *testing.T) {
	// Regular == propagates undefined even when both sides are undefined
	// (identity =?= is the operator that yields true here).
	if v := evalBuiltin(t, `undefined == undefined`); !v.IsUndefined() {
		t.Fatalf("expected undefined for undefined==undefined, got %v", v.Type())
	}
	if v := evalBuiltin(t, `undefined =?= undefined`); !v.IsBool() {
		t.Fatalf("expected bool for undefined=?=undefined")
	} else if b, _ := v.BoolValue(); !b {
		t.Fatalf("expected undefined=?=undefined to be true")
	}

	if v := evalBuiltin(t, `true == true`); !v.IsBool() {
		t.Fatalf("expected bool for bool equality")
	} else if b, _ := v.BoolValue(); !b {
		t.Fatalf("expected true==true")
	}

	// Reals compare with exact IEEE equality (no tolerance).
	if v := evalBuiltin(t, `1 == 1.0000000001`); !v.IsBool() {
		t.Fatalf("expected bool for numeric equality")
	} else if b, _ := v.BoolValue(); b {
		t.Fatalf("expected 1 == 1.0000000001 to be false (exact comparison)")
	}

	// Comparing a string to a non-string is an error in the reference engine,
	// not false.
	if v := evalBuiltin(t, `"a" == 1`); !v.IsError() {
		t.Fatalf("expected error for string vs int equality, got %v", v.Type())
	}
}
