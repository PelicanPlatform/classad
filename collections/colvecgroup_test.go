package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// groupStatsTruth groups by decoding records the ordinary way: the reference the columnar tiers have to
// agree with. It counts every match, reporting non-integer group values separately rather than skipping
// them, because skipping them is how a test hides the case where the implementation drops records.
func groupStatsTruth(t *testing.T, c *Collection, expr, attr string) (map[int64]int, int) {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]int{}
	nonInt := 0
	for ad := range c.Query(q) {
		n, err := ad.EvaluateAttr(attr).IntValue()
		if err != nil {
			nonInt++
			continue
		}
		out[n]++
	}
	return out, nonInt
}

func checkGroupsMatchRows(t *testing.T, c *Collection, expr, attr string) []GroupCount {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.GroupCountQuery(q, attr)
	if !ok {
		t.Errorf("%q GROUP BY %s: declined", expr, attr)
		return nil
	}
	want, nonInt := groupStatsTruth(t, c, expr, attr)
	if nonInt != 0 {
		t.Fatalf("%q GROUP BY %s: fixture has %d matches whose group value is not an integer", expr, attr, nonInt)
	}
	if len(got) != len(want) {
		t.Errorf("%q GROUP BY %s: %d groups, row path found %d", expr, attr, len(got), len(want))
	}
	for _, g := range got {
		n, err := g.Value.IntValue()
		if err != nil {
			t.Errorf("group value %v is not an integer", g.Value)
			continue
		}
		if want[n] != g.Count {
			t.Errorf("%q GROUP BY %s: group %d counted %d, row path %d", expr, attr, n, g.Count, want[n])
		}
	}
	return got
}

// TestGroupCountGeneralPredicates covers the predicates the hand-written column scan cannot lower. The
// ungrouped count has served these since the vector executor landed; grouping declined them and fell to a
// record scan.
func TestGroupCountGeneralPredicates(t *testing.T) {
	c := groupReadFixture(t)
	for _, expr := range []string{
		`Owner == "user3"`,                     // a string comparison
		"RequestMemory > RequestCpus",          // attribute to attribute
		"RequestMemory > RequestCpus * 512",    // arithmetic
		"RequestMemory >= 2048 || ProcId >= 5", // a disjunction
		"WantCheckpoint",                       // a bare boolean column
		`Owner == "user3" || JobStatus == 4`,   // string OR numeric
	} {
		checkGroupsMatchRows(t, c, expr, "ProcId")
		// The point of the change: these must be answered from the columns. Without this the whole test
		// passes by declining every case, which is the behavior it was written to replace.
		if split := lastGroupSplit.Load(); split == nil || split.vecBlocks == 0 {
			t.Errorf("%q: no block was vectorized (%+v), so the grouping fell back everywhere", expr, split)
		}
	}
}

// TestGroupCountScopedBlocks covers the tier where the vector executor declines a block and the grouping
// falls to per-record evaluation WITHIN the block -- it must still contribute its groups.
func TestGroupCountScopedBlocks(t *testing.T) {
	c := groupReadFixture(t)
	// An Elvis operator is not lowered by the vector executor, so every block goes to the scoped path.
	// The exact form matters: `(Missing ?: RequestMemory) > 4096` DOES vectorize, so a test written with
	// that one asserts the scoped tier while never reaching it.
	const expr = `RequestMemory > 4096 ?: false`
	checkGroupsMatchRows(t, c, expr, "ProcId")
	split := lastGroupSplit.Load()
	if split == nil || split.scopeBlocks == 0 {
		t.Fatalf("no block took the scoped path (%+v), so that tier is untested here", split)
	}
	if split.vecBlocks != 0 {
		t.Errorf("%d blocks vectorized: the expression was supposed to be one the executor declines", split.vecBlocks)
	}
}

// TestGroupCountRowWindows covers the tier with no columnar block at all: the active segment, which no
// accelerator covers. A tier that silently contributed no groups would under-report every group.
func TestGroupCountRowWindows(t *testing.T) {
	// Its own fixture: this test APPENDS, and the shared one is read-only by contract.
	c := scopeFixtureCodec(t, groupFixtureRecords)
	defer c.Close()
	// Records appended after the accelerator was built live in the active segment, uncovered.
	for i := groupFixtureRecords; i < groupFixtureRecords+500; i++ {
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\n"+
			"RequestCpus = %d\nOwner = \"user%d\"\nCmd = \"/x\"\nWantCheckpoint = %t\n"+
			"Args = \"--a\"\nIwd = \"/y\"", i, i%10, 1+i%5, 1024+(i%32)*512, 1+i%8, i%512, i%3 == 0)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	const expr = `Owner == "user3" || ProcId >= 8`
	got := checkGroupsMatchRows(t, c, expr, "ProcId")
	split := lastGroupSplit.Load()
	if split == nil || split.rowWindows == 0 {
		t.Fatalf("no window took the row path (%+v), so that tier is untested here", split)
	}
	if len(got) == 0 {
		t.Fatal("no groups at all")
	}
}

// TestGroupCountSupersededRecords covers the churn tier -- a block whose records are mostly superseded
// goes to per-record evaluation, because the vector executor evaluates every record in a block including
// ones nobody can see -- and the visibility rule itself: a superseded record must not be counted in any
// group.
//
// Overwriting a FRACTION of every old segment is what makes this deterministic. Overwriting all of them
// leaves each old segment fully superseded, and a fully superseded segment is reaped -- so there is no
// covered block left to be churn, and the tier is silently not reached. That is what the first version of
// this test did: at 20000 records it reached the tier with exactly one straggler block, and at 6000 with
// none at all.
func TestGroupCountSupersededRecords(t *testing.T) {
	// Its own fixture: this test OVERWRITES most of it, and the shared one is read-only by contract.
	c := scopeFixtureCodec(t, groupFixtureRecords)
	defer c.Close()
	for i := 0; i < groupFixtureRecords; i++ {
		if i%5 == 0 {
			continue // leave a fifth of every old segment visible: mostly dead, not dead
		}
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\n"+
			"RequestCpus = %d\nOwner = \"user%d\"\nCmd = \"/x\"\nWantCheckpoint = %t\n"+
			"Args = \"--a\"\nIwd = \"/y\"", i, 7, 1+i%5, 8192, 1+i%8, i%512, false)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	const expr = "RequestMemory >= 2048 || ProcId >= 5"
	checkGroupsMatchRows(t, c, expr, "ProcId")
	split := lastGroupSplit.Load()
	if split == nil || split.churnBlocks == 0 {
		t.Fatalf("no block hit the churn tier (%+v), so that tier is untested here", split)
	}
}

// TestGroupStatsGeneralPredicateAggregates checks the value aggregates under a general predicate, not
// just the counts -- the aggregate columns are read in the same pass, so a tier that got the matching set
// right could still read the wrong record's value.
func TestGroupStatsGeneralPredicateAggregates(t *testing.T) {
	c := groupReadFixture(t)
	const expr = `Owner == "user3" || ProcId >= 8`
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.GroupStatsQuery(q, "ProcId", []string{"RequestMemory"})
	if !ok {
		t.Fatal("declined")
	}
	// Reference: accumulate the same aggregate by decoding records.
	type acc struct {
		n        int
		min, max int64
		sum      int64
	}
	want := map[int64]*acc{}
	for ad := range c.Query(q) {
		p, err := ad.EvaluateAttr("ProcId").IntValue()
		if err != nil {
			t.Fatal(err)
		}
		m, err := ad.EvaluateAttr("RequestMemory").IntValue()
		if err != nil {
			continue
		}
		a := want[p]
		if a == nil {
			a = &acc{min: m, max: m}
			want[p] = a
		}
		a.n++
		a.sum += m
		if m < a.min {
			a.min = m
		}
		if m > a.max {
			a.max = m
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%d groups, row path found %d", len(got), len(want))
	}
	for _, g := range got {
		p, _ := g.Value.IntValue()
		a := want[p]
		if a == nil {
			t.Errorf("group %d is not in the row path's result", p)
			continue
		}
		ns := g.Stats[0]
		if ns.N != a.n || int64(ns.Min) != a.min || int64(ns.Max) != a.max || ns.IntSum != a.sum {
			t.Errorf("group %d: n=%d min=%v max=%v sum=%d, row path n=%d min=%d max=%d sum=%d",
				p, ns.N, ns.Min, ns.Max, ns.IntSum, a.n, a.min, a.max, a.sum)
		}
	}
	if split := lastGroupSplit.Load(); split == nil || split.vecBlocks == 0 {
		t.Errorf("nothing vectorized (%+v)", split)
	}
}

// TestGroupCountIndexedConstraintStillDeclines pins the routing guard: the vector tier evaluates every
// visible record, so a constraint an index can prune is left to the scan. Removing the guard would make
// exactly those queries slower while every test above still passed.
func TestGroupCountIndexedConstraintStillDeclines(t *testing.T) {
	cd, err := NewZSTDCodec(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Same fixture, but with Owner categorically indexed, so a query on Owner has a pruning index.
	c := New(Options{Shards: 1, SegmentSize: 1 << 16, Codec: cd, CategoricalAttrs: []string{"Owner"}})
	defer c.Close()
	for i := 0; i < groupFixtureRecords; i++ {
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\n"+
			"RequestCpus = %d\nOwner = \"user%d\"\nCmd = \"/x\"", i, i%10, 1+i%5,
			1024+(i%32)*512, 1+i%8, i%512)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments")
	}
	q := mustParseQuery(t, `Owner == "user3"`)
	if !c.indexCanPrune(q) {
		t.Fatal("Owner is indexed but indexCanPrune says otherwise: this test cannot check the guard")
	}
	if _, ok := c.GroupCountQuery(q, "ProcId"); ok {
		t.Error("served a constraint an index can prune; the indexed scan visits far fewer records " +
			"than evaluating every visible one")
	}
	// And the guard is about the INDEX, not about string predicates: the same query on an unindexed
	// attribute is served, so the decline above is the routing decision and not a hidden refusal.
	if _, ok := c.GroupCountQuery(mustParseQuery(t, `Cmd == "/x"`), "ProcId"); !ok {
		t.Error("declined an unindexed string predicate too, so the guard is not what declined above")
	}
}

// BenchmarkGroupCountGeneralPredicate measures what the second tier buys: the same predicate grouped from
// the columns instead of from a record scan.
func BenchmarkGroupCountGeneralPredicate(b *testing.B) {
	c := scopeFixtureCodec(b, 60000)
	defer c.Close()
	for _, expr := range []string{
		`Owner == "user3"`,
		"RequestMemory > RequestCpus * 512",
		"RequestMemory >= 2048 || ProcId >= 5",
	} {
		q, err := vm.Parse(expr)
		if err != nil {
			b.Fatal(err)
		}
		if _, ok := c.GroupCountQuery(q, "ProcId"); !ok {
			b.Fatalf("%q declined", expr)
		}
		b.Run("columnar/"+expr, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				c.GroupCountQuery(q, "ProcId")
			}
		})
		b.Run("rows/"+expr, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				m := map[int64]int{}
				for ad := range c.Query(q) {
					if n, err := ad.EvaluateAttr("ProcId").IntValue(); err == nil {
						m[n]++
					}
				}
			}
		})
	}
}
