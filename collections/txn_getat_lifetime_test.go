package collections

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// getAt hands its caller the segment's DICT HANDLE and then releases the shard read lock. The handle
// holds `data: seg.data` -- for a persistent segment, an mmap that compaction unmaps after dropping the
// write lock -- and getAt takes no pin, so a decode running after it returned could resolve attribute
// names out of unmapped memory. The record bytes are copied and the codec's dictionary is Go-heap, so
// the handle was the only thing that escaped.
//
// The fix makes the handle stop depending on the mapping: getAt builds the id->name cache while the read
// lock still guarantees the segment is alive, and resolve -- the only thing a decode calls -- reads that
// cache. These tests pin both halves: the invariant (the cache IS built before getAt returns) and the
// behaviour under the interleaving that used to be unsafe.

// internedPersistent builds a persistent collection whose sealed segments carry dictionaries, which is
// the only configuration where a dict handle exists to escape at all.
func internedPersistent(t *testing.T, shards, n int) (*Collection, func(i int) []byte) {
	t.Helper()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	c, err := Open(Options{Shards: shards, Dir: t.TempDir(), SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	key := func(i int) []byte { return []byte(fmt.Sprintf("k%05d", i)) }
	ad := func(i, rev int) string {
		return fmt.Sprintf(`[Id=%d; Val=%d; Name="host-%d.example.org"; Rev=%d]`, i, i%100, i, rev)
	}
	for rev := 0; rev < 2; rev++ { // rev 1 supersedes rev 0, so compaction has something to reclaim
		for i := 0; i < n; i++ {
			if err := c.Put(key(i), mustAd(t, ad(i, rev))); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Compact every shard so its live records move into interned (dict-bearing) segments.
	for _, sh := range c.shards {
		c.compactShard(sh, c.currentCodec())
	}
	return c, key
}

func TestReadersDoNotLeakAMappingDependentDict(t *testing.T) {
	const n = 3000
	c, key := internedPersistent(t, 2, n)
	defer c.Close()

	// The premise: at least one shard really does have a dict-bearing segment. Without this the test
	// would pass on a collection where no handle exists to escape, proving nothing.
	dicts := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg.dict.Load() != nil {
				dicts++
			}
		}
		sh.mu.RUnlock()
	}
	if dicts == 0 {
		t.Fatal("no segment carries a dictionary: this configuration cannot exercise the escape")
	}
	t.Logf("%d dict-bearing segments", dicts)

	// BOTH readers that hand a handle out of the lock, not just one. get() is Collection.Get (the
	// non-transactional point read) and getAt() is the snapshot read behind Txn.Get. The first version
	// of this test covered only getAt -- and the stress test below uses Collection.Get, so between them
	// they left get() unfixed and untested while both appeared to pass.
	readers := []struct {
		name string
		read func(sh *shard, h uint64, k []byte) (*segDictHandle, bool)
	}{
		{"shard.get", func(sh *shard, h uint64, k []byte) (*segDictHandle, bool) {
			_, _, d, ok := sh.get(h, k)
			return d, ok
		}},
		{"shard.getAt", func(sh *shard, h uint64, k []byte) (*segDictHandle, bool) {
			sh.mu.RLock()
			s0 := sh.commitSeq
			sh.mu.RUnlock()
			_, _, d, ok := sh.getAt(h, k, s0)
			return d, ok
		}},
	}
	for _, r := range readers {
		dropNameCaches(c)
		checked := 0
		for i := 0; i < n; i++ {
			k := key(i)
			h := c.h.Hash(k)
			sh := c.shards[c.shardOf(k, h)]
			dict, ok := r.read(sh, h, k)
			if !ok {
				t.Fatalf("%s: key %s not found", r.name, k)
			}
			if dict == nil {
				continue // an inline segment: nothing points into the arena
			}
			// The invariant: by the time the read lock is gone, the handle no longer needs the mapping.
			if dict.names.Load() == nil {
				t.Fatalf("%s returned a dict handle whose name cache is not built, for key %s: resolving "+
					"through it after the lock is released reads the segment arena, which compaction may "+
					"have unmapped", r.name, k)
			}
			checked++
		}
		if checked == 0 {
			t.Fatalf("%s: no key resolved through a dict handle: the invariant was never exercised", r.name)
		}
		t.Logf("%s: checked %d keys resolved through a dict handle", r.name, checked)
	}
}

// dropNameCaches clears every segment's id->name cache, so an assertion about what a reader builds is not
// satisfied by a cache some earlier read happened to leave behind.
func dropNameCaches(c *Collection) {
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if d := seg.dict.Load(); d != nil {
				d.names.Store(nil)
			}
		}
		sh.mu.RUnlock()
	}
}

// TestGetDuringCompactionReapDecodesCorrectly drives the interleaving that made this a crash rather than
// a theoretical concern: a reader takes a dict handle, compaction retires and unmaps the segment it came
// from, and only then does the reader decode. Every ad must come back intact -- a torn or empty Name is
// the "garbage that parses" signature, and reading a truly unmapped page takes the process down.
//
// What this does and does not prove: it is a stress test, not a proof. Whether a read of an unmapped page
// faults, returns zeros, or returns whatever the kernel later put at that address is timing- and
// allocator-dependent, which is exactly why the production symptom appeared twice in two different forms.
// TestReadersDoNotLeakAMappingDependentDict is the deterministic half; this one exists to catch the
// failure in the shape a user would see it, and it does force real reaping (asserted below).
func TestGetDuringCompactionReapDecodesCorrectly(t *testing.T) {
	const n = 1500
	c, key := internedPersistent(t, 2, n)
	defer c.Close()

	var stop atomic.Bool
	var bg sync.WaitGroup
	var compactions atomic.Int64

	// Writer + compactor: churn keys so compaction always has garbage to reclaim, and reap the
	// segments readers are reading from.
	bg.Add(2)
	go func() {
		defer bg.Done()
		for rev := 2; !stop.Load(); rev++ {
			for i := 0; i < n; i++ {
				_ = c.Put(key(i), mustAd(t, fmt.Sprintf(
					`[Id=%d; Val=%d; Name="host-%d.example.org"; Rev=%d]`, i, i%100, i, rev)))
			}
		}
	}()
	go func() {
		defer bg.Done()
		for !stop.Load() {
			// compactShard directly rather than Compact(): Compact honours a dead-ratio threshold, and
			// with it the reads finished having reaped 4 records in total -- the window this test exists
			// to cross was never crossed. Forcing each shard reaps on every pass.
			for _, sh := range c.shards {
				c.compactShard(sh, c.currentCodec())
				compactions.Add(1)
			}
		}
	}()

	var bad atomic.Int64
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for pass := 0; pass < 40; pass++ {
				for i := 0; i < n; i++ {
					ad, ok := c.Get(key(i))
					if !ok {
						continue // a concurrent compaction may not have this version; not this test's concern
					}
					// Name is a string attribute: resolving its NAME goes through the dict, and its
					// value is long enough that a torn read shows up rather than landing on a
					// plausible short string.
					v := ad.EvaluateAttr("Name")
					s, err := v.StringValue()
					if err != nil || s != fmt.Sprintf("host-%d.example.org", i) {
						bad.Add(1)
						return
					}
				}
			}
		}()
	}
	readers.Wait()
	stop.Store(true)
	bg.Wait()

	if compactions.Load() < int64(len(c.shards)) {
		t.Fatalf("only %d shard compactions ran: the interleaving under test barely happened",
			compactions.Load())
	}
	if b := bad.Load(); b != 0 {
		t.Errorf("%d reads decoded a wrong or unreadable Name while segments were being reaped", b)
	}
	t.Logf("%d forced shard compactions during the reads", compactions.Load())
}
