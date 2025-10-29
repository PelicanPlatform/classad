package ast

import (
	"testing"
)

func TestIntegerLiteralString(t *testing.T) {
	i := &IntegerLiteral{Value: 42}
	if s := i.String(); s != "42" {
		t.Errorf("IntegerLiteral.String() = %q, want %q", s, "42")
	}
}

func TestRealLiteralString(t *testing.T) {
	r := &RealLiteral{Value: 3.14}
	if s := r.String(); s != "3.14" {
		t.Errorf("RealLiteral.String() = %q, want %q", s, "3.14")
	}
}

func TestStringLiteralString(t *testing.T) {
	s := &StringLiteral{Value: "hello"}
	if str := s.String(); str != `"hello"` {
		t.Errorf("StringLiteral.String() = %q, want %q", str, `"hello"`)
	}
}

func TestBooleanLiteralString(t *testing.T) {
	tests := []struct {
		value bool
		want  string
	}{
		{true, "true"},
		{false, "false"},
	}

	for _, tt := range tests {
		b := &BooleanLiteral{Value: tt.value}
		if s := b.String(); s != tt.want {
			t.Errorf("BooleanLiteral{%v}.String() = %q, want %q", tt.value, s, tt.want)
		}
	}
}

func TestUndefinedLiteralString(t *testing.T) {
	u := &UndefinedLiteral{}
	if s := u.String(); s != "undefined" {
		t.Errorf("UndefinedLiteral.String() = %q, want %q", s, "undefined")
	}
}

func TestErrorLiteralString(t *testing.T) {
	e := &ErrorLiteral{}
	if s := e.String(); s != "error" {
		t.Errorf("ErrorLiteral.String() = %q, want %q", s, "error")
	}
}

func TestAttributeReferenceString(t *testing.T) {
	a := &AttributeReference{Name: "myAttr"}
	if s := a.String(); s != "myAttr" {
		t.Errorf("AttributeReference.String() = %q, want %q", s, "myAttr")
	}
}

func TestBinaryOpString(t *testing.T) {
	b := &BinaryOp{
		Op:    "+",
		Left:  &IntegerLiteral{Value: 2},
		Right: &IntegerLiteral{Value: 3},
	}
	if s := b.String(); s != "(2 + 3)" {
		t.Errorf("BinaryOp.String() = %q, want %q", s, "(2 + 3)")
	}
}

func TestUnaryOpString(t *testing.T) {
	u := &UnaryOp{
		Op:   "-",
		Expr: &IntegerLiteral{Value: 5},
	}
	if s := u.String(); s != "(-5)" {
		t.Errorf("UnaryOp.String() = %q, want %q", s, "(-5)")
	}
}

func TestListLiteralString(t *testing.T) {
	l := &ListLiteral{
		Elements: []Expr{
			&IntegerLiteral{Value: 1},
			&IntegerLiteral{Value: 2},
			&IntegerLiteral{Value: 3},
		},
	}
	if s := l.String(); s != "{1, 2, 3}" {
		t.Errorf("ListLiteral.String() = %q, want %q", s, "{1, 2, 3}")
	}
}

func TestClassAdString(t *testing.T) {
	c := &ClassAd{
		Attributes: []*AttributeAssignment{
			{Name: "x", Value: &IntegerLiteral{Value: 1}},
			{Name: "y", Value: &IntegerLiteral{Value: 2}},
		},
	}
	want := "[x = 1; y = 2]"
	if s := c.String(); s != want {
		t.Errorf("ClassAd.String() = %q, want %q", s, want)
	}
}

func TestFunctionCallString(t *testing.T) {
	f := &FunctionCall{
		Name: "strcat",
		Args: []Expr{
			&StringLiteral{Value: "hello"},
			&StringLiteral{Value: "world"},
		},
	}
	want := `strcat("hello", "world")`
	if s := f.String(); s != want {
		t.Errorf("FunctionCall.String() = %q, want %q", s, want)
	}
}

func TestConditionalExprString(t *testing.T) {
	c := &ConditionalExpr{
		Condition: &BooleanLiteral{Value: true},
		TrueExpr:  &IntegerLiteral{Value: 1},
		FalseExpr: &IntegerLiteral{Value: 0},
	}
	want := "(true ? 1 : 0)"
	if s := c.String(); s != want {
		t.Errorf("ConditionalExpr.String() = %q, want %q", s, want)
	}
}

func TestSelectExprString(t *testing.T) {
	s := &SelectExpr{
		Record: &AttributeReference{Name: "obj"},
		Attr:   "field",
	}
	want := "obj.field"
	if str := s.String(); str != want {
		t.Errorf("SelectExpr.String() = %q, want %q", str, want)
	}
}

func TestSubscriptExprString(t *testing.T) {
	s := &SubscriptExpr{
		Container: &AttributeReference{Name: "list"},
		Index:     &IntegerLiteral{Value: 0},
	}
	want := "list[0]"
	if str := s.String(); str != want {
		t.Errorf("SubscriptExpr.String() = %q, want %q", str, want)
	}
}

func TestRecordLiteralString(t *testing.T) {
	r := &RecordLiteral{
		ClassAd: &ClassAd{
			Attributes: []*AttributeAssignment{
				{Name: "a", Value: &IntegerLiteral{Value: 1}},
			},
		},
	}
	want := "[a = 1]"
	if s := r.String(); s != want {
		t.Errorf("RecordLiteral.String() = %q, want %q", s, want)
	}
}
