package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestCountQueryLeftBiasedLogic: a count the columnar path answers from a query's probes
// alone equals the row path's count. Owner is a string, so `Owner && false` is ERROR and
// `Owner || true` is ERROR; folding either to its literal turned the query into a single
// probe the count was then served from. A presence probe beside a conjunct that is not a
// probe (`ClusterId > Owner`) is not the whole query either.
func TestCountQueryLeftBiasedLogic(t *testing.T) {
	c := presenceFixture(t, 4000)
	defer c.Close()
	for _, expr := range []string{
		`(Owner && false) || ClusterId >= 5`,
		`(Owner && false) || ProcId == 5`,
		`ClusterId >= 5 && (Owner || true)`,
		`ClusterId >= 5 && (true && Owner)`,
		`ProcId is undefined && ClusterId > Owner`,
		`ProcId is undefined && ClusterId < ClusterId`,
		`(ClusterId >= 5 || false) && ProcId >= 0`, // a sound fold: still served exactly
	} {
		want := rowTruth(t, c, expr)
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		got, served := c.CountQuery(q)
		if served && got != want {
			t.Errorf("%s: CountQuery = %d, row path = %d", expr, got, want)
		}
		if !served {
			t.Logf("%s: not served by the columnar path", expr)
		}
	}
}

// TestMatchFoldSoundness: the index pre-filter never drops a slot MatchClassAd matches.
// Each Requirements is `TARGET.N > 2 || <term>`, so the plan prunes on N unless the term
// is accounted for, and every term is TRUE on some slot with N = 1:
//   - left-biased && / ||: a literal on the right of an operator does not decide it;
//   - CurrentTime: a slot may define it, and an undefined one is the time, not undefined;
//   - finite-domain materialization: a function of an attribute is evaluated where the
//     attribute is absent too, and a nonzero number satisfies || as TRUE does.
func TestMatchFoldSoundness(t *testing.T) {
	t.Parallel()
	slots := []string{
		`[ Id = 1; N = 1; A = "str"; V = "1.0"; Requirements = true ]`,
		`[ Id = 2; N = 5; A = true; V = "2.0"; Requirements = true ]`,
		`[ Id = 3; N = 1; A = false; Requirements = true ]`,
		`[ Id = 4; N = 1; A = 5; V = 5; Requirements = true ]`,
		`[ Id = 5; N = 1; V = undefined; Requirements = true ]`,
		`[ Id = 6; N = 1; V = strcat("1", ".0"); Requirements = true ]`,
		`[ Id = 7; N = 1; CurrentTime = 5; Requirements = true ]`,
	}
	jobs := []string{}
	for _, term := range []string{
		`((TARGET.A && false) =!= false)`,
		`isError(TARGET.A && false)`,
		`((true && TARGET.A) =?= true)`,
		`((TARGET.A || false) =?= true)`,
		`(CurrentTime < 1000)`,
		`(MY.CurrentTime =!= undefined)`,
		`ifThenElse(CurrentTime =?= undefined, false, true)`,
		`(TARGET.CurrentTime =?= undefined)`,
		`(TARGET.CurrentTime < 1000)`,
		`ifThenElse(isUndefined(TARGET.V), true, TARGET.V == "9.9")`,
		`ifThenElse(isString(TARGET.V), TARGET.V == "1.0", true)`,
		`!ifThenElse(isUndefined(TARGET.V), false, TARGET.V == "9.9")`,
		`!ifThenElse(isUndefined(TARGET.V), 0, 1)`,
		`ifThenElse(TARGET.V == "1.0", 5, 0)`,
		`ifThenElse(isUndefined(TARGET.V), 7, 0)`,
	} {
		jobs = append(jobs, `[ Requirements = TARGET.N > 2 || `+term+`; Rank = 1 ]`)
	}
	jobs = append(jobs,
		`[ Deadline = CurrentTime + 100; Requirements = TARGET.N > 2 || Deadline < 1000; Rank = 1 ]`,
		`[ Deadline = MY.CurrentTime + 100; Requirements = TARGET.N > 2 || Deadline < 1000; Rank = 1 ]`)
	for _, opts := range []Options{
		{},
		{Shards: 2, CategoricalAttrs: []string{"A", "V"}, ValueAttrs: []string{"N", "CurrentTime"}},
	} {
		c := New(opts)
		for i, s := range slots {
			if err := c.Put([]byte(fmt.Sprintf("s%d", i)), mustAd(t, s)); err != nil {
				t.Fatal(err)
			}
		}
		c.Reindex()
		for _, jobText := range jobs {
			checkEntryPoints(t, "fold", c, jobText, bruteMatch(t, jobText, slots))
		}
		// A resource-side constraint reading CurrentTime: the slot that defines it passes.
		const constraint = `N > 2 || CurrentTime < 1000`
		cq, err := vm.Parse(constraint)
		if err != nil {
			t.Fatal(err)
		}
		job := `[ Requirements = true; Rank = 1 ]`
		want := map[int]bool{}
		for id := range bruteMatch(t, job, slots) {
			if cq.Matches(mustAd(t, slots[id-1])) {
				want[id] = true
			}
		}
		rm, err := c.MatchSortedRankedFiltered(mustAd(t, job), constraint, 0)
		if err != nil {
			t.Fatal(err)
		}
		got := map[int]bool{}
		for _, m := range rm {
			id, _ := m.Ad.EvaluateAttrInt("Id")
			got[int(id)] = true
		}
		if fmt.Sprint(got) != fmt.Sprint(want) || !want[7] {
			t.Errorf("MatchSortedRankedFiltered(%s) = %v, want %v", constraint, got, want)
		}
		// TARGET.CurrentTime has no slot-frame equivalent, so its conjunct drops out of the
		// plan rather than making the whole plan unprunable.
		if len(opts.ValueAttrs) > 0 {
			tj := mustAd(t, `[ Requirements = TARGET.N > 2 && TARGET.CurrentTime =?= undefined ]`)
			if _, prunable := c.slotMatchPlan(tj, jobValues(tj)); !prunable {
				t.Error("a TARGET.CurrentTime conjunct made the slot plan unprunable")
			}
		}
	}
}
