package collections

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestIndexSnapshotPersistsAcrossOpen: a persistent collection writes an index snapshot
// (.idx) beside each segment at Reindex, and a reopen restores the index from it. Proves
// the write hook fires, the on-disk snapshot loads back into a working index, and reopen
// query results are correct.
func TestIndexSnapshotPersistsAcrossOpen(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	opts := Options{Shards: 2, Dir: dir, CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"Memory"}}
	c, err := Open(opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 1200; i++ {
		if err := c.Put([]byte(fmt.Sprintf("m%d", i)),
			mustAd(t, fmt.Sprintf(`[ Id=%d; Owner="u%d"; Memory=%d ]`, i, i%20, (i%8+1)*1024))); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex() // builds indexes and writes .idx snapshots

	// The write hook fired: at least one snapshot exists on disk.
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "*.idx"))
	if len(matches) == 0 {
		t.Fatal("no .idx snapshot files were written")
	}

	// The snapshot loads back into a working index: clear a sealed segment's index and
	// restore it from disk.
	restored := false
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			si := seg.idx.Load()
			if si == nil || int(si.upto) != seg.used {
				continue
			}
			seg.idx.Store(nil)
			if c.loadIndexSnapshot(seg, c.spec.Load()) {
				restored = true
			} else {
				t.Fatal("loadIndexSnapshot failed for a segment with a fresh snapshot")
			}
		}
	}
	if !restored {
		t.Fatal("no sealed segment was restored from its snapshot")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: recovery restores indexes from snapshots; queries are correct.
	c2, err := Open(opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	q, _ := vm.Parse(`Owner == "u3" && Memory >= 4096`)
	got := queryIDs(t, c2, q)
	var want []int
	for i := 0; i < 1200; i++ {
		if i%20 == 3 && (i%8+1)*1024 >= 4096 {
			want = append(want, i)
		}
	}
	if !equalInts(got, want) {
		t.Errorf("after reopen: got %v, want %v", got, want)
	}
}

// TestLiveIndexSnapshotRoundTrip is the correctness anchor for persisted live indexes: a
// segIndex encoded and decoded back must yield IDENTICAL query results (a decode that
// dropped postings would silently lose matches, which re-verify cannot recover). It swaps
// every segment's index for its encode->decode image, then re-runs a battery that
// exercises ==, !=, ranges, =?= exact-case, and presence probes.
func TestLiveIndexSnapshotRoundTrip(t *testing.T) {
	t.Parallel()
	c := New(Options{
		Shards:           4,
		CategoricalAttrs: []string{"Owner", "State"},
		ValueAttrs:       []string{"Memory", "Cpus"},
	})
	owners := []string{"alice", "Alice", "bob", "BOB", "carol"} // mixed case -> exact/exactCase
	states := []string{"Idle", "Running", "Held"}
	for i := 0; i < 1500; i++ {
		var ad string
		switch {
		case i%17 == 0: // a type exception: Memory present but not numeric
			ad = fmt.Sprintf(`[ Id=%d; Owner=%q; State=%q; Memory="lots"; Cpus=%d ]`,
				i, owners[i%len(owners)], states[i%len(states)], i%16)
		case i%13 == 0: // Owner absent (presence probe coverage)
			ad = fmt.Sprintf(`[ Id=%d; State=%q; Memory=%d; Cpus=%d ]`,
				i, states[i%len(states)], (i%8+1)*1024, i%16)
		default:
			ad = fmt.Sprintf(`[ Id=%d; Owner=%q; State=%q; Memory=%d; Cpus=%d ]`,
				i, owners[i%len(owners)], states[i%len(states)], (i%8+1)*1024, i%16)
		}
		if err := c.Put([]byte(fmt.Sprintf("m%d", i)), mustAd(t, ad)); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	queries := []string{
		`Owner == "alice"`,
		`Owner == "bob" || Owner == "carol"`,
		`Owner != "alice"`,
		`Owner =?= "Alice"`, // case-sensitive: must not match "alice"
		`Owner =?= "BOB"`,
		`Memory >= 4096`,
		`Memory > 2048 && Memory <= 6144`,
		`Cpus == 4`,
		`State == "Running" && Memory >= 2048`,
		`Owner =!= "alice"`,
		`Owner isnt undefined`,
		`Owner is undefined`,
	}

	before := map[string][]int{}
	for _, qs := range queries {
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatalf("parse %q: %v", qs, err)
		}
		before[qs] = queryIDs(t, c, q)
	}

	// Swap every segment's index for its encode->decode image.
	spec := c.spec.Load()
	swapped := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil {
				continue
			}
			si := seg.idx.Load()
			if si == nil {
				continue
			}
			blob, err := encodeLiveIndex(si)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := decodeLiveIndex(blob, spec)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got == nil {
				t.Fatal("decode returned a soft miss for a same-spec snapshot")
			}
			if got.upto != si.upto || got.specGen != si.specGen {
				t.Fatalf("decoded meta mismatch: upto %d/%d specGen %d/%d", got.upto, si.upto, got.specGen, si.specGen)
			}
			seg.idx.Store(got)
			swapped++
		}
	}
	if swapped == 0 {
		t.Fatal("no segment indexes were exercised")
	}

	for _, qs := range queries {
		q, _ := vm.Parse(qs)
		after := queryIDs(t, c, q)
		if !equalInts(before[qs], after) {
			t.Errorf("query %q: results changed after index round-trip\n before=%v\n after =%v", qs, before[qs], after)
		}
	}
}

// TestLiveIndexSnapshotSoftMiss: a snapshot from a different spec generation, or a
// non-snapshot blob, decodes to a soft miss (nil, nil) so the caller rebuilds rather than
// installing a wrong index.
func TestLiveIndexSnapshotSoftMiss(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1, CategoricalAttrs: []string{"Owner"}})
	for i := 0; i < 200; i++ {
		if err := c.Put([]byte(fmt.Sprintf("m%d", i)), mustAd(t, fmt.Sprintf(`[ Id=%d; Owner="u%d" ]`, i, i%5))); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	var blob []byte
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if si := seg.idx.Load(); si != nil {
				var err error
				if blob, err = encodeLiveIndex(si); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if blob == nil {
		t.Fatal("no index to encode")
	}

	// A bumped spec generation must be rejected.
	bumped := &indexSpec{gen: c.spec.Load().gen + 1}
	if si, err := decodeLiveIndex(blob, bumped); err != nil || si != nil {
		t.Errorf("spec-gen mismatch should soft-miss (nil,nil); got si=%v err=%v", si, err)
	}
	// Garbage is a soft miss too.
	if si, err := decodeLiveIndex([]byte("not a snapshot"), c.spec.Load()); err != nil || si != nil {
		t.Errorf("garbage should soft-miss (nil,nil); got si=%v err=%v", si, err)
	}
}
