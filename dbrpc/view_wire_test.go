package dbrpc

import (
	"context"
	"strconv"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// TestViewWire exercises the view opcodes end to end: create a view over a seeded base
// table, list it, query it like a table (reads resolve to the backing), confirm writes to a
// view are refused, and drop it.
func TestViewWire(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range []struct {
		key, owner string
	}{{"1", "alice"}, {"2", "alice"}, {"3", "bob"}} {
		ad, _ := classad.ParseOld("Owner = \"" + j.owner + "\"")
		if err := base.Put(j.key, ad); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); cat.Close() }()
	ctx := context.Background()

	spec := db.ViewSpec{
		BaseTable:   "jobs",
		Groups:      []db.ViewGroupCol{{Attr: "Owner", Alias: "label_owner"}},
		Metrics:     []db.ViewMetric{{Func: db.ViewCount, Arg: "*", Alias: "metric_jobs"}},
		Cardinality: 100,
	}
	if err := c.CreateView(ctx, "usage", spec); err != nil {
		t.Fatalf("CreateView: %v", err)
	}

	// Listed as a view, not a table.
	views, err := c.ListViews(ctx)
	if err != nil || len(views) != 1 || views[0] != "usage" {
		t.Fatalf("ListViews = %v, %v; want [usage]", views, err)
	}
	if tables, _ := c.Tables(ctx); contains(tables, "usage") {
		t.Fatal("a view must not appear in the table listing")
	}

	// Queried like a table: two groups (alice, bob).
	rows, err := c.QueryTable(ctx, "usage", "true", 0)
	if err != nil {
		t.Fatalf("query view: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("view query returned %d rows, want 2", len(rows))
	}

	// Writes to a view are refused (a view is not a writable table).
	if _, err := c.BeginTable(ctx, "usage"); err == nil {
		t.Fatal("beginning a transaction on a view should be refused")
	}

	// Drop it.
	if err := c.DropView(ctx, "usage"); err != nil {
		t.Fatalf("DropView: %v", err)
	}
	if views, _ := c.ListViews(ctx); len(views) != 0 {
		t.Fatalf("views after drop = %v, want none", views)
	}
}

// TestViewSealedUnionWire: a continuous aggregate's read unions sealed history (archive)
// with the live backing, so a query returns the full series even after buckets are evicted.
func TestViewSealedUnionWire(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range []struct {
		key   string
		qdate int
	}{{"1", 3600}, {"2", 3700}, {"3", 7200}} { // buckets 3600 (x2), 7200 (x1)
		ad, _ := classad.ParseOld("QDate = " + strconv.Itoa(j.qdate))
		if err := base.Put(j.key, ad); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); cat.Close() }()
	ctx := context.Background()

	spec := db.ViewSpec{
		BaseTable:   "jobs",
		Groups:      []db.ViewGroupCol{{Attr: "QDate", Alias: "time", BucketWidth: 3600}},
		Metrics:     []db.ViewMetric{{Func: db.ViewCount, Arg: "*", Alias: "metric_jobs"}},
		Cardinality: 100,
	}
	if err := c.CreateView(ctx, "jobs_ts", spec); err != nil {
		t.Fatalf("CreateView: %v", err)
	}

	// Seal bucket 3600 (now=7200 => seal starts <= 3600); bucket 7200 stays live.
	v, _ := cat.View("jobs_ts")
	v.Seal(7200)

	// Query the view: the union returns BOTH the sealed bucket (3600) and the live one (7200).
	rows, err := c.QueryTable(ctx, "jobs_ts", "true", 0)
	if err != nil {
		t.Fatalf("query view: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("union query returned %d rows, want 2 (live + sealed)", len(rows))
	}
	times := map[int64]bool{}
	for _, txt := range rows {
		ad, perr := classad.Parse(txt)
		if perr != nil || ad == nil {
			t.Fatalf("parse row %q: %v", txt, perr)
		}
		ts, _ := ad.EvaluateAttrInt("time")
		times[ts] = true
	}
	if !times[3600] || !times[7200] {
		t.Fatalf("union missing a bucket; got times %v, want {3600, 7200}", times)
	}
}
