package collections

import (
	"fmt"
	"testing"
)

// TestColBlockLayoutSharedAcrossBlocks is the regression for the per-block metadata heap: every base
// block of a segment must reference ONE shared *colLayout (not a per-block copy of the hot/cold
// partition + column offsets), and the per-record offsets must round-trip through the packed-u32
// aliased form. A per-block layout map was ~850MB of live heap on a large archive.
func TestColBlockLayoutSharedAcrossBlocks(t *testing.T) {
	c := New(Options{Shards: 1})
	var wires [][]byte
	for i := 0; i < 60; i++ {
		ad := mustAdOld(t, fmt.Sprintf("Cpus=%d\nMemory=%d\nOwner=\"u%d\"\nExtra=%d",
			1+i%8, 1024+i*64, i%5, i*100000)) // Extra escapes narrow widths -> cold tail exercised
		wires = append(wires, c.encodeAd(ad.AST()))
	}
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.80, Fit: 0.90, Strings: true})
	recs := encodeRows(s, wires)

	// Two blocks built under the SAME shared layout, as a segment's row groups are.
	layout := resolveColLayout(s, hotHalf(s))
	b1 := encodeColumnarBlock(s, recs[:30], layout, identityCodec{}, nil)
	b2 := encodeColumnarBlock(s, recs[30:], layout, identityCodec{}, nil)
	if b1.layout != b2.layout {
		t.Fatal("freshly built blocks passed one layout but do not share it")
	}
	offs := make([]uint32, 60)
	for i := range offs {
		offs[i] = uint32(i * 7)
	}
	orig := &colSegment{blocks: []*columnarBlock{b1, b2}, offs: offs}

	got := unmarshalColSegment(marshalColSegment(orig, c.intern.Name), identityCodec{}, c.intern.Intern)
	if got == nil || len(got.blocks) != 2 {
		t.Fatal("unmarshal returned nil or wrong block count")
	}
	// THE MEMORY FIX: the reopen path shares one layout across the segment's base blocks.
	if got.blocks[0].layout != got.blocks[1].layout {
		t.Error("reopened base blocks do not share one colLayout -- per-block layout has regressed")
	}
	// Offsets aliased as packed u32: every record must reconstruct identically to the source block.
	for bi, b := range []*columnarBlock{b1, b2} {
		gb := got.blocks[bi]
		if len(gb.strOffB) != (gb.n+1)*4 || len(gb.coldOffB) != (gb.n+1)*4 {
			t.Fatalf("block %d offset arrays not packed u32: strOffB=%d coldOffB=%d (n=%d)",
				bi, len(gb.strOffB), len(gb.coldOffB), gb.n)
		}
		for k := 0; k < b.n; k++ {
			a, err := b.reconstruct(k, nil)
			if err != nil {
				t.Fatal(err)
			}
			g, err := gb.reconstruct(k, nil)
			if err != nil {
				t.Fatal(err)
			}
			if string(a) != string(g) {
				t.Fatalf("block %d rec %d differs after marshal round-trip", bi, k)
			}
		}
	}
}
