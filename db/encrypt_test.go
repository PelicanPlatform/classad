package db

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func readAll(t *testing.T, dir string) []byte {
	t.Helper()
	var out []byte
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		var num uint64
		var dictID uint32
		if _, e := fmt.Sscanf(d.Name(), "seg-%d.d%d.dat", &num, &dictID); e != nil {
			return nil
		}
		b, _ := os.ReadFile(p)
		out = append(out, b...)
		return nil
	})
	return out
}

// TestEncryptedDBMasterKeyLifecycle covers the pool-key envelope end to end: a two-key
// DB persists a wrapped master, seals the configured attribute on disk, and either key
// alone reopens and decrypts it; a wrong key is refused.
func TestEncryptedDBMasterKeyLifecycle(t *testing.T) {
	dir := t.TempDir()
	pool := KEK{ID: "POOL", Material: []byte("pool-signing-key-material-abcdefghij")}
	token := KEK{ID: "TOKEN", Material: []byte("a-different-token-key-material-klmnop")}
	const secret = `"X509-proxy-BLOB-do-not-leak-7c1e"`

	cfg := func(keys ...KEK) Config {
		return Config{Dir: dir, PoolKeys: keys, EncryptedAttrs: []string{"ClaimId"}}
	}

	db, err := OpenConfig(cfg(pool, token))
	if err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	tx.NewClassAd("j1", mustAd(t, "Owner = \"alice\""))
	if err := tx.SetAttribute("j1", "ClaimId", secret); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The master-key envelope is on disk; the secret is not there in the clear.
	if _, err := os.Stat(filepath.Join(dir, masterKeysFile)); err != nil {
		t.Fatalf("masterkeys.json not written: %v", err)
	}
	if bytes.Contains(readAll(t, dir), []byte("do-not-leak")) {
		t.Fatal("secret ClaimId found in plaintext on disk")
	}

	// Either pool key alone reopens and decrypts.
	for _, k := range []KEK{pool, token} {
		db, err := OpenConfig(cfg(k))
		if err != nil {
			t.Fatalf("reopen with %s: %v", k.ID, err)
		}
		ad, ok := db.LookupClassAd("j1")
		if !ok {
			t.Fatalf("%s: j1 missing", k.ID)
		}
		if v, _ := ad.EvaluateAttrString("ClaimId"); v != "X509-proxy-BLOB-do-not-leak-7c1e" {
			t.Fatalf("%s: ClaimId = %q, want the decrypted secret", k.ID, v)
		}
		db.Close()
	}

	// A key that opens no wrapping row is refused (rather than silently running blind).
	wrong := KEK{ID: "OTHER", Material: []byte("unknown-key")}
	if _, err := OpenConfig(cfg(wrong)); err == nil {
		t.Fatal("opening with an unknown pool key should error")
	}
}

// TestEncryptedAttrsPersistAcrossReopen verifies a runtime SetEncryptedAttrs toggle is
// persisted (indexcfg.json) and reconciled on the next open, so the policy -- and thus
// which attributes are sealed on disk -- is stable across restarts (the HA follower
// convergence path, since config is not op-replicated).
func TestEncryptedAttrsPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	pool := KEK{ID: "POOL", Material: []byte("pool-key-material-for-persist-test-x")}
	cfg := Config{Dir: dir, PoolKeys: []KEK{pool}} // no EncryptedAttrs initially

	db, err := OpenConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetEncryptedAttrs([]string{"Region"}); err != nil {
		t.Fatal(err)
	}
	if got := db.EncryptedAttrNames(); len(got) != 1 || got[0] != "region" {
		t.Fatalf("EncryptedAttrNames = %v, want [region]", got)
	}
	tx := db.Begin()
	tx.NewClassAd("m", mustAd(t, "Region = \"secret-region-eu-west-42\""))
	tx.Commit()
	db.Close()

	// Reopen: the policy is reconciled from indexcfg.json even though cfg has no
	// EncryptedAttrs, and Region reads back decrypted while being sealed on disk.
	db, err = OpenConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := db.EncryptedAttrNames(); len(got) != 1 || got[0] != "region" {
		t.Fatalf("after reopen EncryptedAttrNames = %v, want [region]", got)
	}
	if bytes.Contains(readAll(t, dir), []byte("secret-region-eu-west-42")) {
		t.Fatal("Region value present in plaintext on disk after reopen")
	}
	ad, ok := db.LookupClassAd("m")
	if !ok {
		t.Fatal("m missing")
	}
	if v, _ := ad.EvaluateAttrString("Region"); v != "secret-region-eu-west-42" {
		t.Fatalf("Region = %q, want decrypted", v)
	}
	db.Close()
}

// TestEncryptedDBKeyRotation verifies a rotated-in pool key is added to the envelope on
// the next open, so it can subsequently open the DB alone.
func TestEncryptedDBKeyRotation(t *testing.T) {
	dir := t.TempDir()
	k1 := KEK{ID: "K1", Material: []byte("first-pool-key-material-1234567890ab")}
	k2 := KEK{ID: "K2", Material: []byte("second-pool-key-material-cdefghijkl")}

	// Create with only K1.
	db, err := OpenConfig(Config{Dir: dir, PoolKeys: []KEK{k1}, EncryptedAttrs: []string{"S"}})
	if err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	tx.NewClassAd("a", mustAd(t, "S = \"sec\""))
	tx.Commit()
	db.Close()

	// Reopen with BOTH keys: K2 gets a wrapping row added.
	db, err = OpenConfig(Config{Dir: dir, PoolKeys: []KEK{k1, k2}, EncryptedAttrs: []string{"S"}})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Now K2 alone can open the DB.
	db, err = OpenConfig(Config{Dir: dir, PoolKeys: []KEK{k2}, EncryptedAttrs: []string{"S"}})
	if err != nil {
		t.Fatalf("K2 alone should open after rotation: %v", err)
	}
	ad, ok := db.LookupClassAd("a")
	if !ok {
		t.Fatal("a missing")
	}
	if v, _ := ad.EvaluateAttrString("S"); v != "sec" {
		t.Fatalf("S = %q, want sec", v)
	}
	db.Close()
}
