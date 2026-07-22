package classad

import (
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
)

// This file reproduces HTCondor's ClassAdUnParser (src/classad/sink.cpp) for
// expression trees, so that string()/strcat()/etc. of a list or nested ad emit
// exactly what the reference engine does. The reference unparser echoes source
// parentheses verbatim and adds none of its own (hence the ast.ParenExpr nodes
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

// unparseExprString renders an AST expression in reference (sink) form (new ClassAd).
func unparseExprString(e ast.Expr) string {
	var b strings.Builder
	unparseExpr(&b, e, false)
	return b.String()
}

// unparseExprStringOld renders an AST expression in OLD-ClassAd form: identical to the
// reference form EXCEPT string literals use old-format escaping -- only the delimiter is
// escaped, and backslashes and control characters are emitted verbatim. This matches the
// C++ ClassAdUnParser with SetOldClassAd (src/classad/sink.cpp: the escape switch is
// skipped for oldClassAd) and the lenient old-format lexer, so a value like a Windows path
// or a regex round-trips unchanged instead of gaining a backslash each hop. See marshalOld.
func unparseExprStringOld(e ast.Expr) string {
	var b strings.Builder
	unparseExpr(&b, e, true)
	return b.String()
}

func unparseExpr(b *strings.Builder, e ast.Expr, old bool) {
	switch v := e.(type) {
	case *ast.IntegerLiteral:
		b.WriteString(strconv.FormatInt(v.Value, 10))
	case *ast.RealLiteral:
		b.WriteString(classadReal(v.Value))
	case *ast.StringLiteral:
		if old {
			b.Write(ast.AppendQuoteStringOld(nil, v.Value))
		} else {
			b.WriteString(unparseString(v.Value))
		}
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
		b.WriteString(unparseAttrName(v.Name))
	case *ast.ParenExpr:
		// Explicit source parentheses are echoed verbatim (around any
		// expression, and nested), matching the reference engine.
		b.WriteByte('(')
		unparseExpr(b, v.Inner, old)
		b.WriteByte(')')
	case *ast.BinaryOp:
		unparseExpr(b, v.Left, old)
		b.WriteString(binaryOpUnparse[v.Op])
		unparseExpr(b, v.Right, old)
	case *ast.UnaryOp:
		unparseUnary(b, v, old)
	case *ast.ConditionalExpr:
		unparseExpr(b, v.Condition, old)
		b.WriteString(" ? ")
		unparseExpr(b, v.TrueExpr, old)
		b.WriteString(" : ")
		unparseExpr(b, v.FalseExpr, old)
	case *ast.ElvisExpr:
		unparseExpr(b, v.Left, old)
		b.WriteString(" ?: ")
		unparseExpr(b, v.Right, old)
	case *ast.ListLiteral:
		b.WriteString("{ ")
		for i, el := range v.Elements {
			if i > 0 {
				b.WriteByte(',')
			}
			unparseExpr(b, el, old)
		}
		b.WriteString(" }")
	case *ast.FunctionCall:
		b.WriteString(v.Name)
		b.WriteByte('(')
		for i, a := range v.Args {
			if i > 0 {
				b.WriteByte(',')
			}
			unparseExpr(b, a, old)
		}
		b.WriteByte(')')
	case *ast.SubscriptExpr:
		unparseExpr(b, v.Container, old)
		b.WriteByte('[')
		unparseExpr(b, v.Index, old)
		b.WriteByte(']')
	case *ast.SelectExpr:
		unparseExpr(b, v.Record, old)
		// A bare numeric literal before '.' would re-lex as a real ("0.A"),
		// so separate it with a space. The selected attribute is quoted when
		// it is not a bare identifier (e.g. a keyword like 'true').
		switch v.Record.(type) {
		case *ast.IntegerLiteral, *ast.RealLiteral:
			b.WriteByte(' ')
		}
		b.WriteByte('.')
		b.WriteString(unparseAttrName(v.Attr))
	case *ast.RecordLiteral:
		unparseRecord(b, v.ClassAd, old)
	default:
		b.WriteString("error")
	}
}

// unparseUnary handles the reference engine's special-case folding of a unary
// +/- applied directly to a numeric literal into a signed literal ("(-5)"),
// while any other unary operand keeps the leading-space operator ("( -a)").
func unparseUnary(b *strings.Builder, v *ast.UnaryOp, old bool) {
	// Only unary MINUS on a numeric literal folds into a signed literal
	// ("(-5)"): the reference lexer makes "-5" a negative literal but keeps
	// "+5" as a unary plus, so unary plus is never folded (and a non-literal
	// operand keeps the leading-space operator, e.g. "( -a)", "( +0)").
	if v.Op == "-" {
		switch lit := v.Expr.(type) {
		case *ast.IntegerLiteral:
			// A bare negative integer literal only comes from the parser's
			// -9223372036854775808 (INT64_MIN) rule. The reference does not
			// fold a further unary minus onto it, so keep the leading-space
			// operator ("- -9223372036854775808") instead of collapsing it.
			if lit.Value >= 0 {
				b.WriteString(strconv.FormatInt(-lit.Value, 10))
				return
			}
		case *ast.RealLiteral:
			b.WriteString(classadReal(-lit.Value))
			return
		}
	}
	b.WriteString(unaryOpUnparse[v.Op])
	unparseExpr(b, v.Expr, old)
}

// unparseRecord renders a nested ClassAd literal as "[ a = x; b = y ]".
func unparseRecord(b *strings.Builder, ad *ast.ClassAd, old bool) {
	b.WriteString("[ ")
	for i, attr := range ad.Attributes {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(unparseAttrName(attr.Name))
		b.WriteString(" = ")
		unparseExpr(b, attr.Value, old)
	}
	b.WriteString(" ]")
}
