package db

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// Private attributes are sealed whatever the deployment configured. Before this, sealing required
// PoolKeys, so the default deployment -- no pool keys -- wrote every ClaimId to disk in the clear, and
// the only thing standing between a read-only client and a claim capability was a filter.
//
// Pool keys now decide one thing only: whether the MASTER is protected. Without them the master is
// stored beside the data in the clear, which protects nothing against someone holding the disk -- so
// EncryptionEnabled reports false, and nothing claims otherwise.

const forcedSecret = "ClaimId-capability-must-not-be-on-disk"

func TestPrivateAttrsSealedWithoutPoolKeys(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenConfig(Config{Dir: dir}) // no PoolKeys, no EncryptedAttrs: the default deployment
	if err != nil {
		t.Fatal(err)
	}
	ad, err := classad.ParseOld("Cpus = 4\nOwner = \"alice\"\nClaimId = \"" + forcedSecret + "\"")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Put("k1", ad); err != nil {
		t.Fatal(err)
	}

	// It must not claim at-rest protection it does not have.
	if d.EncryptionEnabled() {
		t.Error("EncryptionEnabled reported true with no pool keys; the master is in the clear, so " +
			"nothing here is protected from someone holding the disk")
	}
	// The value still reads back: sealing is transparent to an entitled reader.
	got, ok := d.LookupClassAd("k1")
	if !ok {
		t.Fatal("the ad was not stored")
	}
	if v, err := got.EvaluateAttr("ClaimId").StringValue(); err != nil || v != forcedSecret {
		t.Errorf("ClaimId read back as (%q, %v), want %q", v, err, forcedSecret)
	}
	d.Close()

	// And it is not on disk in the clear -- the point of the exercise.
	var hits []string
	filepath.WalkDir(dir, func(path string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		if b, e := os.ReadFile(path); e == nil && bytes.Contains(b, []byte(forcedSecret)) {
			hits = append(hits, de.Name())
		}
		return nil
	})
	if len(hits) != 0 {
		t.Errorf("a private attribute was written to disk in the clear, in: %v", hits)
	}
}

func TestUnprotectedMasterPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenConfig(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ad, err := classad.ParseOld("Cpus = 4\nClaimId = \"" + forcedSecret + "\"")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Put("k1", ad); err != nil {
		t.Fatal(err)
	}
	d.Close()

	// The key file is what makes a reopen able to read its own data. It is deliberately in the clear,
	// so it must at least be owner-only.
	info, err := os.Stat(filepath.Join(dir, unprotectedMasterFile))
	if err != nil {
		t.Fatalf("no %s after a keyless open: a reopen could not read its own sealed values: %v",
			unprotectedMasterFile, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s mode is %o, want 600", unprotectedMasterFile, perm)
	}

	d2, err := OpenConfig(Config{Dir: dir})
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer d2.Close()
	got, ok := d2.LookupClassAd("k1")
	if !ok {
		t.Fatal("the ad vanished across a reopen")
	}
	if v, err := got.EvaluateAttr("ClaimId").StringValue(); err != nil || v != forcedSecret {
		t.Errorf("after reopen ClaimId read back as (%q, %v), want %q -- a fresh master would have made "+
			"every sealed value unreadable", v, err, forcedSecret)
	}
}

func TestPoolKeysStillMeanAtRest(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenConfig(Config{
		Dir:      dir,
		PoolKeys: []KEK{{ID: "POOL", Material: []byte("test-pool-key-material-32-bytes!!")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if !d.EncryptionEnabled() {
		t.Error("EncryptionEnabled reported false with pool keys present")
	}
	// The protected form must not leave an unprotected master lying about.
	if _, err := os.Stat(filepath.Join(dir, unprotectedMasterFile)); err == nil {
		t.Errorf("%s exists for a pool-key database; the master is wrapped, so a cleartext copy would "+
			"undo the protection", unprotectedMasterFile)
	}
	if _, err := os.Stat(filepath.Join(dir, masterKeysFile)); err != nil {
		t.Errorf("no %s for a pool-key database: %v", masterKeysFile, err)
	}
}

// TestSnapshotUnprotectedWithoutPoolKeys pins the compatibility decision. Snapshot protection follows
// pool keys, not the presence of a data key: sealing frames under a key whose envelope no pool key can
// open would make the snapshot unrestorable.
func TestSnapshotUnprotectedWithoutPoolKeys(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenConfig(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		ad, err := classad.ParseOld(fmt.Sprintf("Cpus = %d\nOwner = \"alice\"", i))
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Put(fmt.Sprintf("k%d", i), ad); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := d.Snapshot(&buf); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	d.Close()

	// It must restore into a fresh database with no keys at all.
	dir2 := t.TempDir()
	d2, err := OpenConfig(Config{Dir: dir2})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if err := d2.Restore(&buf); err != nil {
		t.Fatalf("Restore of an unprotected snapshot failed: %v", err)
	}
	n := 0
	seq, err := d2.Query("Cpus >= 0")
	if err != nil {
		t.Fatal(err)
	}
	for range seq {
		n++
	}
	if n != 20 {
		t.Errorf("restored %d ads, want 20", n)
	}
	_ = strings.TrimSpace
}
