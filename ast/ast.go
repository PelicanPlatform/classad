// Package ast defines the Abstract Syntax Tree node types for ClassAds expressions.
package ast

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// QuoteAttributeName renders an attribute name so it re-parses: a valid bare
// identifier is returned as-is; anything else (empty, non-identifier
// characters, or a name that would lex as a reserved keyword/literal) is
// single-quoted with '\” and '\\' escaped, matching the lexer's
// quoted-attribute-name unescaping.
func QuoteAttributeName(name string) string {
	if isBareAttributeName(name) {
		return name
	}
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range name {
		if r == '\'' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// QuoteString renders a string value as a double-quoted ClassAd string literal
// that the lexer can read back: it uses only the escapes the lexer decodes
// (\b \f \n \r \t \\ \"), octal (\NNN) for other control characters, and emits
// everything else (printable ASCII and valid Unicode) verbatim. It deliberately
// avoids Go's \xNN hex escapes, which the lexer does not understand.
// stringNeedsEscape reports whether QuoteString must escape any byte of s: a
// quote, a backslash, or a control character (< 0x20). Bytes >= 0x20, including
// every byte of a multi-byte UTF-8 rune, pass through unescaped.
func stringNeedsEscape(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == '"' || c == '\\' {
			return true
		}
	}
	return false
}

func QuoteString(s string) string {
	// Fast path: almost every ClassAd string value (hostnames, arch/OS names,
	// states, versions, ...) contains nothing that needs escaping, so wrap it
	// directly in one allocation instead of walking it rune by rune into a
	// Builder. A byte scan suffices: the escaped set is a quote, a backslash, or a
	// control byte (< 0x20), none of which appear inside a multi-byte UTF-8 rune.
	if !stringNeedsEscape(s) {
		return `"` + s + `"`
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, "\\%03o", r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// AppendQuoteString appends QuoteString(s) to dst and returns the extended slice,
// so a caller building a larger buffer (e.g. an ad's "Name = Value" line) does not
// allocate an intermediate string per value.
func AppendQuoteString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	if !stringNeedsEscape(s) {
		dst = append(dst, s...)
		return append(dst, '"')
	}
	for _, r := range s {
		dst = appendEscapedRune(dst, r)
	}
	return append(dst, '"')
}

// AppendQuoteStringBytes is AppendQuoteString for a value that already lives in a
// byte slice (e.g. a string literal's bytes inside a wire buffer): it quotes s
// without first copying it into a Go string, producing output identical to
// AppendQuoteString(dst, string(s)). The common no-escape case is a plain byte
// copy; the escape path iterates runes via `range string(s)`, which the compiler
// lowers without allocating a string.
func AppendQuoteStringBytes(dst, s []byte) []byte {
	dst = append(dst, '"')
	if !bytesNeedEscape(s) {
		dst = append(dst, s...)
		return append(dst, '"')
	}
	for _, r := range string(s) {
		dst = appendEscapedRune(dst, r)
	}
	return append(dst, '"')
}

// appendEscapedRune appends one rune of a string value using only the escapes the
// lexer decodes (\b \f \n \r \t \\ \"), octal (\NNN) for other control characters,
// and the rune verbatim otherwise. Shared by the string and []byte quoters.
func appendEscapedRune(dst []byte, r rune) []byte {
	switch r {
	case '"':
		return append(dst, '\\', '"')
	case '\\':
		return append(dst, '\\', '\\')
	case '\b':
		return append(dst, '\\', 'b')
	case '\f':
		return append(dst, '\\', 'f')
	case '\n':
		return append(dst, '\\', 'n')
	case '\r':
		return append(dst, '\\', 'r')
	case '\t':
		return append(dst, '\\', 't')
	default:
		if r < 0x20 {
			return append(dst, fmt.Sprintf("\\%03o", r)...)
		}
		return utf8.AppendRune(dst, r)
	}
}

// bytesNeedEscape is stringNeedsEscape for a byte slice (a quote, a backslash, or
// a control byte < 0x20; bytes of a multi-byte UTF-8 rune are all >= 0x20).
func bytesNeedEscape(s []byte) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == '"' || c == '\\' {
			return true
		}
	}
	return false
}

// AppendQuoteAttributeName appends QuoteAttributeName(name) to dst and returns the
// extended slice, the append-to-buffer variant used when rendering many attribute
// names into a shared buffer (record keys, scoped/selected attr refs) without a
// per-name string allocation.
func AppendQuoteAttributeName(dst []byte, name string) []byte {
	if isBareAttributeName(name) {
		return append(dst, name...)
	}
	dst = append(dst, '\'')
	for _, r := range name {
		if r == '\'' || r == '\\' {
			dst = append(dst, '\\')
		}
		dst = utf8.AppendRune(dst, r)
	}
	return append(dst, '\'')
}

func isBareAttributeName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if i == 0 {
			if !unicode.IsLetter(r) && r != '_' {
				return false
			}
		} else if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return !isReservedWord(name)
}

// isReservedWord reports whether name is a ClassAd keyword/literal
// (true/false/undefined/error/is/isnt) compared case-insensitively. It avoids
// strings.ToLower, which allocates a lowercased copy of every mixed-case name --
// and this runs for every attribute reference in every rendered expression, so
// that copy was the dominant allocation on the query serialization path. The
// length switch rejects almost all names before any comparison; strings.EqualFold
// does the case-insensitive compare without allocating. (Keywords are ASCII and
// nothing non-ASCII case-folds to them, so byte length is a sound pre-filter.)
func isReservedWord(name string) bool {
	switch len(name) {
	case 2:
		return strings.EqualFold(name, "is")
	case 4:
		return strings.EqualFold(name, "true") || strings.EqualFold(name, "isnt")
	case 5:
		return strings.EqualFold(name, "false") || strings.EqualFold(name, "error")
	case 9:
		return strings.EqualFold(name, "undefined")
	}
	return false
}

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
	return fmt.Sprintf("%s = %s", QuoteAttributeName(a.Name), a.Value.String())
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
	return QuoteString(s.Value)
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

// AttributeScope represents the scope of an attribute reference.
type AttributeScope int

const (
	// NoScope represents an unscoped attribute reference
	NoScope AttributeScope = iota
	// MyScope represents MY.attr (current ClassAd)
	MyScope
	// TargetScope represents TARGET.attr (target ClassAd in matching)
	TargetScope
	// ParentScope represents PARENT.attr (parent ClassAd)
	ParentScope
)

// AttributeReference represents a reference to an attribute by name.
type AttributeReference struct {
	Name  string
	Scope AttributeScope
}

func (a *AttributeReference) String() string {
	name := QuoteAttributeName(a.Name)
	switch a.Scope {
	case MyScope:
		return "MY." + name
	case TargetScope:
		return "TARGET." + name
	case ParentScope:
		return "PARENT." + name
	default:
		return name
	}
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

// ElvisExpr represents the Elvis operator (expr1 ?: expr2).
// If expr1 evaluates to undefined, returns expr2; otherwise returns expr1.
type ElvisExpr struct {
	Left  Expr // The expression to test for undefined
	Right Expr // The fallback expression if Left is undefined
}

func (e *ElvisExpr) String() string {
	return fmt.Sprintf("(%s ?: %s)", e.Left.String(), e.Right.String())
}

func (e *ElvisExpr) exprNode() {}

// SelectExpr represents attribute selection (expr.attr).
type SelectExpr struct {
	Record Expr
	Attr   string
}

func (s *SelectExpr) String() string {
	// A bare numeric literal before '.' would re-lex as a real ("0.A"), so
	// separate it with a space; the attribute is quoted if it is not a bare
	// identifier (e.g. a keyword like 'true').
	sep := "."
	switch s.Record.(type) {
	case *IntegerLiteral, *RealLiteral:
		sep = " ."
	}
	return s.Record.String() + sep + QuoteAttributeName(s.Attr)
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

// ParenExpr is an expression wrapped in explicit source parentheses. The
// reference engine echoes parentheses verbatim when unparsing -- around any
// expression, including primaries ((x), (5)) and nested ((x)) -- so they are
// preserved as a node rather than a flag. Evaluation is transparent (the inner
// expression's value).
type ParenExpr struct {
	Inner Expr
}

// String is transparent: the AST's String() methods are a debug representation
// that already always-parenthesizes operators, so echoing the explicit source
// parentheses here would double them. The reference-faithful unparser
// (classad/unparse.go) is what emits the preserved parentheses.
func (p *ParenExpr) String() string {
	if p.Inner == nil {
		return "()"
	}
	return p.Inner.String()
}
func (p *ParenExpr) exprNode() {}

// Parenthesize wraps an expression in a ParenExpr, recording that the source
// parenthesized it so unparsing can echo the parentheses (matching the
// reference engine). The wrapped expression is returned for use in grammar
// actions.
func Parenthesize(e Expr) Expr {
	return &ParenExpr{Inner: e}
}
