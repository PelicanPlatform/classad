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
		{"true > false", true, false},
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
	if v := evalBuiltin(t, `undefined == undefined`); !v.IsBool() {
		t.Fatalf("expected bool for undefined==undefined")
	} else if b, _ := v.BoolValue(); !b {
		t.Fatalf("expected undefined==undefined to be true")
	}

	if v := evalBuiltin(t, `true == true`); !v.IsBool() {
		t.Fatalf("expected bool for bool equality")
	} else if b, _ := v.BoolValue(); !b {
		t.Fatalf("expected true==true")
	}

	if v := evalBuiltin(t, `1 == 1.0000000001`); !v.IsBool() {
		t.Fatalf("expected bool for numeric near-equality")
	} else if b, _ := v.BoolValue(); !b {
		t.Fatalf("expected near-equal numeric values to compare true")
	}

	if v := evalBuiltin(t, `"a" == 1`); !v.IsBool() {
		t.Fatalf("expected bool for type-mismatch equality")
	} else if b, _ := v.BoolValue(); b {
		t.Fatalf("expected string vs int equality to be false")
	}
}
