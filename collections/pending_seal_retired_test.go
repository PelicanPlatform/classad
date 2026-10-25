package collections

import (
	"fmt"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestSealWalkSkipsRetiredSegments: a seal names the sealed segment in pendingSeal for the next
// commit's collapse to walk. A seal made while a collapse is already running -- by the collapse's
// own commits -- stays there until a later commit, and nothing removed it when a rewrite retired
// that segment in between. The walk then read a segment whose mapping was gone: a fault, or, with
// the address already reused by a newer segment, another segment's bytes read as records.
//
// Not parallel: it installs the package's walk hook.
func TestSealWalkSkipsRetiredSegments(t *testing.T) {
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 13, DeltaMax: 8, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sh := c.shards[0]
	var stale atomic.Int64
	liveDeltaWalkHook = func(sh *shard, seg *segment) {
		if !slices.Contains(sh.segs, seg) {
			stale.Add(1)
		}
	}
	defer func() { liveDeltaWalkHook = nil }()
	retiredBefore := pendingSealRetired.Load()

	// One delta, so the store replays them and its commits collapse.
	if perr := c.Put([]byte("d.0"), jobAd(0, nil)); perr != nil {
		t.Fatal(perr)
	}
	patch := classad.New()
	patch.InsertAttr("N", 1)
	tx := c.Begin()
	tx.PatchAttrs([]byte("d.0"), patch, nil)
	tx.Commit()
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no delta written")
	}
	// Seal segments while a collapse is in flight, as the collapse's own commits do: each seal is
	// recorded for a later commit to walk.
	c.collapsing.Store(true)
	for i := 0; i < 400; i++ {
		if perr := c.Put([]byte(fmt.Sprintf("%d.0", i)), jobAd(i, nil)); perr != nil {
			t.Fatal(perr)
		}
	}
	sh.mu.RLock()
	pending := slices.Clone(sh.pendingSeal)
	sh.mu.RUnlock()
	if len(pending) == 0 {
		t.Fatal("no segment awaits the walk; the test does not reach the case")
	}
	// A rewrite retires them before that commit comes.
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema")
	}
	c.ColumnarizeSealed()
	c.collapsing.Store(false)
	if perr := c.Put([]byte("x.0"), jobAd(9999, nil)); perr != nil {
		t.Fatal(perr)
	}

	if s := stale.Load(); s != 0 {
		t.Fatalf("the seal-time walk read %d segments that had already been retired", s)
	}
	if pendingSealRetired.Load() == retiredBefore {
		t.Fatal("no pending segment was retired before its walk; the test does not reach the case")
	}
	for i := 0; i < 400; i += 37 {
		if _, ok := c.Get([]byte(fmt.Sprintf("%d.0", i))); !ok {
			t.Fatalf("%d.0 lost", i)
		}
	}
}
