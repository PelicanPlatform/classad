package vm

import (
	"slices"
	"sort"
	"testing"
)

// TestReadPlanSeedsSelectedMember pins that a `.`-path select contributes BOTH the
// record it selects from and the member it selects.
//
// The member is there because a select does not stop at the record: one the record
// does not define chains to the enclosing scope, which is the ad being scanned. A
// store that seeded only the record would evaluate the query against an ad missing
// the attribute the chain resolves to, and drop records the evaluator matches.
func TestReadPlanSeedsSelectedMember(t *testing.T) {
	for _, tc := range []struct {
		expr  string
		seeds []string
	}{
		{`Res.Mem >= 4096`, []string{"Mem", "Res"}},
		{`A.B.Mem >= 4096`, []string{"A", "B", "Mem"}},
		{`Res.Mem >= Cpus`, []string{"Cpus", "Mem", "Res"}},
		{`TARGET.Res.Mem >= 4096`, []string{"Mem"}}, // the record is the target's; the chain is ours
		{`[ pick = Res ].pick.Mem >= 4096`, []string{"Mem", "Res", "pick"}},
		{`Cpus >= 4`, []string{"Cpus"}}, // no select, no extra seed
	} {
		q, err := Parse(tc.expr)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.expr, err)
		}
		got := append([]string(nil), q.ReadPlan().Seeds...)
		sort.Strings(got)
		want := append([]string(nil), tc.seeds...)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: seeds = %v, want %v", tc.expr, got, want)
		}
	}
}
