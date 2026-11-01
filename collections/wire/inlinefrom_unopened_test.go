package wire

import (
	"bytes"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/crypt"
)

// openCounter is a Sealer that counts every use of the key.
type openCounter struct {
	inner Sealer
	uses  atomic.Int64
}

func (o *openCounter) Seal(pt []byte) ([]byte, []byte, error) {
	o.uses.Add(1)
	return o.inner.Seal(pt)
}

func (o *openCounter) Open(nonce, ct []byte) ([]byte, error) {
	o.uses.Add(1)
	return o.inner.Open(nonce, ct)
}

// unopenedAd has every name-carrying node kind beside two sealed values, one a plain literal and one
// an expression holding names.
func unopenedAd() *ast.ClassAd {
	return &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
		attr("Owner", &ast.StringLiteral{Value: "alice"}),
		attr("ClaimId", &ast.StringLiteral{Value: "top-secret"}),
		attr("Requirements", &ast.BinaryOp{Op: "<", Left: ast.NewAttributeReference("Memory", ast.TargetScope),
			Right: &ast.IntegerLiteral{Value: 4096}}),
		attr("Rec", &ast.RecordLiteral{ClassAd: &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("Inner", ast.NewAttributeReference("Deep", ast.NoScope)),
		}}}),
		attr("F", &ast.FunctionCall{Name: "strcat", Args: []ast.Expr{ast.NewAttributeReference("Owner", ast.NoScope)}}),
		attr("Sel", &ast.SelectExpr{Record: ast.NewAttributeReference("Rec", ast.NoScope), Attr: "Inner"}),
		attr("SecretExpr", &ast.BinaryOp{Op: "+", Left: ast.NewAttributeReference("ProcId", ast.NoScope),
			Right: &ast.IntegerLiteral{Value: 1}}),
	}}
}

// TestInlineFromInternedUnopened: the rewrite of an interned ad to inline names that never uses the key
// gives every unsealed attribute exactly as the keyed decode does, carries each sealed value as the
// same bytes it was stored as -- without opening it -- and reads, to a reader with no key, as the
// keyed decode then redaction does.
func TestInlineFromInternedUnopened(t *testing.T) {
	dek, err := crypt.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	key := &openCounter{inner: dekSealer{dek}}
	sealed := func(name string) bool {
		return strings.EqualFold(name, "ClaimId") || strings.EqualFold(name, "SecretExpr")
	}
	tt := NewInternTable()
	interned := EncodeWithHotEnc(nil, unopenedAd(), tt, map[uint32]struct{}{tt.Intern("Owner"): {}}, sealed, key)
	key.uses.Store(0)

	got, ok := AppendInlineFromInternedUnopened(nil, Ad(interned), tt.Name)
	if !ok {
		t.Fatal("refused a well-formed interned ad")
	}
	if n := key.uses.Load(); n != 0 {
		t.Fatalf("the rewrite used the key %d times", n)
	}
	if Ad(got)[2] != flagInlineNames {
		t.Fatalf("header flags = %#x, want inline-names only", Ad(got)[2])
	}
	if bytes.Contains(got, []byte("top-secret")) {
		t.Fatal("the rewrite carries a plaintext secret")
	}

	// The sealed values: present, byte for byte as stored.
	storedNode := func(name string) []byte {
		id, _ := tt.LookupID(name)
		n, ok := Ad(interned).Lookup(id)
		if !ok {
			t.Fatalf("%s missing from the stored ad", name)
		}
		return n
	}
	for _, name := range []string{"ClaimId", "SecretExpr"} {
		n, ok := Ad(got).LookupByName(name)
		if !ok || !IsEncryptedNode(n) {
			t.Fatalf("%s: present %v, sealed %v; want the sealed value carried", name, ok, IsEncryptedNode(n))
		}
		if !bytes.Equal(n, storedNode(name)) {
			t.Fatalf("%s: the sealed value's bytes changed", name)
		}
	}

	// Everything else: what the keyed decode of the stored ad gives, in order.
	keyed, err := DecodeResolveEnc(interned, tt.Name, key)
	if err != nil {
		t.Fatal(err)
	}
	unsealed := &ast.ClassAd{}
	for _, a := range keyed.Attributes {
		if !sealed(a.Name) {
			unsealed.Attributes = append(unsealed.Attributes, a)
		}
	}
	key.uses.Store(0)
	redacted, err := DecodeInlineRedact(got)
	if err != nil {
		t.Fatal(err)
	}
	var plain []string
	for _, a := range redacted.Attributes {
		if sealed(a.Name) {
			if _, undef := a.Value.(*ast.UndefinedLiteral); !undef {
				t.Fatalf("%s reads as %s to a reader with no key, want undefined", a.Name, a.Value)
			}
			continue
		}
		plain = append(plain, a.Name+"="+a.Value.String())
	}
	var want []string
	for _, a := range unsealed.Attributes {
		want = append(want, a.Name+"="+a.Value.String())
	}
	if fmt.Sprint(plain) != fmt.Sprint(want) {
		t.Fatalf("unsealed attributes:\n got %v\nwant %v", plain, want)
	}
	if n := key.uses.Load(); n != 0 {
		t.Fatalf("reading the rewrite with no key used the key %d times", n)
	}

	// The delta flag survives: a rewritten patch is still a patch.
	delta := append([]byte(nil), interned...)
	delta[2] |= flagDelta
	if out, ok := AppendInlineFromInternedUnopened(nil, Ad(delta), tt.Name); !ok || !IsDelta(out) {
		t.Fatalf("delta flag lost (ok %v)", ok)
	}
}

// TestInlineFromInternedUnopenedRefusals: the inputs a keyed decode through the same resolver refuses
// are refused here too, so a caller never gets a record from this that the decode would not have read.
func TestInlineFromInternedUnopenedRefusals(t *testing.T) {
	ad := unopenedAd()
	tt := NewInternTable()
	interned := Encode(nil, ad, tt)
	cases := []struct {
		name    string
		src     []byte
		resolve func(uint32) (string, bool)
	}{
		{"inline-names src", EncodeInline(nil, ad), tt.Name},
		{"standalone src", EncodeStandalone(ad), tt.Name},
		{"unresolvable id", interned, func(uint32) (string, bool) { return "", false }},
		{"no resolver", interned, nil},
		{"truncated", interned[:len(interned)-3], tt.Name},
	}
	for _, tc := range cases {
		if _, ok := AppendInlineFromInternedUnopened(nil, Ad(tc.src), tc.resolve); ok {
			t.Errorf("%s: accepted", tc.name)
		}
		if tc.resolve != nil {
			if _, err := DecodeResolveEnc(tc.src, tc.resolve, nil); err == nil {
				t.Errorf("%s: the keyed decode accepts it, so refusing it here loses a record", tc.name)
			}
		}
	}
	// And the unsealed grammar agrees with the decode across the shapes the subset rewrite is tested on.
	got, ok := AppendInlineFromInternedUnopened(nil, Ad(interned), tt.Name)
	if !ok {
		t.Fatal("refused a well-formed interned ad")
	}
	a, err := DecodeInline(got)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := DecodeResolve(interned, tt.Name)
	if err != nil {
		t.Fatal(err)
	}
	if render(a) != render(ref) {
		t.Fatalf("rewrite disagrees with the decode:\n got %s\nwant %s", render(a), render(ref))
	}
}
