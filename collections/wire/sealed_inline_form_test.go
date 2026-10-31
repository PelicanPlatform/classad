package wire

import (
	"encoding/binary"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/crypt"
)

// internedWithSealedNode builds an interned ad [Id = 7; Secret = <sealed>] whose Secret entry is the
// sealed node bytes given, copied as they are -- what an earlier build's compaction wrote when it
// copied a sealed value unopened out of an inline-names record.
func internedWithSealedNode(tt *InternTable, sealed []byte) Ad {
	b := []byte{magicByte, formatVer, 0, 0, 2} // interned, no hot header, two attributes
	b = binary.AppendUvarint(b, uint64(tt.Intern("Id")))
	b = append(b, nInt)
	b = binary.AppendVarint(b, 7)
	b = binary.AppendUvarint(b, uint64(tt.Intern("Secret")))
	return append(b, sealed...)
}

// sealPlain seals pt as an nEncrypted node.
func sealPlain(t *testing.T, s Sealer, pt []byte) []byte {
	t.Helper()
	nonce, ct, err := s.Seal(pt)
	if err != nil {
		t.Fatal(err)
	}
	b := []byte{nEncrypted}
	b = binary.AppendUvarint(b, uint64(len(nonce)))
	b = append(b, nonce...)
	b = binary.AppendUvarint(b, uint64(len(ct)))
	return append(b, ct...)
}

// inlineSealedNode returns the sealed node the inline-names write path produces for v: its plaintext
// is v's node encoding with every name inline.
func inlineSealedNode(t *testing.T, s Sealer, v ast.Expr) []byte {
	t.Helper()
	ad := &ast.ClassAd{Attributes: []*ast.AttributeAssignment{attr("Secret", v)}}
	inline := EncodeInlineWithHotEnc(nil, ad, nil, func(string) bool { return true }, s)
	node, ok := Ad(inline).LookupByName("Secret")
	if !ok || !IsEncryptedNode(node) {
		t.Fatalf("inline encode did not seal Secret: %x", inline)
	}
	return node
}

// TestSealedInlineFormRecovered: a sealed value whose plaintext is in the inline-names form, inside an
// interned ad, reads as the value it was sealed with on every path that opens one -- the ast decode,
// a single-attribute lookup, the text renderer and the inline-subset assembler -- and each read is
// counted. Before the retry, the record cases failed with "unknown node tag 0x61" on all of them.
func TestSealedInlineFormRecovered(t *testing.T) {
	dek, err := crypt.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	s := dekSealer{dek}
	rec := func(members ...*ast.AttributeAssignment) ast.Expr {
		return &ast.RecordLiteral{ClassAd: &ast.ClassAd{Attributes: members}}
	}
	ab := rec(attr("a", &ast.IntegerLiteral{Value: 1}), attr("b", &ast.StringLiteral{Value: "x"}))
	cases := []struct {
		name      string
		secret    ast.Expr
		recovered bool // false: the value has no record member, so both forms are the same bytes
	}{
		{"record literal", ab, true},
		{"record inside a list", &ast.ListLiteral{Elements: []ast.Expr{&ast.IntegerLiteral{Value: 2}, ab}}, true},
		{"nested record", rec(attr("outer", ab), attr("c", &ast.BooleanLiteral{Value: true})), true},
		{"record with a reference", rec(attr("a", &ast.BinaryOp{Op: "+",
			Left: &ast.AttributeReference{Name: "Id"}, Right: &ast.IntegerLiteral{Value: 1}})), true},
		// A member name starting with a byte that is also a node tag: the interned read takes the
		// name's length for an id and the name for a whole value, and stops short of the end. The
		// interned form wins only on a COMPLETE parse, so the inline read, which is complete, is used.
		{"interned parse stops short", rec(attr("\x00", &ast.IntegerLiteral{Value: 1})), true},
		{"empty record", rec(), false},
		{"string", &ast.StringLiteral{Value: "capability"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.secret.String()
			tt := NewInternTable()
			ad := internedWithSealedNode(tt, inlineSealedNode(t, s, tc.secret))
			wantCount := int64(0)
			if tc.recovered {
				wantCount = 1
			}
			counted := func(path string, read func()) {
				t.Helper()
				before := SealedInlineFormRecoveries()
				read()
				if got := SealedInlineFormRecoveries() - before; got != wantCount {
					t.Errorf("%s: counted %d recoveries, want %d", path, got, wantCount)
				}
			}

			counted("DecodeResolveEnc", func() {
				back, err := DecodeResolveEnc(ad, tt.Name, s)
				if err != nil {
					t.Fatalf("DecodeResolveEnc: %v", err)
				}
				if got := back.Attributes[1].Value.String(); got != want {
					t.Errorf("DecodeResolveEnc: Secret = %s, want %s", got, want)
				}
			})
			node, ok := ad.Lookup(mustID(t, tt, "Secret"))
			if !ok {
				t.Fatal("Lookup(Secret) missed")
			}
			counted("DecodeNodeResolveEnc", func() {
				v, err := DecodeNodeResolveEnc(node, tt.Name, s)
				if err != nil {
					t.Fatalf("DecodeNodeResolveEnc: %v", err)
				}
				if got := v.String(); got != want {
					t.Errorf("DecodeNodeResolveEnc: %s, want %s", got, want)
				}
			})
			counted("AppendNodeTextOldEnc", func() {
				wantText, err := AppendNodeTextInlineOld(nil, mustInlineNode(t, tc.secret))
				if err != nil {
					t.Fatal(err)
				}
				got, err := AppendNodeTextOldEnc([]byte("prefix:"), node, tt, s, false)
				if err != nil {
					t.Fatalf("AppendNodeTextOldEnc: %v", err)
				}
				if string(got) != "prefix:"+string(wantText) {
					t.Errorf("AppendNodeTextOldEnc: %q, want %q", got, "prefix:"+string(wantText))
				}
			})
			counted("AppendAdSubsetInlineFromInterned", func() {
				var sc SubsetScratch
				out, ok := AppendAdSubsetInlineFromInterned(nil, ad, tt.Name, nil, s, &sc)
				if !ok {
					t.Fatal("AppendAdSubsetInlineFromInterned refused the record")
				}
				back, err := DecodeInline(out)
				if err != nil {
					t.Fatalf("decoding the subset: %v", err)
				}
				if len(back.Attributes) != 2 || back.Attributes[1].Value.String() != want {
					t.Errorf("subset = %v, want Secret = %s", back, want)
				}
			})
		})
	}
}

// TestSealedInlineFormNotMisrecovered: sealed plaintext that is not a complete, well-formed value in
// either key form is refused on every path, and nothing is counted. Each case is one way a retry that
// accepted less than a whole, strictly inline parse would hand back a value nobody sealed.
func TestSealedInlineFormNotMisrecovered(t *testing.T) {
	dek, err := crypt.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	s := dekSealer{dek}
	tt := NewInternTable()
	idID := tt.Intern("Id")
	inlineRec := mustInlineNode(t, &ast.RecordLiteral{ClassAd: &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
		attr("a", &ast.IntegerLiteral{Value: 1}),
	}}})
	cases := []struct {
		name   string
		sealed func() []byte
	}{
		// Neither form: the interned read meets 'a' as a node tag, the inline read a name longer
		// than what is left.
		{"garbage", func() []byte { return sealPlain(t, s, []byte{nRecord, 0, 1, 5, 'a', 'b'}) }},
		// A whole inline value and then more: the inline read succeeds but stops short.
		{"trailing bytes", func() []byte { return sealPlain(t, s, append(append([]byte(nil), inlineRec...), nUndefined)) }},
		// Inline keys, but an interned reference inside: no inline encoder writes that.
		{"interned id in inline plaintext", func() []byte {
			pt := []byte{nRecord, 0, 1, 1, 'a', nAttrRef, 0}
			return sealPlain(t, s, binary.AppendUvarint(pt, uint64(idID)))
		}},
		// A sealed value inside a sealed value: no encoder writes that either.
		{"nested seal", func() []byte {
			inner := sealPlain(t, s, []byte{nInt, 2})
			pt := append([]byte{nRecord, 0, 1, 1, 'a'}, inner...)
			return sealPlain(t, s, pt)
		}},
		// Ciphertext that does not authenticate.
		{"tampered", func() []byte {
			b := inlineSealedNode(t, s, &ast.RecordLiteral{ClassAd: &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
				attr("a", &ast.IntegerLiteral{Value: 1}),
			}}})
			b[len(b)-1] ^= 0xff
			return b
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ad := internedWithSealedNode(tt, tc.sealed())
			node, ok := ad.Lookup(mustID(t, tt, "Secret"))
			if !ok {
				t.Fatal("Lookup(Secret) missed")
			}
			before := SealedInlineFormRecoveries()
			if back, err := DecodeResolveEnc(ad, tt.Name, s); err == nil {
				t.Errorf("DecodeResolveEnc accepted it: %v", back)
			}
			if v, err := DecodeNodeResolveEnc(node, tt.Name, s); err == nil {
				t.Errorf("DecodeNodeResolveEnc accepted it: %v", v)
			}
			if txt, err := AppendNodeTextOldEnc(nil, node, tt, s, false); err == nil {
				t.Errorf("AppendNodeTextOldEnc accepted it: %q", txt)
			}
			// The assembler drops a value it cannot open (the tampered case) rather than refuse the
			// record; either way no value may come out.
			var sc SubsetScratch
			if out, ok := AppendAdSubsetInlineFromInterned(nil, ad, tt.Name, nil, s, &sc); ok {
				if _, has := Ad(out).LookupByName("Secret"); has {
					t.Errorf("AppendAdSubsetInlineFromInterned produced a Secret: %x", out)
				}
			}
			if n := SealedInlineFormRecoveries() - before; n != 0 {
				t.Errorf("counted %d recoveries of a value that is not one", n)
			}
		})
	}
}

func mustID(t *testing.T, tt *InternTable, name string) uint32 {
	t.Helper()
	id, ok := tt.LookupID(name)
	if !ok {
		t.Fatalf("%s is not interned", name)
	}
	return id
}

// mustInlineNode returns v's node encoding in the inline-names form.
func mustInlineNode(t *testing.T, v ast.Expr) []byte {
	t.Helper()
	inline := EncodeInline(nil, &ast.ClassAd{Attributes: []*ast.AttributeAssignment{attr("V", v)}})
	node, ok := Ad(inline).LookupByName("V")
	if !ok {
		t.Fatal("LookupByName(V) missed")
	}
	return node
}
