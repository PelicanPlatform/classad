package wire

// AppendAdSubsetInline assembles a self-contained inline-names ad holding the
// subset of src's attribute entries keep selects, appending it to dst and
// returning the extended slice. It is the relay primitive for shipping stored
// ads across a trust/process boundary in wire form: an inline entry is a
// contiguous (name, node) byte range, so the subset is assembled by slice
// copies -- no value is decoded, rendered or re-encoded.
//
// An entry whose node is at-rest encrypted (nEncrypted) is emitted with its
// value OPENED via open when non-nil -- the consumer holds no data key -- and
// skipped entirely when open is nil or fails (matching the fast paths, which
// treat an unopenable value as absent). The emitted ad carries no hot header
// (its consumer renders linearly). Returns ok=false when src is not a
// well-formed inline-names ad; keep=nil keeps every entry.
func AppendAdSubsetInline(dst []byte, src Ad, keep func(name, node []byte) bool, open Sealer) ([]byte, bool) {
	c, ok := src.bodyStart()
	if !ok || !c.inline {
		return dst, false
	}
	hotCount := c.uvarint()
	for i := uint64(0); i < hotCount && c.ok; i++ {
		c.uvarint()
		c.uvarint()
	}
	attrCount := c.uvarint()
	if !c.ok {
		return dst, false
	}

	// Reserve the header now; the entry count is back-patched as a fixed-width
	// uvarint so entries can stream directly into dst with no second buffer.
	dst = append(dst, magicByte, formatVer, flagInlineNames)
	dst = append(dst, 0) // hotCount = 0
	countAt := len(dst)
	dst = append(dst, 0x80, 0x80, 0x80, 0x80, 0x00) // 5-byte uvarint placeholder (value patched below)
	kept := uint64(0)

	for i := uint64(0); i < attrCount && c.ok; i++ {
		nameStart := c.pos
		name := c.readNameBytes()
		if !c.ok {
			return dst, false
		}
		nodeStart := c.pos
		skipNode(c, 0)
		if !c.ok {
			return dst, false
		}
		node := src[nodeStart:c.pos]
		if keep != nil && !keep(name, node) {
			continue
		}
		if IsEncryptedNode(node) {
			plain, opened := OpenEncryptedNode(node, open)
			if !opened {
				continue // no key (or tampered): the value is absent to this consumer
			}
			// Re-emit as name + plain node (the one case a copy substitutes bytes).
			dst = append(dst, src[nameStart:nodeStart]...)
			dst = append(dst, plain...)
			kept++
			continue
		}
		dst = append(dst, src[nameStart:c.pos]...) // contiguous (name, node) entry
		kept++
	}
	if !c.ok {
		return dst, false
	}
	patchUvarint5(dst[countAt:countAt+5], kept)
	return dst, true
}

// patchUvarint5 writes v into a fixed 5-byte uvarint slot (continuation bits on
// the first four bytes), enough for any uint32-ranged count. Fixed width lets
// the encoder reserve the slot before the value is known.
func patchUvarint5(b []byte, v uint64) {
	b[0] = byte(v)&0x7f | 0x80
	b[1] = byte(v>>7)&0x7f | 0x80
	b[2] = byte(v>>14)&0x7f | 0x80
	b[3] = byte(v>>21)&0x7f | 0x80
	b[4] = byte(v >> 28 & 0x7f)
}

// SubsetScratch holds AppendAdSubsetInlineHotFirst's per-scan scratch (hot
// pairs, copied-entry offsets), reused across ads so assembly allocates
// nothing per ad.
type SubsetScratch struct {
	hot    []uint32
	copied []int
}

// AppendAdSubsetInlineHotFirst is AppendAdSubsetInline with the hot-header
// shortcut: entries reachable through the ad's hot pairs are copied first via
// their stored offsets, and when that satisfies the whole selection (kept ==
// needed) the linear walk is skipped entirely -- the projected relay's fast
// path, mirroring the projected text scan's. Anything short of full
// satisfaction falls back to the walk, which skips the entries the hot phase
// already copied. needed <= 0 disables the shortcut (a whole-ad selection must
// walk everything).
func AppendAdSubsetInlineHotFirst(dst []byte, src Ad, keep func(name, node []byte) bool, needed int, open Sealer, sc *SubsetScratch) ([]byte, bool) {
	if needed <= 0 {
		return AppendAdSubsetInline(dst, src, keep, open)
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

	emit := func(entryStart, nameEnd int, name, node []byte) bool {
		if IsEncryptedNode(node) {
			plain, opened := OpenEncryptedNode(node, open)
			if !opened {
				return false
			}
			dst = append(dst, src[entryStart:nameEnd]...)
			dst = append(dst, plain...)
			return true
		}
		dst = append(dst, src[entryStart:nameEnd]...)
		dst = append(dst, node...)
		return true
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
		if keep == nil || !keep(name, node) {
			continue
		}
		dup := false
		for _, off := range sc.copied {
			if off == entryStart {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		if emit(entryStart, nameEnd, name, node) {
			sc.copied = append(sc.copied, entryStart)
			kept++
		}
	}
	if int(kept) == needed {
		patchUvarint5(dst[countAt:countAt+5], kept)
		return dst, true
	}

	// Fallback walk for whatever the hot header did not cover, skipping entries
	// the hot phase already copied.
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
		copiedAlready := false
		for _, off := range sc.copied {
			if off == entryStart {
				copiedAlready = true
				break
			}
		}
		if copiedAlready {
			continue
		}
		node := src[nodeStart:c.pos]
		if keep != nil && !keep(name, node) {
			continue
		}
		if emit(entryStart, nameEnd, name, node) {
			kept++
		}
	}
	if !c.ok {
		return dst, false
	}
	patchUvarint5(dst[countAt:countAt+5], kept)
	return dst, true
}
