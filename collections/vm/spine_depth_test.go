package vm

import (
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// spineSource builds "X == 1 || X == 2 || ... || X == n", the shape a
// machine-generated membership constraint has: one left-deep || spine.
func spineSource(n int) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		if i > 1 {
			sb.WriteString(" || ")
		}
		sb.WriteString("X == ")
		sb.WriteString(strconv.Itoa(i))
	}
	return sb.String()
}

// TestLongSpineParity pins the compiler's depth gate against the tree-walker's
// recursion guard for the one shape whose length no longer costs the tree-walker
// any depth. astHeight counts every node, so a long spine measures taller than
// the depth the tree-walker reaches for it and the compiler delegates it whole.
// Delegation is conservative, not wrong: the delegated path IS the tree-walker,
// so both answer the same, and that answer is the membership result rather than
// an error.
func TestLongSpineParity(t *testing.T) {
	ad, err := classad.Parse(`[X = 4000]`)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{10, 999, 1000, 1001, 1998, 2001, 5000, 20000} {
		src := spineSource(n)
		expr, err := parser.ParseExpr(src)
		if err != nil {
			t.Fatalf("%d terms: %v", n, err)
		}
		want := refEval(ad, expr)
		got := Run(CompileProgram(expr), ad)
		if !valuesEqual(want, got) {
			t.Fatalf("%d terms: tree-walk=%s vm=%s", n, describe(want), describe(got))
		}
		// X = 4000 matches a term only once the spine is that long.
		wantMatch := n >= 4000
		b, err := got.BoolValue()
		if err != nil {
			t.Fatalf("%d terms: got %s, want a boolean", n, describe(got))
		}
		if b != wantMatch {
			t.Fatalf("%d terms: got %v, want %v", n, b, wantMatch)
		}
	}
}

// TestLongSpineQueryParity is the same chain through the Query entry point the
// collection scanners use.
func TestLongSpineQueryParity(t *testing.T) {
	ad, err := classad.Parse(`[X = 4999]`)
	if err != nil {
		t.Fatal(err)
	}
	q, err := Parse(spineSource(5000))
	if err != nil {
		t.Fatal(err)
	}
	if !q.Matches(ad) {
		t.Fatal("a 5000-term membership constraint did not match its own member")
	}
	miss, err := classad.Parse(`[X = 500000]`)
	if err != nil {
		t.Fatal(err)
	}
	if q.Matches(miss) {
		t.Fatal("matched a non-member")
	}
}
