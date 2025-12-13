package parser

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

func TestLexerBasicTokens(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantType int
	}{
		{"integer", "42", INTEGER_LITERAL},
		{"real", "3.14", REAL_LITERAL},
		{"string", `"hello"`, STRING_LITERAL},
		{"true", "true", BOOLEAN_LITERAL},
		{"false", "false", BOOLEAN_LITERAL},
		{"undefined", "undefined", UNDEFINED},
		{"error", "error", ERROR},
		{"identifier", "myVar", IDENTIFIER},
		{"eq", "==", EQ},
		{"ne", "!=", NE},
		{"le", "<=", LE},
		{"ge", ">=", GE},
		{"and", "&&", AND},
		{"or", "||", OR},
		{"is", "is", IS},
		{"isnt", "isnt", ISNT},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lex := NewLexer(tt.input)
			lval := &yySymType{}
			token := lex.Lex(lval)
			if token != tt.wantType {
				t.Errorf("Lex() = %d, want %d", token, tt.wantType)
			}
		})
	}
}

func TestLexerStrings(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`"hello"`, "hello"},
		{`"hello world"`, "hello world"},
		{`"hello\nworld"`, "hello\nworld"},
		{`"hello\tworld"`, "hello\tworld"},
		{`"quote: \""`, `quote: "`},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			lex := NewLexer(tt.input)
			lval := &yySymType{}
			token := lex.Lex(lval)
			if token != STRING_LITERAL {
				t.Fatalf("Expected STRING_LITERAL token, got %d", token)
			}
			if lval.str != tt.want {
				t.Errorf("String value = %q, want %q", lval.str, tt.want)
			}
		})
	}
}

func TestLexerNumbers(t *testing.T) {
	tests := []struct {
		input      string
		wantType   int
		wantInt    int64
		wantReal   float64
		isRealType bool
	}{
		{"42", INTEGER_LITERAL, 42, 0, false},
		{"0", INTEGER_LITERAL, 0, 0, false},
		{"123456", INTEGER_LITERAL, 123456, 0, false},
		{"3.14", REAL_LITERAL, 0, 3.14, true},
		{"0.5", REAL_LITERAL, 0, 0.5, true},
		{"2.5e10", REAL_LITERAL, 0, 2.5e10, true},
		{"1.5E-5", REAL_LITERAL, 0, 1.5e-5, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			lex := NewLexer(tt.input)
			lval := &yySymType{}
			token := lex.Lex(lval)
			if token != tt.wantType {
				t.Fatalf("Token type = %d, want %d", token, tt.wantType)
			}
			if tt.isRealType {
				if lval.real != tt.wantReal {
					t.Errorf("Real value = %g, want %g", lval.real, tt.wantReal)
				}
			} else {
				if lval.integer != tt.wantInt {
					t.Errorf("Integer value = %d, want %d", lval.integer, tt.wantInt)
				}
			}
		})
	}
}

func TestLexerIdentifiers(t *testing.T) {
	tests := []string{
		"x",
		"myVar",
		"MY_VAR",
		"var123",
		"_private",
	}

	for _, tt := range tests {
		t.Run(tt, func(t *testing.T) {
			lex := NewLexer(tt)
			lval := &yySymType{}
			token := lex.Lex(lval)
			if token != IDENTIFIER {
				t.Fatalf("Expected IDENTIFIER token, got %d", token)
			}
			if lval.str != tt {
				t.Errorf("Identifier = %q, want %q", lval.str, tt)
			}
		})
	}
}

func TestLexerComments(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"line comment", "// this is a comment\n42"},
		{"block comment", "/* this is a block comment */ 42"},
		{"multiline block", "/* line 1\nline 2\nline 3 */ 42"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lex := NewLexer(tt.input)
			lval := &yySymType{}
			token := lex.Lex(lval)
			if token != INTEGER_LITERAL {
				t.Errorf("Expected INTEGER_LITERAL after comment, got %d", token)
			}
			if lval.integer != 42 {
				t.Errorf("Expected integer value 42, got %d", lval.integer)
			}
		})
	}
}

func TestNewLexer(t *testing.T) {
	input := "test input"
	lex := NewLexer(input)
	if lex.input != input {
		t.Errorf("NewLexer input = %q, want %q", lex.input, input)
	}
	if lex.pos != 0 {
		t.Errorf("NewLexer pos = %d, want 0", lex.pos)
	}
}

func TestLexerError(t *testing.T) {
	lex := NewLexer("test")
	lex.Error("test error")
	_, err := lex.Result()
	if err == nil {
		t.Error("Expected error, got nil")
	}
}

func TestLexerErrorFormattingUnexpectedChar(t *testing.T) {
	lex := NewLexer("foo\n  @")
	lval := &yySymType{}
	for {
		if tok := lex.Lex(lval); tok == 0 {
			break
		}
	}
	_, err := lex.Result()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	const expected = "parse error at line 2, col 3: unexpected character: @\n  @\n  ^"
	if err.Error() != expected {
		t.Fatalf("unexpected error message:\n got: %q\nwant: %q", err.Error(), expected)
	}
}

func TestLexerErrorFormattingUnterminatedString(t *testing.T) {
	lex := NewLexer("\"foo")
	lval := &yySymType{}
	for {
		if tok := lex.Lex(lval); tok == 0 {
			break
		}
	}
	_, err := lex.Result()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	const expected = "parse error at line 1, col 4: unterminated string starting at byte 0\n\"foo\n   ^"
	if err.Error() != expected {
		t.Fatalf("unexpected error message:\n got: %q\nwant: %q", err.Error(), expected)
	}
}

func TestLexerErrorFormattingUnterminatedBlockComment(t *testing.T) {
	lex := NewLexer("/* unterminated")
	lval := &yySymType{}
	for {
		if tok := lex.Lex(lval); tok == 0 {
			break
		}
	}
	_, err := lex.Result()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	const expected = "parse error at line 1, col 15: unterminated block comment\n/* unterminated\n              ^"
	if err.Error() != expected {
		t.Fatalf("unexpected error message:\n got: %q\nwant: %q", err.Error(), expected)
	}
}

func TestLexerErrorFormattingNullOctalInString(t *testing.T) {
	lex := NewLexer("\"\\000\"")
	lval := &yySymType{}
	for {
		if tok := lex.Lex(lval); tok == 0 {
			break
		}
	}
	_, err := lex.Result()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	const expected = "parse error at line 1, col 6: unterminated string starting at byte 5\n\"\\000\"\n     ^"
	if err.Error() != expected {
		t.Fatalf("unexpected error message:\n got: %q\nwant: %q", err.Error(), expected)
	}
}

func TestLexerErrorFormattingInvalidNumber(t *testing.T) {
	lex := NewLexer("1e+")
	lval := &yySymType{}
	for {
		if tok := lex.Lex(lval); tok == 0 {
			break
		}
	}
	_, err := lex.Result()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	const expected = "parse error at line 1, col 3: invalid real number: 1e+\n1e+\n  ^"
	if err.Error() != expected {
		t.Fatalf("unexpected error message:\n got: %q\nwant: %q", err.Error(), expected)
	}
}

func TestLexerResult(t *testing.T) {
	lex := NewLexer("test")
	expectedResult := &ast.ClassAd{}
	lex.result = expectedResult

	result, err := lex.Result()
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if result != expectedResult {
		t.Errorf("Result mismatch")
	}
}
