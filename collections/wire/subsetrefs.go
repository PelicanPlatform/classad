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

	dst = append(dst, magicByte, formatVer, flagInlineNames)
	dst = append(dst, 0) // hotCount = 0
	countAt := len(dst)
	dst = append(dst, 0x80, 0x80, 0x80, 0x80, 0x00)
	kept := uint64(0)
	sc.copied = sc.copied[:0]
	again := false

	// emit copies one entry and feeds its node to the closure. The node handed to note is the
	// one that will be RENDERED at the far end -- the opened plaintext for a sealed value, the
	// stored bytes otherwise -- so the closure sees what the consumer will evaluate rather than
	// an opaque ciphertext whose references it could not read anyway.
	emit := func(entryStart, nameEnd int, node []byte) bool {
		if IsEncryptedNode(node) {
			plain, opened := OpenEncryptedNode(node, open)
			if !opened {
				return false
			}
			dst = append(dst, src[entryStart:nameEnd]...)
			dst = append(dst, plain...)
			if note(plain) {
				again = true
			}
			return true
		}
		dst = append(dst, src[entryStart:nameEnd]...)
		dst = append(dst, node...)
		if note(node) {
			again = true
		}
		return true
	}

	copiedAlready := func(entryStart int) bool {
		for _, off := range sc.copied {
			if off == entryStart {
				return true
			}
		}
		return false
	}

	// Hot phase: direct entry offsets, no scanning.
	for i := 0; i+1 < len(sc.hot); i += 2 {
		entryStart := entriesStart + int(sc.hot[i+1])
		ec := &cursor{b: src, pos: entryStart, ok: true, inline: true}
		name := ec.readNameBytes()
		if !ec.ok {
			return dst, false
		}
		nameEnd := ec.pos
		nodeStart := ec.pos
		skipNode(ec, 0)
		if !ec.ok {
			return dst, false
		}
		node := src[nodeStart:ec.pos]
		if !keep(name, NameHash32Bytes(name), node) || copiedAlready(entryStart) {
			continue
		}
		if emit(entryStart, nameEnd, node) {
			sc.copied = append(sc.copied, entryStart)
			kept++
		}
	}
	if pending() <= 0 {
		// Nothing is outstanding: every projected attribute AND every reference they surfaced
		// came out of the hot header, so the cold tail cannot hold anything this row still
		// needs and is never touched. Asking whether the closure is SATISFIED, rather than
		// whether it is empty, is what lets this fire for an expression whose references are
		// themselves hot -- the common shape, since the hot header is built from the match
		// closure.
		patchUvarint5(dst[countAt:countAt+5], kept)
		return dst, true
	}

	// Walk the entries, repeating while a pass surfaces a new name: a referenced attribute may
	// sit EARLIER in the ad than the expression that named it, so one pass is not enough. Each
	// pass either grows the wanted set or ends the loop, and the set is bounded by the ad's own
	// attribute count, so the repetition terminates.
	//
	// Deliberately a re-walk rather than an index of the record built up front. Indexing every
	// entry so later passes can skip the parsing costs more than the parsing does: measured on
	// 20k real job ads with a 21-attribute projection, the indexed form ran 382ms against 190ms
	// for this one, because the common case needs a SINGLE pass and pays for an index it never
	// re-reads.
	for {
		again = false
		c = &cursor{b: src, pos: entriesStart, ok: true, inline: true}
		for i := uint64(0); i < attrCount && c.ok; i++ {
			entryStart := c.pos
			name := c.readNameBytes()
			if !c.ok {
				return dst, false
			}
			nameEnd := c.pos
			nodeStart := c.pos
			skipNode(c, 0)
			if !c.ok {
				return dst, false
			}
			if copiedAlready(entryStart) {
				continue
			}
			node := src[nodeStart:c.pos]
			if !keep(name, NameHash32Bytes(name), node) {
				continue
			}
			if emit(entryStart, nameEnd, node) {
				sc.copied = append(sc.copied, entryStart)
				kept++
			}
		}
		if !c.ok {
			return dst, false
		}
		if !again {
			break
		}
	}
	patchUvarint5(dst[countAt:countAt+5], kept)
	return dst, true
}
