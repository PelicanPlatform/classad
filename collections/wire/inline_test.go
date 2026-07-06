package wire

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

func TestInlineRoundTrip(t *testing.T) {
	for _, src := range roundTripCases {
		orig, err := parser.ParseClassAd(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		enc := EncodeInline(nil, orig)
		got, err := DecodeInline(enc)
		if err != nil {
			t.Fatalf("decode %q: %v", src, err)
		}
		if !toClassAd(orig).Equal(toClassAd(got)) {
			t.Errorf("inline round-trip mismatch for %q:\n orig=%s\n  got=%s", src, orig.String(), got.String())
		}
		// Inline ads decode with no intern table via the generic Decode too.
		if _, err := Decode(enc, nil); err != nil {
			t.Errorf("Decode(inline, nil) %q: %v", src, err)
		}
	}
}

func TestInlineLookupByName(t *testing.T) {
	src := `[Owner="alice"; RequestMemory=2048; Rank=Cpus*2; Nested=[k=1]; Req=(Cpus>1)&&(Arch=?="X86_64")]`
	orig, err := parser.ParseClassAd(src)
	if err != nil {
		t.Fatal(err)
	}
	hot := map[string]struct{}{"owner": {}, "rank": {}} // folded
	a := Ad(EncodeInlineWithHot(nil, orig, hot))

	byName := map[string]ast.Expr{}
	for _, at := range orig.Attributes {
		byName[at.Name] = at.Value
	}
	// Hot (Owner, Rank) via the header; non-hot (RequestMemory, Nested, Req) via scan.
	// Case-insensitive lookup too.
	for _, probe := range []string{"Owner", "owner", "RequestMemory", "rank", "Nested", "Req"} {
		node, ok := a.LookupByName(probe)
		if !ok {
			t.Fatalf("%s not found", probe)
		}
		e, err := DecodeNodeInline(node)
		if err != nil {
			t.Fatalf("%s decode: %v", probe, err)
		}
		want := byName[canonName(byName, probe)]
		if e.String() != want.String() {
			t.Errorf("%s = %s, want %s", probe, e.String(), want.String())
		}
	}
	if _, ok := a.LookupByName("Missing"); ok {
		t.Error("absent attribute reported found")
	}
	// LookupByName only applies to inline ads.
	interned := Ad(Encode(nil, orig, NewInternTable()))
	if _, ok := interned.LookupByName("Owner"); ok {
		t.Error("LookupByName should fail on an interned ad")
	}
}

// canonName finds the original-cased key matching probe case-insensitively.
func canonName(byName map[string]ast.Expr, probe string) string {
	for k := range byName {
		if foldEqualBytes([]byte(k), probe) {
			return k
		}
	}
	return probe
}

func TestInlineStreamEncoder(t *testing.T) {
	// Build [Cpus=8; Owner="alice"; Flag=true; Rank=Cpus*2; R=2.5] via the streaming
	// API and check it equals EncodeInline of the parsed ad (same attribute order).
	src := `[Cpus=8; Owner="alice"; Flag=true; Rank=Cpus*2; R=2.5]`
	orig, err := parser.ParseClassAd(src)
	if err != nil {
		t.Fatal(err)
	}
	s := NewInlineStreamEncoder(nil)
	s.Int("Cpus", 8)
	s.String("Owner", "alice")
	s.Bool("Flag", true)
	rank, _ := parser.ParseExpr("Cpus*2")
	s.Expr("Rank", rank)
	s.Real("R", 2.5)
	streamed := s.Bytes(nil)

	if want := EncodeInline(nil, orig); string(streamed) != string(want) {
		t.Errorf("streamed bytes differ from EncodeInline (%d vs %d)", len(streamed), len(want))
	}
	got, err := DecodeInline(streamed)
	if err != nil {
		t.Fatal(err)
	}
	if !toClassAd(orig).Equal(toClassAd(got)) {
		t.Errorf("streamed ad mismatch:\n orig=%s\n  got=%s", orig.String(), got.String())
	}

	// Reset + reuse; the Expr path emits inline node variants (nAttrRefStr).
	s.Reset()
	s.Int("X", 1)
	if g2, err := DecodeInline(s.Bytes(nil)); err != nil || g2.Attributes[0].Name != "X" {
		t.Fatalf("reset/reuse failed: %v", err)
	}
}

func FuzzInlineRoundTrip(f *testing.F) {
	for _, src := range roundTripCases {
		f.Add(src)
	}
	f.Fuzz(func(t *testing.T, src string) {
		orig, err := parser.ParseClassAd(src)
		if err != nil || orig == nil {
			return
		}
		enc := EncodeInline(nil, orig)
		got, err := DecodeInline(enc)
		if err != nil {
			t.Fatalf("decode failed for parseable ad %q: %v", src, err)
		}
		// Stability check must precede Equal, which sorts nested records in place.
		if string(enc) != string(EncodeInline(nil, got)) {
			t.Fatalf("inline re-encode not stable for %q", src)
		}
		if !toClassAd(orig).Equal(toClassAd(got)) {
			t.Fatalf("inline round-trip mismatch for %q", src)
		}
	})
}

// TestInlineVsInternedSize reports the pre-compression size delta from dropping
// interning, on the corpus.
func TestInlineVsInternedSize(t *testing.T) {
	var interned, inline, text int
	tbl := NewInternTable()
	for _, s := range roundTripCases {
		ad, err := parser.ParseClassAd(s)
		if err != nil {
			continue
		}
		text += len(s)
		interned += len(Encode(nil, ad, tbl))
		inline += len(EncodeInline(nil, ad))
	}
	t.Logf("corpus: text=%dB  interned=%dB (%.0f%%)  inline=%dB (%.0f%%)  inline/interned=%.2fx",
		text, interned, 100*float64(interned)/float64(text),
		inline, 100*float64(inline)/float64(text), float64(inline)/float64(interned))
}
