package wire

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/crypt"
)

// refusingSealer fails every Seal, as a sealer over a key AES cannot use does.
type refusingSealer struct{}

var errRefusedSeal = errors.New("refused")

func (refusingSealer) Seal([]byte) ([]byte, []byte, error) { return nil, nil, errRefusedSeal }
func (refusingSealer) Open([]byte, []byte) ([]byte, error) { return nil, errRefusedSeal }

// TestSealFailureEncodesNothing: an ad with a value the predicate names for sealing, whose Seal fails,
// is not encoded at all. The encoder used to write the value's plaintext node in its place, so a store
// that believed it encrypted at rest held the secret in the clear.
func TestSealFailureEncodesNothing(t *testing.T) {
	encrypt := func(name string) bool { return name == "ClaimId" }
	prefix := []byte("existing-bytes")
	encoders := map[string]func(dst []byte) ([]byte, error){
		"EncodeWithHotSealed": func(dst []byte) ([]byte, error) {
			return EncodeWithHotSealed(dst, mkAd(), NewInternTable(), nil, encrypt, refusingSealer{})
		},
		"EncodeInlineWithHotSealed": func(dst []byte) ([]byte, error) {
			return EncodeInlineWithHotSealed(dst, mkAd(), nil, encrypt, refusingSealer{})
		},
		"EncodeInlineDeltaSealed": func(dst []byte) ([]byte, error) {
			return EncodeInlineDeltaSealed(dst, mkAd(), nil, encrypt, refusingSealer{})
		},
	}
	for name, enc := range encoders {
		dst := append([]byte(nil), prefix...)
		out, err := enc(dst)
		if !errors.Is(err, ErrSealFailed) || !errors.Is(err, errRefusedSeal) {
			t.Errorf("%s: err = %v, want ErrSealFailed wrapping the sealer's error", name, err)
		}
		if !bytes.Equal(out, prefix) {
			t.Errorf("%s: returned %q, want dst unchanged", name, out)
		}
		if bytes.Contains(out[:cap(out)], []byte("secret-capability-1234")) {
			t.Errorf("%s: the plaintext reached the destination buffer", name)
		}
	}

	// Nothing to seal, nothing to fail: the same sealer encodes an ad the predicate names nothing in.
	if _, err := EncodeInlineWithHotSealed(nil, mkAd(), nil, func(string) bool { return false }, refusingSealer{}); err != nil {
		t.Errorf("an ad with nothing to seal failed: %v", err)
	}
}

// TestDeprecatedSealEncodersPanic pins the deprecated non-error encoders: a Seal failure panics, since
// they have no error to return and no ad they could return without the plaintext.
func TestDeprecatedSealEncodersPanic(t *testing.T) {
	encrypt := func(name string) bool { return name == "ClaimId" }
	for name, enc := range map[string]func(){
		"EncodeWithHotEnc":       func() { EncodeWithHotEnc(nil, mkAd(), NewInternTable(), nil, encrypt, refusingSealer{}) },
		"EncodeInlineWithHotEnc": func() { EncodeInlineWithHotEnc(nil, mkAd(), nil, encrypt, refusingSealer{}) },
		"EncodeInlineDelta":      func() { EncodeInlineDelta(nil, mkAd(), nil, encrypt, refusingSealer{}) },
	} {
		func() {
			defer func() {
				r := recover()
				if err, ok := r.(error); !ok || !errors.Is(err, ErrSealFailed) {
					t.Errorf("%s: recovered %v, want a panic with ErrSealFailed", name, r)
				}
			}()
			enc()
		}()
	}
}

// openOnlySealer opens with a working key and refuses every Seal: a carried value opens, and the step
// that would seal it again fails.
type openOnlySealer struct{ dekSealer }

func (openOnlySealer) Seal([]byte) ([]byte, []byte, error) { return nil, nil, errRefusedSeal }

// TestResealFailureEncodesNothing: the resealing rewrite of an interned ad opens each carried sealed
// value, rewrites it, and seals it again. When that Seal fails the rewrite returns an error wrapping
// ErrSealFailed and dst as it was passed; the rewritten plaintext is never emitted in place of the
// ciphertext. A value that does not open is a different failure (ErrMalformed), so a writer can report
// the two apart.
func TestResealFailureEncodesNothing(t *testing.T) {
	dek, err := crypt.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	other, err := crypt.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	sealed := func(name string) bool { return name == "ClaimId" }
	tt := NewInternTable()
	interned, err := EncodeWithHotSealed(nil, unopenedAd(), tt, nil, sealed, dekSealer{dek})
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte("existing-bytes")

	var sc SubsetScratch
	dst := append([]byte(nil), prefix...)
	out, n, err := AppendAdInlineFromInternedResealedErr(dst, Ad(interned), tt.Name, nil, openOnlySealer{dekSealer{dek}}, &sc)
	if !errors.Is(err, ErrSealFailed) || !errors.Is(err, errRefusedSeal) || errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrSealFailed wrapping the sealer's error", err)
	}
	if n != 0 || !bytes.Equal(out, prefix) {
		t.Fatalf("re-sealed %d, returned %q; want nothing and dst unchanged", n, out)
	}
	if bytes.Contains(out[:cap(out)], []byte("top-secret")) {
		t.Fatal("the opened plaintext reached the destination buffer")
	}
	if !allZero(sc.plainBuf[:cap(sc.plainBuf)]) {
		t.Fatal("the scratch buffer still holds the opened plaintext")
	}
	if strings.Contains(err.Error(), "top-secret") {
		t.Fatal("the error carries the plaintext")
	}
	if _, _, ok := AppendAdInlineFromInternedResealed(nil, Ad(interned), tt.Name, nil, openOnlySealer{dekSealer{dek}}, &sc); ok {
		t.Fatal("the deprecated form accepted a rewrite whose Seal failed")
	}

	// A value sealed under another key does not open: malformed to this writer, not a Seal failure.
	_, _, err = AppendAdInlineFromInternedResealedErr(nil, Ad(interned), tt.Name, nil, dekSealer{other}, &sc)
	if !errors.Is(err, ErrMalformed) || errors.Is(err, ErrSealFailed) {
		t.Fatalf("other key: err = %v, want ErrMalformed and not ErrSealFailed", err)
	}
	// No sealer cannot re-seal anything.
	if _, _, err = AppendAdInlineFromInternedResealedErr(nil, Ad(interned), tt.Name, nil, nil, &sc); !errors.Is(err, ErrSealFailed) {
		t.Fatalf("no sealer: err = %v, want ErrSealFailed", err)
	}
}
