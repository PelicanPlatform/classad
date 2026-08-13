package dbrpc

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// The mutable-table aggregate's fast paths are all gated on there being NO grouping, so a grouped
// COUNT(*) decoded every matching record even on a table carrying the columnar accelerator. These tests
// cover the RPC path, which is the one production uses -- the db-level tests cover the routing itself.

// acceleratedPair is testPair with a table whose segments seal small enough to be accelerated, and it
// hands back the server's DB so a test can enable the accelerator and ask which tier answered.
func acceleratedPair(t *testing.T) (*Client, *db.DB, func()) {
	t.Helper()
	d, err := db.OpenConfig(db.Config{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	return c, d, func() { c.Close(); s.Close(); d.Close() }
}

func seedGroupable(t *testing.T, c *Client, d *db.DB, n int) {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", i), fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\nRequestCpus = %d\n"+
				"Owner = \"user%d\"", i, i%10, 1+i%5, 1024+(i%32)*512, 1+i%8, i%64)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
}

// attrInt / attrStr read one attribute out of an ad as the server returned it. Client.Query yields ad
// TEXT, so the reference has to parse it -- and parsing with the ordinary parser is the point: it is the
// full-decode path the columnar answer must agree with.
func attrInt(t *testing.T, text, attr string) int64 {
	t.Helper()
	ad, err := classad.ParseOld(text)
	if err != nil {
		if ad, err = classad.Parse(text); err != nil {
			t.Fatalf("parsing %q: %v", text, err)
		}
	}
	v, err := ad.EvaluateAttr(attr).IntValue()
	if err != nil {
		t.Fatalf("%s is not an integer in %q: %v", attr, text, err)
	}
	return v
}

func attrStr(t *testing.T, text, attr string) string {
	t.Helper()
	ad, err := classad.ParseOld(text)
	if err != nil {
		if ad, err = classad.Parse(text); err != nil {
			t.Fatalf("parsing %q: %v", text, err)
		}
	}
	s, err := ad.EvaluateAttr(attr).StringValue()
	if err != nil {
		t.Fatalf("%s is not a string in %q: %v", attr, text, err)
	}
	return s
}

func TestRPCGroupedAggregateServedFromColumns(t *testing.T) {
	c, d, cleanup := acceleratedPair(t)
	defer cleanup()
	const n = 5000
	seedGroupable(t, c, d, n)
	ctx := context.Background()

	countStar := []AggSpec{{Func: AggCount, Arg: "*"}}
	// The shape under test must actually take the columnar path, or this test passes by measuring the
	// scan it was written to replace.
	if _, ok := db.GroupedFromColumns(d, "RequestMemory > 4096", []db.GroupCol{{Attr: "JobStatus"}},
		[]db.AggSpec{{Func: db.AggCount, Arg: "*"}}); !ok {
		t.Fatal("the grouped shape is not served from the columns; nothing here is exercised")
	}

	rows, err := c.Aggregate(ctx, "RequestMemory > 4096", []string{"JobStatus"}, countStar)
	if err != nil {
		t.Fatal(err)
	}
	// Reference: count per JobStatus by reading the matching ads back over the same RPC.
	want := map[string]int{}
	texts, err := c.Query(ctx, "RequestMemory > 4096")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, text := range texts {
		want[fmt.Sprint(attrInt(t, text, "JobStatus"))]++
		total++
	}
	if total == 0 {
		t.Fatal("no ads matched: the comparison is empty")
	}
	if len(rows) != len(want) {
		t.Errorf("got %d groups, want %d", len(rows), len(want))
	}
	sum := 0
	for _, r := range rows {
		if len(r.Group) != 1 || len(r.Values) != 1 {
			t.Fatalf("unexpected row shape %+v", r)
		}
		var got int
		if _, err := fmt.Sscanf(r.Values[0], "%d", &got); err != nil {
			t.Fatalf("unparsable count %q", r.Values[0])
		}
		if got != want[r.Group[0]] {
			t.Errorf("group %q: count %d, reference %d", r.Group[0], got, want[r.Group[0]])
		}
		sum += got
	}
	if sum != total {
		t.Errorf("groups sum to %d but %d ads matched", sum, total)
	}
	// Sorted ascending by group value, so an ORDER BY on the group column needs no re-derivation.
	labels := make([]string, len(rows))
	for i, r := range rows {
		labels[i] = r.Group[0]
	}
	if !sort.SliceIsSorted(labels, func(i, j int) bool {
		var a, b int
		fmt.Sscanf(labels[i], "%d", &a)
		fmt.Sscanf(labels[j], "%d", &b)
		return a < b
	}) {
		t.Errorf("groups not ascending: %v", labels)
	}
}

// TestRPCGroupedAggregateValueAggregates covers the value aggregates over the RPC, where a formatting
// difference (int vs real, an empty MIN) would be as wrong as a numeric one.
func TestRPCGroupedAggregateValueAggregates(t *testing.T) {
	c, d, cleanup := acceleratedPair(t)
	defer cleanup()
	seedGroupable(t, c, d, 5000)
	ctx := context.Background()

	aggs := []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggMin, Arg: "RequestCpus"},
		{Func: AggMax, Arg: "RequestCpus"},
		{Func: AggSum, Arg: "RequestCpus"},
	}
	rows, err := c.Aggregate(ctx, "RequestMemory > 4096", []string{"JobStatus"}, aggs)
	if err != nil {
		t.Fatal(err)
	}
	// Reference over the same RPC.
	type acc struct{ n, min, max, sum int }
	want := map[string]*acc{}
	texts, err := c.Query(ctx, "RequestMemory > 4096")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range texts {
		st := attrInt(t, text, "JobStatus")
		cpus := attrInt(t, text, "RequestCpus")
		k := fmt.Sprint(st)
		a := want[k]
		if a == nil {
			a = &acc{min: int(cpus), max: int(cpus)}
			want[k] = a
		}
		a.n++
		a.sum += int(cpus)
		if int(cpus) < a.min {
			a.min = int(cpus)
		}
		if int(cpus) > a.max {
			a.max = int(cpus)
		}
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d groups, want %d", len(rows), len(want))
	}
	for _, r := range rows {
		a := want[r.Group[0]]
		if a == nil {
			t.Errorf("group %q is not in the reference", r.Group[0])
			continue
		}
		got := fmt.Sprintf("%s %s %s %s", r.Values[0], r.Values[1], r.Values[2], r.Values[3])
		exp := fmt.Sprintf("%d %d %d %d", a.n, a.min, a.max, a.sum)
		if got != exp {
			t.Errorf("group %q: got [%s], want [%s]", r.Group[0], got, exp)
		}
	}
}

// TestRPCGroupedAggregateFallbackStillCorrect pins that the shapes the columnar path declines are still
// answered, and answered right, by the scan behind it -- the decline is a routing decision, not a refusal.
func TestRPCGroupedAggregateFallbackStillCorrect(t *testing.T) {
	c, d, cleanup := acceleratedPair(t)
	defer cleanup()
	seedGroupable(t, c, d, 2000)
	ctx := context.Background()

	// A string group column: out of scope for a numeric histogram, so this must come from the scan.
	if _, ok := db.GroupedFromColumns(d, "RequestMemory > 4096", []db.GroupCol{{Attr: "Owner"}},
		[]db.AggSpec{{Func: db.AggCount, Arg: "*"}}); ok {
		t.Fatal("a string group column was served columnar")
	}
	rows, err := c.Aggregate(ctx, "RequestMemory > 4096", []string{"Owner"},
		[]AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{}
	texts, err := c.Query(ctx, "RequestMemory > 4096")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range texts {
		want[attrStr(t, text, "Owner")]++
	}
	if len(rows) != len(want) {
		t.Errorf("got %d groups, want %d", len(rows), len(want))
	}
	for _, r := range rows {
		var got int
		fmt.Sscanf(r.Values[0], "%d", &got)
		if got != want[r.Group[0]] {
			t.Errorf("group %q: count %d, reference %d", r.Group[0], got, want[r.Group[0]])
		}
	}
}
