package db

import "testing"

// TestOpStatsSnapshotLockAndWrites: transactional writes accumulate the shard
// write-lock counters (via collections), and a Truncate records the DB-wide snapshot
// lock's exclusive hold.
func TestOpStatsSnapshotLockAndWrites(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	fill(t, db, "a", 100)

	op := db.OpStats()
	if op.ShardWriteHold.Count == 0 {
		t.Error("ShardWriteHold.Count = 0; the committed transaction holds shard write locks")
	}
	if op.SnapshotLock.Count != 0 {
		t.Errorf("SnapshotLock.Count = %d before any Truncate/Restore, want 0", op.SnapshotLock.Count)
	}

	db.Truncate()
	op = db.OpStats()
	if op.SnapshotLock.Count != 1 {
		t.Errorf("SnapshotLock.Count = %d after one Truncate, want 1", op.SnapshotLock.Count)
	}
	if op.SnapshotLock.Nanos < 0 {
		t.Errorf("SnapshotLock.Nanos = %d, want >= 0", op.SnapshotLock.Nanos)
	}
}
