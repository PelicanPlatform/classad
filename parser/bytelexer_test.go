package parser

import (
	"bufio"
	"strings"
	"testing"
)

// lexAllStreaming drives the proven StreamingLexer (via Next) to completion,
// returning the token stream and any error. It is the oracle ByteLexer must match.
func lexAllStreaming(src string) ([]LexToken, error) {
	l := NewExprLexer(bufio.NewReader(strings.NewReader(src)))
	var toks []LexToken
	for {
		t, err := l.Next()
		if err != nil {
			return toks, err
		}
		if t.Kind == TokEOF {
			return toks, nil
		}
		toks = append(toks, t)
	}
}

func lexAllByte(src string) ([]LexToken, error) {
	l := NewByteLexer(src)
	var toks []LexToken
	for {
		t, err := l.Next()
		if err != nil {
			return toks, err
		}
		if t.Kind == TokEOF {
			return toks, nil
		}
		toks = append(toks, t)
	}
}

// byteLexerCases exercises every token class and edge case the two lexers must
// agree on: operators (including the multi-char and fused forms), literals,
// scope-prefix folding, comments, string escapes, and the numeric corner cases.
var byteLexerCases = []string{
	// literals
	`1`, `0`, `42`, `3.14`, `.5`, `1.0e10`, `1E-3`, `2.5e+8`, `9223372036854775807`,
	`"hi"`, `""`, `"a\"b\n\t\\"`, `"tab\there"`, `"oct\101\0777"`, `"utf: ✓ �z"`,
	// invalid UTF-8 in string/quoted-name bodies must normalize to U+FFFD, matching
	// the reference's bufio.ReadRune (each bad byte -> one replacement char).
	"\"\xec\"", "\"a\xec\xecb\"", "\"pre\\n\xff\"", "'\xecname'", "\"\xe2\x9c\"",
	`true`, `false`, `TRUE`, `False`, `undefined`, `Undefined`, `error`, `ERROR`,
	// identifiers and scopes
	`x`, `_x9`, `FooBar_123`, `TARGET.Cpus`, `my.rank`, `Parent.Foo`, `target.a.b`,
	`'quoted name'`, `'a\'b'`, `'has space'`,
	// operators / punctuation
	`a+b-c*d/e%f`, `a.b[c]`, `a && b || c`, `a & b | c ^ d`,
	`a == b`, `a != b`, `a =?= b`, `a =!= b`, `a is b`, `a isnt b`,
	`a < b`, `a <= b`, `a > b`, `a >= b`, `a << 2`, `a >> 3`, `a >>> 4`,
	`a ? b : c`, `a ?: b`, `x ? y :z`, `!x`, `~y`, `-z`, `+w`,
	`{1,2,3}`, `[a=1;b=2]`, `f(a, b, c)`,
	// spaced "=?" and "=!" that must NOT fuse
	`a =? b`, `a =! b`, `a = ?b`, `x=!10`,
	// comments and whitespace
	`a /* c */ + b`, "a // line\n + b", `  spaced   `,
	// the big realistic expressions the wire parser cares about
	`(TARGET.RequestCpus <= Cpus) && (TARGET.RequestMemory <= Memory)`,
	`ifThenElse(State =?= "Claimed", RemoteOwner, "none")`,
	`(GPUs =?= undefined) || (GPUs == 0) || ((CPUs > 8 * GPUs) && (Memory > 32000 * GPUs))`,
}

func TestByteLexerMatchesStreaming(t *testing.T) {
	for _, src := range byteLexerCases {
		wantToks, wantErr := lexAllStreaming(src)
		gotToks, gotErr := lexAllByte(src)

		if (wantErr == nil) != (gotErr == nil) {
			t.Errorf("%q: error mismatch: streaming=%v byte=%v", src, wantErr, gotErr)
			continue
		}
		if len(gotToks) != len(wantToks) {
			t.Errorf("%q: token count: streaming=%d byte=%d\n streaming=%v\n byte=%v",
				src, len(wantToks), len(gotToks), wantToks, gotToks)
			continue
		}
		for i := range wantToks {
			w, g := wantToks[i], gotToks[i]
			if w.Kind != g.Kind || w.Str != g.Str || w.Int != g.Int || w.Real != g.Real || w.Bool != g.Bool {
				t.Errorf("%q: token %d: streaming=%+v byte=%+v", src, i, w, g)
			}
		}
	}
}

// TestByteLexerErrorsMatchStreaming checks that malformed inputs both lexers see
// are rejected by both (message text may differ; the fact of an error must not).
func TestByteLexerErrorsMatchStreaming(t *testing.T) {
	for _, src := range []string{
		`"unterminated`, `'unterminated`, `1.`, `1.e5`, `010`, `"bad\q"`, `"\0"`,
		`/* unterminated`, "'newline\nname'",
	} {
		_, wantErr := lexAllStreaming(src)
		_, gotErr := lexAllByte(src)
		if (wantErr == nil) != (gotErr == nil) {
			t.Errorf("%q: error presence mismatch: streaming=%v byte=%v", src, wantErr, gotErr)
		}
	}
}
