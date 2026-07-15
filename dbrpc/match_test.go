package dbrpc

import (
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

func TestMatchTables(t *testing.T) {
	cat, err := db.OpenCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"jobs", "machines"} {
		if _, err := cat.CreateTable(name); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); cat.Close() }()

	// Machines accept any job; the job prefers (ranks by) more Cpus.
	mtx, _ := c.BeginTable("machines")
	_ = mtx.NewClassAd("slot1", "Key = \"slot1\"\nCpus = 8\nRequirements = true")
	_ = mtx.NewClassAd("slot2", "Key = \"slot2\"\nCpus = 4\nRequirements = true")
	_ = mtx.NewClassAd("slot3", "Key = \"slot3\"\nCpus = 16\nRequirements = true")
	if err := mtx.Commit(); err != nil {
		t.Fatal(err)
	}
	jtx, _ := c.BeginTable("jobs")
	_ = jtx.NewClassAd("1.0", "Key = \"1.0\"\nRequestCpus = 4\nRequirements = (TARGET.Cpus >= RequestCpus)\nRank = TARGET.Cpus")
	if err := jtx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Best single match: slot3 (Cpus 16, the highest Rank).
	rows, err := c.MatchTables("jobs", "machines", "Key", "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Request != "1.0" || rows[0].Resource != "slot3" || rows[0].Rank != "16" {
		t.Fatalf("best match = %+v, want {1.0 slot3 16}", rows)
	}

	// Top 3: ranked slot3(16), slot1(8), slot2(4).
	rows, err = c.MatchTables("jobs", "machines", "Key", "", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, r := range rows {
		got = append(got, r.Resource)
	}
	if len(got) != 3 || got[0] != "slot3" || got[1] != "slot1" || got[2] != "slot2" {
		t.Fatalf("ranked matches = %v, want [slot3 slot1 slot2]", got)
	}

	// Resource-side filter (pushed down): only Cpus <= 8 -> best is slot1.
	rows, err = c.MatchTables("jobs", "machines", "Key", "", "Cpus <= 8", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Resource != "slot1" {
		t.Fatalf("filtered match = %+v, want slot1", rows)
	}
}
