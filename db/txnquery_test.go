package db

import (
	"slices"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func txnOwnerAd(owner string, cpus int64) *classad.ClassAd {
	ad := classad.New()
	ad.InsertAttrString("Owner", owner)
	ad.InsertAttr("Cpus", cpus)
	return ad
}

func txnOwners(t *testing.T, seq func(func(*classad.ClassAd) bool)) []string {
	t.Helper()
	var out []string
	for ad := range seq {
		s, _ := ad.EvaluateAttr("Owner").StringValue()
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func openTxnDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// Txn.Query sees the transaction's own uncommitted writes; DB.Query does not. This is the
// distinction the whole transaction-scoped read path exists to provide.
func TestTxnQueryReadYourWrites(t *testing.T) {
	d := openTxnDB(t)

	seed := d.Begin()
	seed.NewClassAd("a", txnOwnerAd("alice", 8))
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}

	tx := d.Begin()
	defer tx.Abort()
	tx.NewClassAd("b", txnOwnerAd("bob", 8))

	seq, err := tx.Query("Cpus >= 4")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := txnOwners(t, seq), []string{"alice", "bob"}; !slices.Equal(got, want) {
		t.Errorf("Txn.Query got %v, want %v", got, want)
	}

	committed, err := d.Query("Cpus >= 4")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := txnOwners(t, committed), []string{"alice"}; !slices.Equal(got, want) {
		t.Errorf("DB.Query got %v, want %v", got, want)
	}
}

// SetAttribute composes with the transaction's own earlier writes, and the query sees the
// composed result -- the read-modify-write an in-transaction UPDATE performs.
func TestTxnQueryAfterSetAttribute(t *testing.T) {
	d := openTxnDB(t)

	tx := d.Begin()
	defer tx.Abort()
	tx.NewClassAd("a", txnOwnerAd("alice", 8))
	if err := tx.SetAttribute("a", "JobStatus", "2"); err != nil {
		t.Fatal(err)
	}

	seq, err := tx.Query("JobStatus == 2")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := txnOwners(t, seq), []string{"alice"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestTxnKeysWhere(t *testing.T) {
	d := openTxnDB(t)

	seed := d.Begin()
	seed.NewClassAd("committed", txnOwnerAd("alice", 8))
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}

	tx := d.Begin()
	defer tx.Abort()
	tx.NewClassAd("staged", txnOwnerAd("bob", 8))
	tx.DestroyClassAd("committed")

	seq, err := tx.KeysWhere("Cpus >= 4")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range seq {
		keys = append(keys, k)
	}
	if want := []string{"staged"}; !slices.Equal(keys, want) {
		t.Errorf("got %v, want %v", keys, want)
	}
}

func TestTxnQueryBadConstraint(t *testing.T) {
	d := openTxnDB(t)
	tx := d.Begin()
	defer tx.Abort()

	if _, err := tx.Query("not ( a constraint"); err == nil {
		t.Error("Txn.Query accepted a malformed constraint")
	}
	if _, err := tx.KeysWhere("not ( a constraint"); err == nil {
		t.Error("Txn.KeysWhere accepted a malformed constraint")
	}
}

// An aborted transaction leaves nothing behind for a later reader.
func TestTxnQueryAbortDiscards(t *testing.T) {
	d := openTxnDB(t)

	tx := d.Begin()
	tx.NewClassAd("a", txnOwnerAd("alice", 8))
	tx.Abort()

	seq, err := d.Query("Cpus >= 4")
	if err != nil {
		t.Fatal(err)
	}
	if got := txnOwners(t, seq); len(got) != 0 {
		t.Errorf("got %v after abort, want no rows", got)
	}
}
