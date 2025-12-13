package parser

import (
	"io"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

func getAttrInt(t *testing.T, ad *ast.ClassAd, name string) int64 {
	t.Helper()
	for _, attr := range ad.Attributes {
		if attr.Name == name {
			if lit, ok := attr.Value.(*ast.IntegerLiteral); ok {
				return lit.Value
			}
			break
		}
	}
	t.Fatalf("attribute %s not found or not integer", name)
	return 0
}

func TestReaderParserConcatenatedAdsWithComments(t *testing.T) {
	input := "/*lead*/[A=1]//line comment\n/*block\ncomment*/[B=2][C=3]/*tail*/"
	p := NewReaderParser(strings.NewReader(input))

	var values []int64
	for {
		ad, err := p.ParseClassAd()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ad.Attributes) == 0 {
			t.Fatalf("parsed empty ClassAd")
		}
		values = append(values, getAttrInt(t, ad, ad.Attributes[0].Name))
	}

	expected := []int64{1, 2, 3}
	if len(values) != len(expected) {
		t.Fatalf("expected %d ads, got %d", len(expected), len(values))
	}
	for i, v := range expected {
		if values[i] != v {
			t.Fatalf("value %d: expected %d, got %d", i, v, values[i])
		}
	}
}

func TestReaderParserCommentsInsideAd(t *testing.T) {
	input := `[A /*c1*/=/*c2*/ 5 /*c3*/; B= /*c4*/6]/*after*/`
	p := NewReaderParser(strings.NewReader(input))

	ad, err := p.ParseClassAd()
	if err != nil {
		t.Fatalf("unexpected error: %v, tokens=%v", err, dumpTokens(t, input))
	}

	a := getAttrInt(t, ad, "A")
	b := getAttrInt(t, ad, "B")

	if a != 5 || b != 6 {
		t.Fatalf("expected A=5 and B=6, got A=%d B=%d", a, b)
	}

	if next, err := p.ParseClassAd(); err != io.EOF {
		if err == nil {
			t.Fatalf("expected EOF, got additional ad: %+v", next)
		}
		t.Fatalf("expected EOF, got error: %v", err)
	}
}

func dumpTokens(t *testing.T, input string) []int {
	t.Helper()
	lex := NewStreamingLexer(strings.NewReader(input))
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

func TestReaderParserUnterminatedBlockComment(t *testing.T) {
	input := "/* unterminated [A=1]"
	p := NewReaderParser(strings.NewReader(input))

	ad, err := p.ParseClassAd()
	if err == nil {
		t.Fatalf("expected error, got ad: %+v", ad)
	}
}

func TestReaderParserComplexExpression(t *testing.T) {
	input := `[X = (Memory / 1024) + Disk]`
	p := NewReaderParser(strings.NewReader(input))

	ad, err := p.ParseClassAd()
	if err != nil {
		t.Fatalf("unexpected error: %v, tokens=%v", err, dumpTokens(t, input))
	}

	if len(ad.Attributes) != 1 {
		t.Fatalf("expected 1 attribute, got %d", len(ad.Attributes))
	}

	attr := ad.Attributes[0]
	if attr.Name != "X" {
		t.Fatalf("expected attribute X, got %s", attr.Name)
	}

	bin, ok := attr.Value.(*ast.BinaryOp)
	if !ok {
		t.Fatalf("expected binary op for X value, got %T", attr.Value)
	}
	if bin.Op != "+" {
		t.Fatalf("expected + op at root, got %s", bin.Op)
	}

	left, ok := bin.Left.(*ast.BinaryOp)
	if !ok {
		t.Fatalf("expected left child to be binary op, got %T", bin.Left)
	}
	if left.Op != "/" {
		t.Fatalf("expected / in nested op, got %s", left.Op)
	}
}
