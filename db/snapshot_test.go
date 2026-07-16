package db

import (
	"bytes"
	"fmt"
	"testing"
)

// fill writes n ads, each with a public and a private (ClaimId) attribute.
func fill(t *testing.T, db *DB, prefix string, n int) {
	t.Helper()
	tx := db.Begin()
	for i := 0; i < n; i++ {
		ad := mustAd(t, fmt.Sprintf("Owner = \"user%d\"\nClaimId = \"secret-%s-%d\"", i, prefix, i))
		tx.NewClassAd(fmt.Sprintf("%s%d", prefix, i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func poolKey(id string) KEK {
	return KEK{ID: id, Material: []byte("pool-key-material-for-snapshot-" + id + "-padding")}
}

// TestSnapshotRestoreEncrypted is the core round-trip: snapshot an encrypted DB, mutate
// it, then restore -- every ad (including decrypted private attributes) comes back and
// the post-snapshot mutations are gone.
func TestSnapshotRestoreEncrypted(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir, PoolKeys: []KEK{poolKey("POOL")}, EncryptedAttrs: []string{"Region"}}
	db, err := OpenConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const n = 500
	fill(t, db, "job", n)

	var snap bytes.Buffer
	if err := db.Snapshot(&snap); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// The snapshot must not contain a secret in the clear.
	if bytes.Contains(snap.Bytes(), []byte("secret-job-0")) {
		t.Fatal("snapshot leaked a private attribute in plaintext")
	}

	// Mutate after the snapshot: add ads and delete some.
	fill(t, db, "extra", 50)
	tx := db.Begin()
	for i := 0; i < 100; i++ {
		tx.DestroyClassAd(fmt.Sprintf("job%d", i))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if db.Len() != n-100+50 {
		t.Fatalf("pre-restore Len = %d, want %d", db.Len(), n-100+50)
	}

	// Restore returns to the exact snapshot state.
	if err := db.Restore(bytes.NewReader(snap.Bytes())); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if db.Len() != n {
		t.Fatalf("post-restore Len = %d, want %d", db.Len(), n)
	}
	if _, ok := db.LookupClassAd("extra0"); ok {
		t.Error("a post-snapshot ad survived restore")
	}
	ad, ok := db.LookupClassAd("job0")
	if !ok {
		t.Fatal("job0 missing after restore")
	}
	if v, _ := ad.EvaluateAttrString("ClaimId"); v != "secret-job-0" {
		t.Fatalf("restored ClaimId = %q, want the decrypted secret", v)
	}
	db.Close()

	// Reopen: restored data persisted and the private attr is still sealed on disk.
	db, err = OpenConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.Len() != n {
		t.Fatalf("reopened Len = %d, want %d", db.Len(), n)
	}
	if bytes.Contains(readAll(t, dir), []byte("secret-job-0")) {
		t.Fatal("restored private attribute is not sealed on disk")
	}
}

// TestSnapshotCrossKeyPortable verifies a snapshot taken by one DB can be restored into a
// DIFFERENT DB that shares a pool key (the embedded master envelope opens with it), and
// that a DB with only a non-matching key cannot.
func TestSnapshotCrossKeyPortable(t *testing.T) {
	shared := poolKey("SHARED")

	src, err := OpenConfig(Config{Dir: t.TempDir(), PoolKeys: []KEK{shared, poolKey("SRCONLY")}})
	if err != nil {
		t.Fatal(err)
	}
	fill(t, src, "k", 40)
	var snap bytes.Buffer
	if err := src.Snapshot(&snap); err != nil {
		t.Fatal(err)
	}
	src.Close()

	// A destination DB with its own master but the SHARED pool key can restore.
	dst, err := OpenConfig(Config{Dir: t.TempDir(), PoolKeys: []KEK{shared, poolKey("DSTONLY")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.Restore(bytes.NewReader(snap.Bytes())); err != nil {
		t.Fatalf("cross-key restore with shared key: %v", err)
	}
	if dst.Len() != 40 {
		t.Fatalf("restored Len = %d, want 40", dst.Len())
	}
	if ad, ok := dst.LookupClassAd("k0"); !ok {
		t.Error("k0 missing after cross-key restore")
	} else if v, _ := ad.EvaluateAttrString("ClaimId"); v != "secret-k-0" {
		t.Errorf("ClaimId = %q, want secret-k-0", v)
	}
	dst.Close()

	// A DB with NO shared key cannot open the snapshot envelope.
	other, err := OpenConfig(Config{Dir: t.TempDir(), PoolKeys: []KEK{poolKey("UNRELATED")}})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Restore(bytes.NewReader(snap.Bytes())); err == nil {
		t.Fatal("restore should fail without a matching pool key")
	}
	// A failed restore must not have wiped the destination (truncate happens only after
	// the key is recovered).
}

// TestSnapshotPlaintext verifies snapshot/restore also work with encryption disabled.
func TestSnapshotPlaintext(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fill(t, db, "p", 30)
	var snap bytes.Buffer
	if err := db.Snapshot(&snap); err != nil {
		t.Fatal(err)
	}
	db.Truncate()
	if db.Len() != 0 {
		t.Fatalf("after Truncate Len = %d, want 0", db.Len())
	}
	if err := db.Restore(bytes.NewReader(snap.Bytes())); err != nil {
		t.Fatal(err)
	}
	if db.Len() != 30 {
		t.Fatalf("restored Len = %d, want 30", db.Len())
	}
}
