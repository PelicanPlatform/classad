package classad

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// Insert marks an ad dirty only when it APPENDS a new attribute, because replacing an
// existing attribute's value cannot change the attribute set and therefore cannot put a
// sorted ad out of order. That is a pure optimization -- nothing about the output changes
// -- so these tests pin the ordering and lookup invariants it must not break, including
// the case it would be easy to get wrong: an ad that is ALREADY unsorted must stay that
// way, since the change may only decline to SET the dirty flag, never clear it.

// names returns the attribute names in the order the ad renders them, which is the sorted
// order ensureSorted establishes.
func names(t *testing.T, c *ClassAd) []string {
	t.Helper()
	_ = c.String() // forces ensureSorted
	a := c.AST()
	out := make([]string, 0, len(a.Attributes))
	for _, attr := range a.Attributes {
		out = append(out, attr.Name)
	}
	return out
}

func eq(t *testing.T, got, want []string, why string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", why, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", why, got, want)
		}
	}
}

// TestInsertInPlaceKeepsOrder covers the optimized path: the attribute already exists, so
// only its value changes and the order must be untouched and still correct.
func TestInsertInPlaceKeepsOrder(t *testing.T) {
	c := New()
	for _, n := range []string{"Zebra", "alpha", "Mid"} {
		c.InsertAttr(n, 1)
	}
	eq(t, names(t, c), []string{"alpha", "Mid", "Zebra"}, "after initial inserts")

	// Replace each value in place; the ad is clean (just sorted) at this point, which is
	// exactly when the old code would have dirtied it for nothing.
	c.InsertAttr("Mid", 42)
	c.InsertAttr("alpha", 43)
	eq(t, names(t, c), []string{"alpha", "Mid", "Zebra"}, "after in-place value updates")
	if v, _ := c.EvaluateAttrInt("Mid"); v != 42 {
		t.Errorf("Mid = %d, want 42", v)
	}
	if v, _ := c.EvaluateAttrInt("alpha"); v != 43 {
		t.Errorf("alpha = %d, want 43", v)
	}
	// Case-insensitivity: a differently-cased name is the SAME attribute, so this must be
	// an in-place update and must not append a second entry.
	c.InsertAttr("MID", 44)
	eq(t, names(t, c), []string{"alpha", "Mid", "Zebra"}, "after a differently-cased in-place update")
	if v, _ := c.EvaluateAttrInt("Mid"); v != 44 {
		t.Errorf("Mid after cased update = %d, want 44", v)
	}
}

// TestInsertAppendStillSorts is the other half: an append CAN disorder the ad, so it must
// still mark it dirty. A change that stopped marking dirty entirely would pass the test
// above and fail here.
func TestInsertAppendStillSorts(t *testing.T) {
	c := New()
	c.InsertAttr("Mid", 1)
	eq(t, names(t, c), []string{"Mid"}, "one attribute")

	// Appending a name that sorts BEFORE the existing one only comes out right if the ad is
	// re-sorted.
	c.InsertAttr("Alpha", 2)
	eq(t, names(t, c), []string{"Alpha", "Mid"}, "append that sorts first")

	c.InsertAttr("Beta", 3)
	eq(t, names(t, c), []string{"Alpha", "Beta", "Mid"}, "append that sorts into the middle")
}

// TestInsertInPlaceOnDirtyAdStaysDirty is the subtle case. An ad built directly from an
// unsorted AST is dirty and has never been sorted; an in-place update must not be taken as
// evidence that it is in order. The optimization is only allowed to skip SETTING the flag.
func TestInsertInPlaceOnDirtyAdStaysDirty(t *testing.T) {
	// Built out of order, deliberately not through a path that sorts.
	raw := &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
		{Name: "Zebra", Value: &ast.IntegerLiteral{Value: 1}},
		{Name: "alpha", Value: &ast.IntegerLiteral{Value: 2}},
		{Name: "Mid", Value: &ast.IntegerLiteral{Value: 3}},
	}}
	c := FromAST(raw)
	if !c.attrsDirty {
		t.Fatal("precondition: an ad from an unsorted AST should start dirty")
	}
	// An in-place update BEFORE anything sorts the ad.
	c.InsertAttr("Zebra", 99)
	eq(t, names(t, c), []string{"alpha", "Mid", "Zebra"}, "dirty ad must still sort after an in-place update")
	if v, _ := c.EvaluateAttrInt("Zebra"); v != 99 {
		t.Errorf("Zebra = %d, want 99", v)
	}
}

// TestInsertInterleaved walks a sequence shaped like a live job's updates: a wide ad that
// is sorted once, then updated in place many times with the occasional new attribute.
func TestInsertInterleaved(t *testing.T) {
	c := New()
	base := []string{"ClusterId", "ProcId", "Owner", "JobStatus", "RemoteUserCpu"}
	for _, n := range base {
		c.InsertAttr(n, 1)
	}
	want := []string{"ClusterId", "JobStatus", "Owner", "ProcId", "RemoteUserCpu"}
	eq(t, names(t, c), want, "initial")

	for i := 0; i < 20; i++ {
		c.InsertAttr("RemoteUserCpu", int64(i)) // in place, every time
		eq(t, names(t, c), want, "during in-place updates")
	}
	// Now a genuinely new attribute that sorts to the front.
	c.InsertAttr("AcctGroup", 7)
	eq(t, names(t, c), append([]string{"AcctGroup"}, want...), "after appending a new attribute")
	if v, _ := c.EvaluateAttrInt("RemoteUserCpu"); v != 19 {
		t.Errorf("RemoteUserCpu = %d, want 19", v)
	}
}
