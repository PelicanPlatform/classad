package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// rollupFixture builds a source table of job-shaped ads and a summary table keyed by Owner.
func rollupFixture(t *testing.T, owners []string, perOwner int) (src, summary *DB) {
	t.Helper()
	src, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	tx := src.Begin()
	n := 0
	for _, o := range owners {
		for i := 0; i < perOwner; i++ {
			ad := mustAd(t, fmt.Sprintf("Owner = %q\nRequestCpus = %d\nJobStatus = 4", o, i+1))
			tx.NewClassAd(fmt.Sprintf("job%d", n), ad)
			n++
		}
	}
	if commitErr := tx.Commit(); commitErr != nil {
		t.Fatal(commitErr)
	}

	summary, err = Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = summary.Close() })
	if err := summary.SetPrimaryKey([]string{"Owner"}); err != nil {
		t.Fatal(err)
	}
	return src, summary
}

// rollup runs the aggregate and writes it, the way the server composes the two halves.
func rollup(t *testing.T, src, summary *DB, constraint string) int {
	t.Helper()
	groupCols := []GroupCol{{Attr: "Owner"}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggSum, Arg: "RequestCpus"}}
	rows, err := src.AggregateColsStats(constraint, groupCols, aggs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := summary.InsertSelectInto(rows, InsertSpec{
		GroupCols: groupCols, Aggs: aggs,
		Names: []string{"Owner", "Jobs", "Cpus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInsertSelectWritesTypedSummaryRows(t *testing.T) {
	src, summary := rollupFixture(t, []string{"alice", "bob"}, 3)
	if n := rollup(t, src, summary, "true"); n != 2 {
		t.Fatalf("inserted %d rows, want 2 (one per owner)", n)
	}
	if got := summary.Len(); got != 2 {
		t.Fatalf("summary holds %d rows, want 2", got)
	}

	// alice's row, addressed by the derived key, carries NUMBERS -- not text that
	// happens to look like numbers. 1+2+3 = 6 cpus over 3 jobs.
	ad, ok := summary.LookupClassAd(`"alice"`)
	if !ok {
		t.Fatalf("no row under the derived key for alice; keys are %v", summaryKeys(t, summary))
	}
	jobs := ad.EvaluateAttr("Jobs")
	if !jobs.IsInteger() {
		t.Fatalf("Jobs is a %v, want an integer", jobs)
	}
	if n, _ := jobs.IntValue(); n != 3 {
		t.Fatalf("Jobs = %d, want 3", n)
	}
	cpus := ad.EvaluateAttr("Cpus")
	if !cpus.IsInteger() {
		t.Fatalf("Cpus is a %v, want an integer", cpus)
	}
	if n, _ := cpus.IntValue(); n != 6 {
		t.Fatalf("Cpus = %d, want 6", n)
	}
	owner := ad.EvaluateAttr("Owner")
	if !owner.IsString() {
		t.Fatalf("Owner is a %v, want a string", owner)
	}
}

// Re-running the roll-up must CONVERGE, not accumulate. This is the property the declared
// key buys: the same group derives the same key, so the second run replaces the first run's
// rows instead of adding a second copy of every one of them.
func TestInsertSelectRerunReplacesRatherThanDuplicates(t *testing.T) {
	src, summary := rollupFixture(t, []string{"alice", "bob"}, 3)
	rollup(t, src, summary, "true")
	rollup(t, src, summary, "true")
	rollup(t, src, summary, "true")
	if got := summary.Len(); got != 2 {
		t.Fatalf("after three runs the summary holds %d rows, want 2", got)
	}
}

// And a re-run after the source changed must UPDATE the row, not leave the stale one.
func TestInsertSelectRerunPicksUpSourceChanges(t *testing.T) {
	src, summary := rollupFixture(t, []string{"alice"}, 3)
	rollup(t, src, summary, "true")

	tx := src.Begin()
	tx.NewClassAd("extra", mustAd(t, "Owner = \"alice\"\nRequestCpus = 10\nJobStatus = 4"))
	if commitErr := tx.Commit(); commitErr != nil {
		t.Fatal(commitErr)
	}
	rollup(t, src, summary, "true")

	ad, ok := summary.LookupClassAd(`"alice"`)
	if !ok {
		t.Fatal("alice's summary row vanished")
	}
	if n, _ := ad.EvaluateAttr("Jobs").IntValue(); n != 4 {
		t.Fatalf("Jobs = %d after the source gained a row, want 4", n)
	}
	if n, _ := ad.EvaluateAttr("Cpus").IntValue(); n != 16 {
		t.Fatalf("Cpus = %d, want 16", n)
	}
}

func TestInsertSelectRefusedWithoutPrimaryKey(t *testing.T) {
	src, summary := rollupFixture(t, []string{"alice"}, 2)
	if err := summary.SetPrimaryKey(nil); err != nil {
		t.Fatal(err)
	}
	groupCols := []GroupCol{{Attr: "Owner"}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}
	rows, err := src.AggregateColsStats("true", groupCols, aggs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = summary.InsertSelectInto(rows, InsertSpec{
		GroupCols: groupCols, Aggs: aggs, Names: []string{"Owner", "Jobs"}})
	if err == nil {
		t.Fatal("insert into a table with no declared key succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "primary key") {
		t.Fatalf("error %v should say the table declares no primary key", err)
	}
}

// A statement that does not produce the key columns is refused once, naming the mismatch --
// not once per row.
func TestInsertSelectRefusedWhenKeyColumnsMissing(t *testing.T) {
	src, summary := rollupFixture(t, []string{"alice"}, 2)
	groupCols := []GroupCol{{Attr: "JobStatus"}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}
	rows, err := src.AggregateColsStats("true", groupCols, aggs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = summary.InsertSelectInto(rows, InsertSpec{
		GroupCols: groupCols, Aggs: aggs, Names: []string{"JobStatus", "Jobs"}})
	if err == nil {
		t.Fatal("insert without the key column succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "Owner") {
		t.Fatalf("error %v should name the missing key column Owner", err)
	}
}

// An aggregate's column needs a real attribute name; the SQL header is not one.
func TestInsertSelectRefusesUnusableColumnName(t *testing.T) {
	spec := InsertSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}},
		Aggs:      []AggSpec{{Func: AggCount, Arg: "*"}},
		Names:     []string{"Owner", "COUNT(*)"},
	}
	err := spec.Validate()
	if err == nil {
		t.Fatal("a COUNT(*) column name was accepted, want a refusal")
	}
	if !strings.Contains(err.Error(), "alias") {
		t.Fatalf("error %v should suggest aliasing the column", err)
	}
}

// Nothing is written when the batch is refused: a roll-up that half-applied would leave a
// mixture of two runs with no marker saying where the boundary is.
func TestInsertSelectWritesNothingOnRefusal(t *testing.T) {
	src, summary := rollupFixture(t, []string{"alice", "bob"}, 2)
	rollup(t, src, summary, "true")
	before := summary.Len()

	// Group by a column the key does not cover, so the whole batch is refused.
	groupCols := []GroupCol{{Attr: "JobStatus"}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}
	rows, err := src.AggregateColsStats("true", groupCols, aggs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := summary.InsertSelectInto(rows, InsertSpec{
		GroupCols: groupCols, Aggs: aggs, Names: []string{"JobStatus", "Jobs"}}); err == nil {
		t.Fatal("expected a refusal")
	}
	if after := summary.Len(); after != before {
		t.Fatalf("summary went from %d to %d rows on a refused insert", before, after)
	}
}

func summaryKeys(t *testing.T, d *DB) []string {
	t.Helper()
	return d.Keys()
}

// How each value KIND lands in the written ad.
//
// The two that matter are not the scalars. UNDEFINED must leave the attribute out, because
// in a ClassAd an absent attribute IS undefined and writing `X = undefined` creates one
// where the query found none -- every later `is undefined` test and every projection then
// sees a different shape. ERROR must refuse, because stored it propagates into everything
// that reads the column with nothing in the row to say which input produced it.
func TestBuildInsertAdsValueKinds(t *testing.T) {
	spec := InsertSpec{
		GroupCols: []GroupCol{{Attr: "G"}},
		Aggs:      []AggSpec{{Func: AggMax, Arg: "v"}},
		Names:     []string{"G", "Val"},
	}
	cases := []struct {
		name  string
		v     classad.Value
		check func(t *testing.T, ad *classad.ClassAd)
	}{{
		name: "integer",
		v:    classad.NewIntValue(7),
		check: func(t *testing.T, ad *classad.ClassAd) {
			if got, _ := ad.EvaluateAttr("Val").IntValue(); got != 7 {
				t.Fatalf("Val = %d, want 7", got)
			}
			if !ad.EvaluateAttr("Val").IsInteger() {
				t.Fatal("Val should be an integer")
			}
		},
	}, {
		name: "real",
		v:    classad.NewRealValue(2.5),
		check: func(t *testing.T, ad *classad.ClassAd) {
			if got, _ := ad.EvaluateAttr("Val").RealValue(); got != 2.5 {
				t.Fatalf("Val = %v, want 2.5", got)
			}
		},
	}, {
		name: "string stays a string",
		v:    classad.NewStringValue("4"),
		check: func(t *testing.T, ad *classad.ClassAd) {
			v := ad.EvaluateAttr("Val")
			if !v.IsString() {
				t.Fatalf("Val is a %v, want a string -- the whole point is that \"4\" is not 4", v)
			}
			if s, _ := v.StringValue(); s != "4" {
				t.Fatalf("Val = %q, want \"4\"", s)
			}
		},
	}, {
		name: "bool",
		v:    classad.NewBoolValue(true),
		check: func(t *testing.T, ad *classad.ClassAd) {
			if !ad.EvaluateAttr("Val").IsBool() {
				t.Fatalf("Val is a %v, want a bool", ad.EvaluateAttr("Val"))
			}
		},
	}, {
		name: "list round-trips",
		v:    classad.NewListValue([]classad.Value{classad.NewIntValue(1), classad.NewStringValue("a")}),
		check: func(t *testing.T, ad *classad.ClassAd) {
			v := ad.EvaluateAttr("Val")
			if !v.IsList() {
				t.Fatalf("Val is a %v, want a list", v)
			}
			elems, err := v.ListValue()
			if err != nil || len(elems) != 2 {
				t.Fatalf("elements = %v (err %v), want 2", elems, err)
			}
			if !elems[0].IsInteger() || !elems[1].IsString() {
				t.Fatalf("element kinds not preserved: %v, %v", elems[0], elems[1])
			}
		},
	}, {
		name: "undefined leaves the attribute OUT",
		v:    classad.NewUndefinedValue(),
		check: func(t *testing.T, ad *classad.ClassAd) {
			if _, ok := ad.Lookup("Val"); ok {
				t.Fatal("undefined wrote an attribute; it must be absent, which is how a ClassAd spells undefined")
			}
			if !ad.EvaluateAttr("Val").IsUndefined() {
				t.Fatal("the absent attribute should still evaluate as undefined")
			}
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := []AggRow{{
				Group:       []string{`"g1"`},
				GroupValues: []classad.Value{classad.NewStringValue("g1")},
				Values:      []string{ValueText(tc.v)},
				TypedValues: []classad.Value{tc.v},
			}}
			ads, err := BuildInsertAds(rows, spec)
			if err != nil {
				t.Fatalf("BuildInsertAds: %v", err)
			}
			if len(ads) != 1 {
				t.Fatalf("got %d ads, want 1", len(ads))
			}
			tc.check(t, ads[0])
		})
	}
}

func TestBuildInsertAdsRefusesErrorValue(t *testing.T) {
	rows := []AggRow{{
		Group:       []string{`"g1"`},
		GroupValues: []classad.Value{classad.NewStringValue("g1")},
		Values:      []string{"error"},
		TypedValues: []classad.Value{classad.NewErrorValue()},
	}}
	_, err := BuildInsertAds(rows, InsertSpec{
		GroupCols: []GroupCol{{Attr: "G"}},
		Aggs:      []AggSpec{{Func: AggMax, Arg: "v"}},
		Names:     []string{"G", "Val"},
	})
	if err == nil {
		t.Fatal("an error value was stored, want a refusal")
	}
	if !strings.Contains(err.Error(), "Val") {
		t.Fatalf("error %v should name the column", err)
	}
}

// A row with no typed aggregate values is refused, not guessed at from the text.
func TestBuildInsertAdsRefusesUntypedAggregates(t *testing.T) {
	rows := []AggRow{{
		Group:       []string{`"g1"`},
		GroupValues: []classad.Value{classad.NewStringValue("g1")},
		Values:      []string{"12"},
		// TypedValues deliberately nil: what a row read off the wire looks like.
	}}
	_, err := BuildInsertAds(rows, InsertSpec{
		GroupCols: []GroupCol{{Attr: "G"}},
		Aggs:      []AggSpec{{Func: AggCount, Arg: "*"}},
		Names:     []string{"G", "Jobs"},
	})
	if err == nil {
		t.Fatal("a row with no typed values was accepted, want a refusal")
	}
	if !strings.Contains(err.Error(), "guessing") {
		t.Fatalf("error %v should say it refuses to guess the type", err)
	}
}
