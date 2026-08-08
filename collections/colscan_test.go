package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// allSegmentWires gathers every shard/segment's live-arena wire ads (for building the schema).
func allSegmentWires(t *testing.T, c *Collection) [][]byte {
	t.Helper()
	var ws [][]byte
	for _, sh := range c.shards {
		sh.mu.RLock()
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil || seg.used == 0 {
				continue
			}
			ws = append(ws, segmentWires(t, seg)...)
		}
	}
	return ws
}

// allBruteCount is the ground truth: every shard/window's visible records counted by the row
// path (ignoring columnar blocks), so it exercises the exact MVCC visibility.
func (c *Collection) allBruteCount(fieldID uint32, match func(int64) bool) int {
	count := 0
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			bruteNumValues(w, s0, func(a wire.Ad) ([]byte, bool) { return a.Lookup(fieldID) }, func(nv colVal) {
				if match(int64(nv.f)) {
					count++
				}
			})
		}
		releaseWindows(wins)
	}
	return count
}

// TestSchemaScanIntCountMVCC verifies the columnar accelerated count equals a row-scan ground
// truth over the same live snapshot -- exercising sealed segments (columnar path), the active
// segment (row fallback), MVCC supersession (updated keys), escaped ints (out-of-width, in the
// cold tail -> reconstruct), and escaped strings (type exception -> no match).
func TestSchemaScanIntCountMVCC(t *testing.T) {
	store := New(Options{Shards: 2, SegmentSize: 1 << 12}) // small -> many sealed segments
	const n = 1200
	put := func(key, src string) {
		if err := store.Put([]byte(key), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		mem := 1024 + (i%64)*512 // fits uint16
		switch {
		case i%50 == 7:
			put(fmt.Sprintf("k%d", i), fmt.Sprintf("Cpus=%d\nMemory=%d\nMachine=\"m%03d\"", 1+i%8, 100000, i)) // out-of-uint16 -> escapes
		case i%50 == 11:
			put(fmt.Sprintf("k%d", i), fmt.Sprintf("Cpus=%d\nMemory=\"unknown\"\nMachine=\"m%03d\"", 1+i%8, i)) // string -> escapes
		default:
			put(fmt.Sprintf("k%d", i), fmt.Sprintf("Cpus=%d\nMemory=%d\nMachine=\"m%03d\"", 1+i%8, mem, i))
		}
	}
	// Update every 3rd key -> supersede its earlier (possibly sealed) version.
	for i := 0; i < n; i += 3 {
		put(fmt.Sprintf("k%d", i), fmt.Sprintf("Cpus=2\nMemory=%d\nMachine=\"m%03d\"", 9000, i))
	}

	wires := allSegmentWires(t, store)
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	memID, _ := store.intern.LookupID("Memory")
	memIdx, ok := s.byID[memID]
	if !ok || s.fields[memIdx].kind != akInt {
		t.Skipf("Memory not an int schema field (width tier)")
	}

	store.EnableSchemaScan(s, []int{memIdx})
	// Confirm the columnar path is actually exercised.
	sealedWithBlock := 0
	for _, sh := range store.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg != sh.act && seg.colblk.Load() != nil {
				sealedWithBlock++
			}
		}
		sh.mu.RUnlock()
	}
	if sealedWithBlock == 0 {
		t.Fatal("no sealed segment got a columnar block; test would not exercise the columnar path")
	}
	t.Logf("sealed segments with columnar block: %d", sealedWithBlock)

	for _, thr := range []int64{4096, 8000, 50000, 0} {
		match := func(v int64) bool { return v > thr }
		want := store.allBruteCount(memID, match)
		got := store.schemaScanIntCount(s, memIdx, match)
		if got != want {
			t.Errorf("Memory > %d: columnar count = %d, row-scan truth = %d", thr, got, want)
		}
	}
	// Cross-check the common threshold against the store's own query engine.
	q, err := vm.Parse("Memory > 4096")
	if err != nil {
		t.Fatal(err)
	}
	qc := 0
	for range store.Query(q) {
		qc++
	}
	if sc := store.schemaScanIntCount(s, memIdx, func(v int64) bool { return v > 4096 }); sc != qc {
		t.Errorf("Memory > 4096: columnar %d != store.Query %d", sc, qc)
	}
}
