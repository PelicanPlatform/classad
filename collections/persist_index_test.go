package collections

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// indexQueryBattery exercises every index path: categorical equality/membership/
// negation, value equality/range/negation, multi-index AND, non-indexed fallback,
// constant folding, and no-match. Shared by the in-memory and persistent tests.
var indexQueryBattery = []string{
	`Arch == "X86_64"`,
	`State == "Unclaimed"`,
	`Owner == "alice" || Owner == "carol"`,
	`Arch != "aarch64"`,
	`!(Owner == "bob")`,
	`Cpus >= 4`,
	`Memory > 4096`,
	`Cpus == 3`,
	`Cpus != 5`,
	`Memory > 2048 && Memory <= 6144`,
	`Cpus >= 2 && Arch == "X86_64" && Memory > 1024`,
	`Owner == "dave" && Cpus < 4`,
	`Cpus > 1000`,
	`Rank > 5`,
	`Cpus >= 2 && Nonexistent == "x"`,
	`Memory > 1024*3`,
}

// buildPersistentIndexedCorpus mirrors buildIndexedCorpus but on a persistent
// (inline, mmap-backed) collection under dir, indexed by name.
func buildPersistentIndexedCorpus(t *testing.T, dir string) (*Collection, map[int]*classad.ClassAd) {
	t.Helper()
	c, err := Open(Options{
		Shards:           8,
		Dir:              dir,
		CategoricalAttrs: []string{"Arch", "State", "Owner"},
		ValueAttrs:       []string{"Cpus", "Memory"},
	})
	if err != nil {
		t.Fatal(err)
	}
	arches := []string{"X86_64", "x86_64", "aarch64", "ppc64le"}
	states := []string{"Unclaimed", "Claimed", "Idle", "Owner"}
	owners := []string{"alice", "bob", "carol", "dave"}
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 400; i++ {
		var text string
		switch {
		case i%37 == 0:
			text = fmt.Sprintf(`[ ID = %d; Arch = "%s"; State = "%s"; Owner = "%s"; Cpus = Base + 1; Memory = 1024 * %d ]`,
				i, arches[i%len(arches)], states[i%len(states)], owners[i%len(owners)], (i%8)+1)
		case i%41 == 0:
			text = fmt.Sprintf(`[ ID = %d; Arch = %d; Owner = "%s"; Cpus = %d; Memory = %d ]`,
				i, i, owners[i%len(owners)], (i%8)+1, ((i%16)+1)*512)
		default:
			text = fmt.Sprintf(`[ ID = %d; Arch = "%s"; State = "%s"; Owner = "%s"; Cpus = %d; Memory = %d ]`,
				i, arches[i%len(arches)], states[i%len(states)], owners[i%len(owners)], (i%8)+1, ((i%16)+1)*512)
		}
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatalf("parse ad %d: %v", i, err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
		src[i] = ad
	}
	c.Reindex()
	return c, src
}

func runIndexBattery(t *testing.T, c *Collection, src map[int]*classad.ClassAd, label string) {
	t.Helper()
	for _, qs := range indexQueryBattery {
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatalf("parse query %q: %v", qs, err)
		}
		got := queryIDs(t, c, q)
		want := bruteIDs(src, q)
		if !equalInts(got, want) {
			t.Errorf("[%s] query %q:\n  index got %d\n  brute   %d\n  got=%v\n  want=%v",
				label, qs, len(got), len(want), got, want)
		}
	}
}

// TestPersistentIndexMatchesFullScan verifies indexed queries on a persistent
// collection are identical to brute force, both while open and after a reopen (where
// the in-memory index is rebuilt from the recovered segments).
func TestPersistentIndexMatchesFullScan(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, src := buildPersistentIndexedCorpus(t, dir)

	// Sanity: the index is actually consulted (a categorical probe resolves).
	if cat, val := c.IndexedAttrs(); len(cat) != 3 || len(val) != 2 {
		t.Fatalf("IndexedAttrs = %v / %v, want 3 categorical + 2 value", cat, val)
	}
	runIndexBattery(t, c, src, "open")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen with the same index configuration: recovery rebuilds the index.
	c2, err := Open(Options{
		Shards:           8,
		Dir:              dir,
		CategoricalAttrs: []string{"Arch", "State", "Owner"},
		ValueAttrs:       []string{"Cpus", "Memory"},
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	if c2.Len() != len(src) {
		t.Fatalf("recovered %d ads, want %d", c2.Len(), len(src))
	}
	runIndexBattery(t, c2, src, "reopen")
}

// TestPersistentIndexAfterUpdatesAndDelete checks the index stays correct across
// updates (a new record supersedes the old, indexed in a newer segment) and deletes,
// on a persistent collection.
func TestPersistentIndexAfterUpdatesAndDelete(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, src := buildPersistentIndexedCorpus(t, dir)

	// Update a slice of keys to a new Owner, and delete another slice.
	for i := 0; i < 400; i += 5 {
		ad, _ := classad.Parse(fmt.Sprintf(`[ ID = %d; Arch = "X86_64"; State = "Claimed"; Owner = "zoe"; Cpus = 7; Memory = 8192 ]`, i))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
		src[i] = ad
	}
	for i := 1; i < 400; i += 11 {
		c.Delete([]byte(fmt.Sprintf("k%d", i)))
		delete(src, i)
	}
	c.Reindex()
	runIndexBattery(t, c, src, "after-mutations")
	// The mutated Owner is queryable via the index.
	q, _ := vm.Parse(`Owner == "zoe" && Cpus == 7`)
	if got, want := queryIDs(t, c, q), bruteIDs(src, q); !equalInts(got, want) {
		t.Fatalf("zoe query: got %v want %v", got, want)
	}
	c.Close()
}

// TestPersistentIndexConcurrent stresses indexed queries on a persistent collection
// under concurrent writers, reindexing, and compaction (which retires and reaps the
// mmap segments an index and an in-flight indexed scan reference). Run with -race.
// Each single query must see every matching key at most once (exactly-once).
func TestPersistentIndexConcurrent(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{
		Shards:           4,
		Dir:              dir,
		SegmentSize:      1 << 13,
		CategoricalAttrs: []string{"Arch"},
		ValueAttrs:       []string{"Cpus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const keys = 200
	for i := 0; i < keys; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustParse(t, fmt.Sprintf(`[ ID=%d; Arch="X86_64"; Cpus=%d ]`, i, i%8))); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	q, _ := vm.Parse(`Cpus >= 4 && Arch == "X86_64"`)

	var stop atomic.Bool
	var bg, readers sync.WaitGroup
	// Reindexer + compactor: race the atomic segIndex swap and segment retirement
	// against indexed readers.
	bg.Add(2)
	go func() {
		defer bg.Done()
		for !stop.Load() {
			c.Reindex()
		}
	}()
	go func() {
		defer bg.Done()
		for !stop.Load() {
			c.Compact()
		}
	}()
	// Writers: churn values (moving ids between Cpus buckets), delete + re-insert.
	for w := 0; w < 2; w++ {
		bg.Add(1)
		go func(w int) {
			defer bg.Done()
			for r := 0; !stop.Load(); r++ {
				i := (r*2 + w) % keys
				k := []byte(fmt.Sprintf("k%d", i))
				_ = c.Put(k, mustParse(t, fmt.Sprintf(`[ ID=%d; Arch="X86_64"; Cpus=%d ]`, i, (i+r)%8)))
			}
		}(w)
	}
	// Readers: run the index query; no id may appear twice in one result.
	for rdr := 0; rdr < 3; rdr++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for it := 0; it < 200; it++ {
				seen := map[int]bool{}
				for ad := range c.Query(q) {
					id, ok := ad.EvaluateAttrInt("ID")
					if !ok {
						t.Errorf("ad without ID")
						return
					}
					if seen[int(id)] {
						t.Errorf("duplicate id %d in one indexed query result", id)
						return
					}
					seen[int(id)] = true
				}
			}
		}()
	}
	readers.Wait()
	stop.Store(true)
	bg.Wait()
}
