package dbrpc

import (
	"fmt"
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
	// Two identical jobs: assignment consumes machines, so they can't both take
	// slot3. Each ranks by Cpus.
	jtx, _ := c.BeginTable("jobs")
	job := "Key = %q\nRequestCpus = 4\nRequirements = (TARGET.Cpus >= RequestCpus)\nRank = TARGET.Cpus"
	_ = jtx.NewClassAd("1.0", fmt.Sprintf(job, "1.0"))
	_ = jtx.NewClassAd("2.0", fmt.Sprintf(job, "2.0"))
	if err := jtx.Commit(); err != nil {
		t.Fatal(err)
	}

	// LIMIT bounds jobs: one job assigned, taking the best machine (slot3, Cpus 16).
	rows, err := c.MatchTables("jobs", "machines", "Key", "", "", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Resource != "slot3" || rows[0].Rank != "16" {
		t.Fatalf("limit-1 assignment = %+v, want one job -> slot3/16", rows)
	}

	// LIMIT 2: both jobs assigned distinct machines, the two best by Rank —
	// slot3(16) and slot1(8). Order between the identical jobs is unspecified, so
	// assert the set and each machine's rank.
	rows, err = c.MatchTables("jobs", "machines", "Key", "", "", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	rankOf := map[string]string{}
	for _, r := range rows {
		rankOf[r.Resource] = r.Rank
	}
	if len(rows) != 2 || len(rankOf) != 2 || rankOf["slot3"] != "16" || rankOf["slot1"] != "8" {
		t.Fatalf("limit-2 assignment = %+v, want distinct {slot3:16, slot1:8}", rows)
	}

	// Resource-side filter (pushed down): with only Cpus <= 8 eligible, the first
	// job's best available is slot1.
	rows, err = c.MatchTables("jobs", "machines", "Key", "", "Cpus <= 8", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Resource != "slot1" {
		t.Fatalf("filtered assignment = %+v, want slot1", rows)
	}
}

// TestMatchAutocluster exercises the autocluster cache under assignment: two
// identical requests (same significant attributes) reuse one ranked-candidate
// computation but are still assigned distinct machines (consumption).
func TestMatchAutocluster(t *testing.T) {
	cat, _ := db.OpenCatalog("")
	_, _ = cat.CreateTable("jobs")
	_, _ = cat.CreateTable("machines")
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); cat.Close() }()

	// Two top-ranked (Cpus 16) machines and a lesser one; identical jobs must be
	// spread across the two 16s rather than both taking one.
	mtx, _ := c.BeginTable("machines")
	_ = mtx.NewClassAd("slot1", "Key = \"slot1\"\nCpus = 16\nRequirements = true")
	_ = mtx.NewClassAd("slot2", "Key = \"slot2\"\nCpus = 16\nRequirements = true")
	_ = mtx.NewClassAd("slot3", "Key = \"slot3\"\nCpus = 8\nRequirements = true")
	_ = mtx.Commit()

	jtx, _ := c.BeginTable("jobs")
	req := "Key = %q\nRequestCpus = 4\nRequirements = (TARGET.Cpus >= RequestCpus)\nRank = TARGET.Cpus"
	_ = jtx.NewClassAd("a", fmt.Sprintf(req, "a"))
	_ = jtx.NewClassAd("b", fmt.Sprintf(req, "b")) // identical to a in matchmaking terms
	_ = jtx.Commit()

	sig := []string{"RequestCpus", "Requirements", "Rank"}
	rows, err := c.MatchTables("jobs", "machines", "Key", "", "", 5, sig)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Request] = r.Resource
	}
	// Both assigned, to the two distinct Cpus-16 slots (order between them is
	// unspecified); slot3 (Cpus 8) is left unused.
	if len(got) != 2 || got["a"] == got["b"] ||
		(got["a"] != "slot1" && got["a"] != "slot2") ||
		(got["b"] != "slot1" && got["b"] != "slot2") {
		t.Fatalf("autocluster assignment = %v, want a,b -> distinct {slot1,slot2}", got)
	}
}
