package collections

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/collections/crypt"
)

// deriveDataKey returns a fresh master and its DataInfo subkey (the DB data key).
func deriveDataKey(t *testing.T) (master, dataKey []byte) {
	t.Helper()
	master, err := crypt.NewMaster()
	if err != nil {
		t.Fatal(err)
	}
	dataKey, err = crypt.Subkey(master, crypt.DataInfo)
	if err != nil {
		t.Fatal(err)
	}
	return master, dataKey
}

// segBytes concatenates the raw bytes of every seg-*.dat file under a persistent
// collection's directory -- the on-disk image, used to assert a secret never lands
// there in the clear.
func segBytes(t *testing.T, dir string) []byte {
	t.Helper()
	var out []byte
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		var num uint64
		var dictID uint32
		if _, e := fmt.Sscanf(d.Name(), "seg-%d.d%d.dat", &num, &dictID); e != nil {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		out = append(out, b...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestEncryptAtRestPersist is the end-to-end persistence test: a secret attribute is
// unreadable on disk, decrypts on read, survives Close/reopen and compaction, and a
// plaintext (unencrypted) attribute in the same ad still indexes and queries.
func TestEncryptAtRestPersist(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	_, dataKey := deriveDataKey(t)
	const secret = "ClaimId-super-secret-capability-9f83a"

	open := func() *Collection {
		c, err := Open(Options{
			Shards:           1,
			Dir:              dir,
			DataKey:          dataKey,
			EncryptedAttrs:   []string{"ClaimId"},
			CategoricalAttrs: []string{"Owner"}, // plaintext attr still indexes
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	c := open()
	const n = 50
	for i := 0; i < n; i++ {
		ad := mustAd(t, fmt.Sprintf(`[Owner="alice"; Cpus=%d; ClaimId=%q]`, i, secret))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}

	// The secret must not be on disk in the clear.
	if bytes.Contains(segBytes(t, dir), []byte(secret)) {
		t.Fatal("secret ClaimId found in plaintext on disk")
	}

	// A read decrypts it.
	got, ok := c.Get([]byte("k0"))
	if !ok {
		t.Fatal("k0 missing")
	}
	if v, _ := got.EvaluateAttrString("ClaimId"); v != secret {
		t.Fatalf("ClaimId = %q, want the decrypted secret", v)
	}
	if v, _ := got.EvaluateAttrString("Owner"); v != "alice" {
		t.Fatalf("Owner = %q, want alice", v)
	}

	// The plaintext attribute still indexes: a query on Owner finds all n.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen with the same key: still decryptable, and compaction (which moves records
	// between segments) preserves decryptability since ciphertext is DB-key portable.
	c = open()
	c.Compact()
	got, ok = c.Get([]byte("k0"))
	if !ok {
		t.Fatal("k0 missing after reopen+compact")
	}
	if v, _ := got.EvaluateAttrString("ClaimId"); v != secret {
		t.Fatalf("after reopen+compact ClaimId = %q, want the secret", v)
	}
	if c.Len() != n {
		t.Fatalf("Len = %d, want %d", c.Len(), n)
	}
	c.Close()

	// Reopen with a WRONG key: the encrypted attribute must fail to decrypt (a scan
	// that decodes bodies errors on it) -- the secret is not recoverable without the
	// right pool-derived data key.
	_, wrongKey := deriveDataKey(t)
	bad, err := Open(Options{Shards: 1, Dir: dir, DataKey: wrongKey, EncryptedAttrs: []string{"ClaimId"}})
	if err != nil {
		t.Fatal(err)
	}
	// The body fails to decrypt under the wrong key, so either the ad does not
	// materialize or its ClaimId is not the secret; the secret is never recovered.
	if ad, ok := bad.Get([]byte("k0")); ok {
		if v, _ := ad.EvaluateAttrString("ClaimId"); v == secret {
			t.Fatal("wrong key recovered the secret")
		}
	}
	bad.Close()
}

// TestEncryptedIndexedOverlapPanics locks the invariant that an encrypted attribute
// cannot also be indexed (its value is opaque at rest).
func TestEncryptedIndexedOverlapPanics(t *testing.T) {
	_, dataKey := deriveDataKey(t)
	defer func() {
		if recover() == nil {
			t.Fatal("New should panic when an attribute is both encrypted and indexed")
		}
	}()
	New(Options{
		DataKey:          dataKey,
		EncryptedAttrs:   []string{"Secret"},
		CategoricalAttrs: []string{"Secret"},
	})
}
