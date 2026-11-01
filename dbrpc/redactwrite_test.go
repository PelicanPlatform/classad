package dbrpc

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// An unprivileged session's transaction reads sealed values as undefined. Its SetAttribute and
// DeleteAttribute that needed a whole record composed it from that read, so a WRITE-level peer
// deleting one unrelated attribute wiped the ad's ClaimId and every other sealed value. These drive
// the writes over a real connection and read the result back with the key, in process.

const writeSessionSecret = "payroll-4471-not-a-private-name"

// writeSessionPair serves an encrypted table to an unprivileged (WRITE, not DAEMON) session. compact
// leaves the ads in interned segments, where a sealed value cannot be carried without the key.
func writeSessionPair(t *testing.T, deltaMax int, compact bool) (*Client, *db.DB) {
	t.Helper()
	d, err := db.OpenConfig(db.Config{
		Dir:            t.TempDir(),
		PoolKeys:       []db.KEK{{ID: "POOL", Material: []byte("test-pool-key-material-32-bytes!!")}},
		EncryptedAttrs: []string{"Salary"},
		DeltaMax:       deltaMax,
		SegmentSize:    16 << 10,
	})
	if err != nil {
		t.Skipf("encrypted table unavailable here: %v", err)
	}
	tx := d.Begin()
	for _, k := range []string{"a", "b", "c", "d"} {
		ad, err := classad.ParseOld(fmt.Sprintf("Cpus = 4\nNote = \"note-%s\"\nClaimId = \"<claim-%s>\"\nSalary = %q",
			k, k, writeSessionSecret))
		if err != nil {
			t.Fatal(err)
		}
		tx.NewClassAd(k, ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if compact {
		// Seal the ads under incompressible fill, then delete most of it: compaction rewrites the
		// mostly dead segments, interned.
		rng := rand.New(rand.NewSource(1))
		tx := d.Begin()
		for i := 0; i < 4000; i++ {
			buf := make([]byte, 400)
			rng.Read(buf)
			ad, _ := classad.ParseOld(fmt.Sprintf("Note = \"%x\"", buf))
			tx.NewClassAd(fmt.Sprintf("fill-%d", i), ad)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		tx = d.Begin()
		for i := 0; i < 4000; i++ {
			if i%10 != 0 {
				tx.DestroyClassAd(fmt.Sprintf("fill-%d", i))
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if d.Compact() == 0 {
			t.Fatal("compaction rewrote nothing")
		}
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{}) }() // WRITE: read-write, no private attributes
	c := NewClient(cconn)
	t.Cleanup(func() { c.Close(); s.Close(); d.Close() })
	return c, d
}

// sessionWrite runs ops in one transaction of the session and commits it.
func sessionWrite(t *testing.T, c *Client, ops func(ctx context.Context, tx *Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ops(ctx, tx); err != nil {
		t.Fatal(err)
	}
	return tx.Commit(ctx)
}

// storedAdText is key's ad read with the key, private attributes included.
func storedAdText(t *testing.T, d *db.DB, key string) string {
	t.Helper()
	ad, ok := d.LookupClassAd(key)
	if !ok {
		t.Fatalf("%s is missing", key)
	}
	return ad.StringWithPrivate()
}

func TestWriteSessionPreservesSealed(t *testing.T) {
	c, d := writeSessionPair(t, 2, false)
	// The reproduction: removing an unrelated attribute.
	if err := sessionWrite(t, c, func(ctx context.Context, tx *Tx) error {
		return tx.DeleteAttribute(ctx, "a", "Note")
	}); err != nil {
		t.Fatal(err)
	}
	// The chain bound: more sets to one key than DeltaMax, one transaction each.
	for k := 0; k < 5; k++ {
		if err := sessionWrite(t, c, func(ctx context.Context, tx *Tx) error {
			return tx.SetAttribute(ctx, "b", "Cpus", fmt.Sprint(10+k))
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A plain set (a delta), and a first write.
	if err := sessionWrite(t, c, func(ctx context.Context, tx *Tx) error {
		if err := tx.SetAttribute(ctx, "c", "Note", `"edited"`); err != nil {
			return err
		}
		return tx.SetAttribute(ctx, "new", "ClaimId", `"<claim-new>"`)
	}); err != nil {
		t.Fatal(err)
	}
	// Named writes to sealed attributes the session cannot read are applied.
	if err := sessionWrite(t, c, func(ctx context.Context, tx *Tx) error {
		if err := tx.SetAttribute(ctx, "d", "Salary", "1"); err != nil {
			return err
		}
		return tx.DeleteAttribute(ctx, "d", "ClaimId")
	}); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"a":   `[ClaimId = "<claim-a>"; Cpus = 4; Salary = "` + writeSessionSecret + `"]`,
		"b":   `[ClaimId = "<claim-b>"; Cpus = 14; Note = "note-b"; Salary = "` + writeSessionSecret + `"]`,
		"c":   `[ClaimId = "<claim-c>"; Cpus = 4; Note = "edited"; Salary = "` + writeSessionSecret + `"]`,
		"d":   `[Cpus = 4; Note = "note-d"; Salary = 1]`,
		"new": `[ClaimId = "<claim-new>"]`,
	}
	for k, w := range want {
		if got := storedAdText(t, d, k); got != w {
			t.Errorf("%s:\n got  %s\n want %s", k, got, w)
		}
	}
}

// TestWriteSessionRefusalLeavesAdIntact: where a whole record would have to carry a sealed value from
// an interned record, the commit fails with the reason and the ad is exactly as it was.
func TestWriteSessionRefusalLeavesAdIntact(t *testing.T) {
	c, d := writeSessionPair(t, 2, true)
	before := storedAdText(t, d, "a")
	err := sessionWrite(t, c, func(ctx context.Context, tx *Tx) error {
		return tx.DeleteAttribute(ctx, "a", "Note")
	})
	if err == nil || !strings.Contains(err.Error(), "sealed-needs-key") {
		t.Fatalf("commit = %v, want a refusal naming sealed-needs-key", err)
	}
	if after := storedAdText(t, d, "a"); after != before {
		t.Errorf("refused write changed the ad:\n before %s\n after  %s", before, after)
	}
	// A set on the same record is stored as a delta past the bound, not refused.
	for k := 0; k < 5; k++ {
		if err := sessionWrite(t, c, func(ctx context.Context, tx *Tx) error {
			return tx.SetAttribute(ctx, "a", "Cpus", fmt.Sprint(20+k))
		}); err != nil {
			t.Fatalf("set %d: %v", k, err)
		}
	}
	if got, want := storedAdText(t, d, "a"), strings.Replace(before, "Cpus = 4", "Cpus = 24", 1); got != want {
		t.Errorf("after sets:\n got  %s\n want %s", got, want)
	}
}
