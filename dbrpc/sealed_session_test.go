package dbrpc

import (
	"context"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// The RPC layer used to call the ENTITLED read for every session and rely on the serializer to drop
// private attributes -- so an unprivileged session's secret was decrypted in this process on every read.
// These check the wiring: an unprivileged session reads with no key at all.
//
// The observable difference is a NON-private encrypted attribute. The serializer strips by private NAME,
// so a deployment encrypting `Salary` at rest served its plaintext to any read-only session. With the
// keyless read it arrives undefined. That is a behaviour change and the point of the exercise.
const sessionSecret = "payroll-9f83a-not-a-private-name"

func encryptedPair(t *testing.T, readOnly bool) (*Client, func()) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.OpenConfig(db.Config{
		Dir:            dir,
		PoolKeys:       []db.KEK{{ID: "POOL", Material: []byte("test-pool-key-material-32-bytes!!")}},
		EncryptedAttrs: []string{"Salary"},
	})
	if err != nil {
		t.Skipf("encrypted table unavailable here: %v", err)
	}
	tx := d.Begin()
	for i := 0; i < 5; i++ {
		// NewClassAd, not NewClassAdOld: the streaming old-text encoder does not seal, so on an
		// encrypted table NewClassAdOld returns false and stores NOTHING. Ignoring that return is how
		// the first version of this test ended up asserting against an empty table.
		ad, err := classad.ParseOld("Cpus = 4\nSalary = \"" + sessionSecret + "\"")
		if err != nil {
			t.Fatal(err)
		}
		tx.NewClassAd(string(rune('a'+i)), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	opts := ServeOptions{ReadOnly: true}
	if !readOnly {
		opts = ServeOptions{IncludePrivate: true}
	}
	go func() { _ = s.ServeConnOpts(sconn, opts) }()
	c := NewClient(cconn)
	return c, func() { c.Close(); s.Close(); d.Close() }
}

func TestUnprivilegedSessionReadsWithNoKey(t *testing.T) {
	ctx := context.Background()

	// Premise: the privileged session does see the value, so withholding means something.
	pc, pclean := encryptedPair(t, false)
	defer pclean()
	prows, err := pc.Query(ctx, "Cpus == 4")
	if err != nil {
		t.Fatal(err)
	}
	if len(prows) == 0 {
		t.Fatal("privileged session read no ads")
	}
	sawPriv := false
	for _, r := range prows {
		if strings.Contains(r, sessionSecret) {
			sawPriv = true
		}
	}
	if !sawPriv {
		t.Fatalf("privileged session did not see the encrypted value; premise fails: %q", prows[0])
	}

	// The unprivileged session must not get it -- and must still get the ads.
	c, clean := encryptedPair(t, true)
	defer clean()
	rows, err := c.Query(ctx, "Cpus == 4")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(prows) {
		t.Errorf("unprivileged session read %d ads, privileged %d: redaction drops values, not ads",
			len(rows), len(prows))
	}
	for _, r := range rows {
		if strings.Contains(r, sessionSecret) {
			t.Fatalf("an unprivileged session read an encrypted value: %q", r)
		}
	}
	t.Logf("privileged %d ads (value visible), unprivileged %d ads (value withheld)", len(prows), len(rows))
}
