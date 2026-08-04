package collections

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestEncryptedInterning is the phase-2 acceptance test: an encryption-at-rest collection now
// INTERNS its segments at compaction (interned ids + a plaintext name dictionary + SEALED value
// nodes). Reads decrypt correctly over the interned+encrypted segments (before and after a
// reopen), and the secret value never appears in plaintext in any segment file.
func TestEncryptedInterning(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	_, dataKey := deriveDataKey(t)
	const secret = "ClaimId-super-secret-capability-9f83a2b7c1"
	const n = 3000

	open := func() *Collection {
		c, err := Open(Options{
			Shards: 2, Dir: dir, SegmentSize: 1 << 16,
			DataKey:        dataKey,
			EncryptedAttrs: []string{"Secret"},
			ValueAttrs:     []string{"Val"}, // plaintext attr still indexes
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%05d", i)),
			mustAd(t, fmt.Sprintf("[Id=%d; Val=%d; Secret=%q]", i, i%100, secret))); err != nil {
			t.Fatal(err)
		}
	}
	// Force interning via compaction (encrypted collections are mutable).
	for _, sh := range c.shards {
		c.compactShard(sh, c.currentCodec())
	}
	c.reindexAfterCompaction()

	assertInterned := func(tag string, col *Collection) {
		sealed, interned := 0, 0
		for _, sh := range col.shards {
			sh.mu.RLock()
			act := sh.act
			for _, seg := range sh.segs {
				if seg == nil || seg == act || seg.used == 0 {
					continue
				}
				sealed++
				if seg.dict.Load() != nil {
					interned++
				}
			}
			sh.mu.RUnlock()
		}
		if sealed == 0 || interned != sealed {
			t.Fatalf("%s: %d/%d sealed interned (want all)", tag, interned, sealed)
		}
		t.Logf("%s: %d sealed segments, all interned+encrypted", tag, sealed)
	}
	verify := func(tag string, col *Collection) {
		for i := 0; i < n; i++ {
			a, ok := col.Get([]byte(fmt.Sprintf("k%05d", i)))
			if !ok {
				t.Fatalf("%s: Get k%05d missing", tag, i)
			}
			if v, _ := a.EvaluateAttrInt("Id"); int(v) != i {
				t.Fatalf("%s: Id=%d want %d", tag, v, i)
			}
			if s, _ := a.EvaluateAttrString("Secret"); s != secret { // decrypted correctly
				t.Fatalf("%s: Secret=%q not decrypted", tag, s)
			}
		}
		cnt := 0
		for range col.Query(mustQuery(t, "Val >= 50")) { // query on the plaintext attr
			cnt++
		}
		if cnt != n/2 {
			t.Fatalf("%s: Query(Val>=50)=%d want %d", tag, cnt, n/2)
		}
	}
	// The secret value must not appear in plaintext in ANY segment file on disk.
	assertSealedOnDisk := func(tag string) {
		var files []string
		filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && filepath.Ext(p) == ".dat" {
				files = append(files, p)
			}
			return nil
		})
		if len(files) == 0 {
			t.Fatalf("%s: no segment files found", tag)
		}
		for _, p := range files {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte(secret)) {
				t.Fatalf("%s: secret found in plaintext in %s (not sealed!)", tag, filepath.Base(p))
			}
		}
	}

	assertInterned("post-compact", c)
	verify("post-compact", c)
	assertSealedOnDisk("post-compact")

	// The sample path (CollectSamples -> hot-set refresh / dict retrain) runs over the interned
	// segments now. It must open+re-seal, never emitting the plaintext secret into a retrained
	// zstd dictionary or leaving it in a sample. Retrain, then re-check disk + reads.
	c.RefreshHotSet(2000, 8)
	if _, err := c.RetrainDict(2000); err != nil {
		t.Fatal(err)
	}
	verify("post-retrain", c)
	assertSealedOnDisk("post-retrain")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2 := open()
	assertInterned("post-reopen", c2)
	verify("post-reopen", c2)
	assertSealedOnDisk("post-reopen")
	c2.Close()

	// Wrong key over an interned+encrypted segment: the plaintext name dictionary still resolves
	// ids, but every sealed value node fails to open -- decode must error cleanly (no panic) and
	// the secret is never recovered.
	_, wrongKey := deriveDataKey(t)
	bad, err := Open(Options{
		Shards: 2, Dir: dir, SegmentSize: 1 << 16,
		DataKey: wrongKey, EncryptedAttrs: []string{"Secret"}, ValueAttrs: []string{"Val"},
	})
	if err != nil {
		t.Fatal(err)
	}
	recovered := 0
	for i := 0; i < n; i++ {
		if a, ok := bad.Get([]byte(fmt.Sprintf("k%05d", i))); ok {
			if s, _ := a.EvaluateAttrString("Secret"); s == secret {
				recovered++
			}
		}
	}
	if recovered != 0 {
		t.Fatalf("wrong key recovered the secret in %d/%d records", recovered, n)
	}
	bad.Close()
}

// TestEncryptedInterningMixed exercises the per-segment dispatch that is the crux of the whole
// design: a single encrypted collection holding BOTH inline+encrypted segments (recently filled,
// not yet compacted) and interned+encrypted segments (compacted). Reads must decrypt across the
// mix -- decodeAdDict routes each segment by seg.dict (nil -> DecodeInlineEnc, non-nil ->
// DecodeResolveEnc) -- and recovery must restore that same mix (dict only for the interned ones).
func TestEncryptedInterningMixed(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	_, dataKey := deriveDataKey(t)
	const secret = "ClaimId-mixed-secret-77b3ce01"
	const n1, n2 = 2000, 2000 // n1 compacted to interned; n2 written after -> inline

	open := func() *Collection {
		c, err := Open(Options{
			Shards: 1, Dir: dir, SegmentSize: 1 << 16,
			DataKey: dataKey, EncryptedAttrs: []string{"Secret"}, ValueAttrs: []string{"Val"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	put := func(c *Collection, lo, hi int) {
		for i := lo; i < hi; i++ {
			if err := c.Put([]byte(fmt.Sprintf("k%05d", i)),
				mustAd(t, fmt.Sprintf("[Id=%d; Val=%d; Secret=%q]", i, i%100, secret))); err != nil {
				t.Fatal(err)
			}
		}
	}
	c := open()
	put(c, 0, n1)
	for _, sh := range c.shards { // compact the first tranche -> interned+encrypted
		c.compactShard(sh, c.currentCodec())
	}
	c.reindexAfterCompaction()
	put(c, n1, n1+n2) // write more AFTER compaction -> fresh inline+encrypted sealed segments

	assertMix := func(tag string, col *Collection) {
		interned, inlineSealed := 0, 0
		for _, sh := range col.shards {
			sh.mu.RLock()
			act := sh.act
			for _, seg := range sh.segs {
				if seg == nil || seg == act || seg.used == 0 {
					continue
				}
				if seg.dict.Load() != nil {
					interned++
				} else {
					inlineSealed++
				}
			}
			sh.mu.RUnlock()
		}
		if interned == 0 || inlineSealed == 0 {
			t.Fatalf("%s: want BOTH kinds, got interned=%d inlineSealed=%d", tag, interned, inlineSealed)
		}
		t.Logf("%s: %d interned + %d inline sealed segments coexist", tag, interned, inlineSealed)
	}
	verify := func(tag string, col *Collection) {
		for i := 0; i < n1+n2; i++ {
			a, ok := col.Get([]byte(fmt.Sprintf("k%05d", i)))
			if !ok {
				t.Fatalf("%s: Get k%05d missing", tag, i)
			}
			if s, _ := a.EvaluateAttrString("Secret"); s != secret {
				t.Fatalf("%s: Secret at k%05d not decrypted (%q)", tag, i, s)
			}
		}
		cnt := 0
		for range col.Query(mustQuery(t, "Val >= 50")) {
			cnt++
		}
		if cnt != (n1+n2)/2 {
			t.Fatalf("%s: Query(Val>=50)=%d want %d", tag, cnt, (n1+n2)/2)
		}
	}

	assertMix("pre-reopen", c)
	verify("pre-reopen", c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2 := open() // recovery must restore the same mix: dict for interned segs, nil for inline
	defer c2.Close()
	assertMix("post-reopen", c2)
	verify("post-reopen", c2)
}

// TestEncryptedInterningAppendOnly is the append-only (archive) analog: an encrypted append-only
// collection interns its segments at reseal (RetrainDict), sealing values, and reads decrypt.
func TestEncryptedInterningAppendOnly(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	_, dataKey := deriveDataKey(t)
	const secret = "ClaimId-archive-secret-4a1c9e77"
	const n = 4000

	open := func() *Collection {
		c, err := Open(Options{
			AppendOnly: true, Dir: dir, SegmentSize: 1 << 16,
			DataKey: dataKey, EncryptedAttrs: []string{"Secret"}, ValueAttrs: []string{"Ts"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%05d", i)),
			mustAd(t, fmt.Sprintf("[Id=%d; Ts=%d; Val=%d; Secret=%q]", i, i, i%100, secret))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.RetrainDict(n); err != nil { // reseal every segment -> interned + encrypted
		t.Fatalf("RetrainDict: %v", err)
	}

	assertInterned := func(tag string, col *Collection) {
		sealed, interned := 0, 0
		for _, sh := range col.shards {
			sh.mu.RLock()
			act := sh.act
			for _, seg := range sh.segs {
				if seg == nil || seg == act || seg.used == 0 {
					continue
				}
				sealed++
				if seg.dict.Load() != nil {
					interned++
				}
			}
			sh.mu.RUnlock()
		}
		if sealed == 0 || interned != sealed {
			t.Fatalf("%s: %d/%d sealed interned (want all)", tag, interned, sealed)
		}
		t.Logf("%s: %d sealed segments, all interned+encrypted", tag, sealed)
	}
	verify := func(tag string, col *Collection) {
		total := 0
		for a := range col.Scan() {
			total++
			if s, _ := a.EvaluateAttrString("Secret"); s != secret {
				t.Fatalf("%s: Secret=%q not decrypted", tag, s)
			}
		}
		if total != n {
			t.Fatalf("%s: scan total=%d want %d", tag, total, n)
		}
		zc := 0
		for range col.Query(mustQuery(t, "Ts >= 3000")) { // zone-pruned range on plaintext Ts
			zc++
		}
		if zc != 1000 {
			t.Fatalf("%s: Query(Ts>=3000)=%d want 1000", tag, zc)
		}
	}
	assertSealedOnDisk := func(tag string) {
		found := false
		filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(p) != ".dat" {
				return nil
			}
			found = true
			b, _ := os.ReadFile(p)
			if bytes.Contains(b, []byte(secret)) {
				t.Fatalf("%s: secret found in plaintext in %s", tag, filepath.Base(p))
			}
			return nil
		})
		if !found {
			t.Fatalf("%s: no segment files found", tag)
		}
	}

	assertInterned("post-reseal", c)
	verify("post-reseal", c)
	assertSealedOnDisk("post-reseal")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2 := open()
	defer c2.Close()
	assertInterned("post-reopen", c2)
	verify("post-reopen", c2)
	assertSealedOnDisk("post-reopen")
}
