//go:build unix

package collections

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

// These tests inject ENOSPC at ONE rebuildable-metadata write site at a time (via writeFaultHook,
// keyed by path), so it is deterministic which path is under test -- complementing the aggregate
// full-filesystem test. Each asserts the site's write was actually reached AND that the failure
// degrades gracefully (rebuild/scan/clean error), never corrupting data or losing acknowledged
// writes across a reopen.

// verifyAll checks every key k0..upto-1 that survives reads back with Id==i (no wrong values).
func verifyAll(t *testing.T, c *Collection, upto int, label string) {
	t.Helper()
	for i := 0; i < upto; i++ {
		ad, ok := c.Get([]byte(fmt.Sprintf("k%d", i)))
		if !ok {
			t.Errorf("%s: k%d missing", label, i)
			continue
		}
		if id, ok := ad.EvaluateAttrInt("Id"); !ok || id != int64(i) {
			t.Errorf("%s: k%d wrong value Id=%d,ok=%v", label, i, id, ok)
		}
	}
}

// TestENOSPCSidecarWriteDegrades: failing the per-segment key-index sidecar (.idx) write must not
// fail the store or corrupt data -- the sidecar is rebuildable, so a segment falls back to a scan
// and reopen reconstructs it.
func TestENOSPCSidecarWriteDegrades(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}

	var fired atomic.Bool
	writeFaultHook = func(p string) error {
		if strings.HasSuffix(p, ".idx") {
			fired.Store(true)
			return unix.ENOSPC
		}
		return nil
	}
	defer func() { writeFaultHook = nil }()

	const n = 3000
	for i := 0; i < n; i++ {
		ad := mustAd(t, fmt.Sprintf(`[Id=%d;pad=%q]`, i, strings.Repeat("x", 200)))
		if putErr := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); putErr != nil {
			t.Fatalf("Put(k%d) failed on a (rebuildable) sidecar ENOSPC: %v", i, putErr)
		}
	}
	c.Reindex() // rebuilds sealed-segment sidecars synchronously -> writes .idx (which the hook fails)
	if !fired.Load() {
		t.Fatal("sidecar (.idx) write site was never reached; test did not exercise it")
	}
	verifyAll(t, c, n, "pre-reopen")

	writeFaultHook = nil
	if closeErr := c.Close(); closeErr != nil {
		t.Logf("Close returned %v (acceptable)", closeErr)
	}
	c2, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatalf("reopen failed after sidecar ENOSPC: %v", err)
	}
	defer c2.Close()
	if c2.Len() != n {
		t.Fatalf("after reopen Len=%d, want %d", c2.Len(), n)
	}
	verifyAll(t, c2, n, "reopen")
}

// TestENOSPCSnapshotWriteDegrades: failing the clean-shutdown directory snapshot must not corrupt
// the store -- reopen falls back to a full segment scan and recovers every record.
func TestENOSPCSnapshotWriteDegrades(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	const n = 500
	for i := 0; i < n; i++ {
		if putErr := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); putErr != nil {
			t.Fatal(putErr)
		}
	}

	var fired atomic.Bool
	writeFaultHook = func(p string) error {
		if strings.HasSuffix(p, "dir.snap") {
			fired.Store(true)
			return unix.ENOSPC
		}
		return nil
	}
	defer func() { writeFaultHook = nil }()

	// Close writes the snapshot; the failing write makes Close report an error (acceptable).
	_ = c.Close()
	if !fired.Load() {
		t.Fatal("dir-snapshot write site was never reached; test did not exercise it")
	}

	writeFaultHook = nil
	c2, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatalf("reopen failed after snapshot ENOSPC: %v", err)
	}
	defer c2.Close()
	if c2.Len() != n {
		t.Fatalf("after reopen (no snapshot -> scan) Len=%d, want %d", c2.Len(), n)
	}
	verifyAll(t, c2, n, "reopen-scan")
}

// TestENOSPCDictWriteFailsCleanly: failing the dictionary (.zst) write during a retrain must
// surface an error (the dict is not rebuildable -- segments reference it) and must not corrupt the
// already-committed data; a reopen stays consistent.
func TestENOSPCDictWriteFailsCleanly(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	// retrainAd produces dictionary-trainable ads (Owner="user_i", etc.); RetrainDict then trains
	// and persists a real .zst dictionary.
	const n = 400
	wantOwner := func(i int) string { return fmt.Sprintf("user_%d", i) }
	for i := 0; i < n; i++ {
		if putErr := c.Put([]byte(fmt.Sprintf("k%d", i)), retrainAd(t, i)); putErr != nil {
			t.Fatal(putErr)
		}
	}
	verifyOwner := func(store *Collection, label string) {
		t.Helper()
		for i := 0; i < n; i++ {
			ad, ok := store.Get([]byte(fmt.Sprintf("k%d", i)))
			if !ok {
				t.Errorf("%s: k%d missing", label, i)
				continue
			}
			if o, ok := ad.EvaluateAttrString("Owner"); !ok || o != wantOwner(i) {
				t.Errorf("%s: k%d Owner=%q,ok=%v want %q", label, i, o, ok, wantOwner(i))
			}
		}
	}

	var fired atomic.Bool
	writeFaultHook = func(p string) error {
		if strings.HasSuffix(p, ".zst") {
			fired.Store(true)
			return unix.ENOSPC
		}
		return nil
	}
	defer func() { writeFaultHook = nil }()

	// RetrainDict persists a new dictionary; the failing .zst write must be reported, not swallowed.
	if _, retrainDictErr := c.RetrainDict(n); retrainDictErr == nil {
		t.Error("RetrainDict should fail when the dictionary write hits ENOSPC")
	}
	if !fired.Load() {
		t.Fatal("dict (.zst) write site was never reached; test did not exercise it")
	}

	// The pre-retrain data must be intact (a failed retrain must not corrupt or drop it).
	writeFaultHook = nil
	verifyOwner(c, "post-failed-retrain")
	if closeErr := c.Close(); closeErr != nil {
		t.Logf("Close returned %v", closeErr)
	}
	c2, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatalf("reopen failed after dict ENOSPC: %v", err)
	}
	defer c2.Close()
	if c2.Len() != n {
		t.Fatalf("after reopen Len=%d, want %d", c2.Len(), n)
	}
	verifyOwner(c2, "reopen")
}
