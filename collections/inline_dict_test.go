package collections

import (
	"bytes"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// TestDecodeAdDictAndSelfContained covers the two per-segment-interned decode helpers:
// decodeAdDict resolves an interned record via its dict, and toSelfContained rewrites an
// interned record into inline bytes that decode later WITHOUT the dict (the deferred/cold-path
// conversion watch-style captures need).
func TestDecodeAdDictAndSelfContained(t *testing.T) {
	c := New(Options{Shards: 1})
	table := wire.NewInternTable()
	ad, err := classad.ParseOld("Memory=4096\nCpus=8\nName=\"slot1@h\"\nRank=Memory*1.0\nReq=(Arch==\"X86_64\")")
	if err != nil {
		t.Fatal(err)
	}
	interned := wire.Encode(nil, ad.AST(), table)
	codec := identityCodec{}
	stored := codec.Compress(nil, interned)
	dict := appendSegDict(nil, table.Names())
	h := &segDictHandle{data: dict, base: 0}

	// Reference: the same interned record decoded with the in-memory table.
	refAST, err := wire.Decode(interned, table)
	if err != nil {
		t.Fatal(err)
	}
	want := wire.EncodeInline(nil, refAST)

	// decodeAdDict resolves ids via the dict, yielding the same ad.
	got, err := c.decodeAdDict(h, stored, codec)
	if err != nil {
		t.Fatalf("decodeAdDict: %v", err)
	}
	if !bytes.Equal(want, wire.EncodeInline(nil, got.AST())) {
		t.Errorf("decodeAdDict != table decode")
	}

	// toSelfContained rewrites to inline bytes that decode with no dict at all.
	sc := c.toSelfContained(h, stored, codec)
	dec, err := codec.Decompress(nil, sc)
	if err != nil {
		t.Fatal(err)
	}
	scAST, err := wire.DecodeInline(dec) // no dict / no table
	if err != nil {
		t.Fatalf("DecodeInline(self-contained): %v", err)
	}
	if !bytes.Equal(want, wire.EncodeInline(nil, scAST)) {
		t.Errorf("toSelfContained bytes decode differently than the original")
	}
	// And the self-contained bytes flow through the ordinary (dict-less) decodeAd.
	got2, err := c.decodeAdDict(nil, sc, codec)
	if err != nil {
		t.Fatalf("decodeAd(self-contained): %v", err)
	}
	if !bytes.Equal(want, wire.EncodeInline(nil, got2.AST())) {
		t.Errorf("decodeAd(self-contained) != original")
	}

	// dict==nil is a pass-through: toSelfContained returns the input unchanged.
	if sc2 := c.toSelfContained(nil, stored, codec); !bytes.Equal(sc2, stored) {
		t.Errorf("toSelfContained(nil) should be identity")
	}
}
