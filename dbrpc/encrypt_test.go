package dbrpc

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// encServerPair wires a client to a server backed by an encryption-enabled in-memory DB,
// served with opts. An in-memory DB with pool keys mints an ephemeral master, so
// encryption is active without touching disk.
func encServerPair(t *testing.T, opts ServeOptions) (*Client, *db.DB, func()) {
	t.Helper()
	d, err := db.OpenConfig(db.Config{PoolKeys: []db.KEK{{ID: "POOL", Material: []byte("dbrpc-test-pool-key-material-123456")}}})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, opts) }()
	c := NewClient(cconn)
	return c, d, func() { c.Close(); s.Close(); d.Close() }
}

// TestEncryptSetRequiresDaemon verifies the encryption toggle is DAEMON-only: a
// read-write-but-unprivileged connection is refused, while a privileged one succeeds
// and the change is visible through diagnostics.
func TestEncryptSetRequiresDaemon(t *testing.T) {
	// Unprivileged (WRITE-level: not read-only, but not Privileged) -> refused.
	c, _, cleanup := encServerPair(t, ServeOptions{})
	if _, err := c.SetEncryptedAttrs(context.Background(), "ads", "Region"); err == nil {
		t.Fatal("encrypt.set should be refused without DAEMON privilege")
	}
	cleanup()

	// Privileged -> accepted, and reflected in diagnostics.
	c, _, cleanup = encServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	if _, err := c.SetEncryptedAttrs(context.Background(), "ads", "Region", "Zone"); err != nil {
		t.Fatalf("privileged encrypt.set: %v", err)
	}
	diag, err := c.Diagnostics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !diag.EncryptionEnabled {
		t.Error("diagnostics should report encryption enabled")
	}
	if len(diag.EncryptedAttrs) != 2 || diag.EncryptedAttrs[0] != "region" || diag.EncryptedAttrs[1] != "zone" {
		t.Errorf("EncryptedAttrs = %v, want [region zone]", diag.EncryptedAttrs)
	}
}

// TestTruncateRequiresDaemon verifies truncate is DAEMON-only and, when authorized,
// empties the table.
func TestTruncateRequiresDaemon(t *testing.T) {
	// Unprivileged -> refused, data intact.
	c, d, cleanup := encServerPair(t, ServeOptions{})
	tx := d.Begin()
	tx.NewClassAd("a", mustAd(t, "N = 1"))
	tx.Commit()
	if _, err := c.TruncateTable(context.Background(), "ads"); err == nil {
		t.Fatal("truncate should be refused without DAEMON privilege")
	}
	if d.Len() != 1 {
		t.Fatalf("unauthorized truncate changed the data: Len = %d", d.Len())
	}
	cleanup()

	// Privileged -> empties the table.
	c, d, cleanup = encServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	tx = d.Begin()
	tx.NewClassAd("a", mustAd(t, "N = 1"))
	tx.NewClassAd("b", mustAd(t, "N = 2"))
	tx.Commit()
	if _, err := c.TruncateTable(context.Background(), "ads"); err != nil {
		t.Fatalf("privileged truncate: %v", err)
	}
	if d.Len() != 0 {
		t.Fatalf("after truncate Len = %d, want 0", d.Len())
	}
}

// TestSnapshotRestoreOverRPC round-trips a backup through the client API and confirms
// both ops are DAEMON-gated.
func TestSnapshotRestoreOverRPC(t *testing.T) {
	// DAEMON connection: snapshot, wipe, restore.
	c, d, cleanup := encServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	tx := d.Begin()
	tx.NewClassAd("a", mustAd(t, "Owner = \"x\"\nClaimId = \"top-secret-rpc\""))
	tx.NewClassAd("b", mustAd(t, "Owner = \"y\""))
	tx.Commit()

	var snap bytes.Buffer
	if err := c.Snapshot(context.Background(), &snap); err != nil {
		t.Fatalf("Snapshot over RPC: %v", err)
	}
	if bytes.Contains(snap.Bytes(), []byte("top-secret-rpc")) {
		t.Fatal("snapshot bytes leaked a private attribute over the wire")
	}
	if _, err := c.TruncateTable(context.Background(), "ads"); err != nil {
		t.Fatal(err)
	}
	if d.Len() != 0 {
		t.Fatal("truncate did not empty the table")
	}
	if err := c.Restore(context.Background(), bytes.NewReader(snap.Bytes())); err != nil {
		t.Fatalf("Restore over RPC: %v", err)
	}
	if d.Len() != 2 {
		t.Fatalf("after restore Len = %d, want 2", d.Len())
	}
	ad, ok := d.LookupClassAd("a")
	if !ok {
		t.Fatal("a missing after restore")
	}
	if v, _ := ad.EvaluateAttrString("ClaimId"); v != "top-secret-rpc" {
		t.Fatalf("restored ClaimId = %q", v)
	}

	// Unprivileged connection: both refused.
	c2, _, cleanup2 := encServerPair(t, ServeOptions{})
	defer cleanup2()
	if err := c2.Snapshot(context.Background(), &bytes.Buffer{}); err == nil {
		t.Error("snapshot should be refused without DAEMON privilege")
	}
	if err := c2.Restore(context.Background(), bytes.NewReader(snap.Bytes())); err == nil {
		t.Error("restore should be refused without DAEMON privilege")
	}
}

// TestSnapshotRestoreStreamingLarge round-trips a dataset large enough to span many
// 64 KiB stream chunks, exercising the chunked snapshot (out) and restore (in) paths.
func TestSnapshotRestoreStreamingLarge(t *testing.T) {
	c, d, cleanup := encServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	const n = 8000
	tx := d.Begin()
	for i := 0; i < n; i++ {
		tx.NewClassAd(fmt.Sprintf("k%d", i), mustAd(t, fmt.Sprintf("Owner = \"user%d\"\nCpus = %d\nClaimId = \"sekret-%d\"", i%16, i%64, i)))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var snap bytes.Buffer
	if err := c.Snapshot(context.Background(), &snap); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Len() < 64*1024 {
		t.Fatalf("snapshot is %d bytes -- expected multiple chunks (>64 KiB)", snap.Len())
	}
	if _, err := c.TruncateTable(context.Background(), "ads"); err != nil {
		t.Fatal(err)
	}
	if err := c.Restore(context.Background(), bytes.NewReader(snap.Bytes())); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if d.Len() != n {
		t.Fatalf("after restore Len = %d, want %d", d.Len(), n)
	}
	// Spot-check a few ads survived intact, including the private attribute.
	for _, i := range []int{0, 1234, n - 1} {
		ad, ok := d.LookupClassAd(fmt.Sprintf("k%d", i))
		if !ok {
			t.Fatalf("k%d missing", i)
		}
		if v, _ := ad.EvaluateAttrString("ClaimId"); v != fmt.Sprintf("sekret-%d", i) {
			t.Fatalf("k%d ClaimId = %q", i, v)
		}
	}
}

// TestEncryptSetReadOnlyRejected confirms the toggle is also refused on a read-only
// connection (it is a mutating admin op), independent of the DAEMON check.
func TestEncryptSetReadOnlyRejected(t *testing.T) {
	c, _, cleanup := encServerPair(t, ServeOptions{ReadOnly: true, Privileged: true})
	defer cleanup()
	if _, err := c.SetEncryptedAttrs(context.Background(), "ads", "Region"); err == nil {
		t.Fatal("encrypt.set should be refused on a read-only connection")
	}
}
