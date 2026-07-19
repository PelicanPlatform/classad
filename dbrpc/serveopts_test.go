package dbrpc

import (
	"context"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// mustAd parses old-ClassAd text (newline-separated) or fails the test.
func mustAd(t *testing.T, text string) *classad.ClassAd {
	t.Helper()
	ad, err := classad.ParseOld(text)
	if err != nil {
		t.Fatalf("parsing ad %q: %v", text, err)
	}
	return ad
}

// serveOptsPair wires a client to a server connection served with opts.
func serveOptsPair(t *testing.T, opts ServeOptions) (*Client, func()) {
	t.Helper()
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, opts) }()
	c := NewClient(cconn)
	return c, func() { c.Close(); s.Close(); d.Close() }
}

// seed writes one ad with a private attribute over a full-access server.
func seedPrivate(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	ad := mustAd(t, "Cpus = 4\nCapability = \"secret-claim-id\"")
	tx.NewClassAd("s1", ad)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestServeReadOnlyRejectsMutation confirms a read-only connection refuses writes
// but still serves reads and snapshots.
func TestServeReadOnlyRejectsMutation(t *testing.T) {
	c, cleanup := serveOptsPair(t, ServeOptions{ReadOnly: true})
	defer cleanup()

	tx, err := c.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin on a read-only conn should succeed (read snapshot): %v", err)
	}
	if err := tx.NewClassAd(context.Background(), "x", "N = 1"); err == nil {
		t.Fatal("NewClassAd on a read-only connection should be rejected")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("rejection error = %v, want a read-only message", err)
	}
	if err := tx.SetAttribute(context.Background(), "x", "N", "2"); err == nil {
		t.Fatal("SetAttribute on a read-only connection should be rejected")
	}
	_ = tx.Abort(context.Background())
}

// TestServeStripsPrivate confirms private attributes are stripped by default and
// only surface when the connection opts into IncludePrivate.
func TestServeStripsPrivate(t *testing.T) {
	// Default (stripped): serve a DB that already holds a private attribute.
	d := seedPrivate(t)
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{ReadOnly: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()

	rows, err := c.Query(context.Background(), "Cpus == 4")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("Query returned %d rows, want 1", len(rows))
	}
	if strings.Contains(rows[0], "secret-claim-id") || strings.Contains(rows[0], "Capability") {
		t.Fatalf("private attribute leaked to a stripped connection: %q", rows[0])
	}

	// Privileged connection to the same DB sees the secret.
	pconn, psconn := netPipe()
	go func() { _ = s.ServeConnOpts(psconn, ServeOptions{IncludePrivate: true}) }()
	pc := NewClient(pconn)
	defer pc.Close()
	prows, err := pc.Query(context.Background(), "Cpus == 4")
	if err != nil {
		t.Fatal(err)
	}
	if len(prows) != 1 || !strings.Contains(prows[0], "secret-claim-id") {
		t.Fatalf("privileged Query = %v, want the Capability secret present", prows)
	}
}
