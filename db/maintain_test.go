package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestMaintainAutoTunesIndexes: a Maintain pass adds an index for a repeatedly-queried
// attribute (demand-driven) and refreshes the hot set, and the change persists.
func TestMaintainAutoTunesIndexes(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	for i := 0; i < 300; i++ {
		ad, _ := classad.ParseOld(fmt.Sprintf("Owner = \"user%d\"\nArch = \"X86_64\"", i%25))
		tx.NewClassAd(fmt.Sprintf("%d.0", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Generate demand on Owner (equality queries).
	for i := 0; i < 20; i++ {
		seq, err := d.Query(`Owner == "user1"`)
		if err != nil {
			t.Fatal(err)
		}
		for range seq {
		}
	}
	d.Maintain(MaintainOptions{HotTopN: 4, MinIndexDemand: 5, IndexBudgetHighFrac: 0.5})

	cat, _ := d.IndexedAttrs()
	if !contains(cat, "Owner") {
		t.Errorf("Maintain should have auto-added a categorical index on the demanded Owner; got %v", cat)
	}
	// The auto-added index is persisted and provenance is auto.
	if auto := d.c.AutoIndexNames(); !contains(auto, "Owner") {
		t.Errorf("auto-added Owner index should carry auto provenance; got %v", auto)
	}
	d.Close()
	d2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if cat, _ := d2.IndexedAttrs(); !contains(cat, "Owner") {
		t.Errorf("auto-added index did not persist across reopen; got %v", cat)
	}
}
