package db

import (
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestCatalogDeltaMaxReachesTables: delta records are configured on the CATALOG rather than per
// CreateTable call, because OpenCatalogConfig opens every existing table directory at startup and
// CreateTable then returns the already-open table without re-applying options. This checks the
// setting reaches a table both ways -- one created during this process and one inherited from a
// previous open -- because only the second is what a restarted daemon actually does.
func TestCatalogDeltaMaxReachesTables(t *testing.T) {
	dir := t.TempDir()
	write := func(cat *Catalog, table, key string, status int64) {
		t.Helper()
		d, err := cat.CreateTable(table)
		if err != nil {
			t.Fatal(err)
		}
		tx := d.Begin()
		ad := classad.New()
		ad.InsertAttrString("Key", key)
		ad.InsertAttr("JobStatus", status)
		for i := 0; i < 40; i++ {
			ad.InsertAttr("Pad"+string(rune('A'+i%26))+string(rune('0'+i/26)), int64(i))
		}
		tx.NewClassAd(key, ad)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		for r := 1; r <= 6; r++ { // repeated single-attribute updates: the delta shape
			tx := d.Begin()
			if err := tx.SetAttribute(key, "JobStatus", "5"); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}

	cat, err := OpenCatalogConfig(CatalogConfig{Dir: dir, DeltaMax: 4})
	if err != nil {
		t.Fatal(err)
	}
	write(cat, "jobs", "1.0", 1)
	jobs, _ := cat.Table("jobs")
	deltas, fulls := jobs.DeltaStats()
	if deltas == 0 {
		t.Fatalf("catalog DeltaMax did not reach a table created in this process (%d full records)", fulls)
	}
	cat.Close()

	// Reopen: the table now exists on disk, so it is opened by OpenCatalogConfig rather than by
	// CreateTable. A setting that only worked on first creation would read zero here.
	cat2, err := OpenCatalogConfig(CatalogConfig{Dir: dir, DeltaMax: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	write(cat2, "jobs", "2.0", 1)
	jobs2, _ := cat2.Table("jobs")
	if d2, f2 := jobs2.DeltaStats(); d2 == 0 {
		t.Fatalf("catalog DeltaMax did not reach a table inherited from a previous open (%d full)", f2)
	}
	// And the contents survive the round trip.
	ad, ok := jobs2.LookupClassAd("1.0")
	if !ok {
		t.Fatal("key written before the reopen is missing")
	}
	if v, got := ad.EvaluateAttrInt("JobStatus"); !got || v != 5 {
		t.Fatalf("JobStatus = %d (present=%v), want 5", v, got)
	}
	if n := ad.Size(); n < 40 {
		t.Fatalf("ad has %d attributes after reopen, want the whole ad", n)
	}
	if _, err := filepath.Abs(dir); err != nil {
		t.Fatal(err)
	}
}

// TestCatalogDeltaMaxPerTable: DeltaMaxFor overrides the catalog-wide setting by table name, in
// BOTH directions -- enabling delta records where the catalog has them off, and turning them off
// where the catalog (or the default) has them on. The second direction is the one that matters
// now that zero means DefaultDeltaMax: without it there would be no way to exempt a table.
func TestCatalogDeltaMaxPerTable(t *testing.T) {
	t.Run("override on, catalog off", func(t *testing.T) {
		on, off := runDeltaPair(t, CatalogConfig{DeltaMax: DeltaMaxOff, DeltaMaxFor: map[string]int{"jobs": 4}})
		if on == 0 {
			t.Error("jobs had a DeltaMaxFor override and stored no delta records")
		}
		if off != 0 {
			t.Errorf("users had the catalog's DeltaMaxOff and stored %d delta records", off)
		}
	})
	t.Run("override off, catalog default", func(t *testing.T) {
		// DeltaMax left at zero: the default is ON, so jobs gets deltas without being asked and
		// users is exempted only by its explicit override.
		jobs, users := runDeltaPair(t, CatalogConfig{DeltaMaxFor: map[string]int{"users": DeltaMaxOff}})
		if jobs == 0 {
			t.Error("jobs stored no delta records under the default: delta records are supposed to be on")
		}
		if users != 0 {
			t.Errorf("users had DeltaMaxOff and stored %d delta records", users)
		}
	})
}

// runDeltaPair writes the delta-shaped workload (one whole ad, then repeated single-attribute
// updates) to a "jobs" and a "users" table under cfg, and reports how many delta records each
// stored.
func runDeltaPair(t *testing.T, cfg CatalogConfig) (jobsDeltas, usersDeltas int64) {
	t.Helper()
	cfg.Dir = t.TempDir()
	cat, err := OpenCatalogConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	for _, table := range []string{"jobs", "users"} {
		d, err := cat.CreateTable(table)
		if err != nil {
			t.Fatal(err)
		}
		tx := d.Begin()
		ad := classad.New()
		ad.InsertAttr("JobStatus", 1)
		ad.InsertAttrString("Owner", "alice")
		tx.NewClassAd("1.0", ad)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		for r := 1; r <= 6; r++ {
			tx := d.Begin()
			if err := tx.SetAttribute("1.0", "JobStatus", "5"); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	jobs, _ := cat.Table("jobs")
	users, _ := cat.Table("users")
	jd, _ := jobs.DeltaStats()
	ud, _ := users.DeltaStats()
	return jd, ud
}
