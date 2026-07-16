package dbrpc

import (
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
	if _, err := c.SetEncryptedAttrs("ads", "Region"); err == nil {
		t.Fatal("encrypt.set should be refused without DAEMON privilege")
	}
	cleanup()

	// Privileged -> accepted, and reflected in diagnostics.
	c, _, cleanup = encServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	if _, err := c.SetEncryptedAttrs("ads", "Region", "Zone"); err != nil {
		t.Fatalf("privileged encrypt.set: %v", err)
	}
	diag, err := c.Diagnostics()
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

// TestEncryptSetReadOnlyRejected confirms the toggle is also refused on a read-only
// connection (it is a mutating admin op), independent of the DAEMON check.
func TestEncryptSetReadOnlyRejected(t *testing.T) {
	c, _, cleanup := encServerPair(t, ServeOptions{ReadOnly: true, Privileged: true})
	defer cleanup()
	if _, err := c.SetEncryptedAttrs("ads", "Region"); err == nil {
		t.Fatal("encrypt.set should be refused on a read-only connection")
	}
}
