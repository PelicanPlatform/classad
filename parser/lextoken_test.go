package parser

import (
	"strings"
	"testing"
)

func kindName(k int) string {
	switch k {
	case TokEOF:
		return "EOF"
	case IDENTIFIER:
		return "IDENT"
	case STRING_LITERAL:
		return "STR"
	case INTEGER_LITERAL:
		return "INT"
	case REAL_LITERAL:
		return "REAL"
	case BOOLEAN_LITERAL:
		return "BOOL"
	case UNDEFINED:
		return "UNDEF"
	case ERROR:
		return "ERROR"
	case INT64_MIN_MAGNITUDE:
		return "INT64MIN"
	case ELVIS:
		return "ELVIS"
	case OR:
		return "OR"
	case AND:
		return "AND"
	case EQ:
		return "EQ"
	case NE:
		return "NE"
	case IS:
		return "IS"
	case ISNT:
		return "ISNT"
	case LE:
		return "LE"
	case GE:
		return "GE"
	case LSHIFT:
		return "LSHIFT"
	case RSHIFT:
		return "RSHIFT"
	case URSHIFT:
		return "URSHIFT"
	}
	if k > 0 && k < 128 {
		return "'" + string(rune(k)) + "'"
	}
	return "?"
}

func lexAll(t *testing.T, input string) string {
	t.Helper()
	l := NewExprLexer(strings.NewReader(input))
	var b strings.Builder
	for i := 0; i < 200; i++ {
		tok, err := l.Next()
		if err != nil {
			b.WriteString("<err:" + err.Error() + ">")
			break
		}
		if tok.Kind == TokEOF {
			break
		}
		b.WriteString(kindName(tok.Kind))
		switch tok.Kind {
		case IDENTIFIER, STRING_LITERAL:
			b.WriteString("(" + tok.Str + ")")
		}
		b.WriteByte(' ')
	}
	return strings.TrimSpace(b.String())
}

// TestLexTokenStream locks how the lexer tokenizes the constructs a wire-emitting
// parser must handle -- scopes glued into one identifier, selects/subscripts/calls,
// lists, records, and the operator set -- via the exported Next/LexToken API.
func TestLexTokenStream(t *testing.T) {
	cases := []struct{ in, want string }{
		{`a + 1`, `IDENT(a) '+' INT`},
		{`TARGET.Cpus`, `IDENT(TARGET.Cpus)`}, // scope glued into the identifier
		{`MY.Rank`, `IDENT(MY.Rank)`},
		{`a.b.c`, `IDENT(a) '.' IDENT(b) '.' IDENT(c)`}, // non-scope dots are selects
		{`strcat("x", "y")`, `IDENT(strcat) '(' STR(x) ',' STR(y) ')'`},
		{`{1, 2, 3}`, `'{' INT ',' INT ',' INT '}'`},
		{`[a = 1; b = 2]`, `'[' IDENT(a) '=' INT ';' IDENT(b) '=' INT ']'`},
		{`(a + b) * c`, `'(' IDENT(a) '+' IDENT(b) ')' '*' IDENT(c)`},
		{`a == b`, `IDENT(a) EQ IDENT(b)`},
		{`a != b`, `IDENT(a) NE IDENT(b)`},
		{`a =?= b`, `IDENT(a) IS IDENT(b)`}, // meta-eq == the `is` keyword token
		{`a =!= b`, `IDENT(a) ISNT IDENT(b)`},
		{`a is b`, `IDENT(a) IS IDENT(b)`},
		{`a && b || c`, `IDENT(a) AND IDENT(b) OR IDENT(c)`},
		{`L[0]`, `IDENT(L) '[' INT ']'`},
		{`a ? b : c`, `IDENT(a) '?' IDENT(b) ':' IDENT(c)`},
		{`a ?: b`, `IDENT(a) ELVIS IDENT(b)`},
		{`x >= 1 && x <= 2`, `IDENT(x) GE INT AND IDENT(x) LE INT`},
		{`a << 2 >> 3 >>> 4`, `IDENT(a) LSHIFT INT RSHIFT INT URSHIFT INT`},
		{`-5`, `'-' INT`},
		{`!x`, `'!' IDENT(x)`},
		{`~y`, `'~' IDENT(y)`},
		{`"he\"llo"`, `STR(he"llo)`},
		{`true`, `BOOL`},
		{`undefined`, `UNDEF`},
		{`error`, `ERROR`},
	}
	for _, c := range cases {
		if got := lexAll(t, c.in); got != c.want {
			t.Errorf("lex %q\n  got  %s\n  want %s", c.in, got, c.want)
		}
	}
}
