package collections

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// putDynAds populates c with n ads carrying ID, a string Owner/Arch and a numeric
// Memory, and returns the source ads keyed by ID for brute-force checking.
func putDynAds(t *testing.T, c *Collection, n int) map[int]*classad.ClassAd {
	t.Helper()
	owners := []string{"alice", "bob", "carol", "dave", "eve"}
	arches := []string{"X86_64", "aarch64"}
	src := map[int]*classad.ClassAd{}
	for i := 0; i < n; i++ {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ID=%d; Owner="%s"; Arch="%s"; Memory=%d ]`,
			i, owners[i%len(owners)], arches[i%len(arches)], ((i%16)+1)*512))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
		src[i] = ad
	}
	return src
}

// assertMatchesBrute fails if the collection query result differs from evaluating q
// against every source ad — the invariant that must hold no matter what the index
// state is.
func assertMatchesBrute(t *testing.T, c *Collection, src map[int]*classad.ClassAd, qs string) {
	t.Helper()
	q, err := vm.Parse(qs)
	if err != nil {
		t.Fatalf("parse %q: %v", qs, err)
	}
	got := queryIDs(t, c, q)
	want := bruteIDs(src, q)
	if !equalInts(got, want) {
		t.Errorf("query %q: got %v, want %v", qs, got, want)
	}
}

// attrCoverage reports, across all non-empty live segments, how many have an index
// covering attr and the total segment count. segsWith==0 means fully reclaimed;
// segsWith==total (and total>0) means fully backfilled.
func attrCoverage(c *Collection, name string) (segsWith, total int) {
	id, ok := c.intern.LookupID(name)
	if !ok {
		return 0, 0
	}
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 {
				continue
			}
			total++
			if si := seg.idx.Load(); si != nil && (si.cat[id] != nil || si.val[id] != nil) {
				segsWith++
			}
		}
		sh.mu.RUnlock()
	}
	return segsWith, total
}

// segsWithIndex counts non-empty live segments that currently have any index.
func segsWithIndex(c *Collection) int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 {
				continue
			}
			if seg.idx.Load() != nil {
				n++
			}
		}
		sh.mu.RUnlock()
	}
	return n
}

// TestDynamicAddBackfill: adding an index at runtime is correct before it is built
// (full scan), and Reindex backfills it into existing segments.
func TestDynamicAddBackfill(t *testing.T) {
	c := New(Options{Shards: 4}) // no indexes
	src := putDynAds(t, c, 300)

	// Unindexed: correct via full scan, and no segment covers Owner.
	assertMatchesBrute(t, c, src, `Owner == "alice"`)
	if w, _ := attrCoverage(c, "Owner"); w != 0 {
		t.Fatalf("Owner covered before AddIndex: %d segments", w)
	}

	if !c.AddIndex([]string{"Owner"}, []string{"Memory"}) {
		t.Fatal("AddIndex reported no change")
	}
	// After add, before Reindex: still correct (existing segments full-scanned).
	assertMatchesBrute(t, c, src, `Owner == "alice"`)
	assertMatchesBrute(t, c, src, `Owner == "bob" && Memory > 4096`)
	if w, _ := attrCoverage(c, "Owner"); w != 0 {
		t.Fatalf("Owner covered before Reindex: %d segments", w)
	}

	c.Reindex()
	// Backfilled into every segment, still correct.
	if w, total := attrCoverage(c, "Owner"); total == 0 || w != total {
		t.Fatalf("Owner coverage after Reindex: %d/%d segments", w, total)
	}
	if w, total := attrCoverage(c, "Memory"); w != total {
		t.Fatalf("Memory coverage after Reindex: %d/%d segments", w, total)
	}
	assertMatchesBrute(t, c, src, `Owner == "alice"`)
	assertMatchesBrute(t, c, src, `Owner == "bob" && Memory > 4096`)
	assertMatchesBrute(t, c, src, `Memory <= 1024`)
}

// TestDynamicDropReclaim: dropping an index stops it being consulted immediately and
// reclaims its postings at the next Reindex; dropping the last index clears segment
// indexes entirely.
func TestDynamicDropReclaim(t *testing.T) {
	c := New(Options{
		Shards:           4,
		CategoricalAttrs: []string{"Owner"},
		ValueAttrs:       []string{"Memory"},
	})
	src := putDynAds(t, c, 300)
	c.Reindex()
	if w, total := attrCoverage(c, "Owner"); total == 0 || w != total {
		t.Fatalf("Owner not indexed after initial Reindex: %d/%d", w, total)
	}

	if !c.DropIndex("Owner") {
		t.Fatal("DropIndex(Owner) reported no change")
	}
	// Correct immediately (now full-scanned for Owner), Memory still indexed.
	assertMatchesBrute(t, c, src, `Owner == "alice"`)
	assertMatchesBrute(t, c, src, `Memory > 4096`)

	c.Reindex()
	if w, _ := attrCoverage(c, "Owner"); w != 0 {
		t.Fatalf("Owner postings not reclaimed after Reindex: %d segments", w)
	}
	if w, total := attrCoverage(c, "Memory"); total == 0 || w != total {
		t.Fatalf("Memory coverage lost: %d/%d", w, total)
	}
	assertMatchesBrute(t, c, src, `Owner == "alice"`)

	// Drop the last index -> segment indexes cleared.
	if !c.DropIndex("Memory") {
		t.Fatal("DropIndex(Memory) reported no change")
	}
	c.Reindex()
	if n := segsWithIndex(c); n != 0 {
		t.Fatalf("segment indexes not cleared after dropping all: %d remain", n)
	}
	assertMatchesBrute(t, c, src, `Memory > 4096`)
}

// TestAddDropSemantics covers idempotence, categorical-over-value precedence, and
// no-op drops.
func TestAddDropSemantics(t *testing.T) {
	c := New(Options{Shards: 2})

	if !c.AddIndex([]string{"Owner"}, nil) {
		t.Error("first AddIndex should change config")
	}
	if c.AddIndex([]string{"Owner"}, nil) {
		t.Error("re-adding Owner should be a no-op")
	}

	// Value then categorical for the same attr: categorical wins.
	c.AddIndex(nil, []string{"Memory"})
	if _, val := c.IndexedAttrs(); !contains(val, "Memory") {
		t.Errorf("Memory should be a value index, got val=%v", val)
	}
	if !c.AddIndex([]string{"Memory"}, nil) {
		t.Error("promoting Memory to categorical should change config")
	}
	cat, val := c.IndexedAttrs()
	if !contains(cat, "Memory") || contains(val, "Memory") {
		t.Errorf("Memory should be categorical only: cat=%v val=%v", cat, val)
	}

	// Same attr in both args of one call -> categorical only.
	c2 := New(Options{Shards: 2})
	c2.AddIndex([]string{"X"}, []string{"X"})
	cat, val = c2.IndexedAttrs()
	if !contains(cat, "X") || contains(val, "X") {
		t.Errorf("X should be categorical only: cat=%v val=%v", cat, val)
	}

	if c.DropIndex("NeverInterned") {
		t.Error("dropping an unknown attr should be a no-op")
	}
	if !c.DropIndex("Owner") {
		t.Error("dropping Owner should change config")
	}
}

func TestSuggestDropsUnused(t *testing.T) {
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Owner", "Arch"}})
	src := putDynAds(t, c, 200)
	c.Reindex()
	for i := 0; i < 5; i++ {
		assertMatchesBrute(t, c, src, `Owner == "alice"`) // Owner queried, Arch never
	}
	drops := c.SuggestDrops(1000)
	var archFlagged, ownerFlagged bool
	for _, d := range drops {
		if d.Attr == "Arch" && d.Reason == "unused" {
			archFlagged = true
		}
		if d.Attr == "Owner" {
			ownerFlagged = true
		}
	}
	if !archFlagged {
		t.Errorf("Arch (never queried) not flagged unused; drops=%+v", drops)
	}
	if ownerFlagged {
		t.Errorf("Owner (queried) should not be flagged; drops=%+v", drops)
	}
}

func TestAutoTuneAdds(t *testing.T) {
	c := New(Options{Shards: 4})
	src := putDynAds(t, c, 300)
	for i := 0; i < 5; i++ {
		assertMatchesBrute(t, c, src, `Owner == "alice"`)
		assertMatchesBrute(t, c, src, `Memory > 4096`)
	}
	res := c.AutoTune(AutoTuneOptions{Reindex: true})
	if !res.Changed {
		t.Fatal("AutoTune made no change despite demand")
	}
	kinds := map[string]string{}
	for _, ch := range res.Changes {
		if ch.Action != "add" {
			t.Errorf("unexpected action %q", ch.Action)
		}
		kinds[ch.Attr] = ch.Kind
	}
	if kinds["Owner"] != "categorical" {
		t.Errorf("Owner: got %q, want categorical", kinds["Owner"])
	}
	if kinds["Memory"] != "value" {
		t.Errorf("Memory: got %q, want value", kinds["Memory"])
	}
	// Reindex:true means the new indexes are already built.
	if w, total := attrCoverage(c, "Owner"); total == 0 || w != total {
		t.Errorf("Owner not backfilled by AutoTune: %d/%d", w, total)
	}
	assertMatchesBrute(t, c, src, `Owner == "alice" && Memory > 4096`)
}

func TestAutoTuneDropsUnused(t *testing.T) {
	// Ghost is configured but no ad has it and no query uses it -> unused -> dropped.
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Ghost"}})
	putDynAds(t, c, 100)
	res := c.AutoTune(AutoTuneOptions{DropUnused: true, Reindex: true})
	if !res.Changed {
		t.Fatal("AutoTune should have dropped the unused Ghost index")
	}
	if cat, val := c.IndexedAttrs(); len(cat) != 0 || len(val) != 0 {
		t.Errorf("Ghost not dropped: cat=%v val=%v", cat, val)
	}
	// Without DropUnused, an unused index is left alone.
	c2 := New(Options{Shards: 4, CategoricalAttrs: []string{"Ghost"}})
	putDynAds(t, c2, 100)
	if res := c2.AutoTune(AutoTuneOptions{Reindex: true}); res.Changed {
		t.Errorf("AutoTune without DropUnused should not drop: %+v", res.Changes)
	}
}

// TestDynamicConcurrent hammers a static dataset with concurrent index mutation,
// reindexing, and querying. Because the data never changes, every query must always
// equal the brute-force answer regardless of index state; a mismatch or a race is a
// bug. Run under -race.
func TestDynamicConcurrent(t *testing.T) {
	c := New(Options{Shards: 8})
	src := putDynAds(t, c, 500)
	const qs = `Owner == "alice" && Memory > 4096`
	q, err := vm.Parse(qs)
	if err != nil {
		t.Fatal(err)
	}
	want := bruteIDs(src, q)

	stop := make(chan struct{})
	var mutators, readers sync.WaitGroup

	// Mutator: cycle the index configuration until the readers are done.
	mutators.Add(1)
	go func() {
		defer mutators.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			switch i % 4 {
			case 0:
				c.AddIndex([]string{"Owner"}, []string{"Memory"})
			case 1:
				c.Reindex()
			case 2:
				c.DropIndex("Owner")
			case 3:
				c.AddIndex([]string{"Arch"}, nil)
			}
		}
	}()

	// Readers: results must stay correct throughout.
	for r := 0; r < 3; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 200; i++ {
				got := queryIDs(t, c, q)
				if !equalInts(got, want) {
					t.Errorf("concurrent query mismatch: got %d, want %d", len(got), len(want))
					return
				}
			}
		}()
	}

	readers.Wait()
	close(stop)
	mutators.Wait()
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}
