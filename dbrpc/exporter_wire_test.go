package dbrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// exporterPair starts a catalog-backed server with the given options and returns a client.
func exporterPair(t *testing.T, cat *db.Catalog, opts ServeOptions) *Client {
	t.Helper()
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, opts) }()
	c := NewClient(cconn)
	t.Cleanup(func() { c.Close(); s.Close() })
	return c
}

func TestExporterWire(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	ctx := context.Background()

	priv := exporterPair(t, cat, ServeOptions{Privileged: true})

	def := db.ExporterDef{
		Name:   "jobs",
		Kind:   "kafka",
		Config: json.RawMessage(`{"brokers":["b:9092"],"topic":"htc.jobs"}`),
	}
	if err := priv.CreateExporter(ctx, def); err != nil {
		t.Fatalf("CreateExporter: %v", err)
	}

	// List (name+kind, no config).
	infos, err := priv.ListExporters(ctx)
	if err != nil || len(infos) != 1 || infos[0].Name != "jobs" || infos[0].Kind != "kafka" {
		t.Fatalf("ListExporters = %v, %v", infos, err)
	}

	// Get returns the full definition including config.
	got, ok, err := priv.GetExporter(ctx, "jobs")
	if err != nil || !ok || string(got.Config) != string(def.Config) {
		t.Fatalf("GetExporter = %+v, %v, %v", got, ok, err)
	}
	if _, ok, _ := priv.GetExporter(ctx, "ghost"); ok {
		t.Fatal("GetExporter of a missing exporter should report not-found")
	}

	// State: absent, then set, then read back.
	if _, ok, err := priv.GetExporterState(ctx, "jobs"); err != nil || ok {
		t.Fatalf("fresh exporter state: ok=%v err=%v", ok, err)
	}
	state := []byte{0x00, 0x01, 0xff, 0x2a} // opaque binary blob
	if err := priv.PutExporterState(ctx, "jobs", state); err != nil {
		t.Fatalf("PutExporterState: %v", err)
	}
	blob, ok, err := priv.GetExporterState(ctx, "jobs")
	if err != nil || !ok || !bytes.Equal(blob, state) {
		t.Fatalf("GetExporterState = %v, %v, %v", blob, ok, err)
	}

	// Drop.
	if err := priv.DropExporter(ctx, "jobs"); err != nil {
		t.Fatalf("DropExporter: %v", err)
	}
	if infos, _ := priv.ListExporters(ctx); len(infos) != 0 {
		t.Fatalf("after drop, ListExporters = %v", infos)
	}
}

// TestExporterWirePrivilege: an unprivileged (WRITE-level) connection may list exporters
// but is refused every DAEMON-only op (create/drop/get/state).
func TestExporterWirePrivilege(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	ctx := context.Background()

	// Seed one exporter via a privileged connection.
	priv := exporterPair(t, cat, ServeOptions{Privileged: true})
	if err := priv.CreateExporter(ctx, db.ExporterDef{Name: "jobs", Kind: "kafka"}); err != nil {
		t.Fatal(err)
	}

	unpriv := exporterPair(t, cat, ServeOptions{}) // WRITE-level, not Privileged

	// Listing is allowed (name+kind only).
	if infos, err := unpriv.ListExporters(ctx); err != nil || len(infos) != 1 {
		t.Fatalf("unprivileged ListExporters = %v, %v", infos, err)
	}
	// Every DAEMON-only op is refused.
	if err := unpriv.CreateExporter(ctx, db.ExporterDef{Name: "x", Kind: "kafka"}); err == nil {
		t.Fatal("unprivileged CreateExporter should be refused")
	}
	if err := unpriv.DropExporter(ctx, "jobs"); err == nil {
		t.Fatal("unprivileged DropExporter should be refused")
	}
	if _, _, err := unpriv.GetExporter(ctx, "jobs"); err == nil {
		t.Fatal("unprivileged GetExporter should be refused (config may hold credentials)")
	}
	if err := unpriv.PutExporterState(ctx, "jobs", []byte("x")); err == nil {
		t.Fatal("unprivileged PutExporterState should be refused")
	}
	if _, _, err := unpriv.GetExporterState(ctx, "jobs"); err == nil {
		t.Fatal("unprivileged GetExporterState should be refused")
	}

	// The privileged client was not disturbed.
	if _, ok, _ := priv.GetExporter(ctx, "jobs"); !ok {
		t.Fatal("exporter should still exist")
	}
}
