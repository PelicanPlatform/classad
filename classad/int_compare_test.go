package classad

import (
	"fmt"
	"math"
	"testing"
)

// The reference engine's Operation::doComparison runs both operands through coerceToNumber, which
// turns booleans into integers and promotes an integer to a double only when the other operand is a
// real. Two integers then reach compareIntegers and compare as long long; an int/real or real/real
// pair reaches compareReals and compares as double. =?= and =!= skip the promotion: different types
// are simply not identical.
//
// So past 2^53, where neighboring integers share a double, an integer comparison must still tell them
// apart, and an integer compared with a real must not.

// cmpOperand is one comparison operand: an integer, a real, or a boolean.
type cmpOperand struct {
	kind byte // 'i', 'r' or 'b'
	i    int64
	r    float64
	b    bool
}

func (o cmpOperand) String() string {
	switch o.kind {
	case 'i':
		return fmt.Sprintf("%d", o.i)
	case 'r':
		return fmt.Sprintf("%.17g", o.r)
	}
	return fmt.Sprintf("%v", o.b)
}

func (o cmpOperand) insert(ad *ClassAd, name string) {
	switch o.kind {
	case 'i':
		ad.InsertAttr(name, o.i)
	case 'r':
		ad.InsertAttrFloat(name, o.r)
	default:
		ad.InsertAttrBool(name, o.b)
	}
}

// refCompare restates doComparison / coerceToNumber / compareIntegers / compareReals for numeric and
// boolean operands, independently of the evaluator under test.
func refCompare(op string, a, b cmpOperand) bool {
	if op == "=?=" || op == "=!=" {
		same := a.kind == b.kind
		if same {
			switch a.kind {
			case 'i':
				same = a.i == b.i
			case 'r':
				same = a.r == b.r
			default:
				same = a.b == b.b
			}
		}
		return same == (op == "=?=")
	}
	toInt := func(o cmpOperand) cmpOperand {
		if o.kind == 'b' {
			o.kind = 'i'
			if o.b {
				o.i = 1
			}
		}
		return o
	}
	a, b = toInt(a), toInt(b)
	if a.kind == 'i' && b.kind == 'i' {
		switch op {
		case "<":
			return a.i < b.i
		case "<=":
			return a.i <= b.i
		case ">":
			return a.i > b.i
		case ">=":
			return a.i >= b.i
		case "==":
			return a.i == b.i
		}
		return a.i != b.i
	}
	x, y := a.r, b.r
	if a.kind == 'i' {
		x = float64(a.i)
	}
	if b.kind == 'i' {
		y = float64(b.i)
	}
	switch op {
	case "<":
		return x < y
	case "<=":
		return x <= y
	case ">":
		return x > y
	case ">=":
		return x >= y
	case "==":
		return x == y
	}
	return x != y
}

// intCompareOperands sits on the boundaries where float64 stops telling integers apart: 2^53,
// 2^63 and their negatives, with the reals those integers round to.
func intCompareOperands() []cmpOperand {
	const p53 = int64(1) << 53
	var out []cmpOperand
	for _, i := range []int64{
		math.MinInt64, math.MinInt64 + 1, -p53 - 3, -p53 - 2, -p53 - 1, -p53, -p53 + 1,
		-1, 0, 1, p53 - 1, p53, p53 + 1, p53 + 2, p53 + 3, 1<<62 + 1, math.MaxInt64 - 1, math.MaxInt64,
	} {
		out = append(out, cmpOperand{kind: 'i', i: i})
	}
	for _, r := range []float64{
		-math.Pow(2, 63), -math.Pow(2, 53) - 2, -math.Pow(2, 53), 0, 0.5, 1,
		math.Pow(2, 53), math.Pow(2, 53) + 2, math.Pow(2, 53) + 4, math.Pow(2, 63),
	} {
		out = append(out, cmpOperand{kind: 'r', r: r})
	}
	return append(out, cmpOperand{kind: 'b', b: true}, cmpOperand{kind: 'b'})
}

var intCompareOps = []string{"<", "<=", ">", ">=", "==", "!=", "=?=", "=!="}

// TestIntCompareMatchesReference evaluates every operator over every operand pair through the
// tree-walking evaluator and checks it against the reference's rule.
func TestIntCompareMatchesReference(t *testing.T) {
	operands := intCompareOperands()
	exprs := make(map[string]*Expr, len(intCompareOps))
	for _, op := range intCompareOps {
		e, err := ParseExpr("A " + op + " B")
		if err != nil {
			t.Fatal(err)
		}
		exprs[op] = e
	}
	for _, a := range operands {
		for _, b := range operands {
			ad := New()
			a.insert(ad, "A")
			b.insert(ad, "B")
			for _, op := range intCompareOps {
				got, err := ad.EvaluateExprWithTarget(exprs[op], nil).BoolValue()
				if err != nil {
					t.Fatalf("%v %s %v: not a boolean", a, op, b)
				}
				if want := refCompare(op, a, b); got != want {
					t.Errorf("%v %s %v = %v, reference gives %v", a, op, b, got, want)
				}
			}
		}
	}
}

// TestIntCompareLiterals covers the same rule with the operands written as literals, the form a
// constraint takes.
func TestIntCompareLiterals(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{"9007199254740993 > 9007199254740992", true},
		{"9007199254740992 < 9007199254740993", true},
		{"9007199254740993 <= 9007199254740992", false},
		{"9007199254740992 >= 9007199254740993", false},
		{"9007199254740993 == 9007199254740992", false},
		{"9007199254740993 != 9007199254740992", true},
		{"9223372036854775807 > 9223372036854775806", true},
		// An integer against a real promotes the integer: both sides are 2^53 as doubles.
		{"9007199254740993 > 9007199254740992.0", false},
		{"9007199254740993 == 9007199254740992.0", true},
		{"9007199254740993 =?= 9007199254740992.0", false},
	} {
		v := evalBuiltin(t, tc.expr)
		got, err := v.BoolValue()
		if err != nil || got != tc.want {
			t.Errorf("%s = %v, want %v", tc.expr, v, tc.want)
		}
	}
}

// TestListMinMaxIntExact pins min() and max() to the reference's minMax, which picks the extremum
// with the < and > operators and so compares two integers exactly.
func TestListMinMaxIntExact(t *testing.T) {
	const p53 = int64(1) << 53
	ints := func(vs ...int64) []Value {
		out := make([]Value, len(vs))
		for i, v := range vs {
			out[i] = NewIntValue(v)
		}
		return out
	}
	for _, tc := range []struct {
		vals     []Value
		min, max int64
	}{
		{ints(p53, p53+1), p53, p53 + 1},
		{ints(p53+1, p53), p53, p53 + 1},
		{ints(p53+3, p53+1, p53+2), p53 + 1, p53 + 3},
		{ints(math.MaxInt64-1, math.MaxInt64), math.MaxInt64 - 1, math.MaxInt64},
		{ints(math.MaxInt64, math.MaxInt64-1), math.MaxInt64 - 1, math.MaxInt64},
		{ints(-p53-1, -p53), -p53 - 1, -p53},
		{ints(math.MinInt64+1, math.MinInt64), math.MinInt64, math.MinInt64 + 1},
	} {
		for _, fn := range []struct {
			name string
			f    func([]Value) Value
			want int64
		}{{"min", Min, tc.min}, {"max", Max, tc.max}} {
			v := fn.f(tc.vals)
			got, err := v.IntValue()
			if err != nil || got != fn.want {
				t.Errorf("%s(%v) = %v, want %d", fn.name, tc.vals, v, fn.want)
			}
		}
	}
	// A real among the integers makes the result a real, as before.
	if v := Max([]Value{NewIntValue(p53 + 1), NewRealValue(0.5)}); !v.IsReal() {
		t.Errorf("max with a real = %v, want a real", v)
	}
}
