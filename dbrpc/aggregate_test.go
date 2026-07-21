package dbrpc

import (
	"context"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

func TestAggregateGroupBy(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()

	tx, _ := c.Begin(context.Background())
	_ = tx.NewClassAd(context.Background(), "1", "Owner = \"alice\"\nCpus = 4")
	_ = tx.NewClassAd(context.Background(), "2", "Owner = \"alice\"\nCpus = 8")
	_ = tx.NewClassAd(context.Background(), "3", "Owner = \"bob\"\nCpus = 16")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	// GROUP BY Owner: COUNT(*), SUM(Cpus), MAX(Cpus).
	rows, err := c.Aggregate(context.Background(), "true", []string{"Owner"}, []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggSum, Arg: "Cpus"},
		{Func: AggMax, Arg: "Cpus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d groups, want 2", len(rows))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Group[0] < rows[j].Group[0] })

	// alice: count 2, sum 12, max 8
	if rows[0].Group[0] != "alice" || rows[0].Values[0] != "2" || rows[0].Values[1] != "12" || rows[0].Values[2] != "8" {
		t.Fatalf("alice group = %+v", rows[0])
	}
	// bob: count 1, sum 16, max 16
	if rows[1].Group[0] != "bob" || rows[1].Values[0] != "1" || rows[1].Values[1] != "16" || rows[1].Values[2] != "16" {
		t.Fatalf("bob group = %+v", rows[1])
	}
}

func TestAggregateMultiColumnGroup(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	tx, _ := c.Begin(context.Background())
	_ = tx.NewClassAd(context.Background(), "1", "Owner = \"alice\"\nState = \"Run\"\nCpus = 4")
	_ = tx.NewClassAd(context.Background(), "2", "Owner = \"alice\"\nState = \"Idle\"\nCpus = 8")
	_ = tx.NewClassAd(context.Background(), "3", "Owner = \"alice\"\nState = \"Run\"\nCpus = 2")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Aggregate(context.Background(), "true", []string{"Owner", "State"}, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	// (alice,Run)->2, (alice,Idle)->1
	counts := map[string]string{}
	for _, r := range rows {
		counts[r.Group[0]+"/"+r.Group[1]] = r.Values[0]
	}
	if counts["alice/Run"] != "2" || counts["alice/Idle"] != "1" {
		t.Fatalf("multi-column group counts = %v", counts)
	}
}

func TestAggregateNoGroup(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	tx, _ := c.Begin(context.Background())
	_ = tx.NewClassAd(context.Background(), "1", "Cpus = 4")
	_ = tx.NewClassAd(context.Background(), "2", "Cpus = 8")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Aggregate(context.Background(), "true", nil, []AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggAvg, Arg: "Cpus"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Values[0] != "2" || rows[0].Values[1] != "6" {
		t.Fatalf("no-group aggregate = %+v", rows)
	}
}

// TestAggregateCoercion checks the aggregates follow ClassAd type-coercion rules
// (via the shared classad.Sum/Avg/Min/Max): int sums stay integer, an int+real
// mix promotes to real, and min/max are numeric (a string argument is an error).
func TestAggregateCoercion(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	tx, _ := c.Begin(context.Background())
	_ = tx.NewClassAd(context.Background(), "1", "N = 4\nX = 4\nName = \"alice\"")
	_ = tx.NewClassAd(context.Background(), "2", "N = 8\nX = 1.5\nName = \"bob\"")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Aggregate(context.Background(), "true", nil, []AggSpec{
		{Func: AggSum, Arg: "N"},    // 4 + 8 -> integer 12
		{Func: AggSum, Arg: "X"},    // 4 + 1.5 -> real 5.5
		{Func: AggMin, Arg: "Name"}, // strings are not numeric -> error
	})
	if err != nil {
		t.Fatal(err)
	}
	v := rows[0].Values
	if v[0] != "12" {
		t.Errorf("SUM(N) = %q, want 12 (integer)", v[0])
	}
	if v[1] != "5.5" {
		t.Errorf("SUM(X) = %q, want 5.5 (real)", v[1])
	}
	if v[2] != "error" {
		t.Errorf("MIN(Name) = %q, want error (min is numeric)", v[2])
	}
}

// TestAggregatePrivateRefused ensures a stripped connection cannot aggregate on a
// private attribute.
func TestAggregatePrivateRefused(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{ReadOnly: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()

	if _, err := c.Aggregate(context.Background(), "true", []string{"Capability"}, []AggSpec{{Func: AggCount, Arg: "*"}}); err == nil {
		t.Fatal("grouping by a private attribute on a stripped connection should be refused")
	}
}

func TestAggregateBucketed(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()

	tx, _ := c.Begin(context.Background())
	// QDate (unix seconds) + Cpus; 1h buckets align at 3600 boundaries.
	_ = tx.NewClassAd(context.Background(), "1", "QDate = 3600\nCpus = 1")
	_ = tx.NewClassAd(context.Background(), "2", "QDate = 3700\nCpus = 2") // same bucket as 1
	_ = tx.NewClassAd(context.Background(), "3", "QDate = 7200\nCpus = 4")
	_ = tx.NewClassAd(context.Background(), "4", "QDate = 7300\nCpus = 8") // same bucket as 3
	_ = tx.NewClassAd(context.Background(), "5", "Cpus = 5")               // no QDate: drops out
	_ = tx.NewClassAd(context.Background(), "6", "QDate = \"soon\"\nCpus = 9")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := c.AggregateBucketed(context.Background(), "true",
		[]GroupCol{{Attr: "QDate", BucketWidth: 3600}},
		[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggSum, Arg: "Cpus"}})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Group[0] < rows[j].Group[0] })
	if len(rows) != 2 {
		t.Fatalf("got %d buckets, want 2: %+v", len(rows), rows)
	}
	// bucket 3600: count 2, sum 3 ; bucket 7200: count 2, sum 12
	if rows[0].Group[0] != "3600" || rows[0].Values[0] != "2" || rows[0].Values[1] != "3" {
		t.Errorf("bucket 3600 = %+v", rows[0])
	}
	if rows[1].Group[0] != "7200" || rows[1].Values[0] != "2" || rows[1].Values[1] != "12" {
		t.Errorf("bucket 7200 = %+v", rows[1])
	}
}

func TestAggregateBucketedWithLabel(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()

	tx, _ := c.Begin(context.Background())
	_ = tx.NewClassAd(context.Background(), "1", "QDate = 3600\nOwner = \"alice\"")
	_ = tx.NewClassAd(context.Background(), "2", "QDate = 3700\nOwner = \"alice\"")
	_ = tx.NewClassAd(context.Background(), "3", "QDate = 3800\nOwner = \"bob\"")
	_ = tx.NewClassAd(context.Background(), "4", "QDate = 7200\nOwner = \"alice\"")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Bucket the time AND group by a raw label column.
	rows, err := c.AggregateBucketed(context.Background(), "true",
		[]GroupCol{{Attr: "QDate", BucketWidth: 3600}, {Attr: "Owner"}},
		[]AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Group[0]+"/"+r.Group[1]] = r.Values[0]
	}
	// bucket 3600: alice=2, bob=1 ; bucket 7200: alice=1
	if got["3600/alice"] != "2" || got["3600/bob"] != "1" || got["7200/alice"] != "1" {
		t.Fatalf("bucketed+label counts = %v", got)
	}
}
