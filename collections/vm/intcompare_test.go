package vm

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// Integer comparisons past 2^53 must come out the same from the tree-walking evaluator, the scalar
// interpreter and the vector executor. The vector kernels compare two integers as int64 and an
// int/real pair as float64, which is the reference's rule (doComparison: coerceToNumber, then
// compareIntegers or compareReals); the other two take theirs from the evaluator. Neighboring
// integers past 2^53 share a float64, so any path that compares two integers as doubles disagrees
// here and nowhere else.

var intCompareOperands = []string{
	"-9223372036854775808", "-9223372036854775807",
	"-9007199254740995", "-9007199254740994", "-9007199254740993", "-9007199254740992", "-9007199254740991",
	"0", "1",
	"9007199254740991", "9007199254740992", "9007199254740993", "9007199254740994", "9007199254740995",
	"4611686018427387905", "9223372036854775806", "9223372036854775807",
	"-9223372036854775808.0", "-9007199254740992.0", "0.5",
	"9007199254740992.0", "9007199254740994.0", "9223372036854775808.0",
	"true", "false",
}

func intCompareSource() *testSource {
	recs := make([]map[string]string, 0, len(intCompareOperands)*len(intCompareOperands))
	for _, a := range intCompareOperands {
		for _, b := range intCompareOperands {
			recs = append(recs, map[string]string{"A": a, "B": b})
		}
	}
	return newTestSource(recs)
}

func TestIntCompareEnginesAgree(t *testing.T) {
	src := intCompareSource()
	n := len(src.recs)
	var exprs []string
	for _, op := range []string{"<", "<=", ">", ">=", "==", "!=", "=?=", "=!="} {
		exprs = append(exprs, "A "+op+" B")
		for _, lit := range []string{"9007199254740992", "9007199254740993", "9223372036854775806", "-9007199254740993", "9007199254740992.0"} {
			exprs = append(exprs, "A "+op+" "+lit, lit+" "+op+" A")
		}
	}
	var scratch VecScratch
	for _, expr := range exprs {
		q, err := Parse(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		tree, err := classad.ParseExpr(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		vec, ok := q.VecEval(src, n, &scratch)
		if !ok {
			t.Fatalf("%s: the vector executor declined a plain comparison", expr)
		}
		for i := 0; i < n; i++ {
			ad := src.ad(i)
			want := ad.EvaluateExprWithTarget(tree, nil)
			if got := q.Eval(ad); !sameValue(got, want) {
				t.Errorf("%s with A=%s B=%s: scalar interpreter %s, evaluator %s",
					expr, src.recs[i]["A"], src.recs[i]["B"], got, want)
			}
			if got := vec.value(i); !sameValue(got, want) {
				t.Errorf("%s with A=%s B=%s: vector executor %s, evaluator %s",
					expr, src.recs[i]["A"], src.recs[i]["B"], got, want)
			}
		}
	}
}
