package collections

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestDeleteConvergesDuringColumnarize is the DELETE analogue of
// TestPresenceCountRaceDuringColumnarize: it pins down the other half of the same root cause. When a
// sealed segment is columnarized its records move to new offsets, and the swap used to publish the new
// segment before its key index was built or the directory reconciled. In that window a SCAN could
// still see a record (it reads segment data directly) but BY-KEY resolution could not
// (findCurrent hit stale directory offsets, lookupSealed found no key index) -- so
// db.DeleteWhere would rescan the row every round and try to delete it, forever, without ever
// removing it (an effective hang up to maxDeleteRounds).
//
// The fix reconciles by-key resolution to the new offsets IN the swap's critical section, so the
// window never exists. This test asserts the invariant directly and deterministically (no reliance on
// hitting a timing window): the moment columnarizeSealedSegment returns -- before the end-of-pass
// reindexAfterCompaction runs -- every key a scan can still see must resolve by key, both for a read
// (Get) and for the delete the server drives (Delete must report removed==true).
func TestDeleteConvergesDuringColumnarize(t *testing.T) {
	dir := t.TempDir()
	// Small segments => many sealed segments to columnarize; a large budget so a single
	// ColumnarizeSealed pass would sweep them all (we instead drive per-segment below).
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 12, ColumnarSegmentBudget: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const n = 3000
	putAd := func(i int) {
		ad, perr := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; ProcId=0; Owner="user%d"; JobStatus=2; RequestMemory=%d; `+
				`RequestCpus=%d; RequestDisk=%d; QDate=%d; Sentinel=1 ]`,
			i, i%37, (i%16)*1024, 1+i%8, (i%32)*2048, 1600000000+i))
		if perr != nil {
			t.Fatal(perr)
		}
		if perr := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); perr != nil {
			t.Fatal(perr)
		}
	}
	for i := 0; i < n; i++ {
		putAd(i)
	}

	sch, hot, ok := c.deriveSchema(4096, 8)
	if !ok || !c.installSchemaScan(sch, hot) {
		t.Skip("schema scan did not enable")
	}
	c.EnableSchemaScan(sch, hot)

	st := c.schemaScan.Load()
	if st == nil || st.schema == nil {
		t.Skip("no schema derived")
	}

	sh := c.shards[0]

	// currentKeysIn returns the keys whose CURRENT (non-superseded) record lives in seg right now --
	// the records a mid-columnarize window would strand for by-key resolution.
	currentKeysIn := func(seg *segment) [][]byte {
		var keys [][]byte
		sh.mu.RLock()
		for off := 0; off < seg.used; {
			o := uint32(off)
			total := recTotalLen(seg.data, o)
			if total == 0 {
				break
			}
			if !recIsDict(seg.data, o) && !recIsCol(seg.data, o) && recSuperseded(seg.data, o) == seqMax {
				keys = append(keys, append([]byte(nil), recKey(seg.data, o)...))
			}
			off += int(total)
		}
		sh.mu.RUnlock()
		return keys
	}

	// scanKeySet returns every key a SCAN currently sees (reads segment data directly, so it observes
	// a record even while that record's segment sits in the columnarize window).
	scanKeySet := func() map[string]bool {
		set := map[string]bool{}
		c.ForEachAd(func(key string, _ *classad.ClassAd) bool {
			set[key] = true
			return true
		})
		return set
	}

	// columnarizeSealedSegment requires maintMu (as compaction/reseal do), so hold it across the whole
	// per-segment drive; Get/Delete/ForEachAd take only the shard lock, so they interleave fine.
	c.maintMu.Lock()
	defer c.maintMu.Unlock()

	sh.mu.Lock()
	act := sh.act
	var srcs []*segment
	for _, seg := range sh.segs {
		if seg != nil && seg != act && seg.used > 0 && !seg.columnarized() {
			srcs = append(srcs, seg)
		}
	}
	sh.mu.Unlock()
	if len(srcs) == 0 {
		t.Skip("no sealed segments to columnarize")
	}

	checked := 0
	for _, src := range srcs {
		keys := currentKeysIn(src)
		if len(keys) == 0 {
			continue
		}
		if !c.columnarizeSealedSegment(sh, src, st.schema, st.hot) {
			continue
		}

		// The bug's window: dst is swapped in, but reindexAfterCompaction has NOT run. A scan must
		// still see these records, and -- the property the fix restores -- by-key resolution must too.
		seen := scanKeySet()
		for _, k := range keys {
			if !seen[string(k)] {
				t.Fatalf("key %q vanished from a scan after columnarize (unexpected)", k)
			}
			// Read resolution: the same findCurrent/lookupSealed path Get shares with Delete.
			if _, found := c.Get(k); !found {
				t.Fatalf("key %q: a scan sees the row but by-key Get misses it right after the "+
					"columnarize swap -- this is the window DeleteWhere spins in forever", k)
			}
			// Delete resolution: exactly what db.DeleteWhere -> DestroyClassAd -> shard.del drives.
			// removed==true means the sweep converges instead of re-matching the row every round.
			if !c.Delete(k) {
				t.Fatalf("key %q: by-key Delete failed to remove a record a scan still sees right "+
					"after the columnarize swap (the DeleteWhere non-convergence hang)", k)
			}
			checked++
		}
		// The deletes must actually be gone from the scan too (no duplicate live version survived).
		gone := scanKeySet()
		for _, k := range keys {
			if gone[string(k)] {
				t.Fatalf("key %q still visible to a scan after by-key Delete (a duplicate live "+
					"version survived the columnarize swap)", k)
			}
		}
	}
	if checked == 0 {
		t.Skip("no keys exercised")
	}
	t.Logf("verified by-key Get+Delete convergence for %d keys across %d columnarized segments",
		checked, len(srcs))
}

// TestDeleteWhereStyleConvergesUnderConcurrentColumnarize is the concurrent counterpart: a background
// ColumnarizeSealed loop runs while a foreground loop drives the exact scan-then-delete-by-key
// convergence db.DeleteWhere uses. Every key a scan matches must be removable by key; if by-key
// resolution ever misses a scan-visible row, the per-key sweep cannot converge -- the hang under test.
func TestDeleteWhereStyleConvergesUnderConcurrentColumnarize(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 12, ColumnarSegmentBudget: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const n = 4000
	putAd := func(i int) {
		ad, perr := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; ProcId=0; Owner="user%d"; JobStatus=2; RequestMemory=%d; `+
				`RequestCpus=%d; RequestDisk=%d; QDate=%d; Sentinel=1 ]`,
			i, i%37, (i%16)*1024, 1+i%8, (i%32)*2048, 1600000000+i))
		if perr != nil {
			t.Fatal(perr)
		}
		if perr := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); perr != nil {
			t.Fatal(perr)
		}
	}
	for i := 0; i < n; i++ {
		putAd(i)
	}
	sch, hot, ok := c.deriveSchema(4096, 8)
	if !ok || !c.installSchemaScan(sch, hot) {
		t.Skip("schema scan did not enable")
	}
	c.EnableSchemaScan(sch, hot)

	// scanMatches reports whether any current ad matches q (a scan oracle, like DeleteWhere's
	// matchingKeys: reads segment data directly).
	matches := func(i int) bool {
		q, perr := vm.Parse(fmt.Sprintf("ClusterId == %d && Sentinel == 1", i))
		if perr != nil {
			t.Fatal(perr)
		}
		for range c.Query(q) {
			return true
		}
		return false
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	var nonConverged atomic.Int64

	// Columnarizer: churn the sealed segments the whole time.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			c.ColumnarizeSealed()
		}
	}()

	// Delete/re-put driver: for each key, converge like DeleteWhere (while a scan matches, delete by
	// key), then re-insert it so the population keeps cycling through sealed/columnarizing segments.
	wg.Add(1)
	go func() {
		defer wg.Done()
		key := func(i int) []byte { return []byte(fmt.Sprintf("%d.0", i)) }
		i := 0
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			k := i % n
			i++
			rounds := 0
			for matches(k) {
				c.Delete(key(k))
				rounds++
				if rounds > 100 {
					// The scan keeps matching the row but by-key delete cannot remove it: the
					// non-convergence DeleteWhere would report as "did not converge".
					nonConverged.Add(1)
					t.Errorf("key %d: scan-then-delete-by-key did not converge in %d rounds "+
						"during concurrent columnarize", k, rounds)
					break
				}
			}
			if nonConverged.Load() > 0 {
				return
			}
			putAd(k) // keep a live population to columnarize and delete
		}
	}()

	time.Sleep(4 * time.Second)
	stop.Store(true)
	wg.Wait()

	if nonConverged.Load() != 0 {
		t.Fatalf("%d keys failed to converge under concurrent columnarize", nonConverged.Load())
	}
}
