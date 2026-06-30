package classad

import (
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
)

// This file reproduces HTCondor's ClassAdUnParser (src/classad/sink.cpp) for
// expression trees, so that string()/strcat()/etc. of a list or nested ad emit
// exactly what the reference engine does. The reference unparser echoes source
// parentheses verbatim and adds none of its own (hence the Parenthesized flag
// preserved by the parser); operators carry their spacing in the tables below.

// binaryOpUnparse maps a Go AST binary operator to its reference unparse
// spelling (with surrounding spaces). Note "is"/"isnt" cover =?= / =!=.
var binaryOpUnparse = map[string]string{
	"<": " < ", "<=": " <= ", "!=": " != ", "==": " == ",
	">=": " >= ", ">": " > ", "is": " is ", "isnt": " isnt ",
	"+": " + ", "-": " - ", "*": " * ", "/": " / ", "%": " % ",
	"||": " || ", "&&": " && ",
	"|": " | ", "^": " ^ ", "&": " & ",
	"<<": " << ", ">>": " >> ", ">>>": " >>> ",
}

// unaryOpUnparse maps a Go AST unary operator to its reference spelling, which
// carries a leading space (so "(!a)" unparses as "( !a)").
var unaryOpUnparse = map[string]string{
	"-": " -", "+": " +", "!": " !", "~": " ~",
}

// unparseExprString renders an AST expression in reference (sink) form.
func unparseExprString(e ast.Expr) string {
	var b strings.Builder
	unparseExpr(&b, e)
	return b.String()
}

func unparseExpr(b *strings.Builder, e ast.Expr) {
	switch v := e.(type) {
	case *ast.IntegerLiteral:
		b.WriteString(strconv.FormatInt(v.Value, 10))
	case *ast.RealLiteral:
		b.WriteString(classadReal(v.Value))
	case *ast.StringLiteral:
		b.WriteString(unparseString(v.Value))
	case *ast.BooleanLiteral:
		if v.Value {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case *ast.UndefinedLiteral:
		b.WriteString("undefined")
	case *ast.ErrorLiteral:
		b.WriteString("error")
	case *ast.AttributeReference:
		switch v.Scope {
		case ast.MyScope:
			b.WriteString("MY.")
		case ast.TargetScope:
			b.WriteString("TARGET.")
		case ast.ParentScope:
			b.WriteString("PARENT.")
		}
		b.WriteString(v.Name)
	case *ast.BinaryOp:
		paren(b, v.Parenthesized, func() {
			unparseExpr(b, v.Left)
			b.WriteString(binaryOpUnparse[v.Op])
			unparseExpr(b, v.Right)
		})
	case *ast.UnaryOp:
		paren(b, v.Parenthesized, func() { unparseUnary(b, v) })
	case *ast.ConditionalExpr:
		paren(b, v.Parenthesized, func() {
			unparseExpr(b, v.Condition)
			b.WriteString(" ? ")
			unparseExpr(b, v.TrueExpr)
			b.WriteString(" : ")
			unparseExpr(b, v.FalseExpr)
		})
	case *ast.ElvisExpr:
		paren(b, v.Parenthesized, func() {
			unparseExpr(b, v.Left)
			b.WriteString(" ?: ")
			unparseExpr(b, v.Right)
		})
	case *ast.ListLiteral:
		b.WriteString("{ ")
		for i, el := range v.Elements {
			if i > 0 {
				b.WriteByte(',')
			}
			unparseExpr(b, el)
		}
		b.WriteString(" }")
	case *ast.FunctionCall:
		b.WriteString(v.Name)
		b.WriteByte('(')
		for i, a := range v.Args {
			if i > 0 {
				b.WriteByte(',')
			}
			unparseExpr(b, a)
		}
		b.WriteByte(')')
	case *ast.SubscriptExpr:
		unparseExpr(b, v.Container)
		b.WriteByte('[')
		unparseExpr(b, v.Index)
		b.WriteByte(']')
	case *ast.SelectExpr:
		unparseExpr(b, v.Record)
		b.WriteByte('.')
		b.WriteString(v.Attr)
	case *ast.RecordLiteral:
		unparseRecord(b, v.ClassAd)
	default:
		b.WriteString("error")
	}
}

func paren(b *strings.Builder, on bool, body func()) {
	if on {
		b.WriteByte('(')
	}
	body()
	if on {
		b.WriteByte(')')
	}
}

// unparseUnary handles the reference engine's special-case folding of a unary
// +/- applied directly to a numeric literal into a signed literal ("(-5)"),
// while any other unary operand keeps the leading-space operator ("( -a)").
func unparseUnary(b *strings.Builder, v *ast.UnaryOp) {
	if v.Op == "-" || v.Op == "+" {
		switch lit := v.Expr.(type) {
		case *ast.IntegerLiteral:
			n := lit.Value
			if v.Op == "-" {
				n = -n
			}
			b.WriteString(strconv.FormatInt(n, 10))
			return
		case *ast.RealLiteral:
			r := lit.Value
			if v.Op == "-" {
				r = -r
			}
			b.WriteString(classadReal(r))
			return
		}
	}
	b.WriteString(unaryOpUnparse[v.Op])
	unparseExpr(b, v.Expr)
}

// unparseRecord renders a nested ClassAd literal as "[ a = x; b = y ]".
func unparseRecord(b *strings.Builder, ad *ast.ClassAd) {
	b.WriteString("[ ")
	for i, attr := range ad.Attributes {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(attr.Name)
		b.WriteString(" = ")
		unparseExpr(b, attr.Value)
	}
	b.WriteString(" ]")
}
