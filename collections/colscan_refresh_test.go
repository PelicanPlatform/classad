package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestSchemaScanPersistentInline is the regression guard for the inline-record path: a
// persistent collection stores ads with inline names (not interned ids), so the id-keyed
// accelerator must canonicalize records before reading them. Without that, buildAdSchema sees
// zero fields and the accelerator silently never enables. Here it must enable and count
// correctly over both sealed segments (columnar blocks) and the active segment (brute
// fallback), matching the row-scan engine.
func TestSchemaScanPersistentInline(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 12}) // persistent ⇒ inline names
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !store.inline {
		t.Fatal("expected a persistent collection to be inline-encoded")
	}
	const n = 1000
	for i := 0; i < n; i++ {
		ad := mustAdOld(t, fmt.Sprintf("Cpus=%d\nMemory=%d\nDisk=%d\nMachine=\"m%05d\"", 1+i%8, 1024+(i%64)*256, i*4096, i))
		if err := store.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	mq, _ := vm.Parse("true")
	for i := 0; i < 20; i++ {
		for range store.QueryProject(mq, []string{"Memory"}) {
		}
	}
	if !store.BuildAndEnableSchemaScan(2000, 4) {
		t.Fatal("BuildAndEnableSchemaScan returned false on a persistent store (inline records not canonicalized?)")
	}
	for _, expr := range []string{"Memory > 4096", "Memory <= 4096", "Memory >= 2000 && Memory < 9000"} {
		got, ok := store.CountConstraint(expr)
		if !ok {
			t.Errorf("%q: CountConstraint declined on a persistent store", expr)
			continue
		}
		q, _ := vm.Parse(expr)
		want := 0
		for range store.Query(q) {
			want++
		}
		if got != want {
			t.Errorf("%q: columnar %d != store.Query %d (inline path)", expr, got, want)
		}
	}
}

// TestBuildAndEnableSchemaScanRefreshSafe verifies a second BuildAndEnableSchemaScan (as a
// repeated Maintain pass would issue) keeps the stable schema chosen at first enable and extends
// coverage to segments sealed since -- rather than minting a fresh schema that would orphan the
// earlier segments' blocks onto the brute-scan fallback. It asserts the schema pointer is reused
// and every sealed segment's block matches it, and that counts stay correct across the refresh.
func TestBuildAndEnableSchemaScanRefreshSafe(t *testing.T) {
	store := New(Options{Shards: 1, SegmentSize: 1 << 12}) // small -> sealed segments
	put := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			ad := mustAdOld(t, fmt.Sprintf("Cpus=%d\nMemory=%d\nDisk=%d\nMachine=\"m%05d\"", 1+i%8, 1024+(i%64)*256, i*4096, i))
			if err := store.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
				t.Fatal(err)
			}
		}
	}
	put(0, 800)
	mq, _ := vm.Parse("true")
	for i := 0; i < 20; i++ {
		for range store.QueryProject(mq, []string{"Memory"}) {
		}
	}
	if !store.BuildAndEnableSchemaScan(2000, 4) {
		t.Fatal("first BuildAndEnableSchemaScan returned false")
	}
	first := store.schemaScan.Load()
	if first == nil {
		t.Fatal("schema scan not enabled after first build")
	}

	// A second batch seals more segments, then a refresh pass runs.
	put(800, 1600)
	if !store.BuildAndEnableSchemaScan(2000, 4) {
		t.Fatal("refresh BuildAndEnableSchemaScan returned false")
	}
	second := store.schemaScan.Load()
	if second.schema != first.schema {
		t.Fatalf("refresh minted a new schema (%p != %p); it must reuse the stable one", second.schema, first.schema)
	}

	// Every sealed segment must carry a block built against the live schema -- none orphaned.
	sealed, orphaned := 0, 0
	for _, sh := range store.shards {
		sh.mu.RLock()
		act := sh.act
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil || seg == act || seg.used == 0 {
				continue
			}
			sealed++
			cs := seg.colblk.Load()
			if cs == nil || cs.schema() != second.schema {
				orphaned++
			}
		}
	}
	if sealed == 0 {
		t.Fatal("expected sealed segments across two batches; got none")
	}
	if orphaned != 0 {
		t.Errorf("%d/%d sealed segments orphaned (block schema != live schema) after refresh", orphaned, sealed)
	}

	// Counts over the full (both-batch) data stay correct through the columnar path.
	for _, expr := range []string{"Memory > 4096", "Memory <= 4096", "Memory >= 2000 && Memory < 9000"} {
		got, ok := store.CountConstraint(expr)
		if !ok {
			t.Errorf("%q: CountConstraint declined after refresh", expr)
			continue
		}
		q, _ := vm.Parse(expr)
		want := 0
		for range store.Query(q) {
			want++
		}
		if got != want {
			t.Errorf("%q: columnar %d != store.Query %d after refresh", expr, got, want)
		}
	}
}
