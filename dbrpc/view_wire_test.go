package dbrpc

import (
	"context"
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
