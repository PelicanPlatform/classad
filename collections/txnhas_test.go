package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestTxnHasMatchesGet pins Has to the question Get answers. Has takes a different route
// through the shard -- it stops at the record location instead of copying and decoding --
// so the two can only be trusted to agree if that is asserted directly, across the states
// where they could diverge: never written, committed, buffered, deleted, superseded, and
// read at a snapshot that predates a later write.
func TestTxnHasMatchesGet(t *testing.T) {
	// A small segment size with a large-ish payload forces segments to fill and seal, so
	// the keys written first are resolved through the SEALED key index rather than the
	// in-directory chain -- Has has to agree with Get on both routes.
	c := New(Options{Shards: 4, SegmentSize: 1 << 12})
	ad := func(v int) *classad.ClassAd {
		a := classad.New()
		a.InsertAttr("V", int64(v))
		a.InsertAttrString("Pad", strings.Repeat("x", 256))
		return a
	}
	// Enough keys to spread across shards and to push some records out of the directory.
	const n = 500
	tx := c.Begin()
	for i := 0; i < n; i++ {
		tx.Put([]byte(fmt.Sprintf("k%d", i)), ad(i))
	}
	if r := tx.Commit(); r.Conflicted() {
		t.Fatalf("seed commit conflicted: %v", r.Conflicts)
	}

	check := func(tx *Txn, key string, why string) {
		t.Helper()
		_, want := tx.Get([]byte(key))
		if got := tx.Has([]byte(key)); got != want {
			t.Errorf("%s: Has(%q) = %v, Get ok = %v", why, key, got, want)
		}
	}

	// Committed keys, absent keys.
	tx = c.Begin()
	for i := 0; i < n; i++ {
		check(tx, fmt.Sprintf("k%d", i), "committed")
		check(tx, fmt.Sprintf("absent%d", i), "never written")
	}
	// Buffered write of a new key, buffered overwrite, buffered delete of a live key,
	// and a buffered delete of a key that was never there.
	tx.Put([]byte("fresh"), ad(1))
	check(tx, "fresh", "buffered put")
	tx.Put([]byte("k0"), ad(99))
	check(tx, "k0", "buffered overwrite")
	tx.Delete([]byte("k1"))
	check(tx, "k1", "buffered delete")
	tx.Delete([]byte("neverthere"))
	check(tx, "neverthere", "buffered delete of absent key")
	if r := tx.Commit(); r.Conflicted() {
		t.Fatalf("commit conflicted: %v", r.Conflicts)
	}

	// After the commit: the delete must be visible to both, the insert to both.
	tx = c.Begin()
	check(tx, "k1", "after committed delete")
	check(tx, "fresh", "after committed insert")
	check(tx, "k0", "after committed overwrite")

	// A snapshot taken BEFORE a later commit must report the state it was opened at.
	old := c.Begin()
	old.Get([]byte("k2")) // pin the snapshot for k2's shard
	tx2 := c.Begin()
	tx2.Delete([]byte("k2"))
	tx2.Put([]byte("late"), ad(7))
	if r := tx2.Commit(); r.Conflicted() {
		t.Fatalf("late commit conflicted: %v", r.Conflicts)
	}
	check(old, "k2", "deleted after our snapshot")
	check(c.Begin(), "k2", "deleted, fresh snapshot")
	check(c.Begin(), "late", "inserted, fresh snapshot")
}

// TestTxnHasEvicted covers Has's other resolution route. A key whose record has been
// evicted from the resident directory -- which is every key after a reopen, the normal
// state of a long-lived persistent store -- is found through the sealed KEY INDEX
// instead of the bucket chain. Has re-implements getAt's resolution, so both routes have
// to be pinned or a Has that silently reports "absent" for every pre-restart key would
// still pass the in-memory test.
func TestTxnHasEvicted(t *testing.T) {
	c := openEvicted(t, 3000)
	defer c.Close()

	tx := c.Begin()
	for i := 0; i < 3000; i += 7 {
		key := fmt.Sprintf("c%05d", i)
		_, want := tx.Get([]byte(key))
		if !want {
			t.Fatalf("setup: %s missing from the reopened store", key)
		}
		if got := tx.Has([]byte(key)); got != want {
			t.Errorf("evicted: Has(%q) = %v, Get ok = %v", key, got, want)
		}
	}
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("nope%05d", i)
		_, want := tx.Get([]byte(key))
		if got := tx.Has([]byte(key)); got != want {
			t.Errorf("absent: Has(%q) = %v, Get ok = %v", key, got, want)
		}
	}
}
