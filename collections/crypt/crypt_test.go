package crypt

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"runtime"
	"sync"
	"testing"
)

func TestMasterKeyWrapMultiKeyOpen(t *testing.T) {
	master, err := NewMaster()
	if err != nil {
		t.Fatal(err)
	}
	pool := KEK{ID: "POOL", Material: []byte("pool-signing-key-material-............")}
	token := KEK{ID: "TOKEN", Material: []byte("another-different-key-material-.......")}

	rowP, err := WrapMaster(master, pool)
	if err != nil {
		t.Fatal(err)
	}
	rowT, err := WrapMaster(master, token)
	if err != nil {
		t.Fatal(err)
	}
	rows := []MasterKeyRow{rowP, rowT}

	// Either key alone recovers the same master.
	for _, keys := range [][]KEK{{pool}, {token}, {token, pool}} {
		got, err := OpenMaster(rows, keys)
		if err != nil {
			t.Fatalf("OpenMaster with %d key(s): %v", len(keys), err)
		}
		if !bytes.Equal(got, master) {
			t.Errorf("recovered master mismatch")
		}
	}

	// A key that matches no row (rotated away / unknown id) -> ErrNoKey.
	other := KEK{ID: "OTHER", Material: []byte("x")}
	if _, err := OpenMaster(rows, []KEK{other}); err != ErrNoKey {
		t.Errorf("unknown key: err = %v, want ErrNoKey", err)
	}
	// Right id, WRONG material -> unwrap fails -> ErrNoKey (not a panic, not the master).
	if _, err := OpenMaster(rows, []KEK{{ID: "POOL", Material: []byte("wrong-material")}}); err != ErrNoKey {
		t.Errorf("wrong material: err = %v, want ErrNoKey", err)
	}
	// Tampered ciphertext for a matching key -> ErrNoKey.
	bad := rowP
	bad.Wrapped = append([]byte(nil), rowP.Wrapped...)
	bad.Wrapped[0] ^= 0xff
	if _, err := OpenMaster([]MasterKeyRow{bad}, []KEK{pool}); err != ErrNoKey {
		t.Errorf("tampered wrap: err = %v, want ErrNoKey", err)
	}
}

func TestSubkeysDistinctAndDeterministic(t *testing.T) {
	master, _ := NewMaster()
	data1, _ := Subkey(master, DataInfo)
	data2, _ := Subkey(master, DataInfo)
	backup, _ := Subkey(master, BackupInfo)
	if !bytes.Equal(data1, data2) {
		t.Error("Subkey not deterministic for the same label")
	}
	if bytes.Equal(data1, backup) {
		t.Error("data and backup subkeys must differ")
	}
	if len(data1) != KeySize {
		t.Errorf("subkey size = %d, want %d", len(data1), KeySize)
	}
	// A different master yields a different data key.
	other, _ := NewMaster()
	odata, _ := Subkey(other, DataInfo)
	if bytes.Equal(data1, odata) {
		t.Error("subkey should depend on the master")
	}
}

func TestSegmentDEKWrapRoundTrip(t *testing.T) {
	master, _ := NewMaster()
	dataKey, _ := Subkey(master, DataInfo)

	dek, err := NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	nonce, wrapped, err := WrapDEK(dek, dataKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapDEK(dataKey, nonce, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, dek) {
		t.Error("unwrapped DEK mismatch")
	}
	// The backup key must NOT unwrap a data-key-wrapped DEK.
	backupKey, _ := Subkey(master, BackupInfo)
	if _, err := UnwrapDEK(backupKey, nonce, wrapped); err == nil {
		t.Error("wrong key unwrapped the DEK")
	}
}

func TestSealOpenTamperDetected(t *testing.T) {
	dek, _ := NewDEK()
	pt := []byte("secret: X509_USER_PROXY contents / ClaimId")
	nonce, ct, err := Seal(dek, pt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(dek, nonce, ct)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("round trip failed: %v", err)
	}
	ct[len(ct)-1] ^= 0x01 // flip a tag bit
	if _, err := Open(dek, nonce, ct); err == nil {
		t.Error("GCM tag tampering not detected")
	}
}

// refOpen opens a Seal result with a cipher built straight from the standard library, sharing
// nothing with this package -- so a test can tell which key a value was really sealed under, even
// if this package's own cipher handling were wrong.
func refOpen(key, nonce, ct []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return g.Open(nil, nonce, ct, nil)
}

// TestAEADMatchesSealOpen: an AEAD produces exactly what the package-level Seal and Open do --
// either opens the other's output, the layout is the standard nonce plus ciphertext-and-tag, and
// tampering or a wrong key is refused.
func TestAEADMatchesSealOpen(t *testing.T) {
	key, _ := NewDEK()
	a, err := NewAEAD(key)
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte("ClaimId <127.0.0.1:9618>#1700000000#1#...")

	nonce, ct, err := a.Seal(pt)
	if err != nil {
		t.Fatal(err)
	}
	if len(nonce) != nonceSize || len(ct) != len(pt)+16 {
		t.Fatalf("layout: nonce %d bytes, ct %d bytes; want %d and %d", len(nonce), len(ct), nonceSize, len(pt)+16)
	}
	if got, oerr := Open(key, nonce, ct); oerr != nil || !bytes.Equal(got, pt) {
		t.Fatalf("package Open of an AEAD seal: %q, %v", got, oerr)
	}
	if got, oerr := refOpen(key, nonce, ct); oerr != nil || !bytes.Equal(got, pt) {
		t.Fatalf("stdlib open of an AEAD seal: %q, %v", got, oerr)
	}
	n2, ct2, err := Seal(key, pt)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.Open(n2, ct2); err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("AEAD open of a package seal: %q, %v", got, err)
	}
	if bytes.Equal(nonce, n2) {
		t.Fatal("two seals drew the same nonce")
	}

	// The nonce shares its allocation with the ciphertext; appending to it must not reach it.
	before := append([]byte(nil), ct...)
	_ = append(nonce, 0xAA, 0xBB, 0xCC)
	if !bytes.Equal(ct, before) {
		t.Fatal("appending to the nonce overwrote the ciphertext")
	}

	if _, err := a.Open(nonce[:nonceSize-1], ct); err == nil {
		t.Error("short nonce accepted")
	}
	bad := append([]byte(nil), ct...)
	bad[0] ^= 1
	if _, err := a.Open(nonce, bad); err == nil {
		t.Error("tampered ciphertext opened")
	}
	other, _ := NewDEK()
	b, _ := NewAEAD(other)
	if _, err := b.Open(nonce, ct); err == nil {
		t.Error("another key opened the ciphertext")
	}
	if _, err := NewAEAD(make([]byte, 7)); err == nil {
		t.Error("NewAEAD accepted a 7-byte key")
	}
}

// TestAEADBoundToKey: each AEAD seals under the key it was built with and no other -- building one
// for a new key never hands back a cipher for an old one, and the caller reusing its key buffer, or
// the pool dropping its ciphers, does not change the key either.
func TestAEADBoundToKey(t *testing.T) {
	k1, _ := NewDEK()
	k2, _ := NewDEK()
	a1, err := NewAEAD(k1)
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte("secret")
	if _, _, err = a1.Seal(pt); err != nil {
		t.Fatal(err)
	}
	a2, err := NewAEAD(k2)
	if err != nil {
		t.Fatal(err)
	}
	check := func(stage string, a *AEAD, want, wrong []byte) {
		t.Helper()
		nonce, ct, err := a.Seal(pt)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := refOpen(want, nonce, ct); err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("%s: not sealed under its own key: %v", stage, err)
		}
		if _, err := refOpen(wrong, nonce, ct); err == nil {
			t.Fatalf("%s: sealed under the other key", stage)
		}
		rn, rct, _ := Seal(want, pt)
		if got, err := a.Open(rn, rct); err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("%s: cannot open its own key's seal: %v", stage, err)
		}
	}
	check("second key", a2, k2, k1)
	check("first key after second built", a1, k1, k2)

	// The key is copied: the caller zeroing its buffer, and the pool being emptied by two
	// collections (a pooled value survives one in the victim cache) so the cipher is rebuilt from
	// the AEAD's own copy, leave it sealing under the original key.
	k1copy := append([]byte(nil), k1...)
	clear(k1)
	runtime.GC()
	runtime.GC()
	check("after the key buffer was cleared", a1, k1copy, k2)
}

// TestAEADConcurrent: one AEAD sealing and opening from many goroutines at once, as a parallel scan
// opens sealed values; run under -race.
func TestAEADConcurrent(t *testing.T) {
	key, _ := NewDEK()
	a, err := NewAEAD(key)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				pt := []byte(fmt.Sprintf("goroutine %d value %d", g, i))
				nonce, ct, err := a.Seal(pt)
				if err != nil {
					errs <- err
					return
				}
				got, err := a.Open(nonce, ct)
				if err != nil || !bytes.Equal(got, pt) {
					errs <- fmt.Errorf("round trip %q: %q, %v", pt, got, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// BenchmarkSealOpen compares building the cipher per call (Seal, Open) with an AEAD built once, on a
// value the size of a typical sealed attribute.
func BenchmarkSealOpen(b *testing.B) {
	key, _ := NewDEK()
	pt := bytes.Repeat([]byte("x"), 64)
	a, err := NewAEAD(key)
	if err != nil {
		b.Fatal(err)
	}
	nonce, ct, _ := a.Seal(pt)
	b.Run("seal/per-call", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, _, err := Seal(key, pt); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("seal/aead", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, _, err := a.Seal(pt); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("open/per-call", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := Open(key, nonce, ct); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("open/aead", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := a.Open(nonce, ct); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("open/aead-parallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if _, err := a.Open(nonce, ct); err != nil {
					b.Error(err)
					return
				}
			}
		})
	})
}
