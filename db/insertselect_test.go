package db

import (
	"fmt"
	"strings"
	"testing"
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
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
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
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
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
