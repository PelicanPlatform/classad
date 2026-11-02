package classad

import "testing"

// The alias resolver's own tests.
//
// Its only caller is the vm, which lives in another module, so without these the two
// functions are exercised everywhere and covered nowhere -- and the contract that matters is
// exactly the one a caller cannot see: what ResolveAlias does when NO resolver is installed.
// Answering undefined there would be a wrong answer rather than a slow one, because the
// reference would silently stop reading the record it was written against.

func TestResolveAliasWithoutAResolverDeclines(t *testing.T) {
	e := NewEvaluator(New())
	v, ok := e.ResolveAlias(0, "Owner")
	if ok {
		t.Fatalf("ResolveAlias reported a value with no resolver installed: %v", v)
	}
	// The caller must fall back, so the value is meaningless; what matters is ok == false.
	if !v.IsUndefined() && v != (Value{}) {
		t.Errorf("a declined resolution should not carry a usable value, got %v", v)
	}
}

func TestResolveAliasUsesTheInstalledResolver(t *testing.T) {
	e := NewEvaluator(New())
	var gotAlias int
	var gotName string
	e.SetAliasResolver(func(alias int, name string) Value {
		gotAlias, gotName = alias, name
		return NewIntValue(int64(alias) + 1)
	})

	v, ok := e.ResolveAlias(1, "RequestMemory")
	if !ok {
		t.Fatal("ResolveAlias declined although a resolver is installed")
	}
	if gotAlias != 1 {
		t.Errorf("resolver saw alias %d, want 1", gotAlias)
	}
	// The name is handed over with its original casing: the resolver is what knows how its
	// own backing spells attribute names, so this must not fold it on the way through.
	if gotName != "RequestMemory" {
		t.Errorf("resolver saw name %q, want %q", gotName, "RequestMemory")
	}
	n, err := v.IntValue()
	if err != nil || n != 2 {
		t.Errorf("got %v (err %v), want 2", v, err)
	}
}

func TestSetAliasResolverNilClearsIt(t *testing.T) {
	e := NewEvaluator(New())
	e.SetAliasResolver(func(int, string) Value { return NewIntValue(7) })
	if _, ok := e.ResolveAlias(0, "X"); !ok {
		t.Fatal("resolver did not take effect")
	}
	e.SetAliasResolver(nil)
	if _, ok := e.ResolveAlias(0, "X"); ok {
		t.Fatal("clearing the resolver left it installed; an evaluation after a join would " +
			"keep answering from a resolver that no longer belongs to it")
	}
}

// A resolver may report that a side does not define an attribute, which is also how an
// absent side reads -- a LEFT JOIN's unmatched row has no member for the inner side. Both
// are undefined, and ResolveAlias passes that through as a VALUE rather than as a decline,
// because declining means something else entirely: no resolver at all.
func TestResolverUndefinedIsAValueNotADecline(t *testing.T) {
	e := NewEvaluator(New())
	e.SetAliasResolver(func(int, string) Value { return NewUndefinedValue() })
	v, ok := e.ResolveAlias(0, "Missing")
	if !ok {
		t.Fatal("an undefined attribute must resolve, not decline")
	}
	if !v.IsUndefined() {
		t.Errorf("got %v, want undefined", v)
	}
}
