package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/PelicanPlatform/classad/parser"
)

// TestFastStringMatchesParser verifies fastString unescapes a quoted string
// literal byte-for-byte the same as the full parser (the lexer's scanString), and
// defers to the parser for anything it does not handle.
func TestFastStringMatchesParser(t *testing.T) {
	t.Parallel()
	handled := []string{
		`"hello"`, `""`, `"a\"b"`, `"a\\b"`, `"tab\tnl\nret\r"`,
		`"\101\102"`, `"\7"`, `"\40"`, `"\377"`, `"a\'b"`, `"\b\f"`,
		`"{[ p=\"primary\"; a=\"1.2.3.4\"; port=9618; noUDP=true; ]}"`, // AddressV1-like
		`"slot1@host.example.com"`,
	}
	for _, val := range handled {
		table := wire.NewInternTable()
		expr, err := parser.ParseExpr(val)
		if err != nil {
			t.Fatalf("parser %q: %v", val, err)
		}
		sl, ok := expr.(*ast.StringLiteral)
		if !ok {
			t.Fatalf("%q did not parse to a string literal (%T)", val, expr)
		}
		enc := wire.NewStreamEncoder(table, nil)
		var buf []byte
		if !fastString(enc, "A", val, &buf) {
			t.Errorf("fastString deferred on %q (expected handled)", val)
			continue
		}
		ad, err := wire.Decode(enc.Bytes(nil), table)
		if err != nil {
			t.Fatalf("decode %q: %v", val, err)
		}
		got := ad.Attributes[0].Value.(*ast.StringLiteral).Value
		if got != sl.Value {
			t.Errorf("val %q: fastString %q != parser %q", val, got, sl.Value)
		}
	}

	// Must defer to the parser: not lone string literals, or escapes the lexer
	// treats specially/rejects, or non-ASCII.
	deferred := []string{
		`"a" + "b"`, `{1, 2}`, `x == y`, `"a" == "b"`, `5`, `true`,
		`"\0"`, `"\00"`, `"\x"`, `"unterminated`, "\"héllo\"", `foo`,
	}
	for _, val := range deferred {
		enc := wire.NewStreamEncoder(wire.NewInternTable(), nil)
		var buf []byte
		if fastString(enc, "A", val, &buf) {
			t.Errorf("fastString handled %q (expected deferred to parser)", val)
		}
	}
}
