package db

import (
	"bytes"
	"fmt"
	"testing"
)

func fillTable(t *testing.T, d *DB, prefix string, n int) {
	t.Helper()
	tx := d.Begin()
	for i := 0; i < n; i++ {
		tx.NewClassAd(fmt.Sprintf("%s%d", prefix, i),
			mustAd(t, fmt.Sprintf("Owner = \"u%d\"\nClaimId = \"sec-%s-%d\"", i, prefix, i)))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestCatalogSnapshotRestore backs up and restores a whole multi-table catalog: every
// table returns to its snapshotted state, private attributes decrypt, and post-snapshot
// mutations across tables are reverted.
func TestCatalogSnapshotRestore(t *testing.T) {
	poolKeys := []KEK{poolKey("POOL")}
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir(), PoolKeys: poolKeys, EncryptedAttrs: []string{"Region"}})
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := cat.CreateTable("jobs")
	machines, _ := cat.CreateTable("machines")
	fillTable(t, jobs, "j", 300)
	fillTable(t, machines, "m", 120)

	var snap bytes.Buffer
	if err := cat.Snapshot(&snap); err != nil {
		t.Fatalf("catalog Snapshot: %v", err)
	}
	if bytes.Contains(snap.Bytes(), []byte("sec-j-0")) {
		t.Fatal("catalog snapshot leaked a private attribute")
	}

	// Mutate both tables after the snapshot.
	fillTable(t, jobs, "extra", 50)
	machines.Truncate()
	if jobs.Len() != 350 || machines.Len() != 0 {
		t.Fatalf("pre-restore lens jobs=%d machines=%d", jobs.Len(), machines.Len())
	}

	if err := cat.Restore(bytes.NewReader(snap.Bytes())); err != nil {
		t.Fatalf("catalog Restore: %v", err)
	}
	// Re-fetch the tables (restore recreates via CreateTable, returning the existing one).
	jobs, _ = cat.Table("jobs")
	machines, _ = cat.Table("machines")
	if jobs.Len() != 300 || machines.Len() != 120 {
		t.Fatalf("post-restore lens jobs=%d (want 300) machines=%d (want 120)", jobs.Len(), machines.Len())
	}
	if _, ok := jobs.LookupClassAd("extra0"); ok {
		t.Error("a post-snapshot ad survived the catalog restore")
	}
	if ad, ok := machines.LookupClassAd("m0"); !ok {
		t.Error("m0 missing after catalog restore")
	} else if v, _ := ad.EvaluateAttrString("ClaimId"); v != "sec-m-0" {
		t.Errorf("machines m0 ClaimId = %q, want sec-m-0", v)
	}
	cat.Close()
}

// TestCatalogSnapshotCrossPool restores a catalog backup into a different catalog that
// shares a pool key.
func TestCatalogSnapshotCrossPool(t *testing.T) {
	shared := poolKey("SHARED")
	src, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir(), PoolKeys: []KEK{shared}})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := src.CreateTable("a")
	fillTable(t, a, "x", 40)
	var snap bytes.Buffer
	if err := src.Snapshot(&snap); err != nil {
		t.Fatal(err)
	}
	src.Close()

	dst, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir(), PoolKeys: []KEK{shared, poolKey("DSTONLY")}})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.Restore(bytes.NewReader(snap.Bytes())); err != nil {
		t.Fatalf("cross-pool catalog restore: %v", err)
	}
	da, ok := dst.Table("a")
	if !ok || da.Len() != 40 {
		t.Fatalf("restored table a missing or wrong size")
	}
}
