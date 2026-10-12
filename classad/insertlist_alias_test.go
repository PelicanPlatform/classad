package classad

import "testing"

// TestInsertListElementDoesNotAliasSource: a list expression copied from one ad into
// another (Lookup + InsertExpr) is shared AST. Appending to the copy must not grow the
// source's list.
func TestInsertListElementDoesNotAliasSource(t *testing.T) {
	src, err := Parse(`[L = {1, 2}]`)
	if err != nil {
		t.Fatal(err)
	}
	dst := New()
	l, _ := src.Lookup("L")
	dst.InsertExpr("L", l)

	three, _ := ParseExpr("3")
	dst.InsertListElement("L", three)

	if got := dst.String(); got != "[L = {1, 2, 3}]" {
		t.Errorf("dst = %s, want [L = {1, 2, 3}]", got)
	}
	if got := src.String(); got != "[L = {1, 2}]" {
		t.Errorf("appending to dst's list changed src: %s", got)
	}
	if got := l.String(); got != "{1, 2}" {
		t.Errorf("appending to dst's list changed the looked-up Expr: %s", got)
	}
}
