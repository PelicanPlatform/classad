package collections

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// groupReadFixture is the ONE fixture the read-only grouping tests share.
//
// Each of them used to build its own, and building one costs ~15s under the race detector (20k puts plus
// 80 full scans of demand training). Eleven of those pushed the collections race job past its 25-minute
// CI budget -- it timed out inside unrelated tests, which is how a cheap-looking `20000` in a fixture call
// turns into a red build nobody can attribute. One shared build is ~15s for all of them.
//
// It is never closed: the tests only read, and the process exit reclaims it. A test that MUTATES the
// collection must build its own, or it changes what every later test sees.
var (
	groupFixtureOnce sync.Once
	groupFixtureColl *Collection
)

func groupReadFixture(tb testing.TB) *Collection {
	tb.Helper()
	groupFixtureOnce.Do(func() { groupFixtureColl = scopeFixtureCodec(tb, groupFixtureRecords) })
	if groupFixtureColl == nil {
		tb.Skip("shared grouping fixture unavailable (no sealed segments)")
	}
	return groupFixtureColl
}

// groupFixtureRecords is sized to what these tests actually need -- several sealed segments, so there are
// multiple columnar blocks and more than one window -- and no larger. 20000 records bought nothing a
// quarter of that does not.
const groupFixtureRecords = 3000

// rowGroupTruth computes the same GROUP BY by decoding records the ordinary way, which is what the columnar
// path has to agree with.
//
// It counts EVERY matching record, returning the number whose group value is not an integer separately
// instead of skipping them. Skipping them is how a first version of this test managed to pass while the
// implementation silently dropped exactly those records: the test and the code had the same blind spot, so
// their agreement proved nothing about the records that differ.
func rowGroupTruth(t *testing.T, c *Collection, constraint, attr string) (map[int64]int, int) {
	t.Helper()
	q, err := vm.Parse(constraint)
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

// rowGroupTexts is rowGroupTruth for a group column that is not necessarily numeric: it keys by the
// rendering of the row path's own evaluated value, so the reference stays the ordinary evaluator rather
// than a second implementation of the grouping rules.
//
// Both sides key by classad.Value.String() on values the two paths each produced, which is also how two
// storage keys that render alike get folded together -- the same merge the db layer does by rendered
// text, and the reason an absent attribute and a literal "undefined" land in one group.
func rowGroupTexts(t *testing.T, c *Collection, constraint, attr string) map[string]int {
	t.Helper()
	q, err := vm.Parse(constraint)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for ad := range c.Query(q) {
		out[ad.EvaluateAttr(attr).String()]++
	}
	return out
}

// checkGroupTexts asserts a columnar group set is exactly the row path's partition.
func checkGroupTexts(t *testing.T, c *Collection, got []GroupCount, constraint, attr string) {
	t.Helper()
	want := rowGroupTexts(t, c, constraint, attr)
	gotM := map[string]int{}
	for _, g := range got {
		gotM[g.Value.String()] += g.Count
	}
	for k, n := range want {
		if gotM[k] != n {
			t.Errorf("%q GROUP BY %s: group %s counted %d, row path %d", constraint, attr, k, gotM[k], n)
		}
	}
	for k, n := range gotM {
		if _, ok := want[k]; !ok {
			t.Errorf("%q GROUP BY %s: group %s (%d records) is not in the row path's result at all",
				constraint, attr, k, n)
		}
	}
}

// TestGroupCountStringDictionary is the dictionary half of the string group column: the path where a
// block's distinct values are resolved to groups ONCE and the per-record step is an array index.
//
// Which reader a block uses is decided by the DATA, not by the query (see dictWorthwhile), so this uses
// the fixture built for dictionaries -- the shared scope fixture leaves every string field positional --
// and asserts the encoding happened, or it would exercise none of this and still pass every count.
func TestGroupCountStringDictionary(t *testing.T) {
	c := strDictFixture(t, 6000, 200)
	defer c.Close()
	if withDict, blocks := dictStats(t, c, "Owner"); withDict == 0 {
		t.Fatalf("no block of %d dictionary-encodes Owner: the dictionary reader is not exercised", blocks)
	}
	for _, tc := range []struct{ constraint, attr string }{
		{"RequestMemory > 4096", "Owner"},
		{"RequestMemory > 4096", "Cmd"},
		{"RequestMemory > 4096", "Iwd"},
		{`Owner == "user3"`, "Owner"}, // the group column IS the predicate column
		{`Owner != "user3"`, "Cmd"},   // a string predicate, so the vector tier
		{"RequestCpus >= 0", "Owner"}, // every record matches, via the predicate tier
	} {
		q, err := vm.Parse(tc.constraint)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := c.GroupCountQuery(q, tc.attr)
		if !ok {
			t.Errorf("%q GROUP BY %s: declined", tc.constraint, tc.attr)
			continue
		}
		checkGroupTexts(t, c, got, tc.constraint, tc.attr)
	}
	got, ok := c.GroupCountAll("Owner")
	if !ok {
		t.Fatal("GROUP BY Owner over every record: declined")
	}
	checkGroupTexts(t, c, got, "true", "Owner")
}

// TestGroupCountStringColumn is the anti-vacuity gate for the string group column: these shapes must be
// SERVED from the columns, not merely answered correctly by falling back to the scan, which is the whole
// point of reading the group value from the columns.
//
// The shared fixture leaves every string field POSITIONAL (its Owner has ~512 distinct values over a
// ~378-record row group, so dictWorthwhile declines), so this covers the walk-the-region reader across
// the tiers and TestGroupCountStringDictionary covers the other one.
func TestGroupCountStringColumn(t *testing.T) {
	c := groupReadFixture(t)
	if withDict, _ := dictStats(t, c, "Owner"); withDict != 0 {
		t.Fatalf("Owner is dictionary-encoded in %d blocks of the shared fixture; this test is the "+
			"positional reader's and no longer covers it", withDict)
	}
	for _, tc := range []struct{ constraint, attr, why string }{
		{"RequestMemory > 4096", "Owner", "numeric predicate, string group column"},
		{"RequestMemory > 4096", "Cmd", "a second string column"},
		{"RequestMemory > 4096", "Iwd", "distinct per record: ~3000 groups over several blocks"},
		{`Owner == "user3"`, "Cmd", "a STRING predicate routes through the vector tier"},
		{"RequestMemory > RequestCpus", "Owner", "attribute-to-attribute: the vector tier again"},
		{"ClusterId > 9999999", "Owner", "nothing matches: no groups at all"},
	} {
		q, err := vm.Parse(tc.constraint)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := c.GroupCountQuery(q, tc.attr)
		if !ok {
			t.Errorf("%q GROUP BY %s: declined, but this is served from the columns (%s)",
				tc.constraint, tc.attr, tc.why)
			continue
		}
		checkGroupTexts(t, c, got, tc.constraint, tc.attr)
		// The groups must partition the matches: the same records the ungrouped count returns.
		total := 0
		for _, g := range got {
			total += g.Count
		}
		if plain, ok := c.CountQuery(q); ok && total != plain {
			t.Errorf("%q GROUP BY %s: groups sum to %d but count(*) is %d",
				tc.constraint, tc.attr, total, plain)
		}
	}
	// The unconstrained form is a separate entry point with no predicate to analyze.
	got, ok := c.GroupCountAll("Owner")
	if !ok {
		t.Fatal("GROUP BY Owner over every record: declined")
	}
	checkGroupTexts(t, c, got, "true", "Owner")
	total := 0
	for _, g := range got {
		total += g.Count
	}
	if total != c.Len() {
		t.Errorf("GROUP BY Owner: groups sum to %d but the collection holds %d records", total, c.Len())
	}
	// Sorted, so an ORDER BY on the group column needs no re-derivation.
	for i := 1; i < len(got); i++ {
		a, _ := got[i-1].Value.StringValue()
		b, _ := got[i].Value.StringValue()
		if !groupValueLess(got[i-1].Value, got[i].Value) {
			t.Errorf("GROUP BY Owner: groups not ordered at %d (%q then %q)", i, a, b)
		}
	}
}

func TestGroupCountQueryMatchesRowPath(t *testing.T) {
	c := groupReadFixture(t)

	for _, tc := range []struct{ constraint, attr string }{
		{"RequestMemory > 4096", "ProcId"},
		{"RequestMemory > 4096 && RequestCpus >= 4", "JobStatus"},
		{"ProcId >= 0", "JobStatus"},
		{"JobStatus == 1", "ProcId"},
		{"RequestCpus >= 0", "RequestCpus"}, // group column IS the predicate column
	} {
		q, err := vm.Parse(tc.constraint)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := c.GroupCountQuery(q, tc.attr)
		if !ok {
			t.Fatalf("%q GROUP BY %s: declined; this shape should be columnar", tc.constraint, tc.attr)
		}
		want, nonInt := rowGroupTruth(t, c, tc.constraint, tc.attr)
		if nonInt != 0 {
			t.Fatalf("%q GROUP BY %s: fixture has %d matches whose group value is not an integer, so this "+
				"case cannot check agreement -- it belongs in the decline test", tc.constraint, tc.attr, nonInt)
		}
		if len(got) != len(want) {
			t.Errorf("%q GROUP BY %s: %d groups, row path found %d", tc.constraint, tc.attr, len(got), len(want))
		}
		total := 0
		for _, g := range got {
			n, err := g.Value.IntValue()
			if err != nil {
				t.Errorf("group value %v is not an integer", g.Value)
				continue
			}
			if want[n] != g.Count {
				t.Errorf("%q GROUP BY %s: group %d counted %d, row path %d",
					tc.constraint, tc.attr, n, g.Count, want[n])
			}
			total += g.Count
		}
		// The groups must partition the matches: the same records the ungrouped count returns, no more.
		if plain, ok := c.CountQuery(q); ok && total != plain {
			t.Errorf("%q GROUP BY %s: groups sum to %d but count(*) is %d",
				tc.constraint, tc.attr, total, plain)
		}
		// Sorted ascending, so an ORDER BY on the group column needs no re-derivation.
		for i := 1; i < len(got); i++ {
			a, _ := got[i-1].Value.IntValue()
			b, _ := got[i].Value.IntValue()
			if a >= b {
				t.Errorf("%q GROUP BY %s: groups not ascending at %d (%d then %d)", tc.constraint, tc.attr, i, a, b)
			}
		}
	}
}

// TestGroupCountQueryDeclinesUnservable pins the declines that are about the GROUP COLUMN: this path
// answers by reading that column, so a column it cannot read is out of scope by construction, not merely
// unimplemented.
//
// It used to assert that a STRING column declines too. That stopped being true when the group column
// learned to read strings through the block dictionary, which is what TestGroupCountStringColumn now
// covers.
//
// It used to also assert that `Owner == "user3"`, `RequestMemory > RequestCpus` and `true` decline. Those
// were true of the predicate ANALYSIS and stopped being true when the second tier evaluated the query
// against the columns instead -- so the assertions failed on a change that made those queries 9-22x
// faster. Asserting which tier answers pins an implementation detail that improvements are supposed to
// change; the answers for those shapes are checked against the row path in TestGroupCountGeneralPredicates
// instead, which is the contract.
func TestGroupCountQueryDeclinesUnservable(t *testing.T) {
	c := groupReadFixture(t)
	for _, tc := range []struct{ constraint, attr, why string }{
		{"RequestMemory > 4096", "NoSuchAttr", "attribute absent from the schema"},
		{"RequestMemory > 4096", "WantCheckpoint", "a bool is bit-packed, not a column this can read"},
	} {
		q, err := vm.Parse(tc.constraint)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := c.GroupCountQuery(q, tc.attr); ok {
			t.Errorf("%q GROUP BY %s: expected a decline (%s)", tc.constraint, tc.attr, tc.why)
		}
	}
	// A query that cannot be lowered at all still declines, so the fallback stays reachable -- without
	// this the test would pass on an implementation that claimed to serve everything.
	//
	// It used to use `size(Args) > 0`, which stopped declining when strict built-in calls became
	// native, and then ifThenElse, which stopped declining when conditionals became a branch. That is
	// four expired assertions in this one test, so the guard is now something that cannot be lowered
	// for a STRUCTURAL reason rather than a current-implementation one: unparse() takes the SYNTAX of
	// its argument, not its value, and no value-level instruction can carry an AST.
	if _, ok := c.GroupCountQuery(mustParseQuery(t, `unparse(RequestMemory) == "RequestMemory"`), "ProcId"); ok {
		t.Error("served a non-native query; nothing can evaluate that against columns")
	}
}

// BenchmarkGroupCount is the point of the change: the grouped form should cost what the ungrouped count
// costs, not 210x it.
func BenchmarkGroupCount(b *testing.B) {
	c := scopeFixtureCodec(b, 60000)
	defer c.Close()
	q, err := vm.Parse("RequestMemory > 4096")
	if err != nil {
		b.Fatal(err)
	}
	if _, ok := c.GroupCountQuery(q, "ProcId"); !ok {
		b.Fatal("declined")
	}
	b.Run("columnar_group_count", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			c.GroupCountQuery(q, "ProcId")
		}
	})
	b.Run("ungrouped_count_for_reference", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			c.CountQuery(q)
		}
	})
	b.Run("row_scan_grouping", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			m := map[int64]int{}
			for ad := range c.Query(q) {
				if n, err := ad.EvaluateAttr("ProcId").IntValue(); err == nil {
					m[n]++
				}
			}
		}
	})
	_ = fmt.Sprint
}

// heteroGroupFixture is a collection where the group attribute is NOT uniformly a number: some records omit
// it, some hold a string, some hold an expression. The row path makes a group out of each of those
// renderings, and the columnar path has to produce the same partition or decline.
func heteroGroupFixture(tb testing.TB, n int, shape func(i int) string) *Collection {
	tb.Helper()
	cd, err := NewZSTDCodec(nil)
	if err != nil {
		tb.Fatal(err)
	}
	c := New(Options{Shards: 1, SegmentSize: 1 << 16, Codec: cd})
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("ClusterId = %d\nRequestMemory = %d\n%s", i, 1024+(i%32)*512, shape(i))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(tb, src)); err != nil {
			tb.Fatal(err)
		}
	}
	q, err := vm.Parse("RequestMemory >= 0 && Tally >= 0")
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		for range c.Query(q) {
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		tb.Skip("no sealed segments")
	}
	return c
}

// TestGroupCountHeterogeneousGroupValues covers a group column whose values are not uniformly numbers.
//
// Every SCALAR shape is served: the column read now renders a string as its text and an absent attribute
// as `undefined`, which is what the row path's projection yields for them, so the partition matches
// instead of the query being handed back. An EXPRESSION still declines -- only evaluating it produces a
// value -- and that decline is what keeps the result from silently missing a group.
func TestGroupCountHeterogeneousGroupValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		shape  func(i int) string
		served bool
	}{
		// A record that simply lacks the attribute: both paths group it under "undefined".
		{"attribute_absent", func(i int) string {
			if i%50 == 7 {
				return "Other = 1"
			}
			return fmt.Sprintf("Tally = %d", i%10)
		}, true},
		// Present but a string: both paths group it under that text, beside the numeric groups.
		{"attribute_is_string", func(i int) string {
			if i%50 == 7 {
				return `Tally = "many"`
			}
			return fmt.Sprintf("Tally = %d", i%10)
		}, true},
		// Present but a boolean: a numeric column's aggregates coerce it to 1/0, but as a GROUP value
		// it has to render "true", which is why the key keeps its kind.
		{"attribute_is_bool", func(i int) string {
			if i%50 == 7 {
				return "Tally = true"
			}
			return fmt.Sprintf("Tally = %d", i%10)
		}, true},
		// An expression: its value exists only once something evaluates it, so this must decline --
		// even though the row path renders it perfectly well and makes a group out of it.
		{"attribute_is_expression", func(i int) string {
			if i%50 == 7 {
				return `Tally = strcat("g", ClusterId % 3)`
			}
			return fmt.Sprintf("Tally = %d", i%10)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := heteroGroupFixture(t, groupFixtureRecords, tc.shape)
			defer c.Close()
			q, err := vm.Parse("RequestMemory > 4096")
			if err != nil {
				t.Fatal(err)
			}
			// The point of the fixture: the row path really does see records whose group value is not
			// an integer, so the case is about those values and not an artifact of a uniform fixture.
			if _, nonInt := rowGroupTruth(t, c, "RequestMemory > 4096", "Tally"); nonInt == 0 {
				t.Fatal("fixture is uniform after all: no matching record has a non-integer Tally")
			}
			got, ok := c.GroupCountQuery(q, "Tally")
			if ok != tc.served {
				t.Fatalf("served = %v, want %v", ok, tc.served)
			}
			if !ok {
				// Declining is only acceptable because the shape is otherwise servable -- so the
				// decline is about the values, not about the query being out of scope to begin with.
				c2 := heteroGroupFixture(t, groupFixtureRecords,
					func(i int) string { return fmt.Sprintf("Tally = %d", i%10) })
				defer c2.Close()
				if _, ok := c2.GroupCountQuery(q, "Tally"); !ok {
					t.Error("declined the same query on a uniform fixture, so the decline above was " +
						"not about the unrepresentable values")
				}
				return
			}
			checkGroupTexts(t, c, got, "RequestMemory > 4096", "Tally")
		})
	}
}

// TestGroupCountAllMatchesRowPath covers the no-constraint form, which GroupCountQuery cannot serve
// (there is no predicate to analyze) and so is a separate entry point.
func TestGroupCountAllMatchesRowPath(t *testing.T) {
	c := groupReadFixture(t)
	for _, attr := range []string{"ProcId", "JobStatus", "RequestCpus"} {
		got, ok := c.GroupCountAll(attr)
		if !ok {
			t.Fatalf("GROUP BY %s over every record: declined", attr)
		}
		want, nonInt := rowGroupTruth(t, c, "true", attr)
		if nonInt != 0 {
			t.Fatalf("fixture has %d records whose %s is not an integer", nonInt, attr)
		}
		if len(got) != len(want) {
			t.Errorf("GROUP BY %s: %d groups, row path found %d", attr, len(got), len(want))
		}
		total := 0
		for _, g := range got {
			n, err := g.Value.IntValue()
			if err != nil {
				t.Errorf("group value %v is not an integer", g.Value)
				continue
			}
			if want[n] != g.Count {
				t.Errorf("GROUP BY %s: group %d counted %d, row path %d", attr, n, g.Count, want[n])
			}
			total += g.Count
		}
		// Unconstrained, the groups must account for every record.
		if total != c.Len() {
			t.Errorf("GROUP BY %s: groups sum to %d but the collection holds %d records", attr, total, c.Len())
		}
	}
	// A string column is served too, through the block dictionary; TestGroupCountStringColumn checks
	// its partition against the row path.
	if _, ok := c.GroupCountAll("Owner"); !ok {
		t.Error("GROUP BY Owner (a string) declined over every record")
	}
}

// TestGroupCountAdjacentKindCollision covers group values that share raw BITS and differ only in kind,
// placed next to each other.
//
// An integer 1 and a boolean true are stored as the same int64 and are two groups, rendering "1" and
// "true"; so are an integer 0 and false. The group key carries the kind for exactly that reason, and
// anything that carries a group forward from one record to the next -- a cached accumulator, a memo,
// a per-block key vector -- has to carry the kind with it. Dropping it reads as a count moved from one
// real group to another and as a group that is simply not in the result.
//
// The fixture places each boolean DIRECTLY AFTER the integer it collides with, and nowhere else does
// that integer appear, so only a carry-forward can merge them. The surrounding values are 2..9 for the
// same reason: if 0 and 1 occurred freely both groups would exist anyway and the mistake would hide
// inside their counts.
func TestGroupCountAdjacentKindCollision(t *testing.T) {
	c := heteroGroupFixture(t, groupFixtureRecords, func(i int) string {
		switch i % 50 {
		case 6:
			return "Tally = 1"
		case 7:
			return "Tally = true"
		case 16:
			return "Tally = 0"
		case 17:
			return "Tally = false"
		}
		return fmt.Sprintf("Tally = %d", 2+i%8)
	})
	defer c.Close()
	// A constraint every record satisfies: the colliding pair has to stay ADJACENT among the
	// survivors, and a predicate that dropped either one would leave the memo nothing to confuse.
	const constraint = "ClusterId >= 0"
	q, err := vm.Parse(constraint)
	if err != nil {
		t.Fatal(err)
	}
	want := rowGroupTexts(t, c, constraint, "Tally")
	for _, k := range []string{"0", "1", "true", "false"} {
		if want[k] == 0 {
			t.Fatalf("fixture produced no %s group, so the collision this test is about is absent", k)
		}
	}
	got, ok := c.GroupCountQuery(q, "Tally")
	if !ok {
		t.Fatal("declined: this test needs the columnar path, which is the one that caches groups")
	}
	checkGroupTexts(t, c, got, constraint, "Tally")
}
