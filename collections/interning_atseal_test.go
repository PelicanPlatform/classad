package collections

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// countSealedSegs returns (sealed, interned): sealed = non-active segments with data; interned =
// those carrying a per-segment dictionary. Used to assert interning-at-seal progress.
func countSealedSegs(c *Collection) (sealed, interned int) {
	for _, sh := range c.shards {
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
	return
}

// TestArchiveInternAtSeal exercises Options.InternAtSeal: an append-only archive interns each
// segment as it seals (via the Archive.Append eager-seal hook -> InternSealed), with NO
// RetrainDict. The eager hook interns during appends; a final InternSealed flushes the last
// straggler (the segment sealed by the final append, whose interning would otherwise wait for the
// next append) and is idempotent. Reads/queries are correct, and recovery restores the interned
// segments across a reopen.
func TestArchiveInternAtSeal(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 4000

	a, err := CreateArchive(ArchiveOptions{
		Dir: dir, SegmentSize: 1 << 16, InternAtSeal: true, ValueAttrs: []string{"Ts"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := a.Append(mustAd(t,
			fmt.Sprintf("[Id=%d; Ts=%d; Val=%d; Host=\"h%d.example.org\"]", i, i, i%100, i))); err != nil {
			t.Fatal(err)
		}
	}

	// The eager seal hook must have interned segments DURING appends -- no RetrainDict was called.
	if _, internedEager := countSealedSegs(a.c); internedEager == 0 {
		t.Fatal("InternAtSeal: no segments interned during appends (eager hook did not fire)")
	}
	// Flush the final straggler and confirm every sealed segment is interned; idempotent re-call.
	a.c.InternSealed()
	a.c.InternSealed()
	sealed, interned := countSealedSegs(a.c)
	if sealed == 0 || interned != sealed {
		t.Fatalf("post-InternSealed: %d/%d sealed interned (want all)", interned, sealed)
	}
	t.Logf("interned %d/%d sealed segments at seal (no RetrainDict)", interned, sealed)

	verify := func(tag string, ar *Archive) {
		total := 0
		for a := range ar.c.Scan() {
			total++
			_ = a
		}
		if total != n {
			t.Fatalf("%s: scan total=%d want %d", tag, total, n)
		}
		vc := 0
		for range ar.c.Query(mustQuery(t, "Val >= 50")) {
			vc++
		}
		if vc != n/2 {
			t.Fatalf("%s: Query(Val>=50)=%d want %d", tag, vc, n/2)
		}
		zc := 0
		for range ar.c.Query(mustQuery(t, "Ts >= 3000")) { // zone-pruned range on the monotonic Ts
			zc++
		}
		if zc != 1000 {
			t.Fatalf("%s: Query(Ts>=3000)=%d want 1000", tag, zc)
		}
	}
	verify("pre-reopen", a)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	a2, err := OpenArchive(ArchiveOptions{
		Dir: dir, SegmentSize: 1 << 16, InternAtSeal: true, ValueAttrs: []string{"Ts"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	// Recovery must restore every segment interned before close. The formerly-ACTIVE segment
	// (never sealed pre-close, so never interned at seal) is now a sealed inline segment, so the
	// interned count can be one short until a fresh InternSealed transcodes it.
	if s, in := countSealedSegs(a2.c); in < interned {
		t.Fatalf("post-reopen: %d interned, recovery lost some (had %d before close, %d sealed now)", in, interned, s)
	}
	verify("post-reopen", a2) // reads correct over the recovered interned + inline mix
	a2.c.InternSealed()       // intern the recovered formerly-active segment too
	if s, in := countSealedSegs(a2.c); s == 0 || in != s {
		t.Fatalf("post-reopen after InternSealed: %d/%d interned (want all)", in, s)
	}
	verify("post-reopen-interned", a2)
}

// TestArchiveInternAtSealOptOut confirms the default (off): without InternAtSeal, an archive's
// sealed segments stay INLINE at seal (interning only happens on an explicit RetrainDict/Rewrite
// or InternSealed), so today's behavior is unchanged.
func TestArchiveInternAtSealOptOut(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 4000
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 16}) // InternAtSeal: false
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < n; i++ {
		if err := a.Append(mustAd(t, fmt.Sprintf("[Id=%d; Val=%d]", i, i%100))); err != nil {
			t.Fatal(err)
		}
	}
	if sealed, interned := countSealedSegs(a.c); sealed == 0 || interned != 0 {
		t.Fatalf("opt-out: %d/%d sealed interned (want 0 interned)", interned, sealed)
	}
	// The manual maintenance pass still interns on demand (it is gated on append-only, not the
	// InternAtSeal option).
	a.c.InternSealed()
	if sealed, interned := countSealedSegs(a.c); sealed == 0 || interned != sealed {
		t.Fatalf("after manual InternSealed: %d/%d interned (want all)", interned, sealed)
	}
}

// TestInternSealedEncrypted checks InternSealed composes with encryption at rest: an encrypted
// append-only collection (raw Options -- the Archive wrapper does not expose encryption) interns
// its sealed segments on the manual pass, sealing the designated values. (The eager Append
// trigger is Archive-only; encrypted archives drive InternSealed directly.)
func TestInternSealedEncrypted(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	_, dataKey := deriveDataKey(t)
	const secret = "ClaimId-atseal-secret-b19f4c"
	const n = 3000
	c, err := Open(Options{
		AppendOnly: true, Dir: dir, SegmentSize: 1 << 16,
		DataKey: dataKey, EncryptedAttrs: []string{"Secret"}, ValueAttrs: []string{"Ts"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%05d", i)),
			mustAd(t, fmt.Sprintf("[Id=%d; Ts=%d; Secret=%q]", i, i, secret))); err != nil {
			t.Fatal(err)
		}
	}
	c.InternSealed()
	sealed, interned := countSealedSegs(c)
	if sealed == 0 || interned != sealed {
		t.Fatalf("%d/%d sealed interned (want all)", interned, sealed)
	}
	// Reads decrypt over the interned+encrypted segments.
	total := 0
	for a := range c.Scan() {
		total++
		if s, _ := a.EvaluateAttrString("Secret"); s != secret {
			t.Fatalf("Secret not decrypted: %q", s)
		}
	}
	if total != n {
		t.Fatalf("scan total=%d want %d", total, n)
	}
	// The secret is sealed on disk (never plaintext in a segment file).
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == ".dat" {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(secret)) {
				t.Fatalf("secret in plaintext in %s", filepath.Base(p))
			}
		}
		return nil
	})
}
