package wire

// CLOSING A SHIPPED SUBSET UNDER ITS OWN REFERENCES.
//
// AppendAdSubsetInline ships exactly the entries keep selects, which is right for a relay
// reproducing HTCondor's projection protocol and wrong for a consumer that EVALUATES what it
// receives: an attribute holding an expression over its siblings (Requirements, Rank, ...)
// arrives without them and evaluates to undefined. The text projection already has the
// answer -- it resolves each emitted expression's unscoped/MY. references against the same ad
// and repeats while new ones surface -- and this is that closure for the wire-row assembler.
//
// The closure is computed PER RECORD, from the record's own bytes, because whether an
// attribute is an expression at all is a property of the record, not of the query: two ads
// matching one query can hold a literal and an expression under the same name. A caller that
// decided the closure once for the whole query would be right for most records and silently
// wrong for the rest.

// AppendAdSubsetInlineRefs is AppendAdSubsetInlineHotFirst whose selection is CLOSED under the
// emitted expressions' own attribute references, so the assembled row evaluates
// self-contained at the far end.
//
// keep is the caller's membership test, consulted per entry; note is called with every node
// that was emitted and collects that node's unscoped/MY.-scoped references into the caller's
// wanted set, reporting true when it surfaced a name the set did not already hold. keep must
// accept those names on a later pass -- the two callbacks share the caller's set, and that
// sharing is the whole mechanism.
//
// The hot-header shortcut still applies, and still exits early when the projection was
// satisfied from the hot header alone AND nothing it emitted referenced anything new. Past
// that it walks the entries, repeating the walk while a pass surfaces new names: a referenced
// attribute may sit EARLIER in the ad than the expression that named it, so one pass is not
// enough. Each pass either grows the wanted set or ends the loop, and the set is bounded by
// the ad's own attribute count, so the repetition terminates.
//
// needed <= 0 (a whole-ad selection) ships everything and has no closure to compute; it is
// handed to AppendAdSubsetInline unchanged.
func AppendAdSubsetInlineRefs(dst []byte, src Ad, keep func(name []byte, h uint32, node []byte) bool, note func(node []byte) bool, pending func() int, needed int, open Sealer, sc *SubsetScratch) ([]byte, bool) {
	if needed <= 0 || keep == nil || note == nil || pending == nil {
		return AppendAdSubsetInline(dst, src, func(n, nd []byte) bool { return keep(n, NameHash32Bytes(n), nd) }, open)
	}
	c, ok := src.bodyStart()
	if !ok || !c.inline {
		return dst, false
	}
	hotCount := c.uvarint()
	sc.hot = sc.hot[:0]
	for i := uint64(0); i < hotCount && c.ok; i++ {
		sc.hot = append(sc.hot, uint32(c.uvarint()), uint32(c.uvarint()))
	}
	attrCount := c.uvarint()
	if !c.ok {
		return dst, false
	}
	entriesStart := c.pos
	sc.ents, sc.plain = sc.ents[:0], sc.plain[:0]

	// Hot phase: index the hot entries by their stored offsets and try to satisfy the
	// selection from them alone. Indexing only these -- rather than the whole record up
	// front -- is what keeps the prefix-decompressed read working: the parallel scan hands
	// this assembler a record truncated after the hot prefix, and walking to attrCount
	// would reject every one of them and force a full decompress per ad.
	for i := 0; i+1 < len(sc.hot); i += 2 {
		off := entriesStart + int(sc.hot[i+1])
		if sc.indexed(off) {
			continue // the same entry can appear twice in the hot pairs
		}
		if _, ok := sc.index(src, off); !ok {
			return dst, false
		}
	}
	sc.resolve(keep, note, open)
	if pending() <= 0 {
		// Nothing is outstanding: every projected attribute AND every reference they
		// surfaced came out of the hot header, so the cold tail cannot hold anything this
		// row still needs and is never touched. Asking whether the closure is SATISFIED,
		// rather than whether it is empty, is what lets this fire for an expression whose
		// references are themselves hot -- the common shape, since the hot header is built
		// from the match closure.
		return sc.emit(dst), true
	}

	// Something is still outstanding. Index the record ONCE -- a referenced attribute may
	// sit earlier in the ad than the expression that named it, so the closure needs repeated
	// passes, and they run over the index.
	sc.ents, sc.plain = sc.ents[:0], sc.plain[:0]
	off := entriesStart
	for i := uint64(0); i < attrCount; i++ {
		next, ok := sc.index(src, off)
		if !ok {
			return dst, false
		}
		off = next
	}
	sc.resolve(keep, note, open)
	return sc.emit(dst), true
}

// indexed reports whether the entry at off is already in the index. Only the hot phase asks:
// its offsets are arbitrary and may repeat, while the linear pass visits each entry once, in
// order, so making every caller pay the scan would put back the quadratic this replaces.
func (sc *SubsetScratch) indexed(off int) bool {
	for i := range sc.ents {
		if sc.ents[i].start == int32(off) {
			return true
		}
	}
	return false
}

// index parses the entry at off, appends it to the index and returns the offset of the next
// entry, reporting false if the record is malformed there.
func (sc *SubsetScratch) index(src Ad, off int) (int, bool) {
	ec := &cursor{b: src, pos: off, ok: true, inline: true}
	name := ec.readNameBytes()
	if !ec.ok {
		return 0, false
	}
	nameEnd := ec.pos
	skipNode(ec, 0)
	if !ec.ok {
		return 0, false
	}
	sc.ents = append(sc.ents, subsetEnt{
		start:    int32(off),
		hash:     NameHash32Bytes(name),
		plainOff: -1,
		field:    src[off:nameEnd],
		name:     name,
		node:     src[nameEnd:ec.pos],
	})
	return ec.pos, true
}

// resolve takes every indexed entry the caller wants, repeating while a pass surfaces a name
// the previous ones did not hold -- keep and note share the caller's wanted set, and that
// sharing is the whole mechanism. Each pass either grows that set or ends the loop, and the
// set is bounded by the record's own attribute count, so it terminates.
//
// It reports how many entries it took; whether that is ENOUGH is the caller's question to
// ask, because only the caller knows what is still outstanding (see the pending callback).
func (sc *SubsetScratch) resolve(keep func(name []byte, h uint32, node []byte) bool, note func(node []byte) bool, open Sealer) (kept int) {
	for {
		again := false
		for i := range sc.ents {
			e := &sc.ents[i]
			if e.sel || !keep(e.name, e.hash, e.node) {
				continue
			}
			value := e.node
			if IsEncryptedNode(value) {
				plain, opened := OpenEncryptedNode(value, open)
				if !opened {
					continue // no key for it: drop it, as the plain assembler does
				}
				// Copied rather than retained: Open owns the slice it returns and the
				// interface does not promise it stays valid past the next call.
				e.plainOff, e.plainLen = int32(len(sc.plain)), int32(len(plain))
				sc.plain = append(sc.plain, plain...)
				value = plain
			}
			e.sel = true
			kept++
			if note(value) {
				again = true
			}
		}
		if !again {
			return kept
		}
	}
}

// emit appends the selected entries, in index order, as an inline-names ad.
func (sc *SubsetScratch) emit(dst []byte) []byte {
	dst = append(dst, magicByte, formatVer, flagInlineNames)
	dst = append(dst, 0) // hotCount = 0
	countAt := len(dst)
	dst = append(dst, 0x80, 0x80, 0x80, 0x80, 0x00)
	kept := uint64(0)
	for i := range sc.ents {
		e := &sc.ents[i]
		if !e.sel {
			continue
		}
		dst = append(dst, e.field...)
		dst = append(dst, e.value(sc)...)
		kept++
	}
	patchUvarint5(dst[countAt:countAt+5], kept)
	return dst
}
