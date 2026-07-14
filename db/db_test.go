package db

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func mustAd(t *testing.T, s string) *classad.ClassAd {
	t.Helper()
	ad, err := classad.ParseOld(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ad
}

func TestNewSetDestroyLookup(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// NewClassAd + SetAttribute in one transaction (the qmgmt NewProc pattern).
	tx := db.Begin()
	tx.NewClassAd("1.0", mustAd(t, "ProcId = 0\nClusterId = 1"))
	if err := tx.SetAttribute("1.0", "JobStatus", "1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetAttribute("1.0", "Owner", `"alice"`); err != nil {
		t.Fatal(err)
	}
	// Read-your-writes inside the transaction.
	if v, ok := tx.LookupAttr("1.0", "JobStatus"); !ok || v != "1" {
		t.Fatalf("in-txn LookupAttr JobStatus = %q,%v want 1", v, ok)
	}
	// Not visible in the committed table yet.
	if _, ok := db.LookupClassAd("1.0"); ok {
		t.Fatal("uncommitted ad visible in the table")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	ad, ok := db.LookupClassAd("1.0")
	if !ok {
		t.Fatal("committed ad missing")
	}
	if s, _ := ad.EvaluateAttrInt("JobStatus"); s != 1 {
		t.Fatalf("JobStatus = %d, want 1", s)
	}
	if o, _ := ad.EvaluateAttrString("Owner"); o != "alice" {
		t.Fatalf("Owner = %q, want alice", o)
	}

	// DeleteAttribute + DestroyClassAd.
	tx = db.Begin()
	tx.DeleteAttribute("1.0", "Owner")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if ad, _ := db.LookupClassAd("1.0"); func() bool { _, ok := ad.Lookup("Owner"); return ok }() {
		t.Fatal("Owner not deleted")
	}
	tx = db.Begin()
	tx.DestroyClassAd("1.0")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.LookupClassAd("1.0"); ok {
		t.Fatal("ad not destroyed")
	}
}

func TestAbortDiscards(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	tx := db.Begin()
	tx.NewClassAd("k", mustAd(t, "N = 1"))
	tx.Abort()
	if _, ok := db.LookupClassAd("k"); ok {
		t.Fatal("aborted write became visible")
	}
}

// TestMultipleIndependentTransactions is the feature classad_log.h lacks: two live
// transactions at once, with write-write conflict detection between them.
func TestMultipleIndependentTransactions(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	tx0 := db.Begin()
	tx0.NewClassAd("j", mustAd(t, "JobStatus = 1"))
	if err := tx0.Commit(); err != nil {
		t.Fatal(err)
	}

	a := db.Begin()
	b := db.Begin()
	_, _ = a.LookupClassAd("j") // snapshot
	_, _ = b.LookupClassAd("j")
	if err := a.SetAttribute("j", "JobStatus", "2"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetAttribute("j", "JobStatus", "3"); err != nil {
		t.Fatal(err)
	}
	if err := a.Commit(); err != nil {
		t.Fatalf("first commit should win: %v", err)
	}
	err := b.Commit()
	ce, ok := err.(*ConflictError)
	if !ok || len(ce.Keys) != 1 || ce.Keys[0] != "j" {
		t.Fatalf("second commit = %v, want ConflictError on j", err)
	}
	ad, _ := db.LookupClassAd("j")
	if s, _ := ad.EvaluateAttrInt("JobStatus"); s != 2 {
		t.Fatalf("JobStatus = %d, want 2 (first committer)", s)
	}
}

func TestDBIDPersists(t *testing.T) {
	dir := t.TempDir()
	d1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, inst := d1.ID(), d1.InstanceID()
	if id == "" || inst == "" {
		t.Fatal("empty id/instance")
	}
	d1.Close()

	d2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if d2.ID() != id {
		t.Fatalf("DB id changed across reopen: %s != %s", d2.ID(), id)
	}
	if d2.InstanceID() == inst {
		t.Fatal("instance id should be fresh each open")
	}
}

func TestQueryAndMatchSorted(t *testing.T) {
	d, _ := Open("")
	defer d.Close()
	tx := d.Begin()
	tx.NewClassAd("s1", mustAd(t, "Cpus = 4\nMemory = 4096\nRequirements = true"))
	tx.NewClassAd("s2", mustAd(t, "Cpus = 16\nMemory = 8192\nRequirements = true"))
	tx.NewClassAd("s3", mustAd(t, "Cpus = 8\nMemory = 2048\nRequirements = true"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Constraint query (push-down).
	q, err := d.Query("Cpus >= 8")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range q {
		n++
	}
	if n != 2 {
		t.Fatalf("Query Cpus>=8 matched %d, want 2", n)
	}

	// MatchSorted: job wanting >=4 cpus, ranked by Cpus -> s2(16), s3(8), s1(4).
	job := mustAd(t, `Requirements = TARGET.Cpus >= 4
Rank = TARGET.Cpus`)
	got := d.MatchSorted(job, 2)
	if len(got) != 2 {
		t.Fatalf("MatchSorted top-2 got %d", len(got))
	}
	if c0, _ := got[0].EvaluateAttrInt("Cpus"); c0 != 16 {
		t.Fatalf("top match Cpus = %d, want 16", c0)
	}
}

func TestCommitNondurable(t *testing.T) {
	d, _ := Open("") // in-memory: nondurable == durable, just exercise the path
	defer d.Close()
	tx := d.Begin()
	tx.NewClassAd("k", mustAd(t, "N = 1"))
	if err := tx.CommitNondurable(); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.LookupClassAd("k"); !ok {
		t.Fatal("nondurable commit not visible")
	}
}

func TestForEach(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	tx := db.Begin()
	for _, k := range []string{"a", "b", "c"} {
		tx.NewClassAd(k, mustAd(t, "N = 1"))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	n := 0
	db.ForEach(func(ad *classad.ClassAd) bool { n++; return true })
	if n != 3 {
		t.Fatalf("ForEach saw %d ads, want 3", n)
	}
}
