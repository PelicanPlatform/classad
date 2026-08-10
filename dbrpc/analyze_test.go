package dbrpc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// TestAdminAnalyzeRefreshesHotSet verifies the on-demand "analyze" admin action runs a
// Maintain pass that refreshes the hot set from accumulated read demand: after many projected
// reads of Memory, analyze must front-load Memory (the demand-driven, non-redundant part of
// ANALYSE that a plain reindex would not do).
func TestAdminAnalyzeRefreshesHotSet(t *testing.T) {
	cat, err := db.OpenCatalog("") // in-memory
	if err != nil {
		t.Fatal(err)
	}
	d, err := cat.CreateTable("Startd")
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	for i := 0; i < 300; i++ {
		ad, perr := classad.ParseOld(fmt.Sprintf("Name = \"k%d\"\nMemory = %d\nArch = \"X86_64\"\nDisk = %d", i, 1024+i, i*10))
		if perr != nil {
			t.Fatal(perr)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Build read demand on Memory: projected reads tally recordReads(projection).
	for i := 0; i < 30; i++ {
		seq, qerr := d.QueryProject("true", []string{"Memory"})
		if qerr != nil {
			t.Fatal(qerr)
		}
		for range seq {
		}
	}

	s := NewServerCatalog(cat)
	defer s.Close()
	msg, err := s.admin(d, "analyze", nil, true)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if !strings.Contains(msg, "analyzed") {
		t.Errorf("unexpected analyze message %q", msg)
	}

	hot := d.HotAttrs()
	found := false
	for _, h := range hot {
		if strings.EqualFold(h, "Memory") {
			found = true
		}
	}
	if !found {
		t.Errorf("hot set %v does not include Memory after analyze with read demand on it", hot)
	}
}

// TestAdminAnalyzeRequiresPrivilege verifies analyze is DAEMON-gated like the other admin
// actions.
func TestAdminAnalyzeRequiresPrivilege(t *testing.T) {
	cat, _ := db.OpenCatalog("")
	d, _ := cat.CreateTable("t")
	s := NewServerCatalog(cat)
	defer s.Close()
	if _, err := s.admin(d, "analyze", nil, false); err == nil {
		t.Error("analyze without privilege should be refused")
	}
}

// TestArchiveAdminAnalyze verifies analyze on an append-only archive reindexes (rebuilds the
// per-value histogram / selectivity stats) without error.
func TestArchiveAdminAnalyze(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", db.ArchiveConfig{ValueAttrs: []string{"Memory"}, ZoneAttrs: []string{"Memory"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := a.AppendOld(fmt.Sprintf("Memory = %d", 1024+i)); err != nil {
			t.Fatal(err)
		}
	}
	msg, err := archiveAdmin(a, "analyze", nil, 0)
	if err != nil {
		t.Fatalf("archive analyze: %v", err)
	}
	if !strings.Contains(msg, "analyzed") {
		t.Errorf("unexpected archive analyze message %q", msg)
	}
}
