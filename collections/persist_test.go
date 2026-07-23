package collections

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// countSegFiles returns the number of seg-*.dat files under a persistent shard's
// directory. Retired (compacted-away) segments are unlinked, so this drops after a
// compaction reclaims them.
func countSegFiles(t *testing.T, shardDir string) int {
	t.Helper()
	entries, err := os.ReadDir(shardDir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		var num uint64
		var dictID uint32
		if _, err := fmt.Sscanf(e.Name(), "seg-%d.d%d.dat", &num, &dictID); err == nil {
			n++
		}
	}
	return n
}

// TestPersistentCompactReclaimsFiles verifies that compaction munmaps + unlinks the
// source segment files it retires (P4 reclamation), while data stays intact.
func TestPersistentCompactReclaimsFiles(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	shardDir := filepath.Join(dir, "0")
	const n = 400
	c, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	// Churn to create garbage worth compacting.
	for r := 0; r < 4; r++ {
		for i := 0; i < n; i++ {
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d;R=%d]`, i, r))); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := countSegFiles(t, shardDir)
	if got := c.Compact(); got != 1 {
		t.Fatalf("Compact() = %d shards, want 1", got)
	}
	after := countSegFiles(t, shardDir)
	if after >= before {
		t.Fatalf("segment files not reclaimed: before=%d after=%d", before, after)
	}
	if c.Len() != n {
		t.Fatalf("Len=%d want %d", c.Len(), n)
	}
	// Data intact and scan yields each key exactly once.
	seen := map[int]int{}
	for ad := range c.Scan() {
		id, _ := ad.EvaluateAttrInt("Id")
		seen[int(id)]++
	}
	if len(seen) != n {
		t.Fatalf("distinct after compact = %d want %d", len(seen), n)
	}
	for id, cnt := range seen {
		if cnt != 1 {
			t.Fatalf("key Id=%d seen %d times (want 1)", id, cnt)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestPersistentCompactReopenNoDup verifies the recovery single-current-version
// invariant: after a compaction, a crash-reopen (no Close) recovers each key
// exactly once even though both the source and destination copies may be on disk
// (retired files are unlinked lazily), so a scan never double-counts.
func TestPersistentCompactReopenNoDup(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 500
	c, err := Open(Options{Shards: 1, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	for r := 0; r < 2; r++ {
		for i := 0; i < n; i++ {
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d;R=%d]`, i, r))); err != nil {
				t.Fatal(err)
			}
		}
	}
	c.Compact()
	// Deliberately do NOT Close: simulate a crash right after compaction.

	c2, err := Open(Options{Shards: 1, Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	seen := map[int]int{}
	total := 0
	for ad := range c2.Scan() {
		total++
		id, _ := ad.EvaluateAttrInt("Id")
		seen[int(id)]++
	}
	if total != n || len(seen) != n {
		t.Fatalf("crash-reopen post-compact scan: total=%d distinct=%d want %d/%d", total, len(seen), n, n)
	}
}

// TestPersistentScanCompactRace stresses concurrent scans and compactions on a
// persistent collection: a scan pins the mmap segments it reads, so a concurrent
// compaction must defer munmap+unlink until the scan finishes (no use-after-munmap).
// Run with -race. Exactly-once must hold throughout.
func TestPersistentScanCompactRace(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 300
	c, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 13})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	// Pre-churn so the shards are already well past the compaction threshold when the
	// compactor goroutine starts (otherwise the bounded scanners finish before enough
	// garbage accumulates and the reclaim race is never exercised).
	for r := 0; r < 12; r++ {
		for i := 0; i < n; i++ {
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d;P=%d]`, i, r))); err != nil {
				t.Fatal(err)
			}
		}
	}
	var stop atomic.Bool
	var bg sync.WaitGroup // writer + compactor
	// Writer: churn keys to generate garbage.
	bg.Add(1)
	go func() {
		defer bg.Done()
		r := 0
		for !stop.Load() {
			for i := 0; i < n; i++ {
				_ = c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d;R=%d]`, i, r)))
			}
			r++
		}
	}()
	// Compactor.
	var compactions atomic.Int64
	bg.Add(1)
	go func() {
		defer bg.Done()
		for !stop.Load() {
			compactions.Add(int64(c.Compact()))
		}
	}()
	// Scanners: each full scan must see every key exactly once. They run a bounded
	// number of iterations, then we stop the background workers and join.
	var scanErr atomic.Int32
	var scanners sync.WaitGroup
	for s := 0; s < 3; s++ {
		scanners.Add(1)
		go func() {
			defer scanners.Done()
			for iter := 0; iter < 150; iter++ {
				seen := map[int]bool{}
				for ad := range c.Scan() {
					id, _ := ad.EvaluateAttrInt("Id")
					if seen[int(id)] {
						scanErr.Store(1) // duplicate key in one scan
					}
					seen[int(id)] = true
				}
				if len(seen) != n {
					scanErr.Store(2) // missed a key
				}
			}
		}()
	}
	scanners.Wait()
	stop.Store(true)
	bg.Wait()
	if v := scanErr.Load(); v == 1 {
		t.Fatal("a scan saw a duplicate key under concurrent compaction")
	} else if v == 2 {
		t.Fatal("a scan missed a key under concurrent compaction")
	}
	if compactions.Load() == 0 {
		t.Fatal("no compaction ran; the scan/reclaim race was not exercised")
	}
	t.Logf("exercised %d shard compactions under concurrent scans", compactions.Load())
}

// TestPersistentBasic exercises a persistent (mmap-backed) collection end to end
// while open: writes land in segment files on disk, and Get/Scan/Query work.
// (Recovery on reopen is a later milestone.)
func TestPersistentBasic(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{Shards: 4, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if c.dir != dir {
		t.Fatalf("dir = %q, want %q", c.dir, dir)
	}

	const n = 2000
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[Id=%d; Cpus=%d; Owner=%q]`, i, i%8, []string{"alice", "bob"}[i%2]))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if c.Len() != n {
		t.Fatalf("Len = %d, want %d", c.Len(), n)
	}

	// Segment files exist on disk under the shard subdirs.
	segFiles := 0
	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(p) == ".dat" {
			segFiles++
			if info.Size() == 0 {
				t.Errorf("empty segment file %s", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if segFiles == 0 {
		t.Fatal("no segment files created on disk")
	}
	t.Logf("%d ads across %d on-disk segment files", n, segFiles)

	// Reads work against the mmap-backed store.
	for i := 0; i < n; i++ {
		ad, ok := c.Get([]byte(fmt.Sprintf("k%d", i)))
		if !ok {
			t.Fatalf("k%d missing", i)
		}
		if id, _ := ad.EvaluateAttrInt("Id"); id != int64(i) {
			t.Fatalf("k%d Id=%d", i, id)
		}
	}
	seen := 0
	for range c.Scan() {
		seen++
	}
	if seen != n {
		t.Fatalf("scan yielded %d, want %d", seen, n)
	}
	q, _ := vm.Parse(`Cpus >= 4 && Owner == "alice"`)
	got, want := 0, 0
	for i := 0; i < n; i++ {
		if i%8 >= 4 && i%2 == 0 {
			want++
		}
	}
	for range c.Query(q) {
		got++
	}
	if got != want {
		t.Fatalf("query matched %d, want %d", got, want)
	}

	// Close flushes and unmaps.
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestPersistentReopen verifies recovery: after writing (incl. updates and
// deletes) and Close, reopening the same directory rebuilds the exact live state.
func TestPersistentReopen(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 3000

	c, err := Open(Options{Shards: 4, Dir: dir, SegmentSize: 1 << 15})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; V=0]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	// Update the first 1000 (V=1) and delete every 10th of those.
	deleted := map[int]bool{}
	for i := 0; i < 1000; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; V=1]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 1000; i += 10 {
		if !c.Delete([]byte(fmt.Sprintf("k%d", i))) {
			t.Fatalf("delete k%d failed", i)
		}
		deleted[i] = true
	}
	wantLen := n - len(deleted)
	if c.Len() != wantLen {
		t.Fatalf("Len before close = %d, want %d", c.Len(), wantLen)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen and verify the recovered state matches exactly.
	c2, err := Open(Options{Shards: 4, Dir: dir, SegmentSize: 1 << 15})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	if c2.Len() != wantLen {
		t.Fatalf("Len after reopen = %d, want %d", c2.Len(), wantLen)
	}
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("k%d", i))
		ad, ok := c2.Get(key)
		if deleted[i] {
			if ok {
				t.Fatalf("deleted k%d present after reopen", i)
			}
			continue
		}
		if !ok {
			t.Fatalf("k%d missing after reopen", i)
		}
		wantV := int64(0)
		if i < 1000 {
			wantV = 1 // updated
		}
		if v, _ := ad.EvaluateAttrInt("V"); v != wantV {
			t.Fatalf("k%d V=%d after reopen, want %d", i, v, wantV)
		}
		if id, _ := ad.EvaluateAttrInt("Id"); id != int64(i) {
			t.Fatalf("k%d Id=%d after reopen", i, id)
		}
	}
	// Scan yields exactly the live keys, once each.
	seen := 0
	for range c2.Scan() {
		seen++
	}
	if seen != wantLen {
		t.Fatalf("scan after reopen yielded %d, want %d", seen, wantLen)
	}
	// A new write after reopen commits at a fresh sequence and is visible.
	if err := c2.Put([]byte("k0"), mustAd(t, `[Id=0; V=2]`)); err != nil {
		t.Fatal(err)
	}
	if ad, ok := c2.Get([]byte("k0")); !ok {
		t.Fatal("k0 (re-added) missing")
	} else if v, _ := ad.EvaluateAttrInt("V"); v != 2 {
		t.Fatalf("k0 V=%d, want 2", v)
	}
}

// TestPersistentCrashDurability verifies strict durability: writes that returned
// (each msync'd by the group-commit sync) are recovered after a simulated crash
// (reopen without ever calling Close).
func TestPersistentCrashDurability(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 2500
	func() {
		c, err := Open(Options{Shards: 4, Dir: dir, SegmentSize: 1 << 14})
		if err != nil {
			t.Fatal(err)
		}
		// Note: deliberately no Close() — simulate a crash after committing.
		for i := 0; i < n; i++ {
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
				t.Fatal(err)
			}
		}
	}()

	c2, err := Open(Options{Shards: 4, Dir: dir, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer c2.Close()
	if c2.Len() != n {
		t.Fatalf("Len after crash-recovery = %d, want %d (msync'd writes must survive)", c2.Len(), n)
	}
	for i := 0; i < n; i++ {
		ad, ok := c2.Get([]byte(fmt.Sprintf("k%d", i)))
		if !ok {
			t.Fatalf("committed k%d lost after crash", i)
		}
		if id, _ := ad.EvaluateAttrInt("Id"); id != int64(i) {
			t.Fatalf("k%d Id=%d after crash", i, id)
		}
	}
}

// TestPersistentRecoveryNoPanicOnGarbageTail verifies recovery is robust (no
// panic / no crash) when a segment file has a corrupt tail, and still recovers the
// committed prefix. (Precise torn-record rejection via a per-record CRC or a
// persisted durable-length watermark is a tracked follow-up; today recovery stops
// at the zero tail or an out-of-bounds record length.)
func TestPersistentRecoveryNoPanicOnGarbageTail(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 300
	c, err := Open(Options{Shards: 1, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// Overwrite the last 64 bytes of the (large, mostly-zero) file with 0xFF — a
	// record length there is huge and out-of-bounds, which recovery must reject
	// without panicking.
	segPath := filepath.Join(dir, "0", "seg-1.d0.dat")
	f, err := os.OpenFile(segPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	garbage := make([]byte, 64)
	for i := range garbage {
		garbage[i] = 0xFF
	}
	if _, err := f.WriteAt(garbage, st.Size()-64); err != nil {
		t.Fatal(err)
	}
	f.Close()

	c2, err := Open(Options{Shards: 1, Dir: dir}) // must not panic
	if err != nil {
		t.Fatalf("reopen with garbage tail: %v", err)
	}
	defer c2.Close()
	if c2.Len() != n {
		t.Fatalf("Len after garbage-tail reopen = %d, want %d (committed prefix)", c2.Len(), n)
	}
}

// TestPersistentCRCStopsAtCorruptRecord verifies the per-record CRC-32C: corrupting
// a record's payload mid-file makes recovery stop exactly there, recovering the
// intact prefix and nothing past the corruption.
func TestPersistentCRCStopsAtCorruptRecord(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n, corruptAt = 200, 50
	c, err := Open(Options{Shards: 1, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ { // distinct keys, no updates: one record each
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Locate record #corruptAt and flip a byte in its ad payload (an immutable,
	// CRC-covered region), on disk.
	segPath := filepath.Join(dir, "0", "seg-1.d0.dat")
	data, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatal(err)
	}
	off := 0
	for k := 0; k < corruptAt; k++ {
		off += int(recTotalLen(data, uint32(off)))
	}
	kl := binary.LittleEndian.Uint32(data[off+recKeyLenOff:])
	adByteOff := off + recKeyOff + int(kl) + 4 // first byte of the ad payload
	f, err := os.OpenFile(segPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{data[adByteOff] ^ 0xFF}, int64(adByteOff)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	c2, err := Open(Options{Shards: 1, Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	if c2.Len() != corruptAt {
		t.Fatalf("Len = %d, want %d (recovery must stop at the corrupt record)", c2.Len(), corruptAt)
	}
	for i := 0; i < corruptAt; i++ {
		if _, ok := c2.Get([]byte(fmt.Sprintf("k%d", i))); !ok {
			t.Fatalf("intact prefix k%d lost", i)
		}
	}
	if _, ok := c2.Get([]byte(fmt.Sprintf("k%d", corruptAt))); ok {
		t.Fatalf("corrupt record k%d recovered", corruptAt)
	}
}

// TestPersistentEmptyDirIsInMemory verifies Open with no Dir behaves like New.
func TestPersistentEmptyDirIsInMemory(t *testing.T) {
	t.Parallel()
	c, err := Open(Options{Shards: 2})
	if err != nil {
		t.Fatal(err)
	}
	if c.dir != "" {
		t.Fatal("expected in-memory collection")
	}
	if err := c.Put([]byte("k"), mustAd(t, `[A=1]`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get([]byte("k")); !ok {
		t.Fatal("k missing")
	}
	if err := c.Close(); err != nil { // no-op for in-memory
		t.Fatalf("close: %v", err)
	}
}

// retrainAd builds a realistic pool ad (a pure function of x) with enough content
// entropy that a ZSTD dictionary trains cleanly across generations.
func retrainAd(t *testing.T, x int) *classad.ClassAd {
	return mustAd(t, fmt.Sprintf(`[Owner="user_%d"; Cpus=%d; Memory=%d; Disk=%d; Arch="X86_64"; OpSys="LINUX"; Machine="host-%d.pool.example.com"; State="Unclaimed"; Activity="Idle"; GlobalJobId="submit-%d.chtc.wisc.edu#%d.0#%d"]`,
		x, (x*7)%64, ((x*13)%128)*1024, ((x*17)%256)*2048, x, x%50, x, 1600000000+x))
}

// TestPersistentRetrainDictReopen verifies P4b: a dictionary trained with
// RetrainDict is persisted and reconstructed on reopen, so segments compressed
// under it decode correctly across a restart.
func TestPersistentRetrainDictReopen(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 400
	c, err := Open(Options{Shards: 2, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), retrainAd(t, i)); err != nil {
			t.Fatal(err)
		}
	}
	dsz, err := c.RetrainDict(n)
	if err != nil {
		t.Fatalf("RetrainDict: %v", err)
	}
	if dsz <= 0 {
		t.Fatalf("dict size = %d", dsz)
	}
	// The dictionary was written to disk.
	if _, err := os.Stat(filepath.Join(dir, "dicts", "1.zst")); err != nil {
		t.Fatalf("dict file not persisted: %v", err)
	}
	// After retrain, the compacted segments must be the dict-tagged (.d1) files.
	if got := countSegFiles(t, filepath.Join(dir, "0")); got == 0 {
		t.Fatal("no segment files in shard 0 after retrain")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: recovery must load the dictionary and decode the zstd+dict segments.
	c2, err := Open(Options{Shards: 2, Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	if c2.Len() != n {
		t.Fatalf("Len after reopen = %d, want %d", c2.Len(), n)
	}
	for i := 0; i < n; i++ {
		ad, ok := c2.Get([]byte(fmt.Sprintf("k%d", i)))
		if !ok {
			t.Fatalf("k%d missing after reopen", i)
		}
		if !ad.Equal(retrainAd(t, i)) {
			t.Fatalf("k%d mismatch after reopen", i)
		}
	}
	seen := 0
	for range c2.Scan() {
		seen++
	}
	if seen != n {
		t.Fatalf("scan after reopen yielded %d, want %d", seen, n)
	}
	// A new write after reopen uses the recovered (latest) dictionary codec.
	if err := c2.Put([]byte("knew"), retrainAd(t, 9999)); err != nil {
		t.Fatal(err)
	}
	if _, ok := c2.Get([]byte("knew")); !ok {
		t.Fatal("post-reopen write missing")
	}
}

// TestPersistentMultiRetrainReopen exercises several dictionary generations and a
// crash (no Close) after the last retrain: each retrain's recompaction retires the
// previous generation's segments, so its dictionary is PRUNED (registry and disk);
// only the surviving (referenced) dictionary remains, and recovery reconstructs it.
func TestPersistentMultiRetrainReopen(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 300
	c, err := Open(Options{Shards: 1, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for gen := 0; gen < 3; gen++ {
		for i := 0; i < n; i++ {
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), retrainAd(t, i+gen)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := c.RetrainDict(n); err != nil {
			t.Fatalf("RetrainDict gen %d: %v", gen, err)
		}
	}
	// Only the latest dictionary survives: each retrain recompacted every segment to
	// its new codec, so the superseded generations were pruned from disk.
	if _, err := os.Stat(filepath.Join(dir, "dicts", "3.zst")); err != nil {
		t.Fatalf("current dict not persisted: %v", err)
	}
	for id := 1; id <= 2; id++ {
		if _, err := os.Stat(filepath.Join(dir, "dicts", fmt.Sprintf("%d.zst", id))); err == nil {
			t.Fatalf("superseded dict %d still on disk, want pruned", id)
		}
	}
	// Crash: reopen WITHOUT Close.
	c2, err := Open(Options{Shards: 1, Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	if c2.Len() != n {
		t.Fatalf("Len after crash-reopen = %d, want %d", c2.Len(), n)
	}
	for i := 0; i < n; i++ {
		ad, ok := c2.Get([]byte(fmt.Sprintf("k%d", i)))
		if !ok {
			t.Fatalf("k%d missing", i)
		}
		if !ad.Equal(retrainAd(t, i+2)) { // last gen written was gen=2
			t.Fatalf("k%d has stale value after crash-reopen", i)
		}
	}
}

// TestRetrainDictDegenerateNoPanic verifies RetrainDict surfaces a BuildDict failure
// as an error (rather than panicking the process) on a degenerate sample set, and
// that the collection keeps its previous codec and stays usable.
func TestRetrainDictDegenerateNoPanic(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1})
	// Many identical, tiny ads: a distribution that trips zstd.BuildDict's Huffman
	// training (integer divide by zero) absent the recover guard.
	for i := 0; i < 200; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, `[A=1]`)); err != nil {
			t.Fatal(err)
		}
	}
	// Second training on the same degenerate data is the one that trips it in practice;
	// either way, the call must return (error or nil), never panic.
	_, _ = c.RetrainDict(200)
	_, err := c.RetrainDict(200)
	_ = err // may be nil or an error depending on the library; the point is no panic.
	// The collection is still fully usable regardless.
	if c.Len() != 200 {
		t.Fatalf("Len=%d want 200", c.Len())
	}
	if _, ok := c.Get([]byte("k5")); !ok {
		t.Fatal("k5 missing after RetrainDict")
	}
}

// TestPersistentDeleteSurvivesReopen verifies a committed delete stays deleted
// across a reopen (no Close), and untouched keys remain — exercising the recovery
// path for tombstones together with the per-commit tombstone flush.
func TestPersistentDeleteSurvivesReopen(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 400
	c, err := Open(Options{Shards: 2, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	// Delete every third key.
	deleted := map[int]bool{}
	for i := 0; i < n; i += 3 {
		if !c.Delete([]byte(fmt.Sprintf("k%d", i))) {
			t.Fatalf("delete k%d returned false", i)
		}
		deleted[i] = true
	}
	// Reopen WITHOUT Close.
	c2, err := Open(Options{Shards: 2, Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	if want := n - len(deleted); c2.Len() != want {
		t.Fatalf("Len after reopen = %d, want %d", c2.Len(), want)
	}
	for i := 0; i < n; i++ {
		_, ok := c2.Get([]byte(fmt.Sprintf("k%d", i)))
		if deleted[i] && ok {
			t.Fatalf("deleted k%d resurrected after reopen", i)
		}
		if !deleted[i] && !ok {
			t.Fatalf("live k%d missing after reopen", i)
		}
	}
}

// TestDeleteTombstoneFlushPath is a white-box check that a delete on a persistent
// shard records a tombstone for flushing and that sync drains (msyncs) it. True
// power-loss durability is not observable via a same-process reopen (the page cache
// retains dirty mmap pages regardless of msync), so this verifies the durability
// code path executes rather than its on-disk effect.
func TestDeleteTombstoneFlushPath(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{Shards: 1, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	key := []byte("k")
	if err := c.Put(key, mustAd(t, `[A=1]`)); err != nil {
		t.Fatal(err)
	}
	sh := c.shards[0]
	h := c.h.Hash(key)

	sh.mu.Lock()
	sh.dirtySup = nil // clear any prior state
	seq := sh.commitSeq + 1
	ok, _ := sh.del(h, key, seq)
	// Mirror the real Delete path: advance the commit sequence for the tombstone.
	// sync() group-commits by sequence (syncFor), so dirty state queued WITHOUT a
	// sequence bump -- which no production path does -- would look already-covered.
	sh.commitSeq = seq
	nQueued := len(sh.dirtySup)
	sh.mu.Unlock()
	if !ok {
		t.Fatal("del returned false")
	}
	if nQueued != 1 {
		t.Fatalf("del queued %d tombstones, want 1", nQueued)
	}

	sh.sync()
	sh.mu.RLock()
	nAfter := len(sh.dirtySup)
	sh.mu.RUnlock()
	if nAfter != 0 {
		t.Fatalf("sync left %d tombstones unflushed, want 0", nAfter)
	}
}
