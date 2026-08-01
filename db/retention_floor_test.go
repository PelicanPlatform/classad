package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
)

// TestArchiveGCFloor verifies ArchiveTable.SetGCFloor drives early GC through the db layer,
// respects the persisted MinAge floor, and -- unlike SetRetention -- is NOT persisted, so a
// reopen starts with no floor (no accidental GC across a restart).
func TestArchiveGCFloor(t *testing.T) {
	dir := t.TempDir()
	cfg := ArchiveConfig{
		SegmentSize: 1 << 12, // small ⇒ several segments
		ZoneAttrs:   []string{"CompletionDate"},
		// Loose ceiling that will not fire, plus a MinAge minimum-retention floor (its own attr).
		Retention: collections.Retention{MaxAgeAttr: "CompletionDate", MaxAge: 100000, MinAgeAttr: "CompletionDate", MinAge: 100},
	}

	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateArchiveTable("history", cfg)
	if err != nil {
		t.Fatal(err)
	}
	const n, base = 400, 1000
	const now = base + n - 1 // 1399
	for i := 0; i < n; i++ {
		if err := hist.AppendOld(fmt.Sprintf("N = %d\nCompletionDate = %d", i, base+i)); err != nil {
			t.Fatal(err)
		}
	}
	full := hist.Count()

	// A GC floor above everything, but MinAge protects the newest 100 (CompletionDate > now-100
	// = 1299): early GC drains the consumed older records but leaves the young ones.
	hist.SetGCFloor(1e9)
	if got := hist.GCFloor(); got != 1e9 {
		t.Fatalf("GCFloor = %v, want 1e9", got)
	}
	if _, err := hist.Rotate(now); err != nil {
		t.Fatal(err)
	}
	drained := hist.Count()
	if drained >= full {
		t.Fatalf("GC floor drained nothing (%d→%d)", full, drained)
	}
	if drained == 0 {
		t.Fatalf("GC floor drained everything; MinAge must have kept the newest records")
	}

	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the GC floor is not persisted, so it starts cleared and a Rotate does no early
	// draining (only the loose ceiling applies, which does not fire).
	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	hist2, err := cat2.CreateArchiveTable("history", cfg) // idempotent: reopens with persisted config
	if err != nil {
		t.Fatal(err)
	}
	if got := hist2.GCFloor(); got != 0 {
		t.Fatalf("GCFloor after reopen = %v, want 0 -- the floor must not persist", got)
	}
	countReopen := hist2.Count()
	if _, err := hist2.Rotate(now); err != nil {
		t.Fatal(err)
	}
	if hist2.Count() != countReopen {
		t.Fatalf("Rotate after reopen changed count %d→%d; a stale floor must not GC across a restart", countReopen, hist2.Count())
	}
}
