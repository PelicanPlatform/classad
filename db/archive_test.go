package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
)

// TestArchiveTableBasics covers create, append-only ingest, newest-first + LIMIT query,
// count, and reopen recovery of an archive (history) table.
func TestArchiveTableBasics(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{
		ValueAttrs: []string{"ClusterId"},
		ZoneAttrs:  []string{"CompletionDate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	const n = 200
	for i := 0; i < n; i++ {
		if err := hist.AppendOld(fmt.Sprintf("ClusterId = %d\nCompletionDate = %d\nJobStatus = 4", i, 1000+i)); err != nil {
			t.Fatal(err)
		}
	}
	if hist.Count() != n {
		t.Fatalf("Count = %d, want %d", hist.Count(), n)
	}

	// Newest-first + LIMIT: the 5 most recent (highest ClusterId, appended last).
	seq, err := hist.QueryLimit("true", 5)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for ad := range seq {
		v, _ := ad.EvaluateAttrInt("ClusterId")
		ids = append(ids, v)
	}
	if len(ids) != 5 {
		t.Fatalf("QueryLimit(5) returned %d ads, want 5", len(ids))
	}
	if ids[0] != n-1 {
		t.Fatalf("newest id = %d, want %d (newest-first)", ids[0], n-1)
	}

	// Constrained query (indexed value attr).
	seq2, err := hist.Query("ClusterId == 42")
	if err != nil {
		t.Fatal(err)
	}
	matches := 0
	for ad := range seq2 {
		if v, _ := ad.EvaluateAttrInt("ClusterId"); v != 42 {
			t.Fatalf("constraint query returned ClusterId=%d", v)
		}
		matches++
	}
	if matches != 1 {
		t.Fatalf("ClusterId==42 matched %d, want 1", matches)
	}

	// Not a mutable table.
	if _, ok := cat.Table("history"); ok {
		t.Error("archive should not appear as a mutable table")
	}
	if got := cat.ArchiveTables(); len(got) != 1 || got[0] != "history" {
		t.Errorf("ArchiveTables = %v, want [history]", got)
	}
	cat.Close()

	// Reopen: the archive recovers with its data and config (indexes rebuilt).
	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	h2, ok := cat2.ArchiveTable("history")
	if !ok {
		t.Fatal("archive not recovered on reopen")
	}
	if h2.Count() != n {
		t.Fatalf("recovered Count = %d, want %d", h2.Count(), n)
	}
	seq3, _ := h2.QueryLimit("true", 1)
	for ad := range seq3 {
		if v, _ := ad.EvaluateAttrInt("ClusterId"); v != n-1 {
			t.Errorf("recovered newest id = %d, want %d", v, n-1)
		}
	}
}

// TestArchiveTableNameCollision verifies a name cannot be both a mutable and an archive
// table.
func TestArchiveTableNameCollision(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if _, err := cat.CreateTable("dual"); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreateArchiveTable("dual", ArchiveConfig{}); err == nil {
		t.Error("creating an archive with a mutable table's name should fail")
	}
	if _, err := cat.CreateArchiveTable("hist", ArchiveConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreateTable("hist"); err == nil {
		t.Error("creating a mutable table with an archive's name should fail")
	}
}

// TestArchiveRotation verifies retention-based rotation drops old segments.
func TestArchiveRotation(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	// Tiny segments + keep-at-most-2, so appending many ads then rotating drops the rest.
	hist, err := cat.CreateArchiveTable("h", ArchiveConfig{
		SegmentSize: 4096,
		Retention:   collections.Retention{MaxSegments: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if err := hist.AppendOld(fmt.Sprintf("Id = %d\nPad = \"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\"", i)); err != nil {
			t.Fatal(err)
		}
	}
	before := hist.Count()
	dropped, err := hist.Rotate(0)
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 {
		t.Fatal("rotation dropped no segments despite MaxSegments=2")
	}
	if hist.Count() >= before {
		t.Fatalf("Count did not shrink after rotation: before=%d after=%d", before, hist.Count())
	}
}
