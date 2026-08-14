package db

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestArchiveRowGroupBytesConfigured checks that a budget given at create time reaches the archive.
func TestArchiveRowGroupBytesConfigured(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{
		SegmentSize:   4 << 20,
		RowGroupBytes: 32 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := hist.RowGroupBytes(); got != 32<<10 {
		t.Fatalf("configured RowGroupBytes reached the archive as %d, want %d", got, 32<<10)
	}
}

// TestArchiveRowGroupBytesRuntimeAndPersisted covers the reason this is a setter rather than only a
// config field.
//
// A reopened archive uses its PERSISTED config -- CreateArchiveTable returns the already-open table
// and ignores the argument entirely, which is why the index set needs an explicit reconcile pass. The
// row-group budget needs no reconciliation, because each block records its own layout, so it can be
// changed on a live archive; this checks the change takes effect AND survives a restart, since a
// setting that has to be re-applied after every daemon restart is not one you can leave in a config
// file.
func TestArchiveRowGroupBytesRuntimeAndPersisted(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := hist.RowGroupBytes(); got != 0 {
		t.Fatalf("a fresh archive reports RowGroupBytes=%d, want 0 (the default)", got)
	}
	for i := range 200 {
		if err := hist.AppendOld(fmt.Sprintf("ClusterId = %d\nJobStatus = 4", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := hist.SetRowGroupBytes(64 << 10); err != nil {
		t.Fatal(err)
	}
	if got := hist.RowGroupBytes(); got != 64<<10 {
		t.Fatalf("after SetRowGroupBytes the archive reports %d, want %d", got, 64<<10)
	}
	// Records written before the change must still read back.
	if n := hist.Count(); n != 200 {
		t.Errorf("%d records after changing the budget, want 200: segments sealed under the old budget "+
			"must keep reading, since each block records its own layout", n)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	// It has to survive the restart, or it could not live in a config file.
	var saved ArchiveConfig
	data, err := os.ReadFile(filepath.Join(dir, archivesSubdir, "history", archiveConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.RowGroupBytes != 64<<10 {
		t.Errorf("persisted RowGroupBytes=%d, want %d", saved.RowGroupBytes, 64<<10)
	}
	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	h2, err := cat2.CreateArchiveTable("history", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got := h2.RowGroupBytes(); got != 64<<10 {
		t.Errorf("after reopen the archive reports RowGroupBytes=%d, want the persisted %d", got, 64<<10)
	}
}
