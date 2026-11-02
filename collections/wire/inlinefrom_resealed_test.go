package wire

import (
	"bytes"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/crypt"
)

// TestInlineFromInternedResealed: the resealing rewrite of an interned ad gives, read with the key, what
// the keyed decode of the stored ad gives -- every sealed value included, names and all -- while no
// sealed value appears in the clear, every carried one is sealed in the inline-names form (it opens and
// parses as inline, not through the reader's recovery for misplaced inline plaintext), and a sealed value
// keep rejects is never opened.
func TestInlineFromInternedResealed(t *testing.T) {
	dek, err := crypt.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	key := &openCounter{inner: dekSealer{dek}}
	sealed := func(name string) bool {
		return strings.EqualFold(name, "ClaimId") || strings.EqualFold(name, "SecretExpr") ||
			strings.EqualFold(name, "Rec") || strings.EqualFold(name, "F")
	}
	tt := NewInternTable()
	interned := EncodeWithHotEnc(nil, unopenedAd(), tt, map[uint32]struct{}{tt.Intern("Owner"): {}}, sealed, key)
	keyed, err := DecodeResolveEnc(interned, tt.Name, key)
	if err != nil {
		t.Fatal(err)
	}

	key.uses.Store(0)
	var sc SubsetScratch
	got, n, ok := AppendAdInlineFromInternedResealed(nil, Ad(interned), tt.Name, nil, key, &sc)
	if !ok {
		t.Fatal("refused a well-formed interned ad")
	}
	if n != 4 || key.uses.Load() != 8 {
		t.Fatalf("re-sealed %d values with %d uses of the key, want 4 values, each opened and sealed once", n, key.uses.Load())
	}
	if bytes.Contains(got, []byte("top-secret")) || bytes.Contains(got, []byte("Deep")) || bytes.Contains(got, []byte("ProcId")) {
		t.Fatal("the rewrite carries a sealed value's plaintext")
	}
	if cap(sc.plainBuf) == 0 || !allZero(sc.plainBuf[:cap(sc.plainBuf)]) {
		t.Fatalf("the scratch buffer (cap %d) was not used, or still holds a plaintext", cap(sc.plainBuf))
	}
	for _, name := range []string{"ClaimId", "SecretExpr", "Rec", "F"} {
		node, present := Ad(got).LookupByName(name)
		if !present || !IsEncryptedNode(node) {
			t.Fatalf("%s: present %v, sealed %v; want it carried sealed", name, present, IsEncryptedNode(node))
		}
		// Inline form: it parses with the inline decoder, which refuses an interned id outright.
		if _, derr := DecodeNodeInlineEnc(node, key); derr != nil {
			t.Fatalf("%s: does not read in the inline-names form: %v", name, derr)
		}
	}
	recovered := SealedInlineFormRecoveries()
	back, err := DecodeInlineEnc(got, key)
	if err != nil {
		t.Fatal(err)
	}
	if render(back) != render(keyed) {
		t.Fatalf("rewrite disagrees with the keyed decode:\n got %s\nwant %s", render(back), render(keyed))
	}
	if SealedInlineFormRecoveries() != recovered {
		t.Fatal("the rewrite was read through the inline-form recovery")
	}

	// A sealed value keep rejects is dropped before it is opened.
	key.uses.Store(0)
	dropSealed := func(name, node []byte) bool { return !IsEncryptedNode(node) || !strings.EqualFold(string(name), "Rec") }
	got, n, ok = AppendAdInlineFromInternedResealed(nil, Ad(interned), tt.Name, dropSealed, key, &sc)
	if !ok || n != 3 || key.uses.Load() != 6 {
		t.Fatalf("ok %v, re-sealed %d, %d uses of the key; want 3 values re-sealed and Rec never opened", ok, n, key.uses.Load())
	}
	if _, has := Ad(got).LookupByName("Rec"); has {
		t.Fatal("a rejected sealed value was carried")
	}
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// TestInlineFromInternedResealedLegacyForm: a sealed value an earlier build copied unopened from an
// inline-names record into an interned one holds inline-form plaintext. Re-sealed, it is carried in that
// form, which is the one an inline record needs.
func TestInlineFromInternedResealedLegacyForm(t *testing.T) {
	dek, err := crypt.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	s := dekSealer{dek}
	v := &ast.RecordLiteral{ClassAd: &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
		attr("alpha", ast.NewAttributeReference("Beta", ast.NoScope)),
	}}}
	tt := NewInternTable()
	src := internedWithSealedNode(tt, inlineSealedNode(t, s, v))
	var sc SubsetScratch
	got, n, ok := AppendAdInlineFromInternedResealed(nil, src, tt.Name, nil, s, &sc)
	if !ok || n != 1 {
		t.Fatalf("ok %v, re-sealed %d", ok, n)
	}
	node, _ := Ad(got).LookupByName("Secret")
	back, err := DecodeNodeInlineEnc(node, s)
	if err != nil {
		t.Fatal(err)
	}
	if back.String() != v.String() {
		t.Fatalf("got %s, want %s", back, v)
	}
}

// TestInlineFromInternedResealedRefusals: a carried sealed value that does not open -- tampered, or
// sealed under another key -- refuses the whole rewrite rather than dropping the value or copying it.
// So do the inputs the unopened rewrite refuses, an inline-names src, and a missing key.
func TestInlineFromInternedResealedRefusals(t *testing.T) {
	dek, err := crypt.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	other, err := crypt.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	s := dekSealer{dek}
	all := func(string) bool { return true }
	tt := NewInternTable()
	interned := EncodeWithHotEnc(nil, unopenedAd(), tt, nil, all, s)
	tampered := append([]byte(nil), interned...)
	tampered[len(tampered)-1] ^= 1
	cases := []struct {
		name    string
		src     []byte
		resolve func(uint32) (string, bool)
		key     Sealer
	}{
		{"tampered", tampered, tt.Name, s},
		{"other key", interned, tt.Name, dekSealer{other}},
		{"no key", interned, tt.Name, nil},
		{"inline-names src", EncodeInlineWithHotEnc(nil, unopenedAd(), nil, all, s), tt.Name, s},
		{"standalone src", EncodeStandalone(unopenedAd()), tt.Name, s},
		{"unresolvable id", interned, func(uint32) (string, bool) { return "", false }, s},
		{"truncated", interned[:len(interned)-3], tt.Name, s},
	}
	for _, tc := range cases {
		var sc SubsetScratch
		if _, _, ok := AppendAdInlineFromInternedResealed(nil, Ad(tc.src), tc.resolve, nil, tc.key, &sc); ok {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}
