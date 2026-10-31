package wire

import (
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/PelicanPlatform/classad/parser"
)

// Readers of the wire form must answer a damaged record with ErrMalformed (or ok=false), never a
// panic: a panic in a reader takes the whole daemon down with it, for one bad record. The tests here
// build the malformed bytes directly, so each pins one way a record can be damaged.

// inlineEntry is one inline-names attribute entry: uvarint(len) + name + node.
func inlineEntry(name string, node []byte) []byte {
	b := binary.AppendUvarint(nil, uint64(len(name)))
	b = append(b, name...)
	return append(b, node...)
}

// internedEntry is one interned attribute entry: uvarint(id) + node.
func internedEntry(id uint64, node []byte) []byte {
	return append(binary.AppendUvarint(nil, id), node...)
}

// rawAd frames entries as an ad with the given flags and hot pairs.
func rawAd(flags byte, hot [][2]uint64, entries ...[]byte) []byte {
	b := []byte{magicByte, formatVer, flags}
	b = binary.AppendUvarint(b, uint64(len(hot)))
	for _, h := range hot {
		b = binary.AppendUvarint(b, h[0])
		b = binary.AppendUvarint(b, h[1])
	}
	b = binary.AppendUvarint(b, uint64(len(entries)))
	for _, e := range entries {
		b = append(b, e...)
	}
	return b
}

// idKeyedNodes are the three name-carrying node shapes in their INTERNED (id-keyed) form. None of them
// belongs in an inline-names ad, whose nodes name everything inline and which has no id space at all.
var idKeyedNodes = map[string][]byte{
	"attr-ref": {nAttrRef, 0, 0},                     // Foo, with Foo as id 0
	"func":     {nFunc, 0, 0},                        // f(), with f as id 0
	"select":   {nSelect, nUndefined, 0},             // undefined.Foo, with Foo as id 0
	"nested":   {nBinOp, 0, nInt, 2, nAttrRef, 0, 0}, // 1 + Foo: the id-keyed node below the top
}

// mustMalformed runs fn, failing the test if it panics or returns anything but ErrMalformed.
func mustMalformed(t *testing.T, what string, fn func() error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: panicked: %v", what, r)
		}
	}()
	if err := fn(); !errors.Is(err, ErrMalformed) {
		t.Errorf("%s: err = %v, want ErrMalformed", what, err)
	}
}

// An inline-names ad holding an id-keyed reference -- a remnant written in the wrong id space -- used to
// dereference the decoder's nil resolver. Every renderer and decoder of the inline form must call it
// malformed instead.
func TestInlineAdWithIDKeyedRefIsMalformed(t *testing.T) {
	for shape, node := range idKeyedNodes {
		ad := rawAd(flagInlineNames, nil, inlineEntry("A", node))
		mustMalformed(t, shape+" AppendNodeTextInline", func() error { _, err := AppendNodeTextInline(nil, node); return err })
		mustMalformed(t, shape+" AppendNodeTextInlineOld", func() error { _, err := AppendNodeTextInlineOld(nil, node); return err })
		mustMalformed(t, shape+" AppendNodeTextInlineOldEnc", func() error {
			_, err := AppendNodeTextInlineOldEnc(nil, node, nil, true)
			return err
		})
		mustMalformed(t, shape+" DecodeNodeInline", func() error { _, err := DecodeNodeInline(node); return err })
		mustMalformed(t, shape+" DecodeNodeInlineEnc", func() error { _, err := DecodeNodeInlineEnc(node, nil); return err })
		mustMalformed(t, shape+" DecodeInline", func() error { _, err := DecodeInline(ad); return err })
		mustMalformed(t, shape+" Decode", func() error { _, err := Decode(ad, NewInternTable()); return err })
		mustMalformed(t, shape+" DecodeInlineEnc", func() error { _, err := DecodeInlineEnc(ad, nil); return err })
		mustMalformed(t, shape+" DecodeInlineRedact", func() error { _, err := DecodeInlineRedact(ad); return err })
	}
}

// The same remnant inside a SEALED value: the opened plaintext is decoded by a sub-decoder that inherits
// the inline mode, and with it the absent resolver.
func TestInlineSealedValueWithIDKeyedRefIsMalformed(t *testing.T) {
	var s xorSealer
	nonce, ct, _ := s.Seal(idKeyedNodes["attr-ref"])
	node := []byte{nEncrypted}
	node = binary.AppendUvarint(node, uint64(len(nonce)))
	node = append(node, nonce...)
	node = binary.AppendUvarint(node, uint64(len(ct)))
	node = append(node, ct...)
	ad := rawAd(flagInlineNames, nil, inlineEntry("Secret", node))

	mustMalformed(t, "AppendNodeTextInlineOldEnc", func() error {
		_, err := AppendNodeTextInlineOldEnc(nil, node, s, false)
		return err
	})
	mustMalformed(t, "DecodeNodeInlineEnc", func() error { _, err := DecodeNodeInlineEnc(node, s); return err })
	mustMalformed(t, "DecodeInlineEnc", func() error { _, err := DecodeInlineEnc(ad, s); return err })
}

// A nil *InternTable handed to an interned-form reader has no id space to resolve against, which is the
// same condition as an inline decoder meeting an id: it must be malformed, not a nil dereference inside
// InternTable.Name.
func TestNilInternTableIsMalformed(t *testing.T) {
	ref := idKeyedNodes["attr-ref"]
	rec := []byte{nRecord, 0, 1, 0, nInt, 2} // [<id 0> = 1]: an id-keyed record member
	for name, node := range map[string][]byte{"attr-ref": ref, "record": rec} {
		mustMalformed(t, name+" AppendNodeText", func() error { _, err := AppendNodeText(nil, node, nil); return err })
		mustMalformed(t, name+" AppendNodeTextOld", func() error { _, err := AppendNodeTextOld(nil, node, nil); return err })
		mustMalformed(t, name+" AppendNodeTextOldEnc", func() error {
			_, err := AppendNodeTextOldEnc(nil, node, nil, nil, true)
			return err
		})
		mustMalformed(t, name+" DecodeNode", func() error { _, err := DecodeNode(node, nil); return err })
		mustMalformed(t, name+" DecodeNodeResolve", func() error { _, err := DecodeNodeResolve(node, nil); return err })
	}

	interned := rawAd(0, nil, internedEntry(0, []byte{nInt, 2}))
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("ForEachNamed(nil table) on an interned ad panicked: %v", r)
			}
		}()
		if Ad(interned).ForEachNamed(nil, func(string, []byte) bool { return true }) {
			t.Error("ForEachNamed(nil table) on an interned ad = true, want false: its ids cannot be named")
		}
		if Ad(interned).ForEachNamedRedact(nil, func(string, []byte) bool { return true }) {
			t.Error("ForEachNamedRedact(nil table) on an interned ad = true, want false")
		}
	}()

	// An inline ad needs no table, so a nil one is fine there -- including for redaction, which then
	// has no privacy predicate and keeps everything.
	inline := rawAd(flagInlineNames, nil, inlineEntry("A", []byte{nInt, 2}))
	var got []string
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("ForEachNamedRedact(nil table) on an inline ad panicked: %v", r)
			}
		}()
		if !Ad(inline).ForEachNamedRedact(nil, func(n string, _ []byte) bool { got = append(got, n); return true }) {
			t.Error("ForEachNamedRedact(nil table) on an inline ad = false, want true")
		}
	}()
	if len(got) != 1 || got[0] != "A" {
		t.Errorf("ForEachNamedRedact(nil table) yielded %q, want [A]", got)
	}

	noPanic(t, "AppendAdSubsetInlineFromInterned(nil resolver)", func() {
		if _, ok := AppendAdSubsetInlineFromInterned(nil, Ad(interned), nil, nil, nil, &SubsetScratch{}); ok {
			t.Error("AppendAdSubsetInlineFromInterned(nil resolver) = ok, want false")
		}
	})
}

// An interned id is a uint32. A uvarint beyond that range used to be truncated onto a real id, so a
// damaged id resolved silently to an unrelated attribute's name. It is malformed.
func TestInternedIDBeyondUint32IsMalformed(t *testing.T) {
	tbl := NewInternTable()
	tbl.Intern("Foo") // id 0
	wide := uint64(1) << 32

	refNode := binary.AppendUvarint([]byte{nAttrRef, 0}, wide)
	mustMalformed(t, "AppendNodeText", func() error { _, err := AppendNodeText(nil, refNode, tbl); return err })
	mustMalformed(t, "DecodeNode", func() error { _, err := DecodeNode(refNode, tbl); return err })

	ad := rawAd(0, nil, internedEntry(wide, []byte{nInt, 2}))
	mustMalformed(t, "Decode (key)", func() error { _, err := Decode(ad, tbl); return err })

	noPanic(t, "accessors", func() { checkWideIDAccessors(t, ad, refNode, tbl) })
}

func checkWideIDAccessors(t *testing.T, ad, refNode []byte, tbl *InternTable) {
	if node, ok := Ad(ad).Lookup(0); ok {
		t.Errorf("Lookup(0) found %x under key id 2^32", node)
	}
	if Ad(ad).ForEach(func(uint32, []byte) bool { return true }) {
		t.Error("ForEach over a key id beyond uint32 = true, want false")
	}
	if Ad(ad).ForEachIDNode(func(uint32, []byte) bool { return true }) {
		t.Error("ForEachIDNode over a key id beyond uint32 = true, want false")
	}
	if Ad(ad).ForEachNamed(tbl, func(string, []byte) bool { return true }) {
		t.Error("ForEachNamed over a key id beyond uint32 = true, want false")
	}
	if Ad(ad).ForEachRaw(func(uint32, string, []byte) bool { return true }) {
		t.Error("ForEachRaw over a key id beyond uint32 = true, want false")
	}
	if _, ok := AppendAdSubsetInlineFromInterned(nil, Ad(ad), tbl.Name, nil, nil, &SubsetScratch{}); ok {
		t.Error("AppendAdSubsetInlineFromInterned over a key id beyond uint32 = ok, want false")
	}
	refAd := rawAd(0, nil, internedEntry(0, refNode))
	if _, ok := AppendAdSubsetInlineFromInterned(nil, Ad(refAd), tbl.Name, nil, nil, &SubsetScratch{}); ok {
		t.Error("AppendAdSubsetInlineFromInterned over a ref id beyond uint32 = ok, want false")
	}
	if ids := AppendNodeRefIDs(refNode, nil); len(ids) != 0 {
		t.Errorf("AppendNodeRefIDs collected %v from a ref id beyond uint32", ids)
	}
	var r NodeRefs
	if AppendNodeRefsExact(refNode, false, &r, nil) {
		t.Errorf("AppendNodeRefsExact over a ref id beyond uint32 = complete (%v), want false", r.IDs)
	}
}

// A length near MaxInt overflowed the cursor's bounds check (pos+n wrapped negative), so skip moved the
// cursor to a negative position and the next read indexed out of range.
func TestHugeLengthDoesNotWrapCursor(t *testing.T) {
	huge := binary.AppendUvarint(nil, math.MaxInt64-1)
	str := append([]byte{nString}, huge...)
	refStr := append([]byte{nAttrRefStr, 0}, huge...)
	funcStr := append(append([]byte{nFuncStr}, huge...), 0)
	for name, head := range map[string][]byte{"string": str, "attr-ref-str": refStr, "func-str": funcStr} {
		node := slices.Concat(head, []byte{nUndefined, nUndefined}) // something after, so a wrapped cursor reads on
		noPanic(t, name, func() {
			if _, ok := NodeLen(node); ok {
				t.Errorf("%s: NodeLen ok over a length past the input", name)
			}
			AppendNodeRefIDs(node, nil)
			AppendNodeRefNames(node, nil)
			AppendNodeRefsExact(node, true, &NodeRefs{}, nil)
			AppendNodeRefsExact(node, false, &NodeRefs{}, nil)
		})
	}

	// The same overflow on an attribute KEY of an inline ad.
	key := append(append([]byte{}, huge...), nUndefined, nUndefined)
	ad := rawAd(flagInlineNames, nil, key)
	noPanic(t, "inline key", func() {
		if _, ok := Ad(ad).LookupByName("A"); ok {
			t.Error("LookupByName ok over a key length past the input")
		}
		if Ad(ad).ForEachNameNode(func(_, _ []byte) bool { return true }) {
			t.Error("ForEachNameNode = true over a key length past the input")
		}
		if Ad(ad).ForEachRaw(func(uint32, string, []byte) bool { return true }) {
			t.Error("ForEachRaw = true over a key length past the input")
		}
		if _, ok := AppendAdSubsetInline(nil, Ad(ad), nil, nil); ok {
			t.Error("AppendAdSubsetInline ok over a key length past the input")
		}
		if _, ok := AppendAdMergedInline(nil, Ad(ad), nil, &MergeScratch{}); ok {
			t.Error("AppendAdMergedInline ok over a key length past the input")
		}
		if _, ok := AppendInternedFromInline(nil, Ad(ad), NewInternTable(), nil); ok {
			t.Error("AppendInternedFromInline ok over a key length past the input")
		}
	})

	// And on a sealed node's nonce length, which OpenEncryptedNode sliced by directly.
	enc := append(append([]byte{nEncrypted}, huge...), 0, 0)
	noPanic(t, "OpenEncryptedNode", func() {
		if _, ok := OpenEncryptedNode(enc, xorSealer{}); ok {
			t.Error("OpenEncryptedNode ok over a nonce length past the input")
		}
	})
	encCT := append([]byte{nEncrypted, 1, 1}, huge...)
	noPanic(t, "OpenEncryptedNode ct", func() {
		if _, ok := OpenEncryptedNode(encCT, xorSealer{}); ok {
			t.Error("OpenEncryptedNode ok over a ciphertext length past the input")
		}
	})
}

// A hot-header offset past the end of an inline ad was followed without a bounds check: the cursor
// started beyond the buffer and its first uvarint sliced out of range.
func TestInlineHotOffsetPastEnd(t *testing.T) {
	ad := rawAd(flagInlineNames, [][2]uint64{{uint64(nameHash32("A")), 1 << 20}}, inlineEntry("A", []byte{nInt, 2}))
	noPanic(t, "hot offset", func() {
		// LookupByName falls back to the linear scan, which is fine; it must just not panic.
		if node, ok := Ad(ad).LookupByName("A"); !ok || len(node) != 2 {
			t.Errorf("LookupByName = %x, %v; want the entry found by the linear scan", node, ok)
		}
		if _, ok := Ad(ad).ForEachHotInlineBuf(nil, func(uint32, []byte, []byte) bool { return true }); ok {
			t.Error("ForEachHotInlineBuf ok over a hot offset past the end")
		}
		keep := func(_, _ []byte) bool { return true }
		if _, ok := AppendAdSubsetInlineHotFirst(nil, Ad(ad), keep, 1, nil, &SubsetScratch{}); ok {
			t.Error("AppendAdSubsetInlineHotFirst ok over a hot offset past the end")
		}
		keepH := func(_ []byte, _ uint32, _ []byte) bool { return true }
		note := func([]byte) bool { return false }
		pending := func() int { return 1 }
		if _, ok := AppendAdSubsetInlineRefs(nil, Ad(ad), keepH, note, pending, 1, nil, &SubsetScratch{}); ok {
			t.Error("AppendAdSubsetInlineRefs ok over a hot offset past the end")
		}
	})
}

// A count read off the wire sized an allocation before any of the counted items were read, so a
// damaged count asked for an arbitrarily large slice: a makeslice panic, or the process's memory.
func TestHugeCountDoesNotSizeAllocation(t *testing.T) {
	huge := binary.AppendUvarint(nil, 1<<62)
	attrs := append([]byte{magicByte, formatVer, 0, 0}, huge...)
	mustMalformed(t, "Decode attrCount", func() error { _, err := Decode(attrs, NewInternTable()); return err })
	inline := append([]byte{magicByte, formatVer, flagInlineNames, 0}, huge...)
	mustMalformed(t, "DecodeInline attrCount", func() error { _, err := DecodeInline(inline); return err })
	names := append([]byte{magicByte, formatVer, flagStandalone}, huge...)
	mustMalformed(t, "DecodeStandalone name count", func() error { _, err := DecodeStandalone(names); return err })
	hot := append(append([]byte{magicByte, formatVer, 0}, huge...), 0, 0)
	noPanic(t, "ForEachHot", func() {
		if Ad(hot).ForEachHot(func(uint32, []byte) bool { return true }) {
			t.Error("ForEachHot = true over a hot count past the input")
		}
	})
}

// The transcode read an nAttrRefStr's scope byte by indexing, with no check that the node had one.
func TestTranscodeTruncatedAttrRefStr(t *testing.T) {
	ad := rawAd(flagInlineNames, nil, inlineEntry("A", []byte{nAttrRefStr}))
	noPanic(t, "transcode", func() {
		if _, ok := AppendInternedFromInline(nil, Ad(ad), NewInternTable(), nil); ok {
			t.Error("AppendInternedFromInline ok over a truncated attr-ref")
		}
	})
}

func noPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: panicked: %v", what, r)
		}
	}()
	fn()
}

// FuzzReadersNoPanic feeds arbitrary bytes to every reader of the wire form, as a whole ad and as a
// bare node in each key space, and asserts only that none of them panics. Malformed input is expected
// and every answer is acceptable except a crash.
func FuzzReadersNoPanic(f *testing.F) {
	tbl := NewInternTable()
	for _, src := range roundTripCases {
		ad, err := parser.ParseClassAd(src)
		if err != nil {
			continue
		}
		f.Add(Encode(nil, ad, tbl))
		f.Add(EncodeInline(nil, ad))
		f.Add(EncodeStandalone(ad))
	}
	// The mixed-key-space remnants: id-keyed nodes inside an inline ad, and inline ones in an
	// interned ad.
	for _, node := range idKeyedNodes {
		f.Add(rawAd(flagInlineNames, nil, inlineEntry("A", node)))
		f.Add(node)
	}
	f.Add(rawAd(0, nil, internedEntry(0, []byte{nAttrRefStr, 0, 1, 'A'})))
	f.Add(rawAd(flagInlineNames, [][2]uint64{{1, 1 << 20}}, inlineEntry("A", []byte{nInt, 2})))

	f.Fuzz(func(t *testing.T, b []byte) {
		readAllWays(b, tbl)
	})
}

// readAllWays runs every reader over b. Results are discarded: the property is "returns".
func readAllWays(b []byte, tbl *InternTable) {
	open := xorSealer{}
	_, _ = Decode(b, tbl)
	_, _ = Decode(b, nil)
	_, _ = DecodeInline(b)
	_, _ = DecodeStandalone(b)
	_, _ = DecodeResolve(b, tbl.Name)
	_, _ = DecodeResolveEnc(b, tbl.Name, open)
	_, _ = DecodeResolveEncRedact(b, tbl.Name, nil)
	_, _ = DecodeInlineEnc(b, open)
	_, _ = DecodeInlineRedact(b)

	for _, t := range []*InternTable{tbl, nil} {
		_, _ = AppendNodeText(nil, b, t)
		_, _ = AppendNodeTextOld(nil, b, t)
		_, _ = AppendNodeTextOldEnc(nil, b, t, open, false)
		_, _ = AppendNodeTextOldEnc(nil, b, t, nil, true)
		_, _ = DecodeNode(b, t)
	}
	_, _ = AppendNodeTextInline(nil, b)
	_, _ = AppendNodeTextInlineOld(nil, b)
	_, _ = AppendNodeTextInlineOldEnc(nil, b, open, false)
	_, _ = AppendNodeTextInlineOldEnc(nil, b, nil, true)
	_, _ = DecodeNodeInline(b)
	_, _ = DecodeNodeInlineEnc(b, open)
	_, _ = DecodeNodeResolveEnc(b, tbl.Name, open)
	_, _ = NodeLenKeyed(b, false)
	_, _ = NodeLenKeyed(b, true)
	_ = AppendNodeRefIDs(b, nil)
	_ = AppendNodeRefNames(b, nil)
	_ = AppendNodeRefsExact(b, false, &NodeRefs{}, func(uint32) bool { return false })
	_ = AppendNodeRefsExact(b, true, &NodeRefs{}, nil)
	_, _ = OpenEncryptedNode(b, open)
	_, _ = LiteralValue(b)
	_, _ = NumberValue(b)
	_, _ = StringLiteralValue(b)

	a := Ad(b)
	yes := func(uint32, []byte) bool { return true }
	_, _ = a.Lookup(0)
	_, _ = a.Lookup(1)
	_ = a.ForEach(yes)
	_ = a.ForEachIDNode(yes)
	_ = a.ForEachHot(yes)
	_, _ = a.ForEachHotBuf(nil, yes)
	_, _ = a.ForEachHotInlineBuf(nil, func(uint32, []byte, []byte) bool { return true })
	_ = a.ForEachNameNode(func(_, _ []byte) bool { return true })
	_ = a.ForEachRaw(func(uint32, string, []byte) bool { return true })
	_ = a.ForEachNamed(tbl, func(string, []byte) bool { return true })
	_ = a.ForEachNamedRedact(tbl, func(string, []byte) bool { return true })
	_ = a.ForEachNamed(nil, func(string, []byte) bool { return true })
	_ = a.ForEachNamedRedact(nil, func(string, []byte) bool { return true })
	_, _ = a.LookupByName("a")
	_ = a.AttrCount()
	_, _, _, _, _ = a.SplitBody()

	keep := func(_, _ []byte) bool { return true }
	sc := &SubsetScratch{}
	_, _ = AppendAdSubsetInline(nil, a, keep, open)
	_, _ = AppendAdSubsetInline(nil, a, nil, nil)
	_, _ = AppendAdSubsetInlineHotFirst(nil, a, keep, 1, open, sc)
	_, _ = AppendAdSubsetInlineRefs(nil, a, func(_ []byte, _ uint32, _ []byte) bool { return true },
		func([]byte) bool { return false }, func() int { return 1 }, 1, open, sc)
	_, _ = AppendAdSubsetInlineFromInterned(nil, a, tbl.Name, keep, open, sc)
	_, _ = AppendAdSubsetInlineFromInterned(nil, a, nil, keep, open, sc)
	_, _ = AppendAdMergedInline(nil, a, []Ad{a}, &MergeScratch{})
	_, _ = AppendInternedFromInline(nil, a, NewInternTable(), map[uint32]struct{}{0: {}})
}
