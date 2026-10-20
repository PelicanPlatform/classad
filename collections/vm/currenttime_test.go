package vm

import (
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// TestCurrentTimeProbe verifies a query referencing CurrentTime folds it to a constant at
// compile time, so `CompletionDate > CurrentTime - 86400` yields an indexable range probe on
// CompletionDate (not a non-prunable attr-vs-expr comparison). This is what lets the archive
// prune whole segments/zones for "jobs in the last day" queries.
func TestCurrentTimeProbe(t *testing.T) {
	before := time.Now().Unix()
	q, err := Parse(`CompletionDate > CurrentTime - 86400`)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().Unix()

	probes := q.Probes()
	if len(probes) != 1 {
		t.Fatalf("got %d probes, want 1 (CurrentTime should fold to a constant threshold)", len(probes))
	}
	p := probes[0]
	if p.Attr != "CompletionDate" || p.Op != ">" {
		t.Fatalf("probe = {%s %s}, want {CompletionDate >}", p.Attr, p.Op)
	}
	if len(p.Vals) != 1 {
		t.Fatalf("probe has %d values, want 1", len(p.Vals))
	}
	n, err := p.Vals[0].IntValue()
	if err != nil {
		t.Fatalf("probe threshold is not an integer literal: %v", err)
	}
	if n < before-86400 || n > after-86400 {
		t.Errorf("probe threshold = %d, want ~now-86400 (in [%d, %d])", n, before-86400, after-86400)
	}
}

// TestCurrentTimeMatch verifies a compiled query with CurrentTime matches ads correctly
// (recent kept, old rejected).
func TestCurrentTimeMatch(t *testing.T) {
	now := time.Now().Unix()
	q, err := Parse(`CompletionDate > CurrentTime - 86400`)
	if err != nil {
		t.Fatal(err)
	}
	recent, _ := classad.Parse(`[ CompletionDate = ` + itoa(now-3600) + ` ]`) // 1h ago
	old, _ := classad.Parse(`[ CompletionDate = ` + itoa(now-2*86400) + ` ]`) // 2d ago
	if !q.Matches(recent) {
		t.Error("recent ad (1h ago) should match CompletionDate > CurrentTime - 86400")
	}
	if q.Matches(old) {
		t.Error("old ad (2d ago) should not match CompletionDate > CurrentTime - 86400")
	}
}

// TestNonCurrentTimeUnchanged sanity-checks that a query without CurrentTime still compiles
// and its probes are unaffected (the fold is gated on referencing CurrentTime).
func TestNonCurrentTimeUnchanged(t *testing.T) {
	q, err := Parse(`Memory > 2048`)
	if err != nil {
		t.Fatal(err)
	}
	if p := q.Probes(); len(p) != 1 || p[0].Attr != "Memory" || p[0].Op != ">" {
		t.Fatalf("Memory probe unexpectedly changed: %v", p)
	}
}

// TestCurrentTimeAdDefined verifies an ad that defines CurrentTime supplies its own value to
// a compiled query, as it does to the evaluator, through every evaluation entry point. The
// query used to be compiled with CurrentTime folded to the clock, so the ad's value was
// ignored.
func TestCurrentTimeAdDefined(t *testing.T) {
	for _, src := range []string{`CurrentTime < 100`, `MY.CurrentTime < 100`, `CurrentTime + 1 == 6`} {
		q, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, adSrc := range []string{`[CurrentTime = 5]`, `[]`, `[CurrentTime = 5000]`} {
			ad, err := classad.Parse(adSrc)
			if err != nil {
				t.Fatal(err)
			}
			e, _ := classad.ParseExpr(src)
			want := e.Eval(ad).String()
			if got := q.Eval(ad).String(); got != want {
				t.Errorf("%s on %s: Eval = %s, evaluator = %s", src, adSrc, got, want)
			}
			if got := q.Matcher().Eval(ad).String(); got != want {
				t.Errorf("%s on %s: Matcher.Eval = %s, evaluator = %s", src, adSrc, got, want)
			}
			resolve := func(name string, scope ast.AttributeScope) classad.Value {
				if scope != ast.NoScope && scope != ast.MyScope {
					return classad.NewUndefinedValue()
				}
				if e, ok := ad.Lookup(name); ok {
					return e.Eval(ad)
				}
				return classad.NewUndefinedValue()
			}
			if got := q.Matcher().EvalResolved(resolve).String(); got != want {
				t.Errorf("%s on %s: EvalResolved = %s, evaluator = %s", src, adSrc, got, want)
			}
		}
	}
}

// TestCurrentTimePinned verifies a compiled query evaluates an undefined CurrentTime to the
// time it pinned at compile time, the same time its probes fold to, on every entry point.
func TestCurrentTimePinned(t *testing.T) {
	q, err := Parse(`X == CurrentTime`)
	if err != nil {
		t.Fatal(err)
	}
	if !q.ReadsCurrentTime() {
		t.Fatal("ReadsCurrentTime = false for a query reading CurrentTime")
	}
	q.prog.now = 12345 // a time the clock cannot produce, so only the pin can explain it
	ad, _ := classad.Parse(`[X = 12345]`)
	if !q.Matches(ad) || !q.Matcher().Matches(ad) {
		t.Error("query did not evaluate CurrentTime to the pinned time")
	}
	if p := q.Probes(); len(p) != 1 || p[0].Vals[0].String() != "12345" {
		t.Errorf("probes = %v, want X == 12345", p)
	}
	q2, _ := Parse(`X == 1`)
	if q2.ReadsCurrentTime() {
		t.Error("ReadsCurrentTime = true for a query without CurrentTime")
	}
}

// TestCurrentTimeExactProbes verifies a CurrentTime query's probes, folded at the pinned
// time, are not reported as exact: they are wrong for an ad that defines CurrentTime.
func TestCurrentTimeExactProbes(t *testing.T) {
	q, _ := Parse(`CompletionDate > CurrentTime - 86400`)
	if p, exact := q.ExactProbes(); len(p) != 1 || exact {
		t.Errorf("ExactProbes = %v, %v; want one probe, inexact", p, exact)
	}
	q2, _ := Parse(`CompletionDate > 1700000000`)
	if p, exact := q2.ExactProbes(); len(p) != 1 || !exact {
		t.Errorf("ExactProbes = %v, %v; want one probe, exact", p, exact)
	}
}

// TestCurrentTimeVecDeclines verifies the vectorized executor declines a CurrentTime
// reference: a column reads an absent attribute as undefined, not as the time.
func TestCurrentTimeVecDeclines(t *testing.T) {
	src := newTestSource([]map[string]string{{"A": "1"}, {"A": "2", "CurrentTime": "5"}})
	q, _ := Parse(`A < CurrentTime`)
	if _, ok := q.VecEval(src, 2, nil); ok {
		t.Error("VecEval served a query reading CurrentTime")
	}
}
