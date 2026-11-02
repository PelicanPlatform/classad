package collections

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// mustEncodeAd is encodeAd for a test whose store cannot fail to seal (no key, or a working one).
func mustEncodeAd(w []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return w
}

// TestDataKeyWrongLengthRefused: a data key that is not an AES-256 key is refused where the store is
// built. A key AES rejects outright used to produce a store whose every Seal failed while
// EncryptionEnabled reported true; a 16- or 24-byte one sealed under a weaker cipher than documented.
func TestDataKeyWrongLengthRefused(t *testing.T) {
	for _, n := range []int{1, 16, 24, 31, 33, 64} {
		key := bytes.Repeat([]byte{7}, n)
		t.Run(fmt.Sprintf("%dbytes", n), func(t *testing.T) {
			if _, err := Open(Options{Shards: 1, DataKey: key}); !errors.Is(err, ErrInvalidDataKey) {
				t.Errorf("in-memory Open: err = %v, want ErrInvalidDataKey", err)
			}
			func() {
				defer func() {
					r := recover()
					if r == nil {
						t.Error("New accepted the key")
						return
					}
					if msg := fmt.Sprint(r); !bytes.Contains([]byte(msg), []byte(ErrInvalidDataKey.Error())) {
						t.Errorf("New panicked with %q, want the invalid-key error", msg)
					}
				}()
				New(Options{Shards: 1, DataKey: key})
			}()
			if !mmapSupported {
				return
			}
			dir := filepath.Join(t.TempDir(), "store")
			if _, err := Open(Options{Shards: 1, Dir: dir, DataKey: key}); !errors.Is(err, ErrInvalidDataKey) {
				t.Errorf("persistent Open: err = %v, want ErrInvalidDataKey", err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("refused Open left %s behind (stat err %v)", dir, err)
			}
			if _, err := CreateArchive(ArchiveOptions{Dir: dir, DataKey: key}); !errors.Is(err, ErrInvalidDataKey) {
				t.Errorf("CreateArchive: err = %v, want ErrInvalidDataKey", err)
			}
			if _, err := OpenArchive(ArchiveOptions{Dir: dir, DataKey: key}); !errors.Is(err, ErrInvalidDataKey) {
				t.Errorf("OpenArchive: err = %v, want ErrInvalidDataKey", err)
			}
		})
	}
	// The two accepted shapes: no key, and a 32-byte one.
	_, dataKey := deriveDataKey(t)
	for _, key := range [][]byte{nil, dataKey} {
		c, err := Open(Options{Shards: 1, DataKey: key})
		if err != nil {
			t.Fatalf("Open with a %d-byte key: %v", len(key), err)
		}
		if got, want := c.EncryptionEnabled(), len(key) > 0; got != want {
			t.Errorf("%d-byte key: EncryptionEnabled = %v, want %v", len(key), got, want)
		}
		c.Close()
	}
}

// failingSealer wraps the collection's real sealer and refuses every Seal while failing is set. Open
// always delegates, so records sealed before the failure still read back.
type failingSealer struct {
	real    wire.Sealer
	failing atomic.Bool
}

var errInjectedSeal = errors.New("injected seal failure")

func (s *failingSealer) Seal(pt []byte) ([]byte, []byte, error) {
	if s.failing.Load() {
		return nil, nil, errInjectedSeal
	}
	return s.real.Seal(pt)
}

func (s *failingSealer) Open(nonce, ct []byte) ([]byte, error) { return s.real.Open(nonce, ct) }

// injectSealFailure installs a failingSealer on c (initially failing) and returns it.
func injectSealFailure(c *Collection) *failingSealer {
	fs := &failingSealer{real: c.sealer}
	fs.failing.Store(true)
	c.sealer = fs
	return fs
}

// decompressedRecordsContaining counts the records in c's segments whose DECOMPRESSED bytes hold
// needle. A raw grep of the files proves nothing on a compressed store: zstd hides a plaintext value
// as well as a cipher does.
func decompressedRecordsContaining(t *testing.T, c *Collection, needle string) int {
	t.Helper()
	hits := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			for off := 0; off < seg.used; {
				o := uint32(off)
				total := recTotalLen(seg.data, o)
				if total == 0 {
					break
				}
				if !recIsMarker(seg.data, o) {
					raw, err := seg.codec.Decompress(nil, recAd(seg.data, o))
					if err != nil {
						sh.mu.RUnlock()
						t.Fatalf("segment %d record at %d: %v", seg.id, o, err)
					}
					if bytes.Contains(raw, []byte(needle)) {
						hits++
					}
				}
				off += int(total)
			}
		}
		sh.mu.RUnlock()
	}
	return hits
}

// assertNoPlaintext checks both the decompressed records and the raw files (dictionaries, sidecars).
func assertNoPlaintext(t *testing.T, c *Collection, dir, secret string) {
	t.Helper()
	if n := decompressedRecordsContaining(t, c, secret); n != 0 {
		t.Errorf("%d stored records hold the secret in the clear", n)
	}
	if hits := diskBytesContaining(t, dir, secret); len(hits) != 0 {
		t.Errorf("secret in the clear on disk, in: %v", hits)
	}
}

const failClosedSecret = "ClaimId-must-never-be-stored-in-the-clear-4b1d"

func openFailClosed(t *testing.T, dir string, dataKey []byte, extra func(*Options)) *Collection {
	t.Helper()
	opts := Options{Shards: 1, Dir: dir, DataKey: dataKey, EncryptedAttrs: []string{"Salary"}}
	if extra != nil {
		extra(&opts)
	}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestSealFailureRefusesWrite: a Seal failure refuses the write on every write path, and nothing it
// would have sealed reaches a stored record in the clear. The encoder used to fall back to writing the
// value unsealed, so a failing sealer stored the secret as plaintext while the store reported
// encryption at rest.
func TestSealFailureRefusesWrite(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	_, dataKey := deriveDataKey(t)
	secretAd := func(i int) string {
		return fmt.Sprintf(`[Owner="alice"; Cpus=%d; ClaimId=%q]`, i, failClosedSecret)
	}

	t.Run("Put", func(t *testing.T) {
		dir := t.TempDir()
		c := openFailClosed(t, dir, dataKey, nil)
		defer c.Close()
		injectSealFailure(c)
		if err := c.Put([]byte("k"), mustAd(t, secretAd(0))); !errors.Is(err, wire.ErrSealFailed) {
			t.Fatalf("Put: err = %v, want wire.ErrSealFailed", err)
		}
		if _, ok := c.Get([]byte("k")); ok {
			t.Error("the refused Put was stored")
		}
		assertNoPlaintext(t, c, dir, failClosedSecret)
	})

	t.Run("Update", func(t *testing.T) {
		dir := t.TempDir()
		c := openFailClosed(t, dir, dataKey, nil)
		defer c.Close()
		injectSealFailure(c)
		err := c.Update([]AdUpdate{
			{Key: []byte("plain"), Ad: mustAd(t, `[Owner="bob"; Cpus=1]`)}, // nothing to seal
			{Key: []byte("k"), Ad: mustAd(t, secretAd(0))},
		})
		if !errors.Is(err, wire.ErrSealFailed) {
			t.Fatalf("Update: err = %v, want wire.ErrSealFailed", err)
		}
		if c.Len() != 0 {
			t.Errorf("Len = %d after a refused batch, want 0 (the batch is all or nothing)", c.Len())
		}
		assertNoPlaintext(t, c, dir, failClosedSecret)
	})

	t.Run("UpdateOld", func(t *testing.T) {
		dir := t.TempDir()
		c := openFailClosed(t, dir, dataKey, nil)
		defer c.Close()
		injectSealFailure(c)
		err := c.UpdateOld([]OldAdUpdate{{Key: []byte("k"), Text: fmt.Sprintf("Owner = \"alice\"\nClaimId = %q\n", failClosedSecret)}})
		if !errors.Is(err, wire.ErrSealFailed) {
			t.Fatalf("UpdateOld: err = %v, want wire.ErrSealFailed", err)
		}
		assertNoPlaintext(t, c, dir, failClosedSecret)
	})

	// Txn writes: a whole ad, an explicitly encrypted attribute patched as a delta, and the same patch
	// from a transaction not entitled to sealed values (which composes the record without the key).
	txnCases := []struct {
		name  string
		write func(c *Collection) CommitResult
	}{
		{"TxnPut", func(c *Collection) CommitResult {
			tx := c.Begin()
			tx.Put([]byte("k"), mustAd(t, secretAd(1)))
			return tx.Commit()
		}},
		{"TxnPatchDelta", func(c *Collection) CommitResult {
			tx := c.Begin()
			tx.PatchAttrs([]byte("k"), mustAd(t, fmt.Sprintf(`[Salary=%q]`, failClosedSecret)), nil)
			return tx.Commit()
		}},
		{"TxnPatchWhole", func(c *Collection) CommitResult {
			tx := c.Begin()
			tx.PatchAttrs([]byte("k"), mustAd(t, fmt.Sprintf(`[Salary=%q]`, failClosedSecret)), []string{"Cpus"})
			return tx.Commit()
		}},
		{"TxnPatchKeyless", func(c *Collection) CommitResult {
			tx := c.BeginRedacted()
			tx.PatchAttrs([]byte("k"), mustAd(t, fmt.Sprintf(`[Salary=%q]`, failClosedSecret)), []string{"Cpus"})
			return tx.Commit()
		}},
	}
	for _, tc := range txnCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := openFailClosed(t, dir, dataKey, func(o *Options) { o.DeltaMax = 4 })
			defer c.Close()
			// A sealable base the patch cases chain onto, written while sealing works.
			if err := c.Put([]byte("k"), mustAd(t, `[Owner="alice"; Cpus=1; Salary="base"]`)); err != nil {
				t.Fatal(err)
			}
			injectSealFailure(c)
			res := tc.write(c)
			i := slices.IndexFunc(res.Unapplied, func(k []byte) bool { return string(k) == "k" })
			if i < 0 {
				t.Fatalf("the write was not refused: %+v", res)
			}
			if res.UnappliedReasons[i] != failSeal.String() || !errors.Is(res.UnappliedErrs[i], wire.ErrSealFailed) {
				t.Errorf("refused as %q (%v), want %q / wire.ErrSealFailed", res.UnappliedReasons[i], res.UnappliedErrs[i], failSeal)
			}
			got, ok := c.Get([]byte("k"))
			if !ok {
				t.Fatal("the base record is gone")
			}
			if v, _ := got.EvaluateAttrString("Salary"); v != "base" {
				t.Errorf("Salary = %q, want the base's value (the write was refused)", v)
			}
			assertNoPlaintext(t, c, dir, failClosedSecret)
		})
	}

	t.Run("ArchiveAppend", func(t *testing.T) {
		dir := t.TempDir()
		a, err := CreateArchive(ArchiveOptions{Dir: dir, DataKey: dataKey})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		injectSealFailure(a.c)
		if err := a.Append(mustAd(t, secretAd(0))); !errors.Is(err, wire.ErrSealFailed) {
			t.Fatalf("Append: err = %v, want wire.ErrSealFailed", err)
		}
		assertNoPlaintext(t, a.c, dir, failClosedSecret)
	})
}

// TestSealFailureAbortsCompaction: compaction re-encodes a record with a sealed value through the
// decode/re-seal path (the byte-level transcode cannot carry one). A Seal failure there abandons the
// round and leaves the shard as it was, rather than write the opened value back out unsealed.
func TestSealFailureAbortsCompaction(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	_, dataKey := deriveDataKey(t)
	dir := t.TempDir()
	c := openFailClosed(t, dir, dataKey, nil)
	defer c.Close()
	const n = 1000
	for round := 0; round < 3; round++ { // later rounds supersede earlier ones, giving compaction work
		for i := 0; i < n; i++ {
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
				mustAd(t, fmt.Sprintf(`[Owner="alice"; Cpus=%d; Round=%d; ClaimId=%q]`, i, round, failClosedSecret))); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertNoPlaintext(t, c, dir, failClosedSecret)
	before := segmentsOf(c)
	fs := injectSealFailure(c)
	c.Compact()
	if after := segmentsOf(c); !slices.Equal(before, after) {
		t.Errorf("the shard was rewritten while every Seal failed: segments %p became %p", before, after)
	}
	assertNoPlaintext(t, c, dir, failClosedSecret)
	fs.failing.Store(false)
	for _, k := range []string{"k0", fmt.Sprintf("k%d", n-1)} {
		got, ok := c.Get([]byte(k))
		if !ok {
			t.Fatalf("%s missing after the aborted compaction", k)
		}
		if v, _ := got.EvaluateAttrString("ClaimId"); v != failClosedSecret {
			t.Errorf("%s: ClaimId = %q after the aborted compaction", k, v)
		}
	}
	if c.Len() != n {
		t.Errorf("Len = %d, want %d", c.Len(), n)
	}
	// The same compaction succeeds once sealing does: the shard was eligible, so the refusal above
	// was the seal failure and not a shard with nothing to do.
	c.Compact()
	if slices.Equal(before, segmentsOf(c)) {
		t.Error("Compact rewrote nothing once sealing worked; the fixture gave it no work")
	}
	assertNoPlaintext(t, c, dir, failClosedSecret)
}

// segmentsOf lists c's segments, to tell a shard compaction rewrote from one it left alone.
func segmentsOf(c *Collection) []*segment {
	var out []*segment
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, s := range sh.segs {
			if s != nil {
				out = append(out, s)
			}
		}
		sh.mu.RUnlock()
	}
	return out
}

// TestSealFailureLeavesMigrationIncomplete: the sealed-attribute migration rewrites legacy plaintext
// records sealed. A rewrite whose values will not seal must not count as done -- the migration is
// one-shot per store, and a marker saying it ran would leave the plaintext in place for good.
func TestSealFailureLeavesMigrationIncomplete(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const secret = "ClaimId-legacy-plaintext-fail-closed"
	legacyPlaintextStore(t, dir, secret, 500)
	_, dataKey := deriveDataKey(t)
	c, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16, DataKey: dataKey})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fs := injectSealFailure(c)
	if got := c.MigrateSealedAttrs(0); got != 0 {
		t.Errorf("MigrateSealedAttrs rewrote %d segments while every Seal failed", got)
	}
	if c.sealMigrationDone() {
		t.Fatal("the migration is marked done, but nothing could be sealed")
	}
	fs.failing.Store(false)
	if got := c.MigrateSealedAttrs(0); got == 0 {
		t.Fatal("the retry migrated nothing")
	}
	assertNoPlaintext(t, c, dir, secret)
}

// TestSealFailureKeepsDeltaChainBounded: encoding a write advances the key's chain in the delta tracker
// before the encode can fail. A refused whole record at the bound resets the count for a record that was
// never stored, so without a correction the key's next write chained one more delta past DeltaMax.
func TestSealFailureKeepsDeltaChainBounded(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	_, dataKey := deriveDataKey(t)
	c := openFailClosed(t, t.TempDir(), dataKey, func(o *Options) { o.DeltaMax = 1 })
	defer c.Close()
	patch := func(v string) CommitResult {
		tx := c.Begin()
		tx.PatchAttrs([]byte("k"), mustAd(t, fmt.Sprintf(`[Salary=%q]`, v)), nil)
		return tx.Commit()
	}
	if err := c.Put([]byte("k"), mustAd(t, `[Owner="alice"; Salary="base"]`)); err != nil {
		t.Fatal(err)
	}
	if res := patch("one"); res.HasUnapplied() { // the chain's one permitted delta
		t.Fatalf("first patch refused: %+v", res)
	}
	fs := injectSealFailure(c)
	if res := patch(failClosedSecret); !res.HasUnapplied() { // at the bound: a whole record, refused
		t.Fatal("the patch was not refused")
	}
	fs.failing.Store(false)
	deltasBefore := c.deltas.deltas.Load()
	if res := patch("three"); res.HasUnapplied() {
		t.Fatalf("patch after recovery refused: %+v", res)
	}
	if c.deltas.deltas.Load() != deltasBefore {
		t.Error("the write after the refused one was stored as a delta, past DeltaMax=1")
	}
	got, ok := c.Get([]byte("k"))
	if !ok {
		t.Fatal("k missing")
	}
	if v, _ := got.EvaluateAttrString("Salary"); v != "three" {
		t.Errorf("Salary = %q, want three", v)
	}
}

// TestSealFailureRefusesRedactedReseal: a transaction not entitled to sealed values that rewrites an
// interned record whole has the store open each sealed value the result carries, rewrite it to the
// inline form, and seal it again. When that Seal fails the write is refused as "seal-failed" and the
// record stays as it was: the rewritten plaintext is not stored in place of the ciphertext, and the
// write is not reported as a damaged record ("decode"), which it is not. The write sets nothing sealed,
// so every Seal it makes is a re-seal.
func TestSealFailureRefusesRedactedReseal(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	c, _ := resealFixture(t, 4, func(i int) [3]string {
		return [3]string{`"user"`, `"tok"`, fmt.Sprintf("%q", failClosedSecret)}
	})
	c.deltaMax = 0 // every patch takes the whole-record path
	fs := injectSealFailure(c)
	ctr := &resealSealer{inner: fs}
	c.sealer = ctr
	write := func() CommitResult {
		tx := c.BeginRedacted()
		tx.PatchAttrs([]byte("1.0"), mustAd(t, `[JobStatus=2]`), []string{"Filler0"})
		return tx.Commit()
	}

	res := write()
	if ctr.opens.Load() == 0 || ctr.seals.Load() == 0 {
		t.Fatalf("the write opened %d and sealed %d values; the fixture never reached the re-seal", ctr.opens.Load(), ctr.seals.Load())
	}
	i := slices.IndexFunc(res.Unapplied, func(k []byte) bool { return string(k) == "1.0" })
	if i < 0 {
		t.Fatalf("the write was not refused: %+v", res)
	}
	if res.UnappliedReasons[i] != failSeal.String() || !errors.Is(res.UnappliedErrs[i], wire.ErrSealFailed) {
		t.Errorf("refused as %q (%v), want %q / wire.ErrSealFailed", res.UnappliedReasons[i], res.UnappliedErrs[i], failSeal)
	}
	got, ok := c.Get([]byte("1.0"))
	if !ok {
		t.Fatal("the record is gone")
	}
	if v, _ := got.EvaluateAttrInt("JobStatus"); v != 1 {
		t.Errorf("JobStatus = %d, want 1 (the write was refused)", v)
	}
	if _, has := got.Lookup("Filler0"); !has {
		t.Error("Filler0 was removed by a refused write")
	}
	if v, _ := got.EvaluateAttrString("ClaimId"); v != failClosedSecret {
		t.Errorf("ClaimId = %q, want the stored value", v)
	}
	assertNoPlaintext(t, c, c.dir, failClosedSecret)

	// The same write applies once sealing works: the refusal was the Seal failure, not the record.
	fs.failing.Store(false)
	if res := write(); res.HasUnapplied() || res.Committed != 1 {
		t.Fatalf("the write was refused once sealing worked: %+v", res)
	}
	got, _ = c.Get([]byte("1.0"))
	if v, _ := got.EvaluateAttrString("ClaimId"); v != failClosedSecret {
		t.Errorf("ClaimId = %q after the re-sealed write, want the stored value", v)
	}
	assertNoPlaintext(t, c, c.dir, failClosedSecret)
}
