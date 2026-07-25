package collections

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// sealedIndexed returns (sealed, indexed): the number of sealed (non-active) segments and how
// many of them already carry a sidecar index.
func sealedIndexed(a *Archive) (sealed, indexed int) {
	sh := a.c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	for _, s := range sh.segs {
		if s != nil && s != sh.act {
			sealed++
			if s.msidx.Load() != nil {
				indexed++
			}
		}
	}
	return
}

// TestArchiveEagerIndex verifies that appending to an archive builds the sidecar index of
// each segment as it seals -- so a categorical/value query on just-appended history is
// index-accelerated immediately, without waiting for a periodic reindex or a reopen.
func TestArchiveEagerIndex(t *testing.T) {
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 12, ValueAttrs: []string{"G"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const n = 500
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N=%d; G=%d ]`, i, i%4))
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}

	// Every sealed segment already has a sidecar index -- no explicit Reindex, no reopen.
	sealed, indexed := sealedIndexed(a)
	if sealed < 2 {
		t.Fatalf("expected several sealed segments, got %d", sealed)
	}
	if indexed != sealed {
		t.Errorf("eager index: %d/%d sealed segments have a sidecar (want all)", indexed, sealed)
	}

	// The query is correct over the indexed archive.
	q, _ := vm.Parse(`G == 2`)
	cnt := 0
	for range a.Query(q) {
		cnt++
	}
	if cnt != n/4 {
		t.Errorf("G==2 returned %d, want %d", cnt, n/4)
	}
}

// TestArchiveEagerIndexNoAttrs verifies the eager path is inert (no reindex churn) when the
// archive configures no per-segment indexes -- appends must still succeed and query correctly.
func TestArchiveEagerIndexNoAttrs(t *testing.T) {
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 12}) // no ValueAttrs/CategoricalAttrs
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 300; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N=%d ]`, i))
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	if a.Count() != 300 {
		t.Fatalf("Count = %d, want 300", a.Count())
	}
	q, _ := vm.Parse(`N >= 150`)
	cnt := 0
	for range a.Query(q) {
		cnt++
	}
	if cnt != 150 {
		t.Errorf("N>=150 returned %d, want 150", cnt)
	}
}

// TestArchiveEagerIndexConcurrent exercises the new concurrency: eager reindex (from the
// appending writer) racing the periodic maintenance reindex and concurrent queries. Run
// under -race; reindexMu must serialize the two reindexers.
func TestArchiveEagerIndexConcurrent(t *testing.T) {
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 12, ValueAttrs: []string{"G"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	done := make(chan struct{})
	var wg sync.WaitGroup
	// Periodic "maintenance" reindexer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			a.Reindex()
		}
	}()
	// Concurrent queriers.
	q, _ := vm.Parse(`G == 1`)
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				for range a.Query(q) {
				}
			}
		}()
	}
	// Single writer appending (each seal triggers an eager reindex).
	for i := 0; i < 600; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N=%d; G=%d ]`, i, i%4))
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	wg.Wait()

	if a.Count() != 600 {
		t.Errorf("Count = %d, want 600", a.Count())
	}
}
