package wire

import (
	"testing"

	"github.com/PelicanPlatform/classad/parser"
)

// TestAppendNodeTextMatchesString checks that AppendNodeText renders every wire
// node byte-for-byte the same as decoding it to an ast.Expr and calling String().
// It reuses roundTripCases, which exercises every node type (literals, scoped and
// selected refs, every binary/unary op, lists, records, functions, conditionals,
// elvis, subscripts, parens), so the AST-free render path cannot silently diverge
// from the AST unparser for any node shape.
func TestAppendNodeTextMatchesString(t *testing.T) {
	for _, src := range roundTripCases {
		orig, err := parser.ParseClassAd(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		tbl := NewInternTable()
		enc := Encode(nil, orig, tbl)

		checked := 0
		ok := Ad(enc).ForEach(func(id uint32, node []byte) bool {
			e, err := DecodeNode(node, tbl)
			if err != nil {
				t.Fatalf("DecodeNode (%q): %v", src, err)
			}
			want := e.String()
			got, err := AppendNodeText(nil, node, tbl)
			if err != nil {
				t.Fatalf("AppendNodeText (%q): %v", src, err)
			}
			if string(got) != want {
				name, _ := tbl.Name(id)
				t.Errorf("%s in %q:\n AppendNodeText = %q\n         String = %q", name, src, got, want)
			}
			checked++
			return true
		})
		if !ok {
			t.Fatalf("ForEach reported malformed ad for %q", src)
		}
		if checked == 0 {
			t.Fatalf("no attributes checked for %q", src)
		}
	}
}

// TestAppendNodeTextInlineMatchesString is TestAppendNodeTextMatchesString for the
// inline-names encoding (the form the persistent store keeps), whose nodes use the
// nAttrRefStr/nFuncStr/nSelectStr variants and inline record keys.
func TestAppendNodeTextInlineMatchesString(t *testing.T) {
	for _, src := range roundTripCases {
		orig, err := parser.ParseClassAd(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		enc := EncodeInline(nil, orig)

		// Inline ads key attributes by inline name, not by an interned id, so
		// iterate the parsed attributes and fetch each node via LookupByName.
		checked := 0
		for _, attr := range orig.Attributes {
			node, ok := Ad(enc).LookupByName(attr.Name)
			if !ok {
				t.Fatalf("LookupByName(%s) failed for %q", attr.Name, src)
			}
			e, err := DecodeNodeInline(node)
			if err != nil {
				t.Fatalf("DecodeNodeInline (%q): %v", src, err)
			}
			want := e.String()
			got, err := AppendNodeTextInline(nil, node)
			if err != nil {
				t.Fatalf("AppendNodeTextInline (%q): %v", src, err)
			}
			if string(got) != want {
				t.Errorf("%s in %q:\n AppendNodeTextInline = %q\n               String = %q", attr.Name, src, got, want)
			}
			checked++
		}
		if checked == 0 {
			t.Fatalf("no attributes checked for %q", src)
		}
	}
}
