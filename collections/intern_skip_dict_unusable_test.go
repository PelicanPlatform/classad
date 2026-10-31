package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestInternSealedSkipsUnusableDictionary: a sealed segment whose dictionary failed its checksum has
// no published dictionary, which interning took to mean the segment was still inline. It is interned
// -- its records carry ids nothing can resolve -- so the transcode failed on its first record and was
// retried on every InternSealed. It is now passed over and counted once, and the pass still interns
// the segments it can.
//
// Not parallel: the skip counter is process-wide.
func TestInternSealedSkipsUnusableDictionary(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	opts := ArchiveOptions{Dir: dir, SegmentSize: 1 << 16}
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	appendN := func(a *Archive, from, n int) {
		t.Helper()
		for i := from; i < from+n; i++ {
			if aerr := a.Append(mustAd(t, fmt.Sprintf(`[Id = %d; Val = %d; Host = "h%d.example.org"]`, i, i%100, i))); aerr != nil {
				t.Fatal(aerr)
			}
		}
	}
	appendN(a, 0, 2000)
	a.c.InternSealed()
	if sealed, interned := countSealedSegs(a.c); sealed < 2 || interned != sealed {
		t.Fatalf("setup: %d of %d sealed segments interned, want all of at least 2", interned, sealed)
	}
	sh := a.c.shards[0]
	sh.mu.RLock()
	first := sh.segs[0]
	path, dict := first.path, first.dictRecOff()
	sh.mu.RUnlock()
	if dict == 0 {
		t.Fatal("setup: the first segment has no dictionary record")
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	// A bit in the dictionary's body: its checksum fails, its framing does not.
	flipBit(t, path, int(dict)+recKeyOff+4+segDictHeaderBytes+8)
	_ = os.Remove(filepath.Join(filepath.Dir(path), dirSnapName))

	if a, err = OpenArchive(opts); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	sh = a.c.shards[0]
	sh.mu.RLock()
	bad := sh.segs[0]
	sh.mu.RUnlock()
	if !bad.dictUnusable.Load() || bad.dict.Load() != nil {
		t.Fatal("setup: open did not refuse the first segment's dictionary")
	}

	// More data, so the pass has an inline segment it can intern alongside the one it must not try.
	appendN(a, 2000, 1000)
	skipped := InternSkippedDictUnusable()
	a.c.InternSealed()
	a.c.InternSealed()
	if got := InternSkippedDictUnusable() - skipped; got != 1 {
		t.Errorf("InternSkippedDictUnusable rose by %d over two passes, want 1 (the one segment, once)", got)
	}
	sh.mu.RLock()
	still := sh.segs[0] == bad
	sh.mu.RUnlock()
	if !still || !bad.dictUnusable.Load() {
		t.Error("the segment with the unusable dictionary was replaced")
	}
	sealed, interned := countSealedSegs(a.c)
	if interned != sealed-1 {
		t.Errorf("%d of %d sealed segments interned, want every one but the damaged segment", interned, sealed)
	}
}
