package db

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func TestCatalogCreateDropPersist(t *testing.T) {
	dir := t.TempDir()

	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	machines, err := cat.CreateTable("machines")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreateTable("jobs"); err != nil {
		t.Fatal(err)
	}
	// Write into one table; it must not appear in the other.
	tx := machines.Begin()
	ad, _ := classad.ParseOld("Name = \"slot1\"\nCpus = 8")
	tx.NewClassAd("slot1", ad)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := cat.Tables(); len(got) != 2 || got[0] != "jobs" || got[1] != "machines" {
		t.Fatalf("Tables() = %v, want [jobs machines]", got)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: tables and their data are recovered.
	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	if got := cat2.Tables(); len(got) != 2 {
		t.Fatalf("after reopen Tables() = %v, want 2 tables", got)
	}
	m, ok := cat2.Table("machines")
	if !ok {
		t.Fatal("machines table missing after reopen")
	}
	if _, ok := m.LookupClassAd("slot1"); !ok {
		t.Fatal("machines data lost after reopen")
	}
	if j, ok := cat2.Table("jobs"); !ok || j.Len() != 0 {
		t.Fatalf("jobs table should exist and be empty after reopen")
	}

	// Drop removes the table and its data.
	if err := cat2.DropTable("machines"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cat2.Table("machines"); ok {
		t.Fatal("machines still present after drop")
	}
	cat3, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat3.Close()
	if _, ok := cat3.Table("machines"); ok {
		t.Fatal("dropped table machines reappeared after reopen")
	}
}

func TestValidTableName(t *testing.T) {
	ok := []string{"ads", "machines", "job_ads", "t1", "_x", "A-B"}
	bad := []string{"", "1t", "-x", "a/b", "a.b", "a b", "../etc"}
	for _, n := range ok {
		if !ValidTableName(n) {
			t.Errorf("ValidTableName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if ValidTableName(n) {
			t.Errorf("ValidTableName(%q) = true, want false", n)
		}
	}
}
