package classad

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// TestRootScopeSurvivesParseAndUnparse is the round trip the HTCondor config
// parser depends on: $EVAL prints an expression it could not fully evaluate,
// so a reference that loses its leading dot there changes what the printed
// expression means.
func TestRootScopeSurvivesParseAndUnparse(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{".A0", ".A0"},
		{".x.y", ".x.y"},
		{"a.b", "a.b"},
		{"MY.Foo", "MY.Foo"},
		{"TARGET.Foo", "TARGET.Foo"},
		{"Foo", "Foo"},
	}
	for _, c := range cases {
		for _, parse := range []struct {
			name string
			fn   func(string) (*Expr, error)
		}{{"ParseExpr", ParseExpr}, {"ParseExprOld", ParseExprOld}} {
			e, err := parse.fn(c.src)
			if err != nil {
				t.Errorf("%s(%q): %v", parse.name, c.src, err)
				continue
			}
			if got := e.String(); got != c.want {
				t.Errorf("%s(%q).String() = %q, want %q", parse.name, c.src, got, c.want)
			}
		}
	}
}

// TestRootScopeIsParsedAsRootScope pins the AST, so a round trip that happens
// to print correctly for the wrong reason still fails.
func TestRootScopeIsParsedAsRootScope(t *testing.T) {
	e, err := ParseExprOld(".A0")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ref, ok := e.expr.(*ast.AttributeReference)
	if !ok {
		t.Fatalf("got %T, want *ast.AttributeReference", e.expr)
	}
	if ref.Scope != ast.RootScope {
		t.Errorf("scope = %v, want RootScope", ref.Scope)
	}
	if ref.Name != "A0" {
		t.Errorf("name = %q, want %q", ref.Name, "A0")
	}
}

// TestRootScopeResolvesAgainstTheOutermostAd is the semantic half: inside a
// nested ad, .Foo and Foo are different lookups. The reference engine selects
// state.rootAd for an absolute reference and state.curAd otherwise.
func TestRootScopeResolvesAgainstTheOutermostAd(t *testing.T) {
	outer := New()
	_ = outer.Set("Where", "root")
	inner := New()
	_ = inner.Set("Where", "inner")
	inner.SetParent(outer)

	for _, c := range []struct{ src, want string }{
		{"Where", "inner"}, // unscoped: current ad first
		{".Where", "root"}, // absolute: the outermost ad
		{"MY.Where", "inner"},
	} {
		e, err := ParseExprOld(c.src)
		if err != nil {
			t.Fatalf("parse %q: %v", c.src, err)
		}
		got, err := e.Eval(inner).StringValue()
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s evaluated to %q, want %q", c.src, got, c.want)
		}
	}
}

// TestRootScopeMissIsUndefined: an absolute reference names one ad, so a miss
// there does not walk back down into the nested ad.
func TestRootScopeMissIsUndefined(t *testing.T) {
	outer := New()
	inner := New()
	_ = inner.Set("OnlyInner", int64(1))
	inner.SetParent(outer)

	e, err := ParseExprOld(".OnlyInner")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v := e.Eval(inner); !v.IsUndefined() {
		t.Errorf("got %v, want undefined", v)
	}
}

// TestRootScopeOnAFlatAdMatchesUnscoped: a top-level ad is its own root, which
// is why matchmaking (flat job and slot ads) is unaffected by this change.
func TestRootScopeOnAFlatAdMatchesUnscoped(t *testing.T) {
	ad := New()
	_ = ad.Set("Memory", int64(4096))
	for _, src := range []string{"Memory", ".Memory"} {
		e, err := ParseExprOld(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		got, err := e.Eval(ad).IntValue()
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if got != 4096 {
			t.Errorf("%s = %d, want 4096", src, got)
		}
	}
}

// TestRootScopeInAnUnparsedList is the shape the HTCondor config fuzzer
// actually caught: $EVAL prints a list it could not reduce, and the leading
// dot has to survive into that printed form.
func TestRootScopeInAnUnparsedList(t *testing.T) {
	e, err := ParseExprOld("{1, 1+ .A0}")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, ok := e.Eval(New()).Unparse()
	if !ok {
		t.Fatal("Unparse declined the value")
	}
	if want := "{ 1,1 + .A0 }"; got != want {
		t.Errorf("Unparse() = %q, want %q", got, want)
	}
}
