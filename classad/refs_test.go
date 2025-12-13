package classad

import "testing"

func TestCollectRefsCoversCompositeNodes(t *testing.T) {
	expr, err := ParseExpr(`({AttrA, AttrB}[1] + (-AttrC)) + (AttrD ? AttrE : AttrF) + (AttrG ?: AttrH) + strcat(AttrI, [Inner = AttrJ].Inner)`)
	if err != nil {
		t.Fatalf("failed to parse expression: %v", err)
	}

	ad := New()
	ad.InsertAttr("AttrA", 1)
	ad.InsertAttr("AttrD", 1)

	external := ad.ExternalRefs(expr)
	expected := []string{"AttrB", "AttrC", "AttrE", "AttrF", "AttrG", "AttrH", "AttrI"}

	for _, ref := range expected {
		found := false
		for _, got := range external {
			if got == ref {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected external reference %q not found in %v", ref, external)
		}
	}
}
