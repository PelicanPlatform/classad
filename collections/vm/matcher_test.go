package vm

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// TestMatcherEqualsRun checks that a reused Matcher produces the same value as a
// fresh Run for every (expr, scope) pair — i.e. reusing the evaluator and stack
// changes nothing observable.
func TestMatcherEqualsRun(t *testing.T) {
	scopes := make([]*classad.ClassAd, 0, len(scopeSources))
	for _, ss := range scopeSources {
		ad, err := classad.Parse(ss)
		if err != nil {
			t.Fatalf("parse scope %q: %v", ss, err)
		}
		scopes = append(scopes, ad)
	}
	for _, es := range exprSources {
		expr, err := parser.ParseExpr(es)
		if err != nil {
			t.Fatalf("parse expr %q: %v", es, err)
		}
		q := Compile(expr)
		m := q.Matcher()
		// Reuse the SAME matcher across all scopes, in order, and after that in
		// reverse, to shake out any state that leaks between evaluations.
		order := append(append([]int{}, indices(len(scopes))...), reversed(len(scopes))...)
		for _, i := range order {
			want := Run(q.prog, scopes[i])
			got := m.Eval(scopes[i])
			if !valuesEqual(want, got) {
				t.Errorf("Matcher != Run for expr=%q scope=%q:\n want=%s\n  got=%s",
					es, scopeSources[i], describe(want), describe(got))
			}
		}
	}
}

// TestMatcherStateDoesNotLeak interleaves a cyclic-reference ad (which triggers a
// recovered panic mid-evaluation) with a normal ad through one Matcher, asserting
// the normal ad still evaluates correctly afterward — i.e. the recovered panic
// left no depth/scope corruption behind.
func TestMatcherStateDoesNotLeak(t *testing.T) {
	cyclic, err := classad.Parse(`[a = a + 1; b = b]`)
	if err != nil {
		t.Fatal(err)
	}
	normal, err := classad.Parse(`[a = 41; b = 1]`)
	if err != nil {
		t.Fatal(err)
	}
	q, err := Parse(`a + b`)
	if err != nil {
		t.Fatal(err)
	}
	m := q.Matcher()
	for i := 0; i < 100; i++ {
		// Cyclic ad -> a is cyclic -> a + b is error.
		if v := m.Eval(cyclic); !v.IsError() {
			t.Fatalf("iter %d: cyclic ad expected error, got %s", i, describe(v))
		}
		// Normal ad immediately after -> must be 42, proving no leaked state.
		v := m.Eval(normal)
		iv, cerr := v.IntValue()
		if cerr != nil || iv != 42 {
			t.Fatalf("iter %d: normal ad expected 42, got %s (err=%v)", i, describe(v), cerr)
		}
	}
}

func indices(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func reversed(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = n - 1 - i
	}
	return out
}
