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
		{`Cpus >= 4`, "Cpus>="},                                   // conjunctive: one group
		{`Cpus >= 4 && Arch == "x"`, "Cpus>=,Arch=="},             // one group, two probes
		{`Cpus >= 4 || Arch == "x"`, "Cpus>= | Arch=="},           // two groups
		{`(Cpus >= 4 && Memory > 8) || Owner == "a"`, "Cpus>=,Memory> | Owner=="},
		{`Cpus >= 4 || SomeFlag`, "Cpus>= | SomeFlagtruthy"},      // a bare ref is a truthiness probe
		{`Cpus >= 4 || (2 * Foo)`, "Cpus>= | <none>"},             // a non-ref expression yields no probe
	}
	for _, tc := range cases {
		if got := render(tc.sql); got != tc.want {
			t.Errorf("ProbePlan(%q) = %q, want %q", tc.sql, got, tc.want)
		}
	}
}
