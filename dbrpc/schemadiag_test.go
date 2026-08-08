package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// TestDiagnosticsCarriesDerivedSchema pins the wire contract the `.schema` command depends on:
// the derived per-segment schema has to reach the client, not just the summary counts. It is
// carried by the existing diagnostics op (SchemaScanInfo is a type alias all the way down), so
// this is the test that would fail if that ever stopped being true.
func TestDiagnosticsCarriesDerivedSchema(t *testing.T) {
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()

	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", i), fmt.Sprintf(
			"Cpus = %d\nMemory = %d\nOwner = \"u%d\"", 1+i%8, 1024+(i%16)*256, i%5)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Build the accelerator; without it there is no schema to report and the assertion below
	// would pass vacuously.
	if !d.EnableSchemaScan(2000, 4) {
		t.Skip("no sealed segments to sample in this fixture")
	}

	diag, err := c.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ss := diag.SchemaScan
	if !ss.Enabled {
		t.Fatal("schema scan reported disabled after EnableSchemaScan")
	}
	if len(ss.Schema) == 0 {
		t.Fatal("diagnostics carried no derived schema; `.schema` has nothing to print")
	}
	if len(ss.Schema) != ss.SchemaFields {
		t.Errorf("Schema has %d fields, SchemaFields says %d", len(ss.Schema), ss.SchemaFields)
	}
	for _, f := range ss.Schema {
		if f.Name == "" {
			t.Errorf("field with no name: %+v", f)
		}
		switch f.Kind {
		case "bool", "int", "real", "string":
		default:
			t.Errorf("%s: unexpected kind %q", f.Name, f.Kind)
		}
	}
}
