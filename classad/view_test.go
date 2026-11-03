package classad

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

func viewAST(attrs ...*ast.AttributeAssignment) *ast.ClassAd {
	return &ast.ClassAd{Attributes: attrs}
}

func assign(name string, e ast.Expr) *ast.AttributeAssignment {
	return &ast.AttributeAssignment{Name: name, Value: e}
}

// TestNewViewResolves: a view answers a lookup and an evaluation exactly as the indexed ad built from
// the same attributes does, case-insensitively, including a reference from one attribute to another.
func TestNewViewResolves(t *testing.T) {
	attrs := []*ast.AttributeAssignment{
		assign("Rss", &ast.IntegerLiteral{Value: 4096}),
		assign("MemUse", &ast.BinaryOp{Op: "/", Left: ast.NewAttributeReference("rss", ast.NoScope),
			Right: &ast.IntegerLiteral{Value: 1024}}),
		assign("Owner", &ast.StringLiteral{Value: "u"}),
	}
	view := NewView(viewAST(attrs...))
	owned := New()
	for _, a := range attrs {
		owned.Insert(a.Name, a.Value)
	}
	for _, name := range []string{"MemUse", "memuse", "RSS", "Owner", "Missing"} {
		got, want := view.EvaluateAttr(name), owned.EvaluateAttr(name)
		if got.String() != want.String() {
			t.Errorf("%s: view gives %s, an indexed ad gives %s", name, got.String(), want.String())
		}
	}
	if v := view.EvaluateAttr("MemUse"); v.String() != "4" {
		t.Errorf("MemUse = %s, want 4", v.String())
	}
}

// TestNewViewLastAssignmentWins: a view does not deduplicate, so a repeated name resolves to its last
// assignment -- the rule a lookup follows everywhere else.
func TestNewViewLastAssignmentWins(t *testing.T) {
	view := NewView(viewAST(
		assign("Dup", &ast.IntegerLiteral{Value: 1}),
		assign("dup", &ast.IntegerLiteral{Value: 2}),
	))
	if v := view.EvaluateAttr("DUP"); v.String() != "2" {
		t.Errorf("Dup = %s, want 2 (the last assignment)", v.String())
	}
}

// TestNewViewWriteDoesNotReachTheSource: a view is read-only to the AST it wraps. Writing through it
// detaches it onto its own copy, so the ad it was built over -- which the caller may be reusing -- is
// left alone.
func TestNewViewWriteDoesNotReachTheSource(t *testing.T) {
	src := viewAST(assign("A", &ast.IntegerLiteral{Value: 1}))
	view := NewView(src)
	view.Insert("A", &ast.IntegerLiteral{Value: 9})
	view.Insert("B", &ast.IntegerLiteral{Value: 9})
	if len(src.Attributes) != 1 {
		t.Fatalf("the source AST gained attributes: %d, want 1", len(src.Attributes))
	}
	lit, ok := src.Attributes[0].Value.(*ast.IntegerLiteral)
	if !ok || lit.Value != 1 {
		t.Errorf("the source AST's value was overwritten: %v", src.Attributes[0].Value)
	}
	if v := view.EvaluateAttr("A"); v.String() != "9" {
		t.Errorf("the detached copy did not take the write: A = %s", v.String())
	}
}

// TestNewViewNilAST: a view over nothing is an empty ad, not a panic.
func TestNewViewNilAST(t *testing.T) {
	if v := NewView(nil).EvaluateAttr("Anything"); !v.IsUndefined() {
		t.Errorf("a view over no AST gave %s, want undefined", v.String())
	}
}
