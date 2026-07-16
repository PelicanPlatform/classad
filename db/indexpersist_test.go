package db

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func TestIndexConfigPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()

	// First open: seed data and create runtime indexes + hot attrs.
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seed := func(d *DB) {
		tx := d.Begin()
		for i := 0; i < 50; i++ {
			ad, _ := classad.ParseOld("RequestCpus = 4\nRequestMemory = 4096\nOwner = \"alice\"")
			tx.NewClassAd(key(i), ad)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	seed(d)
	d.AddIndex([]string{"Owner"}, []string{"RequestMemory"})
	d.AddHotAttrs("RequestCpus", "RequestMemory")
	d.Close()

	// Reopen: the indexes and hot set must be back without re-adding them.
	d2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()

	cat, val := d2.IndexedAttrs()
	if !contains(cat, "Owner") {
		t.Fatalf("categorical indexes after reopen = %v, want Owner", cat)
	}
	if !contains(val, "RequestMemory") {
		t.Fatalf("value indexes after reopen = %v, want RequestMemory", val)
	}
	hot := d2.HotAttrs()
	if !contains(hot, "RequestCpus") || !contains(hot, "RequestMemory") {
		t.Fatalf("hot attrs after reopen = %v, want RequestCpus + RequestMemory", hot)
	}

	// The reloaded index is built over the existing ads (Explain has stats).
	ex, err := d2.Explain(`RequestMemory > 1024`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Probes) != 1 || !ex.Probes[0].Indexed {
		t.Fatalf("Explain after reopen = %+v, want an indexed probe", ex)
	}

	// A subsequent drop persists too.
	d2.DropIndex("Owner")
	d3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d3.Close()
	cat3, _ := d3.IndexedAttrs()
	if contains(cat3, "Owner") {
		t.Fatalf("dropped index Owner reappeared after reopen: %v", cat3)
	}
}

// TestIndexProvenancePersistsAcrossReopen: human vs auto index provenance survives a
// restart, so the memory-budget trimmer keeps treating auto indexes as trimmable and
// human indexes as exempt.
func TestIndexProvenancePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	for i := 0; i < 30; i++ {
		ad, _ := classad.ParseOld("Owner = \"alice\"\nGPUs = true")
		tx.NewClassAd(key(i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	d.AddIndex([]string{"Owner"}, nil)      // human
	d.c.AddAutoIndex([]string{"GPUs"}, nil) // auto
	d.saveIndexConfig()
	d.Close()

	d2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	auto := d2.c.AutoIndexNames()
	if !contains(auto, "GPUs") {
		t.Errorf("GPUs should still be auto after reopen; auto=%v", auto)
	}
	if contains(auto, "Owner") {
		t.Errorf("Owner is human and must not become auto after reopen; auto=%v", auto)
	}
}

func key(i int) string {
	return string(rune('a'+i%26)) + string(rune('0'+i%10)) + string(rune('0'+i/10%10))
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
