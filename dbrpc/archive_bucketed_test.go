package dbrpc

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// TestArchiveAggregateBucketedOverRPC covers the bucketed archive GROUP BY end to end: the
// "jobs per owner per day" shape crossing the wire. The result is checked against a
// client-side reduction over the same rows fetched with ArchiveQuery, so the server's
// index-resident bucketing must agree with counting locally.
func TestArchiveAggregateBucketedOverRPC(t *testing.T) {
	const day = 86400
	c, cleanup := catServerPair(t, ServeOptions{})
	defer cleanup()

	if err := c.CreateArchiveTable(context.Background(), "history", db.ArchiveConfig{
		SegmentSize:      1 << 16,
		CategoricalAttrs: []string{"Owner"},
		ZoneAttrs:        []string{"CompletionDate"},
	}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}

	owners := []string{"alice", "bob", "carol"}
	base := int64(1700000000) / day * day
	const n = 1200
	want := map[string]int64{} // "bucket|owner" -> count
	for i := 0; i < n; i++ {
		ts := base + int64(i)*(day/300) // spans several days
		o := owners[i%len(owners)]
		if err := c.ArchiveAppend(context.Background(), "history",
			fmt.Sprintf("CompletionDate = %d\nOwner = %q", ts, o)); err != nil {
			t.Fatalf("ArchiveAppend: %v", err)
		}
		want[fmt.Sprintf("%d|%s", ts/day*day, o)]++
	}

	groups := []GroupCol{{Attr: "Owner"}, {Attr: "CompletionDate", BucketWidth: day}}
	rows, err := c.ArchiveAggregateBucketed(context.Background(), "history", "true", groups,
		[]AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatalf("ArchiveAggregateBucketed: %v", err)
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d (owner,day) rows, want %d", len(rows), len(want))
	}
	var total int64
	for _, r := range rows {
		if len(r.Group) != 2 {
			t.Fatalf("row %+v: want 2 group values", r)
		}
		key := r.Group[1] + "|" + r.Group[0] // group order mirrors the request: [Owner, day]
		got, err := strconv.ParseInt(r.Values[0], 10, 64)
		if err != nil {
			t.Fatalf("non-numeric count %q", r.Values[0])
		}
		if want[key] != got {
			t.Errorf("%s = %d, want %d", key, got, want[key])
		}
		total += got
	}
	if total != n {
		t.Errorf("counts sum to %d, want %d", total, n)
	}
}

// TestArchiveAggregateBucketedZeroWidth checks a zero bucket width behaves as an ordinary
// raw group column, so the bucketed opcode is a strict superset of the plain one.
func TestArchiveAggregateBucketedZeroWidth(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{})
	defer cleanup()

	if err := c.CreateArchiveTable(context.Background(), "history", db.ArchiveConfig{
		CategoricalAttrs: []string{"Owner"},
	}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	for _, o := range []string{"alice", "alice", "bob"} {
		if err := c.ArchiveAppend(context.Background(), "history", fmt.Sprintf("Owner = %q", o)); err != nil {
			t.Fatal(err)
		}
	}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}
	bucketed, err := c.ArchiveAggregateBucketed(context.Background(), "history", "true",
		[]GroupCol{{Attr: "Owner"}}, aggs)
	if err != nil {
		t.Fatalf("ArchiveAggregateBucketed: %v", err)
	}
	plain, err := c.ArchiveAggregate(context.Background(), "history", "true", []string{"Owner"}, aggs)
	if err != nil {
		t.Fatalf("ArchiveAggregate: %v", err)
	}
	toMap := func(rows []AggRow) map[string]string {
		m := map[string]string{}
		for _, r := range rows {
			m[r.Group[0]] = r.Values[0]
		}
		return m
	}
	bm, pm := toMap(bucketed), toMap(plain)
	if len(bm) != len(pm) {
		t.Fatalf("bucketed = %v, plain = %v", bm, pm)
	}
	for k, v := range pm {
		if bm[k] != v {
			t.Errorf("owner %q: bucketed %q, plain %q", k, bm[k], v)
		}
	}
}

// TestArchiveAggregateBucketedWithFilters proves the two extensions compose: a bucketed
// GROUP BY whose aggregate also carries a per-aggregate FILTER rides one opcode, rather than
// needing an opcode per combination or silently dropping the filter.
func TestArchiveAggregateBucketedWithFilters(t *testing.T) {
	const day = 86400
	c, cleanup := catServerPair(t, ServeOptions{})
	defer cleanup()
	if err := c.CreateArchiveTable(context.Background(), "history", db.ArchiveConfig{
		CategoricalAttrs: []string{"Owner"},
		ZoneAttrs:        []string{"CompletionDate"},
	}); err != nil {
		t.Fatal(err)
	}
	base := int64(1700000000) / day * day
	owners := []string{"alice", "bob"}
	want := map[string]int64{} // "bucket|owner" -> alice-only count
	for i := 0; i < 400; i++ {
		o := owners[i%2]
		ts := base + int64(i)*(day/100)
		if err := c.ArchiveAppend(context.Background(), "history",
			fmt.Sprintf("CompletionDate = %d\nOwner = %q", ts, o)); err != nil {
			t.Fatal(err)
		}
		if o == "alice" {
			want[fmt.Sprintf("%d|%s", ts/day*day, o)]++
		}
	}

	groups := []GroupCol{{Attr: "Owner"}, {Attr: "CompletionDate", BucketWidth: day}}
	rows, err := c.ArchiveAggregateBucketed(context.Background(), "history", "true", groups,
		[]AggSpec{{Func: AggCount, Arg: "*", Filter: `Owner == "alice"`}})
	if err != nil {
		t.Fatalf("bucketed + filtered aggregate: %v", err)
	}
	var total int64
	for _, r := range rows {
		got, err := strconv.ParseInt(r.Values[0], 10, 64)
		if err != nil {
			t.Fatalf("non-numeric count %q", r.Values[0])
		}
		key := r.Group[1] + "|" + r.Group[0]
		if r.Group[0] == "alice" && want[key] != got {
			t.Errorf("%s = %d, want %d", key, got, want[key])
		}
		if r.Group[0] == "bob" && got != 0 {
			t.Errorf("bob bucket %s = %d, want 0 (filter must exclude it)", r.Group[1], got)
		}
		total += got
	}
	if total != 200 {
		t.Errorf("filtered counts sum to %d, want 200 (half the rows are alice)", total)
	}
}
