package dbrpc

import (
	"context"
	"fmt"
	"strings"
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
	mtx, _ := c.BeginTable(context.Background(), "machines")
	_ = mtx.NewClassAd(context.Background(), "slot1", "Key = \"slot1\"\nCpus = 8\nRequirements = true")
	_ = mtx.NewClassAd(context.Background(), "slot2", "Key = \"slot2\"\nCpus = 4\nRequirements = true")
	_ = mtx.NewClassAd(context.Background(), "slot3", "Key = \"slot3\"\nCpus = 16\nRequirements = true")
	if err := mtx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Two identical jobs: assignment consumes machines, so they can't both take
	// slot3. Each ranks by Cpus.
	jtx, _ := c.BeginTable(context.Background(), "jobs")
	job := "Key = %q\nRequestCpus = 4\nRequirements = (TARGET.Cpus >= RequestCpus)\nRank = TARGET.Cpus"
	_ = jtx.NewClassAd(context.Background(), "1.0", fmt.Sprintf(job, "1.0"))
	_ = jtx.NewClassAd(context.Background(), "2.0", fmt.Sprintf(job, "2.0"))
	if err := jtx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	// LIMIT bounds jobs: one job assigned, taking the best machine (slot3, Cpus 16).
	rows, err := c.MatchTables(context.Background(), "jobs", "machines", "Key", "", "", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Resource != "slot3" || rows[0].Rank != "16" {
		t.Fatalf("limit-1 assignment = %+v, want one job -> slot3/16", rows)
	}

	// LIMIT 2: both jobs assigned distinct machines, the two best by Rank —
	// slot3(16) and slot1(8). Order between the identical jobs is unspecified, so
	// assert the set and each machine's rank.
	rows, err = c.MatchTables(context.Background(), "jobs", "machines", "Key", "", "", 2, nil)
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
	rows, err = c.MatchTables(context.Background(), "jobs", "machines", "Key", "", "Cpus <= 8", 1, nil)
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
	mtx, _ := c.BeginTable(context.Background(), "machines")
	_ = mtx.NewClassAd(context.Background(), "slot1", "Key = \"slot1\"\nCpus = 16\nRequirements = true")
	_ = mtx.NewClassAd(context.Background(), "slot2", "Key = \"slot2\"\nCpus = 16\nRequirements = true")
	_ = mtx.NewClassAd(context.Background(), "slot3", "Key = \"slot3\"\nCpus = 8\nRequirements = true")
	_ = mtx.Commit(context.Background())

	jtx, _ := c.BeginTable(context.Background(), "jobs")
	req := "Key = %q\nRequestCpus = 4\nRequirements = (TARGET.Cpus >= RequestCpus)\nRank = TARGET.Cpus"
	_ = jtx.NewClassAd(context.Background(), "a", fmt.Sprintf(req, "a"))
	_ = jtx.NewClassAd(context.Background(), "b", fmt.Sprintf(req, "b")) // identical to a in matchmaking terms
	_ = jtx.Commit(context.Background())

	sig := []string{"RequestCpus", "Requirements", "Rank"}
	rows, err := c.MatchTables(context.Background(), "jobs", "machines", "Key", "", "", 5, sig)
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

// TestMatchExplain checks the cross-table match-plan explanation over dbrpc.
func TestMatchExplain(t *testing.T) {
	cat, _ := db.OpenCatalog("")
	_, _ = cat.CreateTable("jobs")
	m, _ := cat.CreateTable("machines")
	_ = m.AddIndex(nil, []string{"Memory"}) // value-index Memory on machines
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); cat.Close() }()

	mtx, _ := c.BeginTable(context.Background(), "machines")
	_ = mtx.NewClassAd(context.Background(), "slot1", "Key=\"slot1\"\nArch=\"X86_64\"\nMemory=8192\nRequirements=true")
	_ = mtx.Commit(context.Background())
	jtx, _ := c.BeginTable(context.Background(), "jobs")
	_ = jtx.NewClassAd(context.Background(), "1.0", "Key=\"1.0\"\nRequestMemory=2048\nRequirements=(TARGET.Arch==\"X86_64\") && (TARGET.Memory>=RequestMemory)")
	_ = jtx.Commit(context.Background())

	ex, err := c.MatchExplain(context.Background(), "jobs", "Key == \"1.0\"", "machines", "")
	if err != nil {
		t.Fatal(err)
	}
	if !ex.HasRequirements {
		t.Fatal("want HasRequirements")
	}
	if ex.Plan != "indexed" {
		t.Errorf("plan = %q, want indexed", ex.Plan)
	}
	var mem, arch bool
	for _, p := range ex.Probes {
		if p.Attr == "Memory" && p.Indexed {
			mem = true
		}
		if p.Attr == "Arch" && !p.Indexed {
			arch = true
		}
	}
	if !mem || !arch {
		t.Errorf("probes = %+v, want indexed Memory + unindexed Arch", ex.Probes)
	}

	// A resource-side filter (WHERE TARGET / NOPREEMPT) is melded into the explanation.
	exT, err := c.MatchExplain(context.Background(), "jobs", "Key == \"1.0\"", "machines", `State =!= "Claimed"`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(exT.SlotPredicate, `State`) {
		t.Errorf("slot predicate = %q, want the resource-side State filter melded in", exT.SlotPredicate)
	}
	resSide := false
	for _, ce := range exT.EvalOrder {
		if ce.ResourceSide && strings.Contains(ce.Text, "State") {
			resSide = true
		}
	}
	if !resSide {
		t.Errorf("eval order = %+v, want a resource-side State conjunct", exT.EvalOrder)
	}
}
