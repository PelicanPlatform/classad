package wire

import (
	"bytes"
	"errors"
	"testing"
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
