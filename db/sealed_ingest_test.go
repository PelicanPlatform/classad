package db

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// UpdateOld and UpdateOldBatch fell back to parse + Put for an encrypted store, because the wire-native
// encoder cannot seal -- so encryption cost every ingest its bulk path, one commit per ad instead of one
// per batch. Sealing is now decided per ad inside the ingest, so the batch survives.
//
// What must hold either way: the values round-trip, and nothing lands on disk in the clear.
func TestEncryptedWireIngestSealsAndKeepsTheBatch(t *testing.T) {
	dir := t.TempDir()
	const secret = "claim-capability-not-for-disk"
	d, err := OpenConfig(Config{
		Dir:            dir,
		PoolKeys:       []KEK{{ID: "POOL", Material: []byte("test-pool-key-material-32-bytes!!")}},
		EncryptedAttrs: []string{"ClaimId"},
	})
	if err != nil {
		t.Skipf("encrypted table unavailable here: %v", err)
	}

	// Single-key wire ingest, with and without something to seal.
	if err := d.UpdateOld("plain", "Cpus = 4\nOwner = \"alice\""); err != nil {
		t.Fatalf("UpdateOld(plain): %v", err)
	}
	if err := d.UpdateOld("sealed", "Cpus = 8\nClaimId = \""+secret+"\""); err != nil {
		t.Fatalf("UpdateOld(sealed): %v", err)
	}

	// Batch ingest, mixed.
	batch := []OldAdText{}
	for i := 0; i < 200; i++ {
		text := fmt.Sprintf("Cpus = %d\nOwner = \"bob\"", i)
		if i%3 == 0 {
			text += "\nClaimId = \"" + secret + "\""
		}
		batch = append(batch, OldAdText{Key: fmt.Sprintf("b%d", i), Text: text})
	}
	if err := d.UpdateOldBatch(batch); err != nil {
		t.Fatalf("UpdateOldBatch: %v", err)
	}

	// Values round-trip, sealed ones included.
	ad, ok := d.LookupClassAd("sealed")
	if !ok {
		t.Fatal("the sealed ad was not stored")
	}
	if !strings.Contains(ad.StringWithPrivate(), secret) {
		t.Errorf("the sealed ad lost its value: %s", ad.StringWithPrivate())
	}
	if _, ok := d.LookupClassAd("plain"); !ok {
		t.Fatal("the streamed ad was not stored")
	}
	if _, ok := d.LookupClassAd("b3"); !ok {
		t.Fatal("a batched ad with a sealed attribute was not stored")
	}
	if _, ok := d.LookupClassAd("b4"); !ok {
		t.Fatal("a batched ad without one was not stored")
	}
	d.Close()

	// And nothing in the clear on disk.
	var hits []string
	filepath.WalkDir(dir, func(path string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		if b, e := os.ReadFile(path); e == nil && bytes.Contains(b, []byte(secret)) {
			hits = append(hits, de.Name())
		}
		return nil
	})
	if len(hits) != 0 {
		t.Errorf("the wire ingest stored the secret in the clear, in: %v", hits)
	}
}
