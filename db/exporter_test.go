package db

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestExporterCRUDAndState(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}

	def := ExporterDef{
		Name:   "jobs",
		Kind:   "kafka",
		Config: json.RawMessage(`{"brokers":["b:9092"],"topic":"htc.jobs"}`),
	}
	if err := cat.CreateExporter(def); err != nil {
		t.Fatalf("CreateExporter: %v", err)
	}
	// Duplicate is rejected.
	if err := cat.CreateExporter(def); err == nil {
		t.Fatal("creating a duplicate exporter should fail")
	}
	// Invalid name / missing kind rejected.
	if err := cat.CreateExporter(ExporterDef{Name: "bad name", Kind: "kafka"}); err == nil {
		t.Fatal("invalid exporter name should fail")
	}
	if err := cat.CreateExporter(ExporterDef{Name: "nokind"}); err == nil {
		t.Fatal("missing kind should fail")
	}

	// Listing + single lookup return the definition verbatim.
	if got := cat.Exporters(); len(got) != 1 || got[0].Name != "jobs" || got[0].Kind != "kafka" {
		t.Fatalf("Exporters = %+v", got)
	}
	got, ok := cat.Exporter("jobs")
	if !ok || string(got.Config) != string(def.Config) {
		t.Fatalf("Exporter(jobs) = %+v, %v", got, ok)
	}

	// No state until the first checkpoint.
	if _, ok, err := cat.LoadExporterState("jobs"); err != nil || ok {
		t.Fatalf("fresh exporter should have no state: ok=%v err=%v", ok, err)
	}
	// Save then load state round-trips.
	state := []byte("cursor=abc;seq=42")
	if err := cat.SaveExporterState("jobs", state); err != nil {
		t.Fatalf("SaveExporterState: %v", err)
	}
	blob, ok, err := cat.LoadExporterState("jobs")
	if err != nil || !ok || !bytes.Equal(blob, state) {
		t.Fatalf("LoadExporterState = %q, %v, %v", blob, ok, err)
	}
	// Overwrite replaces.
	if err := cat.SaveExporterState("jobs", []byte("cursor=def;seq=99")); err != nil {
		t.Fatal(err)
	}
	if blob, _, _ := cat.LoadExporterState("jobs"); string(blob) != "cursor=def;seq=99" {
		t.Fatalf("state after overwrite = %q", blob)
	}
	// State on a non-existent exporter is an error.
	if _, _, err := cat.LoadExporterState("ghost"); err == nil {
		t.Fatal("state of a non-existent exporter should error")
	}
	if err := cat.SaveExporterState("ghost", state); err == nil {
		t.Fatal("saving state for a non-existent exporter should error")
	}
	cat.Close()

	// Reopen: the definition is recovered from disk; the state persists too.
	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	if got := cat2.Exporters(); len(got) != 1 || got[0].Name != "jobs" {
		t.Fatalf("after reopen, Exporters = %+v", got)
	}
	if blob, ok, _ := cat2.LoadExporterState("jobs"); !ok || string(blob) != "cursor=def;seq=99" {
		t.Fatalf("after reopen, state = %q ok=%v", blob, ok)
	}

	// Drop removes definition and state from disk.
	if err := cat2.DropExporter("jobs"); err != nil {
		t.Fatalf("DropExporter: %v", err)
	}
	if got := cat2.Exporters(); len(got) != 0 {
		t.Fatalf("after drop, Exporters = %+v", got)
	}
	// Dropping again is a no-op.
	if err := cat2.DropExporter("jobs"); err != nil {
		t.Fatalf("second DropExporter should be a no-op, got %v", err)
	}

	cat3, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat3.Close()
	if got := cat3.Exporters(); len(got) != 0 {
		t.Fatalf("dropped exporter reappeared after reopen: %+v", got)
	}
}

// TestExporterInMemoryCatalog: an in-memory catalog keeps definitions and state in memory
// (no disk), so the same API works without a directory.
func TestExporterInMemoryCatalog(t *testing.T) {
	cat, err := OpenCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err := cat.CreateExporter(ExporterDef{Name: "m", Kind: "kafka"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveExporterState("m", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if blob, ok, _ := cat.LoadExporterState("m"); !ok || string(blob) != "x" {
		t.Fatalf("in-memory state = %q ok=%v", blob, ok)
	}
	if err := cat.DropExporter("m"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cat.Exporter("m"); ok {
		t.Fatal("exporter should be gone after drop")
	}
}
