package collections

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// fillParallelCorpus populates c with n ads spanning several attributes so queries
// have a range of selectivities.
func fillParallelCorpus(t *testing.T, c *Collection, n int) {
	t.Helper()
	owners := []string{"alice", "bob", "carol", "dave"}
	for i := 0; i < n; i++ {
		ad := mustAd(t, fmt.Sprintf(`[ ID=%d; Cpus=%d; Memory=%d; Owner=%q; State=%q ]`,
			i, i%16, (i%32)*1024, owners[i%len(owners)], []string{"Unclaimed", "Claimed"}[i%2]))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
}

// queryIDSet runs q and returns the sorted IDs of the matches (and flags duplicates).
func queryIDSet(t *testing.T, c *Collection, q *vm.Query) ([]int, bool) {
	t.Helper()
	seen := map[int]bool{}
	dup := false
	var ids []int
	for ad := range c.Query(q) {
		id, ok := ad.EvaluateAttrInt("ID")
		if !ok {
			t.Fatal("result missing ID")
		}
		if seen[int(id)] {
			dup = true
		}
		seen[int(id)] = true
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	return ids, dup
}

// TestParallelQueryMatchesSerial checks that a fanned-out query yields exactly the
// same set of ads (each once) as the serial path, across selectivities. The parallel
// collection uses a tiny segment size (many segments) and a zero work threshold, so
// the fan-out path is actually taken.
func TestParallelQueryMatchesSerial(t *testing.T) {
	t.Parallel()
	const n = 4000
	serial := New(Options{Shards: 4})
	par := New(Options{Shards: 4, SegmentSize: 1 << 12, QueryParallelism: 8})
	par.parallelMinBytes = 0 // force fan-out even for a small corpus
	fillParallelCorpus(t, serial, n)
	fillParallelCorpus(t, par, n)

	// Confirm the parallel collection really has many segments to split.
	segs := 0
	for _, sh := range par.shards {
		segs += len(sh.segs)
	}
	if segs < 8 {
		t.Fatalf("expected many segments for fan-out, got %d", segs)
	}

	queries := []string{
		`Cpus >= 0`,                            // all
		`Cpus > 100`,                           // none
		`Cpus >= 8`,                            // ~half
		`Owner == "alice"`,                     // ~quarter
		`Owner == "bob" || Owner == "carol"`,   // ~half
		`State == "Unclaimed" && Cpus < 4`,     // combined
		`Memory > 8192 && Owner == "dave"`,     // sparse
		`Cpus == 3 || Cpus == 7 || Cpus == 11`, // scattered
	}
	for _, qs := range queries {
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatalf("parse %q: %v", qs, err)
		}
		want, _ := queryIDSet(t, serial, q)
		got, dup := queryIDSet(t, par, q)
		if dup {
			t.Errorf("query %q: parallel result had a duplicate id", qs)
		}
		if !equalInts(got, want) {
			t.Errorf("query %q: parallel got %d ids, serial %d ids (mismatch)", qs, len(got), len(want))
		}
	}
}

// TestParallelQueryEarlyStop verifies breaking out of the iterator early stops the
// scan cleanly (no panic, no leak) and that a bounded consumer sees distinct ads.
func TestParallelQueryEarlyStop(t *testing.T) {
	t.Parallel()
	const n = 4000
	par := New(Options{Shards: 4, SegmentSize: 1 << 12, QueryParallelism: 8})
	par.parallelMinBytes = 0
	fillParallelCorpus(t, par, n)
	q, _ := vm.Parse(`Cpus >= 0`) // matches all

	seen := map[int]bool{}
	for ad := range par.Query(q) {
		id, _ := ad.EvaluateAttrInt("ID")
		if seen[int(id)] {
			t.Fatalf("duplicate id %d before early stop", id)
		}
		seen[int(id)] = true
		if len(seen) == 100 {
			break // stop early
		}
	}
	if len(seen) != 100 {
		t.Fatalf("saw %d ads before break, want 100", len(seen))
	}
}

// TestParallelQueryConcurrent runs many fanned-out queries concurrently, exercising
// the shared worker budget (some queries fall back to serial when it is exhausted)
// and confirming each query is still correct. Run with -race.
func TestParallelQueryConcurrent(t *testing.T) {
	t.Parallel()
	const n = 4000
	par := New(Options{Shards: 4, SegmentSize: 1 << 12, QueryParallelism: 8})
	par.parallelMinBytes = 0
	fillParallelCorpus(t, par, n)

	serial := New(Options{Shards: 4})
	fillParallelCorpus(t, serial, n)
	q, _ := vm.Parse(`Cpus >= 8 && Owner == "alice"`)
	want, _ := queryIDSet(t, serial, q)

	var wg sync.WaitGroup
	var bad atomic.Int32
	for w := 0; w < 12; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			qq, _ := vm.Parse(`Cpus >= 8 && Owner == "alice"`)
			for i := 0; i < 20; i++ {
				seen := map[int]bool{}
				for ad := range par.Query(qq) {
					id, _ := ad.EvaluateAttrInt("ID")
					if seen[int(id)] {
						bad.Store(1)
					}
					seen[int(id)] = true
				}
				got := make([]int, 0, len(seen))
				for id := range seen {
					got = append(got, id)
				}
				sort.Ints(got)
				if !equalInts(got, want) {
					bad.Store(2)
				}
			}
		}()
	}
	wg.Wait()
	if v := bad.Load(); v != 0 {
		t.Fatalf("concurrent parallel queries incorrect (code %d)", v)
	}
}
