package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestMaintainEnablesSchemaScan: a Maintain pass with SchemaScanHotTopN builds the columnar
// accelerator so CountConstraint routes an eligible numeric predicate through it, and a second
// batch + second Maintain keeps the counts correct (refresh extends coverage, no orphaning).
func TestMaintainEnablesSchemaScan(t *testing.T) {
	d, err := OpenConfig(Config{Dir: t.TempDir(), SegmentSize: 1 << 9}) // small ⇒ segments seal even across 16 shards
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	put := func(lo, hi int) {
		tx := d.Begin()
		for i := lo; i < hi; i++ {
			ad, _ := classad.ParseOld(fmt.Sprintf("Memory = %d\nCpus = %d\nName = \"n%05d\"", 1024+(i%64)*256, 1+i%8, i))
			tx.NewClassAd(fmt.Sprintf("%d.0", i), ad)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	countBoth := func(tag, expr string) {
		got, ok := d.CountConstraint(expr)
		if !ok {
			t.Errorf("%s: CountConstraint declined %q", tag, expr)
			return
		}
		seq, err := d.Query(expr)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		for range seq {
			want++
		}
		if got != want {
			t.Errorf("%s: columnar count %d != query count %d for %q", tag, got, want, expr)
		}
	}

	put(0, 800)
	// Read demand on Memory so it lands in the hot tier the schema build ranks by.
	for i := 0; i < 15; i++ {
		seq, err := d.QueryProject("true", []string{"Memory"})
		if err != nil {
			t.Fatal(err)
		}
		for range seq {
		}
	}
	// Before Maintain enables it, CountConstraint declines (schema scan off).
	if _, ok := d.CountConstraint("Memory > 4096"); ok {
		t.Fatal("CountConstraint should decline before schema scan is enabled")
	}
	d.Maintain(MaintainOptions{SchemaScanHotTopN: 4})
	countBoth("first", "Memory > 4096")
	countBoth("first", "Memory <= 4096")

	put(800, 1600)
	d.Maintain(MaintainOptions{SchemaScanHotTopN: 4}) // refresh over the new batch
	countBoth("refresh", "Memory > 4096")
	countBoth("refresh", "Memory <= 4096")
	countBoth("refresh", "Memory >= 2000 && Memory < 9000")
}

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
