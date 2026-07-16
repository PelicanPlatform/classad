package wire

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/crypt"
)

// dekSealer adapts a raw DEK to the wire.Sealer interface via crypt's AES-256-GCM.
type dekSealer struct{ dek []byte }

func (s dekSealer) Seal(pt []byte) (nonce, ct []byte, err error) { return crypt.Seal(s.dek, pt) }
func (s dekSealer) Open(nonce, ct []byte) ([]byte, error)        { return crypt.Open(s.dek, nonce, ct) }

func mkAd() *ast.ClassAd {
	return &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
		{Name: "Owner", Value: &ast.StringLiteral{Value: "alice"}},
		{Name: "ClaimId", Value: &ast.StringLiteral{Value: "secret-capability-1234"}},
		{Name: "Cpus", Value: &ast.IntegerLiteral{Value: 8}},
	}}
}

// TestEncryptedAttributeRoundTrip encrypts one attribute at rest: without the key the
// ad still scans and the plaintext attributes read (encrypted one opaque); with the
// key DecodeInlineEnc recovers every value including the secret.
func TestEncryptedAttributeRoundTrip(t *testing.T) {
	dek, err := crypt.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	sealer := dekSealer{dek}
	enc := map[string]struct{}{"claimid": {}}

	b := EncodeInlineWithHotEnc(nil, mkAd(), nil, enc, sealer)

	// The plaintext of the secret must not appear anywhere in the encoded bytes.
	if idx := indexOf(b, []byte("secret-capability-1234")); idx >= 0 {
		t.Fatalf("secret leaked into the encoded ad at offset %d", idx)
	}

	// Full decode WITH the key recovers all three values.
	ad, err := DecodeInlineEnc(b, sealer)
	if err != nil {
		t.Fatalf("DecodeInlineEnc: %v", err)
	}
	got := map[string]string{}
	for _, a := range ad.Attributes {
		got[a.Name] = a.Value.String()
	}
	if got["Owner"] != `"alice"` || got["Cpus"] != "8" || got["ClaimId"] != `"secret-capability-1234"` {
		t.Fatalf("recovered ad wrong: %v", got)
	}

	// Decode WITHOUT the key errors on the encrypted attribute (opaque, no key).
	if _, err := DecodeInline(b); err == nil {
		t.Fatal("DecodeInline without a key should fail on an encrypted attribute")
	}

	// A wrong key fails to open (GCM tag).
	otherDEK, _ := crypt.NewDEK()
	if _, err := DecodeInlineEnc(b, dekSealer{otherDEK}); err == nil {
		t.Fatal("decode with the wrong key should fail")
	}
}

// TestEncryptedAttributeNotHot verifies an encrypted attribute is never placed in the
// hot header even if requested, and that the plaintext attributes still index normally.
func TestEncryptedAttributeNotHot(t *testing.T) {
	dek, _ := crypt.NewDEK()
	sealer := dekSealer{dek}
	hot := map[string]struct{}{"owner": {}, "claimid": {}}
	enc := map[string]struct{}{"claimid": {}}

	ad := Ad(EncodeInlineWithHotEnc(nil, mkAd(), hot, enc, sealer))
	// Owner (plaintext, hot) is reachable; ClaimId (encrypted) is still present via a scan.
	if _, ok := ad.LookupByName("Owner"); !ok {
		t.Error("Owner should be found")
	}
	if _, ok := ad.LookupByName("ClaimId"); !ok {
		t.Error("ClaimId entry should still be present (as an opaque encrypted node)")
	}
	// Only Owner is hot: the encrypted attr is excluded even though it was in hot.
	nHot := 0
	ad.ForEachHot(func(uint32, []byte) bool { nHot++; return true })
	if nHot != 1 {
		t.Errorf("hot count = %d, want 1 (encrypted attr excluded)", nHot)
	}
}

func indexOf(hay, needle []byte) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if string(hay[i:i+len(needle)]) == string(needle) {
			return i
		}
	}
	return -1
}
