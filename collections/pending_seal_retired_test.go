package collections

import (
	"fmt"
	"math/rand"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestSealWalkSkipsRetiredSegments: a seal names the sealed segment in pendingSeal for the next
// commit's collapse to walk, and nothing removes it when a rewrite -- columnarizing, compaction --
// retires that segment first. The walk then read a segment whose mapping was gone: a fault, or,
// with the address already reused by a newer segment, another segment's bytes read as records.
//
// Not parallel: it installs the package's walk hook.
func TestSealWalkSkipsRetiredSegments(t *testing.T) {
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 13, DeltaMax: 8, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var stale atomic.Int64
	liveDeltaWalkHook = func(sh *shard, seg *segment) {
		if !slices.Contains(sh.segs, seg) {
			stale.Add(1)
		}
	}
	defer func() { liveDeltaWalkHook = nil }()
	retiredBefore := pendingSealRetired.Load()

	// A seed under which a rewrite retires a segment between its seal and its walk -- the
	// interleaving depends on how commits fall across segment boundaries.
	rng := rand.New(rand.NewSource(8))
	const n = 300
	for i := 0; i < n; i++ {
		if perr := c.Put([]byte(fmt.Sprintf("%d.0", i)), jobAd(i, nil)); perr != nil {
			t.Fatal(perr)
		}
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema")
	}
	for range 120 {
		for range 1 + rng.Intn(30) {
			patch := classad.New()
			patch.InsertAttr("N", int64(rng.Intn(100)))
			tx := c.Begin()
			tx.PatchAttrs([]byte(fmt.Sprintf("%d.0", rng.Intn(n))), patch, nil)
			tx.Commit()
		}
		if rng.Intn(2) == 0 {
			c.ColumnarizeSealed()
		} else {
			c.Compact()
		}
	}
	if s := stale.Load(); s != 0 {
		t.Fatalf("the seal-time walk read %d segments that had already been retired", s)
	}
	if pendingSealRetired.Load() == retiredBefore {
		t.Fatal("no pending segment was retired before its walk; the test does not reach the case")
	}
	for i := 0; i < n; i++ {
		if _, ok := c.Get([]byte(fmt.Sprintf("%d.0", i))); !ok {
			t.Fatalf("%d.0 lost", i)
		}
	}
}
