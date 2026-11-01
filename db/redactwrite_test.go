package db

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A transaction not entitled to sealed values (BeginRedacted -- what an unprivileged dbrpc session
// gets) reads them as undefined. SetAttribute and DeleteAttribute that needed a whole record composed
// it from that read and wrote undefined over every sealed value in the ad: one DeleteAttribute of an
// unrelated attribute destroyed the job's ClaimId. These run each such trigger through the db API and
// require a privileged read to show only the named attributes changed.

const redactedSalary = "salary-77231"

// redactedFixture opens an encrypted table holding six ads, each with a private attribute (ClaimId,
// sealed whatever the policy) and an explicitly sealed one (Salary). compact rewrites them into
// interned segments, the form a compaction leaves.
func redactedFixture(t *testing.T, deltaMax int, compact bool) (*DB, map[string]*classad.ClassAd) {
	t.Helper()
	d, err := OpenConfig(Config{
		Dir:            t.TempDir(),
		PoolKeys:       []KEK{{ID: "POOL", Material: []byte("test-pool-key-material-32-bytes!!")}},
		EncryptedAttrs: []string{"Salary"},
		DeltaMax:       deltaMax,
		SegmentSize:    16 << 10, // small, so the fixture's records seal and compaction rewrites them
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	model := map[string]*classad.ClassAd{}
	tx := d.Begin()
	for _, k := range []string{"a", "b", "c", "d", "e", "f"} {
		src := fmt.Sprintf("Cpus = 4\nNote = \"note-%s\"\nClaimId = \"<claim-%s>\"\nSalary = %q", k, k, redactedSalary)
		ad, err := classad.ParseOld(src)
		if err != nil {
			t.Fatal(err)
		}
		tx.NewClassAd(k, ad)
		model[k], _ = classad.ParseOld(src)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if compact {
		// Fill past the active segment so the six ads sit in sealed segments, then delete most of the
		// fill so those segments are mostly dead but not wholly: compaction rewrites them, interned.
		// It needs 64 KiB per shard, so the fill is incompressible.
		rng := rand.New(rand.NewSource(1))
		tx := d.Begin()
		for i := 0; i < 4000; i++ {
			buf := make([]byte, 400)
			rng.Read(buf)
			ad, _ := classad.ParseOld(fmt.Sprintf("Cpus = %d\nNote = \"%x\"", i, buf))
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
	return d, model
}

// redactedCommit runs ops in one redacted transaction and reports whether it was refused for a sealed
// value only the key could carry. Any other failure fails the test.
func redactedCommit(t *testing.T, d *DB, ops func(tx *Txn)) bool {
	t.Helper()
	tx := d.BeginRedacted()
	ops(tx)
	err := tx.Commit()
	if err == nil {
		return false
	}
	var ue *UnappliedError
	if !errors.As(err, &ue) || len(ue.Reasons) == 0 || ue.Reasons[0] != "sealed-needs-key" {
		t.Fatalf("commit: %v", err)
	}
	return true
}

func checkRedactedModel(t *testing.T, d *DB, model map[string]*classad.ClassAd) {
	t.Helper()
	for k, want := range model {
		got, ok := d.LookupClassAd(k)
		if !ok {
			t.Errorf("%s is missing", k)
			continue
		}
		if g, w := got.StringWithPrivate(), want.StringWithPrivate(); g != w {
			t.Errorf("%s:\n got  %s\n want %s", k, g, w)
		}
	}
}

// hasAttr reports whether key still has attribute name, read with the key.
func hasAttr(t *testing.T, d *DB, key, name string) bool {
	t.Helper()
	ad, ok := d.LookupClassAd(key)
	if !ok {
		t.Fatalf("%s is missing", key)
	}
	_, has := ad.Lookup(name)
	return has
}

func setModel(t *testing.T, model map[string]*classad.ClassAd, key, name, expr string) {
	t.Helper()
	ad := model[key]
	if ad == nil {
		ad = classad.New()
		model[key] = ad
	}
	e, err := classad.ParseExpr(expr)
	if err != nil {
		t.Fatal(err)
	}
	ad.InsertExpr(name, e)
}

func TestRedactedTxnPreservesSealed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deltaMax int
		compact  bool
	}{
		{"inline", 2, false},
		{"inline-deltas-off", DeltaMaxOff, false},
		{"compacted", 2, true},
		{"compacted-deltas-off", DeltaMaxOff, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, model := redactedFixture(t, tc.deltaMax, tc.compact)
			// A compacted record's sealed values are keyed by its segment's dictionary and cannot be
			// carried into a new record without the key, so a write that must compose one is refused --
			// never applied with the values lost. Everywhere else every write applies.
			mayRefuse := tc.compact

			// Removal of an unrelated attribute: the reproduction.
			switch refused := redactedCommit(t, d, func(tx *Txn) { tx.DeleteAttribute("a", "Note") }); {
			case !refused:
				model["a"].Delete("Note")
			case !mayRefuse:
				t.Error("removal refused on an inline record")
			}
			if kept := hasAttr(t, d, "a", "Note"); kept != mayRefuse {
				t.Fatalf("compacted=%v but Note kept=%v: the fixture is not the layout it names", tc.compact, kept)
			}
			// Named writes to sealed attributes the session cannot read: applied, as any other write is.
			// They name every sealed value the record holds, so nothing needs carrying, even interned.
			if redactedCommit(t, d, func(tx *Txn) {
				_ = tx.SetAttribute("d", "Salary", "1")
				tx.DeleteAttribute("d", "ClaimId")
			}) {
				t.Fatal("named sealed writes refused")
			}
			setModel(t, model, "d", "Salary", "1")
			model["d"].Delete("ClaimId")
			// The chain bound, one transaction per set so the tracker reaches it.
			for k := 0; k < 6; k++ {
				expr := fmt.Sprint(10 + k)
				if redactedCommit(t, d, func(tx *Txn) { _ = tx.SetAttribute("b", "Cpus", expr) }) {
					if tc.deltaMax > 0 || !mayRefuse {
						t.Fatalf("set %d refused (deltas %d)", k, tc.deltaMax)
					}
					continue
				}
				setModel(t, model, "b", "Cpus", expr)
			}
			// A first write to a key, sealed attribute included.
			if redactedCommit(t, d, func(tx *Txn) {
				_ = tx.SetAttribute("new", "Cpus", "1")
				_ = tx.SetAttribute("new", "ClaimId", `"<claim-new>"`)
			}) {
				t.Fatal("first write refused")
			}
			setModel(t, model, "new", "Cpus", "1")
			setModel(t, model, "new", "ClaimId", `"<claim-new>"`)
			// A plain set: a delta where delta records are on.
			if !redactedCommit(t, d, func(tx *Txn) { _ = tx.SetAttribute("c", "Note", `"edited"`) }) {
				setModel(t, model, "c", "Note", `"edited"`)
			} else if tc.deltaMax > 0 || !mayRefuse {
				t.Fatal("plain set refused")
			}
			checkRedactedModel(t, d, model)
			for _, k := range []string{"a", "b", "c", "e", "f"} {
				got, _ := d.LookupClassAd(k)
				if s := got.StringWithPrivate(); !strings.Contains(s, redactedSalary) || !strings.Contains(s, "<claim-"+k+">") {
					t.Errorf("%s lost a sealed value: %s", k, s)
				}
			}
			d.Compact()
			checkRedactedModel(t, d, model)
		})
	}
}
