package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// escFixture plants all three escape shapes on purpose:
//
//	Always   present and in-slot on every record          -> escNone
//	Gone     absent on some records, never wrong-typed     -> escMissing
//	Typed    present on every record, wrong type on some   -> escExcept
//	Both     absent on some, wrong-typed on others         -> escMixedCls
func escFixture(t *testing.T, c *Collection, n int) ([][]byte, *adSchema) {
	t.Helper()
	var iws [][]byte
	for i := range n {
		var b string
		switch {
		case i%10 < 6:
			b = fmt.Sprintf(`[ Always=%d; Gone=%d; Typed=%d; Both=%d ]`, i, i, i, i)
		case i%10 < 8:
			b = fmt.Sprintf(`[ Always=%d; Typed="str%d"; Both=%d ]`, i, i, i) // Gone absent, Typed wrong-typed
		case i%10 < 9:
			b = fmt.Sprintf(`[ Always=%d; Gone=%d; Typed=%d ]`, i, i, i) // Both absent
		default:
			b = fmt.Sprintf(`[ Always=%d; Gone=%d; Typed=%d; Both="s%d" ]`, i, i, i, i) // Both wrong-typed
		}
		ad, err := classad.Parse(b)
		if err != nil {
			t.Fatal(err)
		}
		iws = append(iws, wire.Encode(nil, ad.AST(), c.intern))
	}
	return iws, buildAdSchema(iws, adSchemaOpts{Presence: 0.50, Fit: 0.95, Strings: false})
}

// TestEscapeClassMatchesColdTail is the correctness contract: for EVERY escaped (record, field), the
// class must agree with what the cold tail actually says. A disagreement is a wrong `is undefined`
// answer, reported confidently and without reading anything -- the worst failure mode available.
func TestEscapeClassMatchesColdTail(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	const n = 400
	iws, s := escFixture(t, c, n)
	var rows [][]byte
	for _, iw := range iws {
		rows = append(rows, s.encode(wire.Ad(iw)))
	}
	blk := encodeColumnarBlock(s, rows, resolveColLayout(s, nil), c.regionCodec(), nil)
	bc, err := newBlockCache(1 << 20)
	if err != nil {
		t.Fatal(err)
	}

	classified, checked := 0, 0
	for k := 0; k < blk.n; k++ {
		for fi, f := range s.fields {
			if !testBit(blk.escapeAt(k), fi) {
				continue
			}
			checked++
			_, found, err := blk.escapedNode(k, f.id, bc)
			if err != nil {
				t.Fatal(err)
			}
			wantMissing := !found
			gotMissing, ok := blk.escapeIsMissing(fi, k)
			if !ok {
				continue // the block declines to say; the caller reads the cold tail
			}
			classified++
			if gotMissing != wantMissing {
				name, _ := c.intern.Name(f.id)
				t.Fatalf("record %d field %s: class says missing=%v, cold tail says missing=%v",
					k, name, gotMissing, wantMissing)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no escapes in the fixture; the test proves nothing")
	}
	if classified == 0 {
		t.Fatal("the class answered for no escape at all")
	}
	t.Logf("escapes %d, classified without a cold-tail read %d (%.1f%%)",
		checked, classified, float64(classified)/float64(checked)*100)
}

// TestEscapeClassCoversAllShapes: the fixture must actually produce all four classes, or the test
// above would be checking a narrower thing than it claims.
func TestEscapeClassCoversAllShapes(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	iws, s := escFixture(t, c, 400)
	var rows [][]byte
	for _, iw := range iws {
		rows = append(rows, s.encode(wire.Ad(iw)))
	}
	blk := encodeColumnarBlock(s, rows, resolveColLayout(s, nil), c.regionCodec(), nil)
	seen := map[uint8]string{}
	for fi, f := range s.fields {
		name, _ := c.intern.Name(f.id)
		seen[blk.escClassOf(fi)] = name
	}
	for _, want := range []struct {
		cls  uint8
		desc string
	}{
		{escNone, "never escapes"},
		{escMissing, "only ever absent"},
		{escExcept, "only ever wrong-typed"},
		{escMixedCls, "both"},
	} {
		if _, ok := seen[want.cls]; !ok {
			t.Errorf("no field classified %s (%d); classes present: %v", want.desc, want.cls, seen)
		}
	}
	// And a mixed field must carry its exceptional record list, or it cannot be disambiguated.
	for fi := range s.fields {
		if blk.escClassOf(fi) == escMixedCls && len(blk.escExcRecs[fi]) == 0 {
			t.Errorf("field %d is mixed but has no exceptional record list", fi)
		}
	}
}

// TestEscapeClassSurvivesPersistence: the classes and the mixed fields' record lists must round-trip,
// or a reopened table silently loses the fast path (or worse, keeps a stale one).
func TestEscapeClassSurvivesPersistence(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	const n = 300
	iws, s := escFixture(t, c, n)
	var rows [][]byte
	for _, iw := range iws {
		rows = append(rows, s.encode(wire.Ad(iw)))
	}
	blk := encodeColumnarBlock(s, rows, resolveColLayout(s, nil), identityCodec{}, nil)
	cs := &colSegment{blocks: []*columnarBlock{blk}, offsB: make([]byte, n*4)}
	blob := marshalColSegment(cs, func(id uint32) (string, bool) { return c.intern.Name(id) })
	got := unmarshalColSegment(blob, identityCodec{}, func(name string) uint32 { return c.intern.Intern(name) })
	if got == nil {
		t.Fatal("unmarshal returned nil")
	}
	rb := got.blocks[0]
	if len(rb.escClass) != len(blk.escClass) {
		t.Fatalf("reloaded %d classes, want %d", len(rb.escClass), len(blk.escClass))
	}
	for i := range blk.escClass {
		if rb.escClass[i] != blk.escClass[i] {
			t.Errorf("field %d: class %d after reload, was %d", i, rb.escClass[i], blk.escClass[i])
		}
	}
	if len(rb.escExcRecs) != len(blk.escExcRecs) {
		t.Fatalf("reloaded %d mixed fields, want %d", len(rb.escExcRecs), len(blk.escExcRecs))
	}
	for fi, want := range blk.escExcRecs {
		gotRecs := rb.escExcRecs[fi]
		if len(gotRecs) != len(want) {
			t.Fatalf("field %d: %d exceptional records after reload, want %d", fi, len(gotRecs), len(want))
		}
		for i := range want {
			if gotRecs[i] != want[i] {
				t.Fatalf("field %d record %d: %d after reload, want %d", fi, i, gotRecs[i], want[i])
			}
		}
	}
}

// TestBlockAbsenceProofIsExact: the whole-block absence bit must be set only when NO record carries
// the field, and must be set whenever that holds. Too eager is a wrong `is undefined` answer for the
// whole block; too shy only costs the work it was meant to skip.
func TestBlockAbsenceProofIsExact(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	const n = 200
	// Ghost is in the schema (present in the sample used to build it) but absent from every record
	// of the block we encode -- the shape a group's non-members produce, and the shape a table gets
	// after a workload stops writing an attribute.
	var sample [][]byte
	for i := range n {
		ad, err := classad.Parse(fmt.Sprintf(`[ Keep=%d; Ghost=%d ]`, i, i))
		if err != nil {
			t.Fatal(err)
		}
		sample = append(sample, wire.Encode(nil, ad.AST(), c.intern))
	}
	s := buildAdSchema(sample, adSchemaOpts{Presence: 0.50, Fit: 0.95, Strings: false})
	ghostID := c.intern.Intern("Ghost")
	gi, ok := s.byID[ghostID]
	if !ok {
		t.Fatal("Ghost not in the schema; the fixture proves nothing")
	}

	var rows [][]byte
	for i := range n {
		ad, err := classad.Parse(fmt.Sprintf(`[ Keep=%d ]`, i)) // no Ghost at all
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, s.encode(wire.Ad(wire.Encode(nil, ad.AST(), c.intern))))
	}
	blk := encodeColumnarBlock(s, rows, resolveColLayout(s, nil), c.regionCodec(), nil)
	if !blk.fieldAbsentFromBlock(gi) {
		t.Error("Ghost is absent from every record but the block does not prove it")
	}
	for fi := range s.fields {
		if fi == gi && blk.fieldAbsentFromBlock(fi) {
			continue
		}
		if blk.fieldAbsentFromBlock(fi) {
			t.Errorf("field %d claimed absent from the whole block, but records carry it", fi)
		}
	}

	// One record carrying Ghost must retract the proof entirely.
	adG, err := classad.Parse(fmt.Sprintf(`[ Keep=%d; Ghost=%d ]`, 0, 7))
	if err != nil {
		t.Fatal(err)
	}
	rows[0] = s.encode(wire.Ad(wire.Encode(nil, adG.AST(), c.intern)))
	blk2 := encodeColumnarBlock(s, rows, resolveColLayout(s, nil), c.regionCodec(), nil)
	if blk2.fieldAbsentFromBlock(gi) {
		t.Error("one record carries Ghost, but the block still claims it absent from all of them")
	}
}
