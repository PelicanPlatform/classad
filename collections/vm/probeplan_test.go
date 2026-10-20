package vm

import (
	"fmt"
	"testing"
)

// TestProbePlanDNF checks the disjunction-of-conjunctions extraction: one group per
// top-level `||` disjunct, each group the AND of its index-satisfiable conjuncts.
func TestProbePlanDNF(t *testing.T) {
	// helper: render a plan as "group0attrs | group1attrs | ..."
	render := func(sql string) string {
		q, err := Parse(sql)
		if err != nil {
			t.Fatalf("parse %q: %v", sql, err)
		}
		out := ""
		for gi, g := range q.ProbePlan() {
			if gi > 0 {
				out += " | "
			}
			for pi, p := range g.Probes {
				if pi > 0 {
					out += ","
				}
				out += fmt.Sprintf("%s%s", p.Attr, p.Op)
			}
			if len(g.Probes) == 0 {
				out += "<none>"
			}
		}
		return out
	}
	cases := []struct{ sql, want string }{
		{`Cpus >= 4`, "Cpus>="},                         // conjunctive: one group
		{`Cpus >= 4 && Arch == "x"`, "Cpus>=,Arch=="},   // one group, two probes
		{`Cpus >= 4 || Arch == "x"`, "Cpus>= | Arch=="}, // two groups
		{`(Cpus >= 4 && Memory > 8) || Owner == "a"`, "Cpus>=,Memory> | Owner=="},
		{`Cpus >= 4 || SomeFlag`, "Cpus>= | SomeFlagtruthy"}, // a bare ref is a truthiness probe
		{`Cpus >= 4 || (2 * Foo)`, "Cpus>= | <none>"},        // a non-ref expression yields no probe
	}
	for _, tc := range cases {
		if got := render(tc.sql); got != tc.want {
			t.Errorf("ProbePlan(%q) = %q, want %q", tc.sql, got, tc.want)
		}
	}
}

// TestProbesLeftBiasedLogic verifies the probes of a query whose && / || has a literal
// operand describe the query as written. && and || are evaluated left to right, and a
// literal on the right decides nothing about a left operand that is ERROR or not boolean:
// with X = "str", `X && false` is ERROR, so `(X && false) || ProcId == 5` matches nothing,
// and `true && X` with X = 5 is TRUE although X itself is not. Folded to `ProcId == 5` or
// to the bare `X`, they would be reported as exactly equivalent to the query, and a count
// answered from them would be wrong.
func TestProbesLeftBiasedLogic(t *testing.T) {
	for _, tc := range []struct {
		src   string
		exact bool
	}{
		{`(X && false) || ProcId == 5`, false},
		{`ProcId == 5 && (true && X)`, false},
		{`ProcId == 5 && (X && true)`, false},
		{`(false || X) && ProcId == 5`, false},
		{`ProcId == 5 && (true && X > 3)`, true}, // a comparison passes through `true &&`
		{`(ProcId == 5 || false) && X > 3`, true},
		{`false || ProcId == 5`, true},
	} {
		q, err := Parse(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		if _, exact := q.ExactProbes(); exact != tc.exact {
			t.Errorf("%s: ExactProbes exact = %v, want %v", tc.src, exact, tc.exact)
		}
	}
	// The fold that remains sound still feeds the planner.
	q, _ := Parse(`ProcId == 5 && (true && X > 3)`)
	if p := q.Probes(); len(p) != 2 {
		t.Errorf("probes = %v, want ProcId == 5 and X > 3", p)
	}
}
