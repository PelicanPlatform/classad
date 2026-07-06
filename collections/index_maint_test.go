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

func mustParse(t *testing.T, s string) *classad.ClassAd {
	t.Helper()
	ad, err := classad.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ad
}

func countQuery(t *testing.T, c *Collection, qs string) int {
	t.Helper()
	q, err := vm.Parse(qs)
	if err != nil {
		t.Fatalf("parse query %q: %v", qs, err)
	}
	n := 0
	for range c.Query(q) {
		n++
	}
	return n
}

// TestIndexMaintenance verifies an update moves an id between value buckets (the
// old value's posting no longer matches) and a delete removes it entirely.
func TestIndexMaintenance(t *testing.T) {
	c := New(Options{
		Shards:           4,
		CategoricalAttrs: []string{"Arch"},
		ValueAttrs:       []string{"Cpus"},
	})
	c.Put([]byte("m"), mustParse(t, `[ Arch = "X86_64"; Cpus = 4 ]`))
	c.Reindex()

	if got := countQuery(t, c, `Arch == "X86_64"`); got != 1 {
		t.Fatalf("after put: Arch==X86_64 => %d, want 1", got)
	}
	if got := countQuery(t, c, `Cpus == 4`); got != 1 {
		t.Fatalf("after put: Cpus==4 => %d, want 1", got)
	}

	// Update to new values.
	c.Put([]byte("m"), mustParse(t, `[ Arch = "aarch64"; Cpus = 8 ]`))
	if got := countQuery(t, c, `Arch == "X86_64"`); got != 0 {
		t.Fatalf("after update: old Arch==X86_64 => %d, want 0 (stale posting not cleaned)", got)
	}
	if got := countQuery(t, c, `Cpus == 4`); got != 0 {
		t.Fatalf("after update: old Cpus==4 => %d, want 0", got)
	}
	if got := countQuery(t, c, `Arch == "aarch64"`); got != 1 {
		t.Fatalf("after update: new Arch==aarch64 => %d, want 1", got)
	}
	if got := countQuery(t, c, `Cpus == 8`); got != 1 {
		t.Fatalf("after update: new Cpus==8 => %d, want 1", got)
	}

	// Delete.
	if !c.Delete([]byte("m")) {
		t.Fatal("delete returned false")
	}
	if got := countQuery(t, c, `Arch == "aarch64"`); got != 0 {
		t.Fatalf("after delete: => %d, want 0", got)
	}
	if got := countQuery(t, c, `Cpus == 8`); got != 0 {
		t.Fatalf("after delete: Cpus==8 => %d, want 0", got)
	}
	// A reindex after the churn still returns nothing for the deleted key.
	c.Reindex()
	if got := countQuery(t, c, `Arch == "aarch64"`); got != 0 {
		t.Fatalf("after reindex+delete: => %d, want 0", got)
	}
}

// TestIndexConcurrent stresses the index under concurrent writers and index-driven
// readers. With -race it guards the maintenance/query locking; it also asserts the
// query invariants hold on a moving target: no duplicate id in a result, and every
// yielded ad actually matches the query (the re-verify contract).
func TestIndexConcurrent(t *testing.T) {
	c := New(Options{
		Shards:           8,
		CategoricalAttrs: []string{"Arch"},
		ValueAttrs:       []string{"Cpus"},
	})
	const keys = 200
	for i := 0; i < keys; i++ {
		c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustParse(t, fmt.Sprintf(`[ ID=%d; Arch="X86_64"; Cpus=%d ]`, i, i%8)))
	}

	c.Reindex()
	q, _ := vm.Parse(`Cpus >= 4 && Arch == "X86_64"`)
	var stop atomic.Bool
	var writers, readers sync.WaitGroup

	// Reindexer: rebuild per-segment indexes concurrently with writers/readers, to
	// exercise the index path and race the atomic segIndex swap against readers.
	writers.Add(1)
	go func() {
		defer writers.Done()
		for !stop.Load() {
			c.Reindex()
			time.Sleep(3 * time.Millisecond)
		}
	}()

	// Writers: churn values (moving ids between buckets), delete + re-insert.
	for w := 0; w < 2; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for r := 0; !stop.Load(); r++ {
				i := (r*2 + w) % keys
				k := []byte(fmt.Sprintf("k%d", i))
				c.Put(k, mustParse(t, fmt.Sprintf(`[ ID=%d; Arch="X86_64"; Cpus=%d ]`, i, (i+r)%8)))
				if r%5 == 0 {
					c.Delete(k)
					c.Put(k, mustParse(t, fmt.Sprintf(`[ ID=%d; Arch="X86_64"; Cpus=%d ]`, i, r%8)))
				}
			}
		}(w)
	}

	// Readers: run the index query, checking invariants on a moving target.
	for rdr := 0; rdr < 2; rdr++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 300; i++ {
				seen := map[int]bool{}
				for ad := range c.Query(q) {
					id, ok := ad.EvaluateAttrInt("ID")
					if !ok {
						t.Errorf("ad without ID")
						return
					}
					if seen[int(id)] {
						t.Errorf("duplicate id %d in one query result", id)
						return
					}
					seen[int(id)] = true
					if !q.Matches(ad) {
						cpus, _ := ad.EvaluateAttrInt("Cpus")
						arch, _ := ad.EvaluateAttrString("Arch")
						t.Errorf("yielded ad id %d does not match: Cpus=%d Arch=%q", id, cpus, arch)
						return
					}
				}
			}
		}()
	}
	readers.Wait()
	stop.Store(true)
	writers.Wait()
}
