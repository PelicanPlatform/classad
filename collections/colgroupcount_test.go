package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

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

func TestGroupCountQueryMatchesRowPath(t *testing.T) {
	c := scopeFixtureCodec(t, 20000)
	defer c.Close()

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

// TestGroupCountQueryDeclinesUnservable pins what falls back, so a later change that starts serving one of
// these has to say so rather than answering it wrongly.
func TestGroupCountQueryDeclinesUnservable(t *testing.T) {
	c := scopeFixtureCodec(t, 20000)
	defer c.Close()
	for _, tc := range []struct{ constraint, attr, why string }{
		{"RequestMemory > 4096", "Owner", "string column"},
		{"RequestMemory > 4096", "NoSuchAttr", "attribute absent from the schema"},
		{`Owner == "user3"`, "ProcId", "predicate is not a numeric comparison"},
		{"true", "ProcId", "no predicate to analyze: the unconstrained case is served elsewhere"},
		{"RequestMemory > RequestCpus", "ProcId", "predicate has no literal"},
	} {
		q, err := vm.Parse(tc.constraint)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := c.GroupCountQuery(q, tc.attr); ok {
			t.Errorf("%q GROUP BY %s: expected a decline (%s)", tc.constraint, tc.attr, tc.why)
		}
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
// it and some hold a string. The row path makes a group out of each of those renderings ("undefined", the
// string), which a column read cannot reproduce -- it cannot even tell the two apart, since both surface as
// "no numeric value here".
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

func TestGroupCountQueryDeclinesUnrepresentableGroups(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(i int) string
	}{
		// A record that simply lacks the attribute: the row path groups it under "undefined".
		{"attribute_absent", func(i int) string {
			if i%50 == 7 {
				return "Other = 1"
			}
			return fmt.Sprintf("Tally = %d", i%10)
		}},
		// A record where it is present but a string: the row path groups it under that text.
		{"attribute_is_string", func(i int) string {
			if i%50 == 7 {
				return `Tally = "many"`
			}
			return fmt.Sprintf("Tally = %d", i%10)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := heteroGroupFixture(t, 20000, tc.shape)
			defer c.Close()
			q, err := vm.Parse("RequestMemory > 4096")
			if err != nil {
				t.Fatal(err)
			}
			// The point of the fixture: the row path really does see records this cannot render, so a
			// decline is the only correct answer and not an artifact of a uniform fixture.
			_, nonInt := rowGroupTruth(t, c, "RequestMemory > 4096", "Tally")
			if nonInt == 0 {
				t.Fatal("fixture is uniform after all: no matching record has a non-integer Tally")
			}
			if got, ok := c.GroupCountQuery(q, "Tally"); ok {
				total := 0
				for _, g := range got {
					total += g.Count
				}
				t.Errorf("served the query with %d groups totalling %d records, but %d matching records "+
					"have a group value it cannot render; those groups would be missing from the result",
					len(got), total, nonInt)
			}
			// And the shape is otherwise servable, so the decline is about the values -- not about the
			// query being outside the fast path to begin with.
			c2 := heteroGroupFixture(t, 20000, func(i int) string { return fmt.Sprintf("Tally = %d", i%10) })
			defer c2.Close()
			if _, ok := c2.GroupCountQuery(q, "Tally"); !ok {
				t.Error("declined the same query on a uniform fixture, so the decline above was not about " +
					"the unrepresentable values")
			}
		})
	}
}

// TestGroupCountAllMatchesRowPath covers the no-constraint form, which GroupCountQuery cannot serve
// (there is no predicate to analyze) and so is a separate entry point.
func TestGroupCountAllMatchesRowPath(t *testing.T) {
	c := scopeFixtureCodec(t, 20000)
	defer c.Close()
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
	// A string column has no numeric column to read, so it declines rather than answering wrongly.
	if _, ok := c.GroupCountAll("Owner"); ok {
		t.Error("GROUP BY Owner (a string) was served as a numeric histogram")
	}
}
