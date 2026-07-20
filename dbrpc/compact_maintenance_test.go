package dbrpc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// TestStartMaintenanceCompactsDeadSpace verifies the standalone compaction pass:
// a high-churn table (records repeatedly superseded) accumulates dead space, and
// the maintenance server reclaims it on the short compaction cadence WITHOUT
// waiting on the expensive retrain-dominated Maintain pass (retrain is off and the
// Maintain interval is set far in the future here, so only compaction can run).
func TestStartMaintenanceCompactsDeadSpace(t *testing.T) {
	cat, err := db.OpenCatalog("") // in-memory catalog
	if err != nil {
		t.Fatal(err)
	}
	d, err := cat.CreateTable("t")
	if err != nil {
		t.Fatal(err)
	}

	big := strings.Repeat("x", 500)
	mkAd := func(i int) *classad.ClassAd {
		ad, perr := classad.ParseOld(fmt.Sprintf("Name = \"k%d\"\nData = \"%s\"", i, big))
		if perr != nil {
			t.Fatal(perr)
		}
		return ad
	}
	// Write 4000 keys, then overwrite them all twice more: ~2/3 of used bytes become
	// dead (superseded), comfortably past the 50% compaction threshold and -- with
	// enough volume to clear the per-shard 64 KiB minimum across the default 16
	// shards -- the minimum-shard floor.
	writePass := func() {
		tx := d.Begin()
		for i := 0; i < 4000; i++ {
			tx.NewClassAd(fmt.Sprintf("k%d", i), mkAd(i))
		}
		if cerr := tx.Commit(); cerr != nil {
			t.Fatal(cerr)
		}
	}
	writePass()
	writePass()
	writePass()

	before := d.Stats()
	if before.UsedBytes == 0 || float64(before.DeadBytes) < 0.5*float64(before.UsedBytes) {
		t.Fatalf("test setup: want dead > 50%% of used; got used=%d dead=%d", before.UsedBytes, before.DeadBytes)
	}

	s := NewServerCatalog(cat)
	defer s.Close()
	// Maintain (retrain) interval far in the future so it cannot run during the
	// test; only the standalone compaction pass (10ms) should reclaim the dead space.
	s.StartMaintenance(time.Hour, db.MaintainOptions{CompactInterval: 10 * time.Millisecond})

	deadline := time.Now().Add(3 * time.Second)
	var after db.Stats
	for time.Now().Before(deadline) {
		after = d.Stats()
		if float64(after.DeadBytes) < 0.5*float64(before.DeadBytes) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if float64(after.DeadBytes) >= 0.5*float64(before.DeadBytes) {
		t.Fatalf("compaction did not reclaim dead space: dead %d -> %d (used %d -> %d)",
			before.DeadBytes, after.DeadBytes, before.UsedBytes, after.UsedBytes)
	}
	// Live data must be intact: a live record must still resolve.
	if _, ok := d.LookupClassAd("k0"); !ok {
		t.Error("k0 missing after compaction")
	}
}

// TestStartMaintenanceCompactionDisabled verifies a negative CompactInterval turns
// the standalone compaction pass off (leaving only the retrain-paced Maintain pass).
func TestStartMaintenanceCompactionDisabled(t *testing.T) {
	cat, err := db.OpenCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	d, err := cat.CreateTable("t")
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("y", 500)
	for pass := 0; pass < 3; pass++ {
		tx := d.Begin()
		for i := 0; i < 4000; i++ {
			ad, _ := classad.ParseOld(fmt.Sprintf("Name = \"k%d\"\nData = \"%s\"", i, big))
			tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
		}
		_ = tx.Commit()
	}
	before := d.Stats()

	s := NewServerCatalog(cat)
	defer s.Close()
	s.StartMaintenance(time.Hour, db.MaintainOptions{CompactInterval: -1})

	time.Sleep(200 * time.Millisecond)
	if d.Stats().DeadBytes < before.DeadBytes {
		t.Errorf("compaction ran despite CompactInterval=-1 (dead %d -> %d)", before.DeadBytes, d.Stats().DeadBytes)
	}
}
