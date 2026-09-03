package db

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func mustPutMarker(t *testing.T, d *DB, key string, marker int) {
	t.Helper()
	ad, err := classad.Parse(fmt.Sprintf("[ Marker=%d ]", marker))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Put(key, ad); err != nil {
		t.Fatal(err)
	}
}

// TestDeleteWhereDeletesLargeFixedSet checks the snapshot-once sweep clears a match set
// larger than deleteBatch in one call.
func TestDeleteWhereDeletesLargeFixedSet(t *testing.T) {
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	const n = deleteBatch*2 + 137
	for i := 0; i < n; i++ {
		mustPutMarker(t, d, fmt.Sprintf("k%d", i), 1)
	}
	got, err := d.DeleteWhere("Marker == 1")
	if err != nil {
		t.Fatalf("DeleteWhere: %v", err)
	}
	if got != n {
		t.Errorf("removed %d, want %d", got, n)
	}
	if left, _ := d.CountConstraint("Marker == 1"); left != 0 {
		t.Errorf("%d rows still match after delete", left)
	}
}

// TestDeleteWhereSnapshotsMatchSetOnce is the regression for the non-convergence hang. It
// deterministically proves DeleteWhere acts only on the rows matching at statement start: a
// new matching row inserted AFTER the snapshot (via the post-scan hook, standing in for a
// concurrent writer) must survive. The old re-scan-every-round design would find and delete
// that row too -- and, faced with a continuously refreshed match set, never converge. The
// snapshot-once design deletes the fixed set and stops, so it always terminates.
func TestDeleteWhereSnapshotsMatchSetOnce(t *testing.T) {
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	mustPutMarker(t, d, "a", 1) // present at statement start -> deleted

	var once sync.Once
	deleteWhereAfterScanHook = func() {
		once.Do(func() { mustPutMarker(t, d, "b", 1) }) // appears AFTER the snapshot -> must survive
	}
	defer func() { deleteWhereAfterScanHook = nil }()

	got, err := d.DeleteWhere("Marker == 1")
	if err != nil {
		t.Fatalf("DeleteWhere: %v", err)
	}
	if got != 1 {
		t.Errorf("deleted %d, want 1 (only the pre-snapshot row)", got)
	}
	if _, ok := d.LookupClassAd("a"); ok {
		t.Error(`"a" (present at start) should have been deleted`)
	}
	if _, ok := d.LookupClassAd("b"); !ok {
		t.Error(`"b" (inserted after the snapshot) was deleted -- DeleteWhere chased a new match instead of snapshotting once`)
	}
}
