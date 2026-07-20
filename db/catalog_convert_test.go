package db

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func mustParseOld(t *testing.T, s string) *classad.ClassAd {
	t.Helper()
	ad, err := classad.ParseOld(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ad
}

// TestConvertTableToMemory: a persistent table's data survives conversion, the on-disk
// backing is removed, the table reports InMemory, and after a catalog reopen the (now
// RAM-only) table is gone -- proving persistence was actually dropped.
func TestConvertTableToMemory(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"1.0", "2.0", "3.0"} {
		if err := d.Put(k, mustParseOld(t, `Owner = "alice"`)); err != nil {
			t.Fatal(err)
		}
	}
	tableDir := filepath.Join(dir, tablesSubdir, "jobs")
	if _, err := os.Stat(tableDir); err != nil {
		t.Fatalf("persistent table dir missing before convert: %v", err)
	}

	if err := cat.ConvertTableToMemory("jobs"); err != nil {
		t.Fatalf("ConvertTableToMemory: %v", err)
	}

	// Data preserved, table now RAM-only, on-disk backing gone.
	got, ok := cat.Table("jobs")
	if !ok {
		t.Fatal("table missing after convert")
	}
	if !got.InMemory() {
		t.Fatal("table should report InMemory after convert")
	}
	if got.Len() != 3 {
		t.Fatalf("Len after convert = %d, want 3", got.Len())
	}
	if _, ok := got.LookupClassAd("2.0"); !ok {
		t.Fatal("key 2.0 missing after convert")
	}
	if _, err := os.Stat(tableDir); !os.IsNotExist(err) {
		t.Fatalf("on-disk table dir still present after convert (err=%v)", err)
	}

	// Idempotent: converting again is a no-op.
	if err := cat.ConvertTableToMemory("jobs"); err != nil {
		t.Fatalf("second ConvertTableToMemory should be a no-op: %v", err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	// After a restart the converted table is not recovered (it was RAM-only).
	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	if _, ok := cat2.Table("jobs"); ok {
		t.Fatal("RAM-only table should not survive a catalog reopen")
	}
}

// TestConvertTableErrors covers the guarded paths.
func TestConvertTableErrors(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	if err := cat.ConvertTableToMemory("nope"); err == nil {
		t.Fatal("converting a nonexistent table should error")
	}
	// A table created in-memory converts to a no-op (already RAM-only).
	if _, err := cat.CreateTableInMemory("mem"); err != nil {
		t.Fatal(err)
	}
	if !mustTable(t, cat, "mem").InMemory() {
		t.Fatal("CreateTableInMemory should yield an in-memory table")
	}
	if _, err := os.Stat(filepath.Join(dir, tablesSubdir, "mem")); !os.IsNotExist(err) {
		t.Fatal("CreateTableInMemory must not create an on-disk dir")
	}
	if err := cat.ConvertTableToMemory("mem"); err != nil {
		t.Fatalf("converting an already-in-memory table should be a no-op: %v", err)
	}
}

func mustTable(t *testing.T, cat *Catalog, name string) *DB {
	t.Helper()
	d, ok := cat.Table(name)
	if !ok {
		t.Fatalf("table %q not found", name)
	}
	return d
}
