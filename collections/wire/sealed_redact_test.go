package wire

import (
	"errors"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// A sealed attribute used to fail the WHOLE ad for a reader without the key. That makes withholding a key
// from an unentitled reader unusable: every ad carrying a ClaimId becomes unreadable rather than partly
// redacted. These pin both halves of the policy, because they are needed for opposite reasons: a read
// path must degrade to undefined, and a WRITE path (compaction, dict retraining decode an ad and re-encode
// it) must still fail loudly rather than write the substitution back over the secret.

// xorSealer is a stand-in Sealer: enough to round-trip, and its Open can be made to fail.
type xorSealer struct{ fail bool }

func (x xorSealer) Seal(pt []byte) ([]byte, []byte, error) {
	ct := make([]byte, len(pt))
	for i := range pt {
		ct[i] = pt[i] ^ 0x5a
	}
	return []byte{1}, ct, nil
}

func (x xorSealer) Open(nonce, ct []byte) ([]byte, error) {
	if x.fail {
		return nil, errors.New("xorSealer: refusing")
	}
	pt := make([]byte, len(ct))
	for i := range ct {
		pt[i] = ct[i] ^ 0x5a
	}
	return pt, nil
}

// sealedAd encodes an ad with one sealed attribute (Secret) and one plain one (Cpus).
func sealedAd(t *testing.T) (b []byte, resolve func(uint32) (string, bool)) {
	t.Helper()
	tbl := NewInternTable()
	ad := &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
		{Name: "Cpus", Value: &ast.IntegerLiteral{Value: 4}},
		{Name: "Secret", Value: &ast.StringLiteral{Value: "s3cr3t"}},
	}}
	b = EncodeWithHotEnc(nil, ad, tbl, nil,
		func(name string) bool { return name == "Secret" }, xorSealer{})
	return b, tbl.Name
}

func TestSealedRedactYieldsUndefinedNotFailure(t *testing.T) {
	b, resolve := sealedAd(t)
	// No key at all: the ad must still decode, with the sealed attribute undefined.
	got, err := DecodeResolveEncRedact(b, resolve, nil)
	if err != nil {
		t.Fatalf("redacting decode failed: %v", err)
	}
	var sawCpus, sawSecret bool
	for _, a := range got.Attributes {
		switch a.Name {
		case "Cpus":
			sawCpus = true
		case "Secret":
			sawSecret = true
			if _, ok := a.Value.(*ast.UndefinedLiteral); !ok {
				t.Errorf("Secret decoded as %T (%s), want undefined -- an ERROR value would make "+
					"`Secret =!= undefined` true and report the secret's existence", a.Value, a.Value)
			}
		}
	}
	if !sawCpus {
		t.Error("the public attribute did not survive a redacting decode")
	}
	if !sawSecret {
		t.Error("the sealed attribute vanished entirely; undefined is the intended substitute")
	}
	if s := got.String(); strings.Contains(s, "s3cr3t") {
		t.Errorf("the secret leaked into the redacted ad: %q", s)
	}
	// A key that refuses to open takes the same path: unentitled and broken are the same to a reader.
	if _, err := DecodeResolveEncRedact(b, resolve, xorSealer{fail: true}); err != nil {
		t.Errorf("redacting decode failed on a refusing sealer: %v", err)
	}
}

func TestSealedStrictStillFailsAndSaysWhy(t *testing.T) {
	b, resolve := sealedAd(t)
	_, err := DecodeResolveEnc(b, resolve, nil)
	if err == nil {
		t.Fatal("strict decode accepted a sealed attribute with no key; a write path would then " +
			"re-encode the substitution over the secret")
	}
	if !errors.Is(err, ErrSealed) {
		t.Errorf("strict decode error is not ErrSealed: %v", err)
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("strict decode error no longer wraps ErrMalformed, which callers switch on: %v", err)
	}
}

func TestSealedOpensWithTheKey(t *testing.T) {
	b, resolve := sealedAd(t)
	for _, name := range []string{"strict", "redacting"} {
		var got *ast.ClassAd
		var err error
		if name == "strict" {
			got, err = DecodeResolveEnc(b, resolve, xorSealer{})
		} else {
			got, err = DecodeResolveEncRedact(b, resolve, xorSealer{})
		}
		if err != nil {
			t.Fatalf("%s decode with the key failed: %v", name, err)
		}
		if !strings.Contains(got.String(), "s3cr3t") {
			t.Errorf("%s decode with the key did not recover the value: %s", name, got.String())
		}
	}
}
