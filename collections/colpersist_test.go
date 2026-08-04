package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// TestColSegmentMarshalRoundTrip serializes a colSegment and checks the decoded block is
// functionally identical: same schema layout, same hot/cold partition, same offs, and every
// record reconstructs byte-identically and scans to the same value.
func TestColSegmentMarshalRoundTrip(t *testing.T) {
	c := New(Options{Shards: 1})
	var wires [][]byte
	for i := 0; i < 120; i++ {
		ad := mustAdOld(t, fmt.Sprintf(
			"Cpus=%d\nMemory=%d\nDisk=%d\nBig=%t\nLoad=%f\nArch=\"X86_64\"\nMachine=\"m%03d.example.org\"\nExtra=%d",
			1+i%8, 1024+i*64, i*4096, i%2 == 0, float64(i)/3, i, i*100000)) // Extra escapes small widths
		wires = append(wires, c.encodeAd(ad.AST()))
	}
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.80, Fit: 0.90, Strings: true})
	recs := encodeRows(s, wires)

	for _, codec := range []Codec{identityCodec{}, mustZSTD(t)} {
		blk := encodeColumnarBlock(s, recs, hotHalf(s), codec)
		offs := make([]uint32, blk.n)
		for i := range offs {
			offs[i] = uint32(i * 7) // arbitrary stand-in arena offsets
		}
		orig := &colSegment{block: blk, offs: offs}

		got := unmarshalColSegment(marshalColSegment(orig, c.intern.Name), codec, c.intern.Intern)
		if got == nil {
			t.Fatalf("%s: unmarshal returned nil", codec.Name())
		}
		gb := got.block
		if gb.n != blk.n || gb.hotStride != blk.hotStride || len(gb.hotNum) != len(blk.hotNum) || len(gb.coldNum) != len(blk.coldNum) {
			t.Fatalf("%s: layout mismatch: n=%d/%d stride=%d/%d hot=%d/%d cold=%d/%d",
				codec.Name(), gb.n, blk.n, gb.hotStride, blk.hotStride, len(gb.hotNum), len(blk.hotNum), len(gb.coldNum), len(blk.coldNum))
		}
		for i := range offs {
			if got.offs[i] != offs[i] {
				t.Fatalf("%s: offs[%d]=%d, want %d", codec.Name(), i, got.offs[i], offs[i])
			}
		}
		// Every record reconstructs byte-identically from the decoded block.
		for k := 0; k < gb.n; k++ {
			a, err := blk.reconstruct(k, nil)
			if err != nil {
				t.Fatal(err)
			}
			b, err := gb.reconstruct(k, nil)
			if err != nil {
				t.Fatal(err)
			}
			if string(a) != string(b) {
				t.Fatalf("%s: rec[%d] differs after marshal round-trip", codec.Name(), k)
			}
			// And decodes to the same attributes as the source ad.
			assertRoundTrip(t, s, wires[k], fmt.Sprintf("%s decoded rec[%d]", codec.Name(), k))
		}
		// A scan on the decoded block matches the source.
		memID, _ := c.intern.LookupID("Memory")
		if fidx, ok := s.byID[memID]; ok && s.fields[fidx].kind == akInt {
			mismatch := 0
			gb.scanInt(fidx, nil, func(k int, p bool, v int64) {
				w := wires[k]
				if node, ok := wire.Ad(w).Lookup(memID); ok {
					if lit, _ := wire.LiteralValue(node); p && lit.Kind == wire.LitInt && v != lit.Int {
						mismatch++
					}
				}
			})
			if mismatch != 0 {
				t.Errorf("%s: %d scan mismatches after round-trip", codec.Name(), mismatch)
			}
		}
	}
}

// TestUnmarshalColSegmentTruncated verifies malformed/truncated data returns nil, not a panic.
func TestUnmarshalColSegmentTruncated(t *testing.T) {
	c := New(Options{Shards: 1})
	var wires [][]byte
	for i := 0; i < 20; i++ {
		wires = append(wires, c.encodeAd(mustAdOld(t, fmt.Sprintf("Cpus=%d\nMemory=%d", 1+i, 1024+i)).AST()))
	}
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.5, Fit: 0.95})
	blk := encodeColumnarBlock(s, encodeRows(s, wires), nil, identityCodec{})
	full := marshalColSegment(&colSegment{block: blk, offs: make([]uint32, blk.n)}, c.intern.Name)
	for _, cut := range []int{0, 1, len(full) / 2, len(full) - 1} {
		if got := unmarshalColSegment(full[:cut], identityCodec{}, c.intern.Intern); got != nil {
			t.Errorf("truncated to %d bytes: expected nil, got a block", cut)
		}
	}
}

// TestColSegmentSchemaNameAnchored is the P3(2a) correctness proof: a persisted block's schema is
// keyed by attribute NAME, not by the runtime intern id, so it survives a reopen that reassigns
// ids. Marshal a block under one intern table, unmarshal under a fresh table that gives the SAME
// names DIFFERENT ids, and confirm routing-by-name still lands on the right field and scans
// correctly. With id-keyed serialization this test fails (byID holds stale ids).
func TestColSegmentSchemaNameAnchored(t *testing.T) {
	c := New(Options{Shards: 1})
	var wires [][]byte
	for i := 0; i < 80; i++ {
		wires = append(wires, c.encodeAd(mustAdOld(t,
			fmt.Sprintf("Cpus=%d\nMemory=%d\nDisk=%d", 1+i%8, 1024+i*64, i*4096)).AST()))
	}
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.8, Fit: 0.95})
	blk := encodeColumnarBlock(s, encodeRows(s, wires), hotHalf(s), identityCodec{})
	data := marshalColSegment(&colSegment{block: blk, offs: make([]uint32, blk.n)}, c.intern.Name)

	// Simulate a reopen: a fresh intern table that assigns DIFFERENT ids to the same names
	// (decoys shift first-seen order so Memory/Cpus/Disk do not land on their original ids).
	reopened := wire.NewInternTable()
	for _, d := range []string{"D0", "D1", "D2", "D3", "D4", "D5"} {
		reopened.Intern(d)
	}
	for _, name := range []string{"Cpus", "Memory", "Disk"} {
		aID, _ := c.intern.LookupID(name)
		if bID := reopened.Intern(name); aID == bID {
			t.Fatalf("setup: %s kept id %d across tables; decoys did not shift ids", name, aID)
		}
	}

	got := unmarshalColSegment(data, identityCodec{}, reopened.Intern)
	if got == nil {
		t.Fatal("unmarshal returned nil")
	}
	// Every field re-bound to the REOPENED table's id, and byID indexes it under that id.
	for _, f := range got.block.schema.fields {
		name, ok := reopened.Name(f.id)
		if !ok {
			t.Fatalf("field id %d not resolvable in the reopened table", f.id)
		}
		if idx, ok := got.block.schema.byID[f.id]; !ok || got.block.schema.fields[idx].id != f.id {
			t.Fatalf("byID does not index field %q (id %d)", name, f.id)
		}
	}
	// Routing by name through the reopened table lands on the Memory int field...
	memID := reopened.Intern("Memory")
	idx, ok := got.block.schema.byID[memID]
	if !ok || got.block.schema.fields[idx].kind != akInt {
		t.Fatalf("Memory (reopened id %d) not resolvable as an int field", memID)
	}
	// ...and the block still scans that field's values correctly (records are positional, so the
	// scan is independent of the intern id -- but the field is FOUND only because it is name-keyed).
	origMemID, _ := c.intern.LookupID("Memory")
	mismatch := 0
	got.block.scanInt(idx, nil, func(k int, p bool, v int64) {
		if node, ok := wire.Ad(wires[k]).Lookup(origMemID); ok {
			if lit, _ := wire.LiteralValue(node); p && lit.Kind == wire.LitInt && v != lit.Int {
				mismatch++
			}
		}
	})
	if mismatch != 0 {
		t.Fatalf("%d Memory scan mismatches after reopen", mismatch)
	}
}
