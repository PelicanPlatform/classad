package db

import (
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/collections"
)

func countAsOf(t *testing.T, db *DB, constraint string, at time.Time) int {
	t.Helper()
	seq, err := db.QueryAsOf(constraint, at)
	if err != nil {
		t.Fatalf("QueryAsOf(%q): %v", constraint, err)
	}
	n := 0
	for range seq {
		n++
	}
	return n
}

// TestDBTimeTravel exercises the db-layer wiring: enabling persists across a reopen,
// AS OF resolves the historical value, and disabling is refused + persisted.
func TestDBTimeTravel(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Disabled by default.
	if _, err := db.QueryAsOf("true", time.Now()); err != collections.ErrTimeTravelDisabled {
		t.Fatalf("QueryAsOf before enable = %v, want ErrTimeTravelDisabled", err)
	}

	// Enable with a fine checkpoint cadence so distinct writes get distinct checkpoints.
	db.SetTimeTravel(time.Hour, time.Millisecond)
	if d, _, on := db.TimeTravel(); !on || d != time.Hour {
		t.Fatalf("TimeTravel() = (%v,_,%v), want (1h,_,true)", d, on)
	}

	putAd(t, db, "k", "Owner = \"alice\"\nJobStatus = 1")
	time.Sleep(20 * time.Millisecond)
	tMid := time.Now()
	time.Sleep(20 * time.Millisecond)
	putAd(t, db, "k", "Owner = \"alice\"\nJobStatus = 2")

	if n := countAsOf(t, db, "JobStatus == 1", tMid); n != 1 {
		t.Errorf("count(JobStatus==1) AS OF mid = %d, want 1", n)
	}
	if n := countAsOf(t, db, "JobStatus == 2", tMid); n != 0 {
		t.Errorf("count(JobStatus==2) AS OF mid = %d, want 0 (not yet written)", n)
	}
	if n := countAsOf(t, db, "JobStatus == 2", time.Now()); n != 1 {
		t.Errorf("count(JobStatus==2) AS OF now = %d, want 1", n)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the setting persisted, and AS OF still resolves (index rebuilt on recovery).
	db2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, on := db2.TimeTravel(); !on {
		t.Fatal("time travel not enabled after reopen (setting did not persist)")
	}
	if n := countAsOf(t, db2, "JobStatus == 1", tMid); n != 1 {
		t.Errorf("after reopen, count(JobStatus==1) AS OF mid = %d, want 1", n)
	}

	// Disable, and confirm it is refused and persists.
	db2.SetTimeTravel(0, 0)
	if _, err := db2.QueryAsOf("true", time.Now()); err != collections.ErrTimeTravelDisabled {
		t.Fatalf("QueryAsOf after disable = %v, want ErrTimeTravelDisabled", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatal(err)
	}
	db3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db3.Close()
	if _, _, on := db3.TimeTravel(); on {
		t.Fatal("time travel still enabled after disable+reopen")
	}
}
