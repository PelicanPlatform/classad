package classad

import (
	"testing"

	"github.com/PelicanPlatform/classad/fuzz/gen"
)

// foldOperands binds X to one value of every kind a fold has to get right: the two
// booleans, undefined, error, and the non-boolean values && / || map (a number to a
// boolean, anything else to error).
var foldOperands = map[string]string{
	"true":      `[X = true]`,
	"false":     `[X = false]`,
	"undefined": `[]`,
	"error":     `[X = error]`,
	"string":    `[X = "str"]`,
	"int":       `[X = 5]`,
	"zero":      `[X = 0]`,
	"real":      `[X = 2.5]`,
	"list":      `[X = {1, 2}]`,
	"record":    `[X = [a = 1]]`,
}

// foldForms are expressions whose constant parts the flattener folds, around an operand
// X whose value it cannot know. Each must evaluate the same folded as unfolded.
var foldForms = []string{
	// A literal on the right decides nothing about the left operand.
	`X && false`, `X && true`, `X || true`, `X || false`,
	`X && undefined`, `X || undefined`, `X && error`, `X || error`,
	// A literal on the left does.
	`false && X`, `true && X`, `true || X`, `false || X`,
	`undefined && X`, `undefined || X`, `error && X`, `error || X`,
	`"str" && X`, `"str" || X`, `5 && X`, `0 || X`, `2.5 && X`,
	// A logical operand passes through an identity literal unchanged...
	`true && (X == 5)`, `(X == 5) && true`, `false || (X < 3)`, `(X < 3) || false`,
	`true && !X`, `!X || false`, `true && (X =?= 5)`, `(X && X) || false`,
	// ... a non-logical one does not.
	`true && (X + 1)`, `(X + 1) && true`, `false || (X * 2)`, `(X * 2) || false`,
	`true && X[0]`, `false || X.a`,
	// The reported reachable shapes: the fold result observed through a total operator.
	`(X && false) =!= false`, `(X && false) =?= error`, `(false || X) =?= X`,
	`isError(X && false)`, `isError(true && X)`, `(X || true) is true`,
	`ifThenElse(X && false, 1, 2)`, `(X && false) ? 1 : 2`, `(X || false) ?: 7`,
	// Other folds.
	`true ? X : 1`, `false ? 1 : X`, `ifThenElse(true, X, 1)`, `ifThenElse(false, 1, X)`,
	`undefined ?: X`, `X ?: 3`, `!!X`, `X == X`, `X =?= X`, `X * 0`, `X - X`,
	`isUndefined(X)`, `X =?= undefined`, `X is undefined && true`,
	`(1 + 2) * X`, `X + (2 * 3)`, `-(-X)`, `(true && false) || X`, `X && (true || false)`,
}

// TestFoldConstantsLogicalSoundness checks Eval(FoldConstants(e)) == Eval(e) for every form
// against every operand kind.
func TestFoldConstantsLogicalSoundness(t *testing.T) {
	for _, form := range foldForms {
		e, err := ParseExpr(form)
		if err != nil {
			t.Fatalf("parse %q: %v", form, err)
		}
		folded := FoldConstants(e.internal())
		for kind, adSrc := range foldOperands {
			ad, err := Parse(adSrc)
			if err != nil {
				t.Fatalf("parse %s: %v", adSrc, err)
			}
			want := ad.EvaluateExpr(e.internal())
			got := ad.EvaluateExpr(folded)
			if got.String() != want.String() {
				t.Errorf("%s with X %s: folded to %s, which evaluates to %s; the expression evaluates to %s",
					form, kind, folded.String(), got.String(), want.String())
			}
		}
	}
}

// TestFoldConstantsLogicalFolds pins the folds that are sound, so the fix does not simply
// stop folding: each left-literal decision and each identity on a logical operand still
// reduces.
func TestFoldConstantsLogicalFolds(t *testing.T) {
	for src, want := range map[string]string{
		`false && X`:             "false",
		`true || X`:              "true",
		`error && X`:             "error",
		`"str" || X`:             "error",
		`true && (X == 5)`:       "(X == 5)",
		`(X == 5) && true`:       "(X == 5)",
		`false || !X`:            "(!X)",
		`(X =?= 1) || false`:     "(X is 1)",
		`true && 5`:              "true",
		`false || "str"`:         "error",
		`undefined && false`:     "false",
		`X && false`:             "(X && false)",
		`X || true`:              "(X || true)",
		`true && X`:              "(true && X)",
		`X && true`:              "(X && true)",
		`(X + 1) || false`:       "((X + 1) || false)",
		`(X && false) =!= false`: "((X && false) isnt false)",
	} {
		e, err := ParseExpr(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		if got := FoldConstants(e.internal()).String(); got != want {
			t.Errorf("FoldConstants(%s) = %s, want %s", src, got, want)
		}
	}
}

// TestFoldConstantsPropertyRandom is the generic form: for random expressions over random
// ads (the differential fuzzer's generator), every attribute's expression evaluates the same
// after FoldConstants as before. FoldConstantsAt agrees too, evaluated at the time it folded
// CurrentTime to, since the generated ads do not define CurrentTime.
func TestFoldConstantsPropertyRandom(t *testing.T) {
	n := 20000
	if testing.Short() {
		n = 2000
	}
	const now = 1700000000
	cfg := gen.DefaultConfig()
	g := gen.New(1, cfg)
	for i := 0; i < n; i++ {
		src := g.ClassAd()
		ad, err := Parse(src)
		if err != nil {
			continue
		}
		for _, attr := range ad.attrs() {
			e := attr.Value
			want := ad.EvaluateExpr(e)
			if got := ad.EvaluateExpr(FoldConstants(e)); got.String() != want.String() {
				t.Fatalf("ad %s\nattribute %s = %s\nfolds to %s, which evaluates to %s, not %s",
					src, attr.Name, e.String(), FoldConstants(e).String(), got.String(), want.String())
			}
		}
	}
	// FoldConstantsAt over expressions that read CurrentTime.
	for _, src := range []string{
		`CurrentTime - 86400`, `MY.CurrentTime > 5 && X`, `X > CurrentTime - 60 || false`,
		`ifThenElse(CurrentTime > 0, X, 1)`, `CurrentTime =?= undefined`,
	} {
		e, err := ParseExpr(src)
		if err != nil {
			t.Fatal(err)
		}
		for kind, adSrc := range foldOperands {
			ad, _ := Parse(adSrc)
			ev := NewEvaluator(ad)
			ev.PinCurrentTime(now)
			want := ev.Evaluate(e.internal())
			got := ev.Evaluate(FoldConstantsAt(e.internal(), now))
			if got.String() != want.String() {
				t.Errorf("%s with X %s: FoldConstantsAt gives %s, want %s", src, kind, got.String(), want.String())
			}
		}
	}
	e, _ := ParseExpr(`CurrentTime`)
	if got := FoldConstantsAt(e.internal(), now).String(); got != "1700000000" {
		t.Errorf("FoldConstantsAt(CurrentTime) = %s, want 1700000000", got)
	}
}
