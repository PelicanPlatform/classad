package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// TestFastStringMatchesParseOld verifies fastString copies a quoted OLD-ClassAd string
// literal byte-for-byte the same as classad.ParseOld (the reference old-ClassAd tokenizer,
// which keeps escapes literal), and defers to the parser for anything it does not handle.
// This is the fast-path half of the TestUpdateOldMatchesParseOld / FuzzIngestOld invariant.
func TestFastStringMatchesParseOld(t *testing.T) {
	t.Parallel()
	// Old-ClassAd strings keep backslashes literal, so escape sequences that the new-ClassAd
	// lexer would interpret (\t \n) or reject (\S \0 \x) all pass through unchanged.
	handled := []string{
		`"hello"`, `""`, `"a\"b"`, `"a\\b"`, `"tab\tnl\nret\r"`,
		`"\101\102"`, `"\7"`, `"\40"`, `"\377"`, `"a\'b"`, `"\b\f"`,
		`"\S"`, `"\S \r (\l) \m"`, `"C:\Users\x"`, `"\0"`, `"\x"`,
		"\"héllo\"", // valid non-ASCII: decoded to runes like scanString
		`"{[ p=\"primary\"; a=\"1.2.3.4\"; port=9618; noUDP=true; ]}"`, // AddressV1-like
		`"slot1@host.example.com"`,
	}
	for _, val := range handled {
		want, ok := parseOldStringValue(t, val)
		if !ok {
			t.Fatalf("reference ParseOld(%q) did not yield a string value", val)
		}
		table := wire.NewInternTable()
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
		if got != want {
			t.Errorf("val %q: fastString %q != ParseOld %q", val, got, want)
		}
	}

	// Must defer to the parser: not a lone string literal (expressions, non-strings), a
	// non-ASCII byte (leave rune decoding to the parser), or an unterminated string.
	deferred := []string{
		`"a" + "b"`, `{1, 2}`, `x == y`, `"a" == "b"`, `5`, `true`,
		`"unterminated`, `foo`,
	}
	for _, val := range deferred {
		enc := wire.NewStreamEncoder(wire.NewInternTable(), nil)
		var buf []byte
		if fastString(enc, "A", val, &buf) {
			t.Errorf("fastString handled %q (expected deferred to parser)", val)
		}
	}
}

// parseOldStringValue returns the string value classad.ParseOld assigns to A in "A = <val>".
func parseOldStringValue(t *testing.T, val string) (string, bool) {
	t.Helper()
	ad, err := classad.ParseOld("A = " + val)
	if err != nil {
		t.Fatalf("ParseOld(%q): %v", val, err)
	}
	return ad.EvaluateAttrString("A")
}
