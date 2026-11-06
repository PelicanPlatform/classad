package db

import (
	"strings"
	"testing"
)

// The explain must describe the plan the join actually runs.
//
// This is the property worth testing and the only one that is not a restatement of the
// code: the report is built by calling the same planner the join calls, so the way it goes
// wrong is by drifting -- reporting the decision of a planner that is no longer the one in
// the path. So the join is RUN, and its stats are the oracle for what the explain claimed.
func TestExplainJoinAgreesWithTheJoinItDescribes(t *testing.T) {
	cat := probeChainFixture(t, 8192, 32, true)
	for _, tc := range []struct {
		name    string
		spec    JoinSpec
		planner string
	}{
		{"chain", probeChainSpec("big1", "big2", "sel"), "chain"},
		{"two tables", JoinSpec{
			Left:  JoinSide{Table: "big1", Alias: "big1"},
			Right: JoinSide{Table: "sel", Alias: "sel"},
			Cond:  []string{"big1.ClusterId == sel.ClusterId"},
			Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
		}, "build-side"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex, err := cat.ExplainJoin(tc.spec, JoinLimits{})
			if err != nil {
				t.Fatal(err)
			}
			_, st := drainChain(t, cat, tc.spec)

			if ex.Planner != tc.planner {
				t.Errorf("planner %q, want %q", ex.Planner, tc.planner)
			}
			if len(ex.Sides) == 0 {
				t.Fatal("the explain named no sides")
			}
			// The streamed side is the one decision everything else follows from.
			streamed := ex.Sides[0]
			if !streamed.Streamed {
				t.Errorf("%s is first but not marked streamed", streamed.Alias)
			}
			wantFrom := st.ChainFromAlias
			if tc.planner == "build-side" {
				wantFrom = tc.spec.Left.Alias
				if st.BuildSideSwapped {
					wantFrom = tc.spec.Right.Alias
				}
			} else if wantFrom == "" {
				wantFrom = tc.spec.Left.Alias
			}
			if streamed.Alias != wantFrom {
				t.Errorf("the explain streams %s, the join streamed %s",
					streamed.Alias, wantFrom)
			}
			wantWhy := st.ChainOrderWhy
			if tc.planner == "build-side" {
				wantWhy = st.BuildSideWhy
			}
			if ex.OrderWhy != wantWhy {
				t.Errorf("the explain's reason is %q, the join's was %q", ex.OrderWhy, wantWhy)
			}

			// Every step the join ran is a step the explain described, in the same order,
			// agreeing on whether that step's table could be narrowed.
			if len(ex.Steps) != len(st.ProbeSteps) {
				t.Fatalf("the explain has %d steps, the join ran %d",
					len(ex.Steps), len(st.ProbeSteps))
			}
			for i, s := range ex.Steps {
				ran := st.ProbeSteps[i]
				if s.Alias != ran.Alias {
					t.Errorf("step %d: explain folds %s, the join folded %s",
						i, s.Alias, ran.Alias)
				}
				// The explain reports ELIGIBILITY, so it may name an attribute where the
				// run then declined on the key count -- but never the reverse: a step the
				// run narrowed must not be one the explain said could not be.
				if ran.Keys > 0 && s.ProbeAttr == "" {
					t.Errorf("step %s was narrowed on %s, but the explain said it could "+
						"not be: %q", s.Alias, ran.Attr, s.ProbeWhy)
				}
			}
		})
	}
}

// Every side gets an access path, including one with no constraint of its own -- "reads
// everything" is a path a reader wants described rather than a hole in the report.
func TestExplainJoinDescribesEverySidesRead(t *testing.T) {
	cat := probeChainFixture(t, 8192, 32, true)
	ex, err := cat.ExplainJoin(probeChainSpec("big1", "big2", "sel"), JoinLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ex.Sides {
		if s.Read == nil {
			t.Errorf("%s has no read plan: %s", s.Alias, s.ReadWhy)
			continue
		}
		if s.Read.Plan == "" {
			t.Errorf("%s has an empty access path", s.Alias)
		}
		if s.Read.TotalAds == 0 {
			t.Errorf("%s reports no rows at all, so nothing was really planned", s.Alias)
		}
	}
	// The report has to say what the join would run under, not what the caller asked for:
	// a plan that stops at a limit the caller never set is the thing worth reporting.
	if ex.Limits.MaxHashRows != DefaultJoinLimits().MaxHashRows {
		t.Errorf("limits report %d max hash rows, want the store's %d",
			ex.Limits.MaxHashRows, DefaultJoinLimits().MaxHashRows)
	}
}

// A reordered plan says so, and says what it read instead.
func TestExplainJoinReportsTheReorder(t *testing.T) {
	cat := probeChainFixture(t, 8192, 32, true)
	ex, err := cat.ExplainJoin(probeChainSpec("big1", "big2", "sel"), JoinLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !ex.Reordered {
		t.Fatalf("the plan reads %v where %v was written, but does not report a reorder",
			[]string{ex.Sides[0].Alias}, ex.AsWritten)
	}
	if strings.Join(ex.AsWritten, ",") != "big1,big2,sel" {
		t.Errorf("as written %v, want the order the caller wrote", ex.AsWritten)
	}
	if ex.OrderCost == 0 || ex.OrderAsWrittenCost == 0 {
		t.Errorf("a reorder was made but priced at %d against %d",
			ex.OrderCost, ex.OrderAsWrittenCost)
	}
	if ex.OrderCost >= ex.OrderAsWrittenCost {
		t.Errorf("the chosen plan is priced at %d, no better than the %d it replaced",
			ex.OrderCost, ex.OrderAsWrittenCost)
	}
}

// An unplannable join is an error, not a report full of blanks.
func TestExplainJoinRefusesWhatItCannotPlan(t *testing.T) {
	cat := probeChainFixture(t, 8192, 32, true)
	spec := probeChainSpec("big1", "big2", "sel")
	spec.Steps[0].Cond = []string{"big1.ClusterId =="}
	if _, err := cat.ExplainJoin(spec, JoinLimits{}); err == nil {
		t.Error("explained a join whose ON condition does not parse")
	}
}
