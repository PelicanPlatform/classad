// Package ast defines the Abstract Syntax Tree node types for ClassAds expressions.
package ast

import "fmt"

// Node is the base interface for all AST nodes.
type Node interface {
	String() string
}

// Expr represents a ClassAd expression.
type Expr interface {
	Node
	exprNode()
}

// ClassAd represents a complete ClassAd (a record of attribute assignments).
type ClassAd struct {
	Attributes []*AttributeAssignment
}

func (c *ClassAd) String() string {
	result := "["
	for i, attr := range c.Attributes {
		if i > 0 {
			result += "; "
		}
		result += attr.String()
	}
	result += "]"
	return result
}

func (c *ClassAd) exprNode() {}

// AttributeAssignment represents an attribute assignment (name = expr).
type AttributeAssignment struct {
	Name  string
	Value Expr
}

func (a *AttributeAssignment) String() string {
	return fmt.Sprintf("%s = %s", a.Name, a.Value.String())
}

// IntegerLiteral represents an integer constant.
type IntegerLiteral struct {
	Value int64
}

func (i *IntegerLiteral) String() string {
	return fmt.Sprintf("%d", i.Value)
}

func (i *IntegerLiteral) exprNode() {}

// RealLiteral represents a floating-point constant.
type RealLiteral struct {
	Value float64
}

func (r *RealLiteral) String() string {
	return fmt.Sprintf("%g", r.Value)
}

func (r *RealLiteral) exprNode() {}

// StringLiteral represents a string constant.
type StringLiteral struct {
	Value string
}

func (s *StringLiteral) String() string {
	return fmt.Sprintf("\"%s\"", s.Value)
}

func (s *StringLiteral) exprNode() {}

// BooleanLiteral represents a boolean constant (true or false).
type BooleanLiteral struct {
	Value bool
}

func (b *BooleanLiteral) String() string {
	if b.Value {
		return "true"
	}
	return "false"
}

func (b *BooleanLiteral) exprNode() {}

// UndefinedLiteral represents an undefined value.
type UndefinedLiteral struct{}

func (u *UndefinedLiteral) String() string {
	return "undefined"
}

func (u *UndefinedLiteral) exprNode() {}

// ErrorLiteral represents an error value.
type ErrorLiteral struct{}

func (e *ErrorLiteral) String() string {
	return "error"
}

func (e *ErrorLiteral) exprNode() {}

// AttributeReference represents a reference to an attribute by name.
type AttributeReference struct {
	Name string
}

func (a *AttributeReference) String() string {
	return a.Name
}

func (a *AttributeReference) exprNode() {}

// BinaryOp represents a binary operation (e.g., +, -, *, /, &&, ||, ==, etc.).
type BinaryOp struct {
	Op    string
	Left  Expr
	Right Expr
}

func (b *BinaryOp) String() string {
	return fmt.Sprintf("(%s %s %s)", b.Left.String(), b.Op, b.Right.String())
}

func (b *BinaryOp) exprNode() {}

// UnaryOp represents a unary operation (e.g., -, !, ~).
type UnaryOp struct {
	Op   string
	Expr Expr
}

func (u *UnaryOp) String() string {
	return fmt.Sprintf("(%s%s)", u.Op, u.Expr.String())
}

func (u *UnaryOp) exprNode() {}

// ListLiteral represents a list of expressions.
type ListLiteral struct {
	Elements []Expr
}

func (l *ListLiteral) String() string {
	result := "{"
	for i, elem := range l.Elements {
		if i > 0 {
			result += ", "
		}
		result += elem.String()
	}
	result += "}"
	return result
}

func (l *ListLiteral) exprNode() {}

// RecordLiteral represents a record (nested ClassAd).
type RecordLiteral struct {
	ClassAd *ClassAd
}

func (r *RecordLiteral) String() string {
	return r.ClassAd.String()
}

func (r *RecordLiteral) exprNode() {}

// FunctionCall represents a function call with arguments.
type FunctionCall struct {
	Name string
	Args []Expr
}

func (f *FunctionCall) String() string {
	result := fmt.Sprintf("%s(", f.Name)
	for i, arg := range f.Args {
		if i > 0 {
			result += ", "
		}
		result += arg.String()
	}
	result += ")"
	return result
}

func (f *FunctionCall) exprNode() {}

// ConditionalExpr represents a ternary conditional expression (cond ? true_expr : false_expr).
type ConditionalExpr struct {
	Condition Expr
	TrueExpr  Expr
	FalseExpr Expr
}

func (c *ConditionalExpr) String() string {
	return fmt.Sprintf("(%s ? %s : %s)", c.Condition.String(), c.TrueExpr.String(), c.FalseExpr.String())
}

func (c *ConditionalExpr) exprNode() {}

// SelectExpr represents attribute selection (expr.attr).
type SelectExpr struct {
	Record Expr
	Attr   string
}

func (s *SelectExpr) String() string {
	return fmt.Sprintf("%s.%s", s.Record.String(), s.Attr)
}

func (s *SelectExpr) exprNode() {}

// SubscriptExpr represents list/record subscripting (expr[index]).
type SubscriptExpr struct {
	Container Expr
	Index     Expr
}

func (s *SubscriptExpr) String() string {
	return fmt.Sprintf("%s[%s]", s.Container.String(), s.Index.String())
}

func (s *SubscriptExpr) exprNode() {}
