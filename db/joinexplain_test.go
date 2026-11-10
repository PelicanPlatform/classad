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

// A plan the planner left alone says so, and still describes itself fully. The order rules
// live in joinbuildside.go and joinchainorder.go; what matters here is that a decline is
// reported as "not reordered" rather than as an absent answer.
func TestExplainJoinReportsAPlanItDidNotChange(t *testing.T) {
	// Archives that keep their newest-first promise, which no plan may reorder.
	cat := probeChainFixture(t, 8192, 32, false)
	ex, err := cat.ExplainJoin(probeChainSpec("big1", "big2", "sel"), JoinLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Reordered {
		t.Errorf("an ordered archive's chain reports a reorder: %s", ex.OrderWhy)
	}
	if !strings.Contains(ex.OrderWhy, "fixed order") {
		t.Errorf("reason %q does not say why the order was kept", ex.OrderWhy)
	}
	if ex.Sides[0].Alias != "big1" {
		t.Errorf("streams %s, want the table as written", ex.Sides[0].Alias)
	}
	// Declined before pricing anything: an order it may not change costs nothing to not
	// compare, and reporting a cost here would invent a comparison that never happened.
	if ex.OrderCost != 0 || ex.OrderAsWrittenCost != 0 {
		t.Errorf("priced at %d against %d without comparing anything",
			ex.OrderCost, ex.OrderAsWrittenCost)
	}
}

// Each step reports its own kind, because a chain may mix them and which one a step is
// decides what its unmatched rows do.
func TestExplainJoinNamesEachStepsKind(t *testing.T) {
	cat := probeChainFixture(t, 8192, 32, true)
	spec := probeChainSpec("big1", "big2", "sel")
	spec.Steps[0].Kind = JoinLeft
	spec.Steps[1].Kind = JoinCross
	ex, err := cat.ExplainJoin(spec, JoinLimits{})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, s := range ex.Steps {
		got = append(got, s.Alias+":"+s.Kind)
	}
	if strings.Join(got, ",") != "big2:left,sel:cross" {
		t.Errorf("steps %v, want big2:left,sel:cross", got)
	}
	// A chain with a non-inner step may not be reordered, and the report must say that
	// rather than leave the reader to infer it from the order being unchanged.
	if ex.Reordered {
		t.Error("a chain with a non-inner step was reported as reordered")
	}
	if !strings.Contains(ex.OrderWhy, "inner") {
		t.Errorf("reason %q does not mention the inner-join rule", ex.OrderWhy)
	}
}

// A side whose table the catalog does not have is reported as a side with no access path,
// not as a failure of the whole report: the other sides' plans are still worth having, and
// the missing one is named.
func TestExplainJoinReportsASideItCannotRead(t *testing.T) {
	cat := probeChainFixture(t, 8192, 32, true)
	spec := probeChainSpec("big1", "big2", "sel")
	spec.Steps[1].Side.Table = "nosuchtable"
	ex, err := cat.ExplainJoin(spec, JoinLimits{})
	if err != nil {
		t.Fatal(err)
	}
	var missing *JoinSideExplain
	for i := range ex.Sides {
		if ex.Sides[i].Table == "nosuchtable" {
			missing = &ex.Sides[i]
		}
	}
	if missing == nil {
		t.Fatal("the report dropped the side it could not read")
	}
	if missing.Read != nil {
		t.Error("a table the catalog does not have reported an access path")
	}
	if !strings.Contains(missing.ReadWhy, "nosuchtable") {
		t.Errorf("ReadWhy is %q, want it to name the table", missing.ReadWhy)
	}
}

// WHAT THE PLAN WILL HOLD.
//
// MaxHashRows bounds the rows a join holds, and since #651 the planner chooses which side to
// hash in order to stay under it -- so that number is what decides whether a plan runs at
// all. The report has to carry it, and has to be honest about being a bound rather than a
// prediction: an index probe narrows a hashed side to the keys the streamed side turns out
// to hold, and counting those means reading the streamed side, which this does not do.

// On a plan with no index to narrow anything, the bound is not merely a bound: it is what
// the join holds, and the join is the oracle for that.
func TestExplainJoinBoundsWhatThePlanHolds(t *testing.T) {
	cat := buildSideFixture(t, 50, 5000, false)
	spec := bsSpec("small", "big")

	ex, err := cat.ExplainJoin(spec, JoinLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if ex.EstHashRows == 0 {
		t.Fatalf("the report gives no bound on what the plan holds: %s", ex.HashWhy)
	}
	if !strings.Contains(ex.HashWhy, "within") {
		t.Errorf("HashWhy is %q, want it to say the plan is within the limit", ex.HashWhy)
	}

	_, st := drainJoin(t, cat, spec)
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	// Anti-vacuity: a probe would make this an inequality rather than an equality, and the
	// point of this fixture is that there is no index for one.
	for _, s := range ex.Steps {
		if s.ProbeAttr != "" {
			t.Fatalf("step %s is probe-eligible, so the bound is not expected to be tight",
				s.Alias)
		}
	}
	if ex.EstHashRows != st.HashRows {
		t.Fatalf("the report bounds the hold at %d and the join held %d",
			ex.EstHashRows, st.HashRows)
	}
}

// Where an index DOES narrow the hashed side, the bound is far above what is held -- which
// is the whole reason it is published as a bound and not as an estimate. A number this far
// out would be worse than none if it were presented as a prediction.
func TestExplainJoinBoundIsAboveWhatAProbeLeavesHeld(t *testing.T) {
	cat := probeChainFixture(t, 8192, 32, true)
	spec := JoinSpec{
		Left:  JoinSide{Table: "sel", Alias: "sel"},
		Right: JoinSide{Table: "big1", Alias: "big1"},
		Cond:  []string{"sel.ClusterId == big1.ClusterId"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}

	ex, err := cat.ExplainJoin(spec, JoinLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Steps) != 1 || ex.Steps[0].ProbeAttr == "" {
		t.Fatalf("the plan does not narrow its hashed side, so there is no gap to check: %+v",
			ex.Steps)
	}

	_, st := drainJoin(t, cat, spec)
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if st.ProbeKeys == 0 {
		t.Fatalf("the probe did not fire, so the bound and the hold would agree anyway: %s",
			st.ProbeWhy)
	}
	if ex.EstHashRows <= st.HashRows {
		t.Fatalf("the report bounds the hold at %d and the join held %d; a bound below what "+
			"was held is not a bound", ex.EstHashRows, st.HashRows)
	}
	// The gap is the point. A bound within a factor of two of the truth would be worth
	// publishing as an estimate; this one is not, and the report must not imply it is.
	if ex.EstHashRows < 2*st.HashRows {
		t.Fatalf("the bound %d is close to the %d actually held, so this fixture no longer "+
			"shows why it is published as a bound", ex.EstHashRows, st.HashRows)
	}
}

// The question a reader most wants answered about a join that is failing: EXPLAIN says it
// will be refused, before it is run.
func TestExplainJoinPredictsARefusal(t *testing.T) {
	cat := buildSideFixture(t, 50, 5000, false)
	spec := bsSpec("small", "big")
	limits := JoinLimits{MaxHashRows: 20}

	ex, err := cat.ExplainJoin(spec, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ex.HashWhy, "will be refused") {
		t.Fatalf("HashWhy is %q, want it to say the plan will be refused", ex.HashWhy)
	}
	if ex.EstHashRows <= limits.MaxHashRows {
		t.Fatalf("the report bounds the hold at %d, within the limit of %d, yet says it "+
			"will be refused", ex.EstHashRows, limits.MaxHashRows)
	}

	// The join is the oracle: it has to actually refuse.
	var st JoinStats
	seq, err := cat.Join(spec, limits, &st)
	if err != nil {
		t.Fatal(err)
	}
	for range seq { //nolint:revive // draining is the point; the error arrives in stats
	}
	if st.Err == nil {
		t.Fatal("the report predicted a refusal and the join ran")
	}
	if !strings.Contains(st.Err.Error(), "would hold more than") {
		t.Fatalf("the join failed for some other reason: %v", st.Err)
	}
}

// A side that cannot be sized means the plan's hold is NOT known, and the sum of the sides
// that could be sized must not be handed back as though it bounded the whole plan.
func TestExplainJoinReportsNoBoundForASideItCannotSize(t *testing.T) {
	cat := probeChainFixture(t, 8192, 32, true)
	spec := probeChainSpec("big1", "big2", "sel")
	spec.Steps[1].Side.Table = "nosuchtable"

	ex, err := cat.ExplainJoin(spec, JoinLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if ex.EstHashRows != 0 {
		t.Fatalf("the report bounds the hold at %d with a side it could not size",
			ex.EstHashRows)
	}
	if !strings.Contains(ex.HashWhy, spec.Steps[1].Side.Alias) {
		t.Fatalf("HashWhy is %q, want it to name the side it could not size", ex.HashWhy)
	}
	// Anti-vacuity: the OTHER side was sizeable, so a partial sum was available to report
	// and was deliberately not reported.
	sized := 0
	for _, s := range ex.Steps {
		if s.EstRecords > 0 {
			sized++
		}
	}
	if sized == 0 {
		t.Fatal("no step could be sized, so withholding the total proves nothing")
	}
}

// A chain holds every table but the one it reads FROM, and the budget is spent across all
// of them -- so the plan's bound is the sum of its steps' and not any one of them.
func TestExplainJoinBoundIsTheSumOfItsSteps(t *testing.T) {
	cat := probeChainFixture(t, 8192, 32, true)
	ex, err := cat.ExplainJoin(probeChainSpec("sel", "big1", "big2"), JoinLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Steps) < 2 {
		t.Fatalf("the plan has %d step(s), so a sum of steps is the same as one step",
			len(ex.Steps))
	}
	sum := 0
	for _, s := range ex.Steps {
		if s.EstRecords == 0 {
			t.Fatalf("step %s reports no size, so the sum below proves nothing", s.Alias)
		}
		sum += s.EstRecords
	}
	// Anti-vacuity: with equal steps, a bound that took one step rather than their sum
	// would be wrong by a factor rather than detectably different.
	if sum == ex.Steps[0].EstRecords {
		t.Fatal("the steps sum to one step's size, so this cannot tell a sum from a pick")
	}
	if ex.EstHashRows != sum {
		t.Fatalf("the plan bounds its hold at %d and its steps at %d between them",
			ex.EstHashRows, sum)
	}
}

// Over the limit is not always fatal, and the report must not say it is. An index probe
// narrows the hashed side to the keys the streamed side turns out to hold, which this does
// not read -- so where a step could be narrowed, the verdict names it instead of claiming a
// refusal it cannot know about.
func TestExplainJoinSaysAProbeMightSaveAPlanOverTheLimit(t *testing.T) {
	cat := probeChainFixture(t, 8192, 32, true)
	spec := JoinSpec{
		Left:  JoinSide{Table: "sel", Alias: "sel"},
		Right: JoinSide{Table: "big1", Alias: "big1"},
		Cond:  []string{"sel.ClusterId == big1.ClusterId"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
	// Low enough that NEITHER orientation fits, so the planner cannot sidestep the question
	// by hashing the other side -- which is what it does at any limit the small side fits.
	ex, err := cat.ExplainJoin(spec, JoinLimits{MaxHashRows: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Steps) != 1 || ex.Steps[0].ProbeAttr == "" {
		t.Fatalf("the hashed side is not probe-eligible, so there is no caveat to make: %+v",
			ex.Steps)
	}
	if !strings.Contains(ex.HashWhy, "unless an index probe narrows") {
		t.Fatalf("HashWhy is %q, want it to say a probe may yet narrow the side", ex.HashWhy)
	}
	if !strings.Contains(ex.HashWhy, ex.Steps[0].Alias) {
		t.Errorf("HashWhy is %q, want it to name %s as the side that might be narrowed",
			ex.HashWhy, ex.Steps[0].Alias)
	}
}
