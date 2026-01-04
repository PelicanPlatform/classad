package parser

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// errReader always returns an error on read.
type errReader struct{}

func (errReader) Read(p []byte) (int, error) {
	return 0, errors.New("boom")
}

// failAfterReader emits a fixed set of byte chunks and then returns an error.
type failAfterReader struct {
	chunks [][]byte
	idx    int
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.chunks) {
		return 0, errors.New("boom")
	}
	n := copy(p, r.chunks[r.idx])
	r.idx++
	return n, nil
}

func TestStreamingLexerPropagatesReadError(t *testing.T) {
	lex := NewStreamingLexer(errReader{})
	lval := &yySymType{}
	if tok := lex.Lex(lval); tok != 0 {
		t.Fatalf("expected tok=0, got %d", tok)
	}
	if lex.err == nil || lex.err.Error() != "boom" {
		t.Fatalf("expected read error 'boom', got %v", lex.err)
	}
}

func TestStreamingLexerOperators(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected []int
	}{
		{"IS", "a=?=b", []int{IDENTIFIER, IS, IDENTIFIER}},
		{"UnreadQuestion", "=?x", []int{int('='), int('?')}},
		{"UnreadBang", "=!x", []int{int('='), int('!')}},
		{"URSHIFT", "1>>>2", []int{INTEGER_LITERAL, URSHIFT, INTEGER_LITERAL}},
		{"LSHIFT", "1<<2", []int{INTEGER_LITERAL, LSHIFT, INTEGER_LITERAL}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lex := NewStreamingLexer(strings.NewReader(tc.input))
			lex.stopAfterClassAd = false
			lval := &yySymType{}
			var tokens []int
			for {
				tok := lex.Lex(lval)
				if tok == 0 {
					break
				}
				tokens = append(tokens, tok)
			}
			if lex.err != nil {
				t.Fatalf("unexpected error: %v", lex.err)
			}
			if len(tokens) != len(tc.expected) {
				t.Fatalf("tokens len=%d want=%d: %v", len(tokens), len(tc.expected), tokens)
			}
			for i, tok := range tc.expected {
				if tokens[i] != tok {
					t.Fatalf("token %d = %d want %d", i, tokens[i], tok)
				}
			}
		})
	}
}

func TestParseClassAdReturnsNilForNonClassAd(t *testing.T) {
	ad, err := ParseClassAd("true")
	if err == nil {
		t.Fatalf("expected parse error for bare expression, got nil")
	}
	if ad != nil {
		t.Fatalf("expected nil ClassAd result for expression input")
	}
}

func TestReaderParserEOF(t *testing.T) {
	p := NewReaderParser(strings.NewReader("   \n\t"))
	ad, err := p.ParseClassAd()
	if err != io.EOF {
		t.Fatalf("expected io.EOF, got %v (ad=%v)", err, ad)
	}
}

func TestReaderParserNonClassAd(t *testing.T) {
	p := NewReaderParser(strings.NewReader("true"))
	ad, err := p.ParseClassAd()
	if err == nil {
		t.Fatalf("expected error for non-ClassAd input, got nil (ad=%v)", ad)
	}
}

func TestReaderParserReadError(t *testing.T) {
	p := NewReaderParser(errReader{})
	_, err := p.ParseClassAd()
	if err == nil {
		t.Fatalf("expected read error, got nil")
	}
}

func TestConvertOldToNewFormatBranches(t *testing.T) {
	// Semicolon already present
	out := convertOldToNewFormat("Foo = 1;\n// comment")
	if !strings.Contains(out, "Foo = 1;\n") {
		t.Fatalf("expected semicolon branch to preserve suffix, got %q", out)
	}
	// Non-assignment line preserved
	out = convertOldToNewFormat("Req = A\n  && B")
	if !strings.Contains(out, "  && B\n") {
		t.Fatalf("expected non-assignment line to be kept, got %q", out)
	}
}

func TestParseOldClassAdError(t *testing.T) {
	_, err := ParseOldClassAd("Foo =")
	if err == nil {
		t.Fatalf("expected parse error for malformed old ClassAd")
	}
}

func TestStreamingLexerSingleCharAndOr(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int
	}{
		{"SingleAmpersand", "&", int('&')},
		{"SinglePipe", "|", int('|')},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lex := NewStreamingLexer(strings.NewReader(tc.input))
			lex.stopAfterClassAd = false

			tok := lex.Lex(&yySymType{})
			if tok != tc.want {
				t.Fatalf("first token = %d want %d", tok, tc.want)
			}
			if lex.err != nil {
				t.Fatalf("unexpected error: %v", lex.err)
			}
			if tok := lex.Lex(&yySymType{}); tok != 0 {
				t.Fatalf("expected second token 0, got %d", tok)
			}
		})
	}
}

func TestStreamingLexerSkipsComments(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected []int
	}{
		{"LineComment", "// comment\n[a=1]", []int{int('['), IDENTIFIER, int('='), INTEGER_LITERAL, int(']')}},
		{"BlockComment", "/*block*/[b=2]", []int{int('['), IDENTIFIER, int('='), INTEGER_LITERAL, int(']')}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lex := NewStreamingLexer(strings.NewReader(tc.input))
			tokens := collectTokens(t, lex)
			if lex.err != nil {
				t.Fatalf("unexpected error: %v", lex.err)
			}
			if len(tokens) != len(tc.expected) {
				t.Fatalf("token count %d want %d: %v", len(tokens), len(tc.expected), tokens)
			}
			for i, tok := range tc.expected {
				if tokens[i] != tok {
					t.Fatalf("token %d = %d want %d", i, tokens[i], tok)
				}
			}
		})
	}
}

func collectTokens(t *testing.T, lex *StreamingLexer) []int {
	t.Helper()
	var tokens []int
	lval := &yySymType{}
	for {
		tok := lex.Lex(lval)
		if tok == 0 {
			break
		}
		tokens = append(tokens, tok)
	}
	return tokens
}

func TestStreamingLexerUnreadRuneNoSeen(t *testing.T) {
	lex := NewStreamingLexer(strings.NewReader(""))
	lex.unreadRune(1)
	if lex.hasPending {
		t.Fatalf("expected no pending rune after unread with empty history")
	}
}

func TestStreamingLexerInvalidNumbers(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantMessage string
	}{
		{"InvalidReal", "1e", "invalid real number: 1e"},
		{"InvalidInteger", strings.Repeat("9", 30), "invalid integer"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lex := NewStreamingLexer(strings.NewReader(tc.input))
			lex.stopAfterClassAd = false
			tok := lex.Lex(&yySymType{})
			if tok != 0 {
				t.Fatalf("expected tok=0 after error, got %d", tok)
			}
			if lex.err == nil || !strings.Contains(lex.err.Error(), tc.wantMessage) {
				t.Fatalf("expected error containing %q, got %v", tc.wantMessage, lex.err)
			}
		})
	}
}

func TestParseScopedIdentifierNoScope(t *testing.T) {
	name, scope := ParseScopedIdentifier("Attr")
	if name != "Attr" || scope != ast.NoScope {
		t.Fatalf("expected unchanged identifier and NoScope, got %q and %v", name, scope)
	}
}

func TestParseScopedIdentifierParent(t *testing.T) {
	name, scope := ParseScopedIdentifier("PARENT.child")
	if name != "child" || scope != ast.ParentScope {
		t.Fatalf("expected child/ParentScope, got %q and %v", name, scope)
	}
}

func TestStreamingLexerLineCommentError(t *testing.T) {
	lex := NewStreamingLexer(&failAfterReader{chunks: [][]byte{[]byte("// no newline")}})
	tok := lex.Lex(&yySymType{})
	if tok != 0 {
		t.Fatalf("expected token 0 on error, got %d", tok)
	}
	if lex.err == nil || !strings.Contains(lex.err.Error(), "boom") {
		t.Fatalf("expected propagated read error, got %v", lex.err)
	}
}

func TestStreamingLexerBlockCommentError(t *testing.T) {
	lex := NewStreamingLexer(&failAfterReader{chunks: [][]byte{[]byte("/* unterminated")}})
	if tok := lex.Lex(&yySymType{}); tok != 0 {
		t.Fatalf("expected token 0 on error, got %d", tok)
	}
	if lex.err == nil || !strings.Contains(lex.err.Error(), "boom") {
		t.Fatalf("expected propagated read error, got %v", lex.err)
	}
}

func TestStreamingLexerUnterminatedEscape(t *testing.T) {
	lex := NewStreamingLexer(strings.NewReader("\"abc\\"))
	lex.stopAfterClassAd = false
	tok := lex.Lex(&yySymType{})
	if tok != STRING_LITERAL {
		t.Fatalf("expected STRING_LITERAL, got %d", tok)
	}
	if lex.err == nil || !strings.Contains(lex.err.Error(), "unterminated escape sequence") {
		t.Fatalf("expected unterminated escape error, got %v", lex.err)
	}
}

func TestStreamingLexerInvalidEscape(t *testing.T) {
	lex := NewStreamingLexer(strings.NewReader("\"\\y\""))
	lex.stopAfterClassAd = false
	_ = lex.Lex(&yySymType{})
	if lex.err == nil || !strings.Contains(lex.err.Error(), "invalid escape sequence") {
		t.Fatalf("expected invalid escape sequence error, got %v", lex.err)
	}
}

func TestStreamingLexerLineCommentEOF(t *testing.T) {
	lex := NewStreamingLexer(strings.NewReader("// trailing comment"))
	if tok := lex.Lex(&yySymType{}); tok != 0 {
		t.Fatalf("expected no tokens, got %d", tok)
	}
	if lex.err != nil {
		t.Fatalf("unexpected error for comment at EOF: %v", lex.err)
	}
}

func TestStreamingLexerScopedIdentifierEOF(t *testing.T) {
	lex := NewStreamingLexer(strings.NewReader("MY.attr"))
	tok := lex.Lex(&yySymType{})
	if tok != IDENTIFIER {
		t.Fatalf("expected IDENTIFIER token, got %d", tok)
	}
	if lex.err != nil {
		t.Fatalf("unexpected error: %v", lex.err)
	}
}

func TestParserComplexExpressionCoverage(t *testing.T) {
	input := `[
A = (1 + 2 * 3 >= 4) ? strcat("x", "y") : {5, 6};
B = [C = {1, 2}];
D = (1 << 2) >> 1;
E = !true || false && ~0;
F = (1 % 2) + (-3);
]`
	lex := NewLexer(input)
	yyParse(lex)
	if _, err := lex.Result(); err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
}

func TestParserSyntaxErrorPath(t *testing.T) {
	lex := NewLexer("[A = ]")
	yyParse(lex)
	if _, err := lex.Result(); err == nil {
		t.Fatalf("expected syntax error, got nil")
	}
}

func TestStreamingLexerSingleCharOperators(t *testing.T) {
	cases := []struct {
		input string
		token int
	}{
		{"+", int('+')},
		{"-", int('-')},
		{"*", int('*')},
		{"/", int('/')},
		{"%", int('%')},
		{"?", int('?')},
		{":", int(':')},
		{"^", int('^')},
		{"~", int('~')},
		{".", int('.')},
	}

	for _, tc := range cases {
		lex := NewStreamingLexer(strings.NewReader(tc.input))
		lex.stopAfterClassAd = false
		lval := &yySymType{}
		tok := lex.Lex(lval)
		if tok != tc.token {
			t.Fatalf("input %q: got token %d want %d", tc.input, tok, tc.token)
		}
		if next := lex.Lex(lval); next != 0 {
			t.Fatalf("input %q: expected EOF token 0, got %d", tc.input, next)
		}
		if lex.err != nil {
			t.Fatalf("input %q: unexpected error %v", tc.input, lex.err)
		}
	}
}

func TestParseExpr(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "simple integer",
			input:   "42",
			wantErr: false,
		},
		{
			name:    "simple arithmetic",
			input:   "2 + 3",
			wantErr: false,
		},
		{
			name:    "complex arithmetic",
			input:   "2 + 3 * 4",
			wantErr: false,
		},
		{
			name:    "boolean expression",
			input:   "(x > 5) && (y < 10)",
			wantErr: false,
		},
		{
			name:    "string literal",
			input:   `"hello world"`,
			wantErr: false,
		},
		{
			name:    "list literal",
			input:   "{1, 2, 3, 4, 5}",
			wantErr: false,
		},
		{
			name:    "function call",
			input:   `strcat("Hello", " ", "World")`,
			wantErr: false,
		},
		{
			name:    "conditional expression",
			input:   `x > 0 ? "positive" : "non-positive"`,
			wantErr: false,
		},
		{
			name:    "attribute reference",
			input:   "MY.attr",
			wantErr: false,
		},
		{
			name:    "boolean literal",
			input:   "true",
			wantErr: false,
		},
		{
			name:    "undefined literal",
			input:   "undefined",
			wantErr: false,
		},
		{
			name:    "error literal",
			input:   "error",
			wantErr: false,
		},
		{
			name:    "record literal",
			input:   "[a = 1; b = 2]",
			wantErr: false,
		},
		{
			name:    "nested expression",
			input:   "((1 + 2) * (3 + 4))",
			wantErr: false,
		},
		{
			name:    "syntax error",
			input:   "2 + +",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, err := ParseExpr(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseExpr(%q) expected error, got nil", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseExpr(%q) unexpected error: %v", tt.input, err)
			}
			if expr == nil {
				t.Fatalf("ParseExpr(%q) returned nil expression", tt.input)
			}
			// Verify it implements ast.Expr interface
			if _, ok := expr.(ast.Expr); !ok {
				t.Fatalf("ParseExpr(%q) result does not implement ast.Expr", tt.input)
			}
		})
	}
}

func TestParseExprTypeChecking(t *testing.T) {
	// Test that ParseExpr returns different types for different inputs
	tests := []struct {
		name        string
		input       string
		expectedTyp string
	}{
		{"integer", "42", "*ast.IntegerLiteral"},
		{"real", "3.14", "*ast.RealLiteral"},
		{"string", `"hello"`, "*ast.StringLiteral"},
		{"boolean", "true", "*ast.BooleanLiteral"},
		{"list", "{1, 2}", "*ast.ListLiteral"},
		{"binary op", "1 + 2", "*ast.BinaryOp"},
		{"attribute ref", "x", "*ast.AttributeReference"},
		{"function call", "func()", "*ast.FunctionCall"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, err := ParseExpr(tt.input)
			if err != nil {
				t.Fatalf("ParseExpr(%q) unexpected error: %v", tt.input, err)
			}
			typeName := fmt.Sprintf("%T", expr)
			if typeName != tt.expectedTyp {
				t.Fatalf("ParseExpr(%q) returned type %s, want %s", tt.input, typeName, tt.expectedTyp)
			}
		})
	}
}
