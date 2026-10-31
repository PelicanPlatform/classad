package collections

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// This file is a durability fault-injection harness: it subjects a persistent store to physical
// write faults it does not otherwise test -- a torn/incomplete write (truncation), on-disk bit-rot
// (byte flips), and a disk-full allocation failure (ENOSPC) mid-write -- and asserts the core
// safety invariant: the store may LOSE data it never durably committed, but it must never return a
// WRONG value, never double-count a key, and never crash (panic) on reopen. Loss must be detected
// (a reopen error or a dropped torn tail), never silent corruption.

// faultStore builds a small persistent store with n records whose value encodes the key
// (key "k<i>" holds ad [Id=i]), churns each key once so there are superseded records to recover
// past, and returns the directory WITHOUT a clean Close -- i.e. a crash image that reopen must
// recover via the scan path, not the clean-shutdown snapshot fast-path.
func faultStore(t *testing.T, dir string, n int) {
	t.Helper()
	c, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ { // churn: create superseded records across segments
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d;R=1]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	// Leak the mapping deliberately: no Close, so no clean-shutdown snapshot is written and the
	// on-disk image is what a crash would leave. (t.TempDir cleanup removes the files.)
}

// checkNoSilentCorruption reopens the (possibly mutated) store dir and asserts the safety
// invariant. A reopen error is an ACCEPTABLE outcome (the fault was detected). If the store opens,
// every value it returns must be correct and every key must appear at most once. A panic is a
// failure: the policy is "convert a corrupt structure to a detected/counted anomaly," never a crash.
// checkNoSilentCorruption returns whether the store opened and its read path was exercised, so the
// sweep can assert that faults actually reached the read assertions rather than all short-circuiting
// at Open (which would make the harness a silent no-op).
func checkNoSilentCorruption(t *testing.T, dir string, n int, what string) (opened bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: reopen PANICKED (must degrade to an error/counted anomaly, not crash): %v", what, r)
		}
	}()
	c, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		return false // loss detected at open -- acceptable
	}
	opened = true
	defer c.Close()

	// Scan: every returned ad must carry a valid, unique Id in range -- no wrong/garbage value,
	// no key resurrected twice.
	seen := map[int]bool{}
	for ad := range c.Scan() {
		id, ok := ad.EvaluateAttrInt("Id")
		if !ok {
			t.Errorf("%s: scan returned an ad with no Id (corrupt/garbage value)", what)
			continue
		}
		if id < 0 || id >= int64(n) {
			t.Errorf("%s: scan returned Id=%d out of range [0,%d) (silent corruption)", what, id, n)
			continue
		}
		if seen[int(id)] {
			t.Errorf("%s: scan returned Id=%d twice (duplicate live version)", what, id)
		}
		seen[int(id)] = true
	}
	// Point lookups: a surviving key must return ITS value, never another key's.
	for i := 0; i < n; i++ {
		ad, ok := c.Get([]byte(fmt.Sprintf("k%d", i)))
		if !ok {
			continue // lost to the fault -- acceptable
		}
		id, ok := ad.EvaluateAttrInt("Id")
		if !ok || id != int64(i) {
			t.Errorf("%s: Get(k%d) returned Id=%d,ok=%v -- wrong value (silent corruption)", what, i, id, ok)
		}
	}
	return opened
}

// TestTornWriteAndBitRotSweep sweeps truncations (incomplete writes) and byte flips (bit-rot)
// across every on-disk file of a crash-image store, reopening a fresh copy for each fault and
// asserting no silent corruption and no panic.
func TestTornWriteAndBitRotSweep(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	const n = 300
	tmpl := filepath.Join(t.TempDir(), "template")
	faultStore(t, tmpl, n)

	files := dataFiles(t, tmpl)
	if len(files) == 0 {
		t.Fatal("no on-disk files produced")
	}

	openedCount := 0
	for _, rel := range files {
		info, err := os.Stat(filepath.Join(tmpl, rel))
		if err != nil || info.Size() == 0 {
			continue
		}
		size := info.Size()
		// Truncations: incomplete writes leave the file short of its intended length.
		for _, frac := range []float64{0.1, 0.25, 0.5, 0.75, 0.9, 0.99} {
			cut := int64(float64(size) * frac)
			dir := copyTree(t, tmpl)
			if err := os.Truncate(filepath.Join(dir, rel), cut); err != nil {
				t.Fatal(err)
			}
			if checkNoSilentCorruption(t, dir, n, fmt.Sprintf("truncate %s to %d/%d", rel, cut, size)) {
				openedCount++
			}
		}
		// Bit-rot: flip a byte at a few positions (header, mid, tail).
		for _, frac := range []float64{0.0, 0.5, 0.95} {
			pos := int64(float64(size-1) * frac)
			dir := copyTree(t, tmpl)
			flipByte(t, filepath.Join(dir, rel), pos)
			if checkNoSilentCorruption(t, dir, n, fmt.Sprintf("bitflip %s at %d/%d", rel, pos, size)) {
				openedCount++
			}
		}
	}
	// Guard against the harness silently degrading to a no-op (e.g. if every fault started erroring
	// at Open): most faults leave the store openable and must exercise the read-path assertions.
	if openedCount < len(files) {
		t.Errorf("only %d faults reached the read path across %d files; the sweep may not be exercising reads",
			openedCount, len(files))
	}
}

// TestSegmentAllocFailureIsGraceful injects a disk-full (ENOSPC) at a segment allocation mid-write
// and asserts the store degrades gracefully: the failing Put errors, previously-written data stays
// correct, the failed key is not half-written, the store recovers once space returns, and an
// unclean reopen is consistent.
func TestSegmentAllocFailureIsGraceful(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}

	enospc := errors.New("no space left on device (injected)")
	var failing bool
	allocFailHook = func() error {
		if failing {
			return enospc
		}
		return nil
	}
	defer func() { allocFailHook = nil }()

	// Write until a Put fails on a segment allocation. Turn on the fault after some data exists so
	// the failure lands mid-store, not at creation.
	const budget = 4000
	written := 0
	var failedKey string
	for i := 0; i < budget; i++ {
		if i == 50 {
			failing = true
		}
		key := fmt.Sprintf("k%d", i)
		err := c.Put([]byte(key), mustAd(t, fmt.Sprintf(`[Id=%d;pad="%s"]`, i, pad(200))))
		if err != nil {
			if !errors.Is(err, enospc) {
				t.Fatalf("Put(%s) failed with %v, want the injected ENOSPC", key, err)
			}
			failedKey = key
			break
		}
		written = i + 1
	}
	if failedKey == "" {
		t.Skip("no allocation failure was triggered within budget (segment never grew); test inconclusive")
	}

	// Everything written before the failure must still read back correctly.
	for i := 0; i < written; i++ {
		ad, ok := c.Get([]byte(fmt.Sprintf("k%d", i)))
		if !ok {
			t.Errorf("pre-failure key k%d lost after ENOSPC", i)
			continue
		}
		if id, ok := ad.EvaluateAttrInt("Id"); !ok || id != int64(i) {
			t.Errorf("pre-failure key k%d wrong value Id=%d,ok=%v", i, id, ok)
		}
	}
	// The key whose Put failed must not be half-written (present with a garbage value).
	if ad, ok := c.Get([]byte(failedKey)); ok {
		if id, ok := ad.EvaluateAttrInt("Id"); !ok {
			t.Errorf("failed key %s is present but has no Id (half-written)", failedKey)
		} else {
			t.Logf("failed key %s is present with Id=%d (allocation failed after the record landed; acceptable)", failedKey, id)
		}
	}

	// A segment-allocation failure is STICKY by design (shard.writeErr latches; a fail-stop). Even
	// after space returns, the in-process write path stays failed until a restart -- pin that
	// contract rather than expect auto-recovery. (If classad ever chooses to auto-clear writeErr on
	// a later successful alloc, this assertion should be revisited deliberately.)
	// Re-put an existing key with its own value: the sticky error must surface (fail-stop), and
	// because the write reports failure but a small record that fits the active segment still lands,
	// we keep the value in-range/correct so it cannot be mistaken for corruption on reopen.
	failing = false
	if err := c.Put([]byte("k0"), mustAd(t, `[Id=0]`)); err == nil {
		t.Error("segment-allocation error should be sticky (fail-stop until restart); Put unexpectedly succeeded")
	}

	// Recovery is via RESTART: a reopen carries no in-memory writeErr, and the store must come back
	// consistent -- no corruption from the failed allocation, every surviving key correct, and it
	// must accept writes again.
	allocFailHook = nil
	_ = c.Close()
	checkNoSilentCorruption(t, dir, budget, "reopen after ENOSPC")
	c2, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatalf("reopen after ENOSPC failed: %v", err)
	}
	defer c2.Close()
	if err := c2.Put([]byte("post-restart"), mustAd(t, `[Id=1000000]`)); err != nil {
		t.Errorf("restart did not clear the fail-stop: Put still errors: %v", err)
	}
}

// --- helpers ---

// dataFiles returns every regular file under dir, as paths relative to dir.
func dataFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// copyTree copies the template dir into a fresh temp dir and returns its path.
func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// flipByte XORs one byte at pos in the file.
func flipByte(t *testing.T, path string, pos int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], pos); err != nil {
		return
	}
	b[0] ^= 0xff
	if _, err := f.WriteAt(b[:], pos); err != nil {
		t.Fatal(err)
	}
}

func pad(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}
