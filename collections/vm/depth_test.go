package vm

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// TestDeepExpressionParity guards the depth-limit delegation in CompileProgram: a
// natively-compiled flat interpreter tracks no per-node recursion depth, so a
// pathologically deep operator chain would compute a value where the tree-walker
// hits its maxEvalDepth guard and returns error. CompileProgram delegates such
// expressions wholesale, so the vm matches the tree-walker at every depth,
// including across the guard boundary (~2000).
func TestDeepExpressionParity(t *testing.T) {
	ad, err := classad.Parse(`[x = 1; y = 2]`)
	if err != nil {
		t.Fatal(err)
	}
	// A deep unary chain, a deep parenthesis nest, and a deep binary chain.
	shapes := []func(n int) string{
		func(n int) string { return strings.Repeat("!", n) + "Missing" },
		func(n int) string { return strings.Repeat("(", n) + "x" + strings.Repeat(")", n) },
		func(n int) string { return "x" + strings.Repeat("+1", n) },
	}
	for si, shape := range shapes {
		for _, n := range []int{50, 999, 1000, 1001, 1999, 2001, 4000} {
			src := shape(n)
			expr, err := parser.ParseExpr(src)
			if err != nil {
				// Some shapes at extreme depth may exceed the parser's own limits;
				// that is fine -- skip, the point is parity where it parses.
				continue
			}
			want := refEval(ad, expr)
			got := Run(CompileProgram(expr), ad)
			if !valuesEqual(want, got) {
				t.Errorf("shape %d n=%d: tree-walk=%s vm=%s", si, n, describe(want), describe(got))
			}
		}
	}
}
