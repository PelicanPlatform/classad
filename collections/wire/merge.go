package wire

// MERGING ADS BY SPLICING BYTES, NOT BY DECODING THEM.
//
// A delta record holds only the attributes one write changed, so reading a key means merging a
// chain of them over a whole record. The obvious way -- decode each participant into an object,
// insert the changed attributes, encode the result -- decodes and re-encodes every attribute in
// the ad to apply a change to two of them. On a real queue replay that was 52% of the ingest's
// allocation, and the ad's own values were the bulk of it.
//
// It is not necessary. In an inline-names ad an attribute entry is a CONTIGUOUS byte range --
// uvarint(nameLen), the name, then a self-delimiting node -- and nothing outside the entry refers
// to its interior. So a merge is: walk the base's entries, emit either the base's bytes or the
// overlay's bytes for that name, then emit whatever the overlay added. Values are never decoded,
// re-encoded, or even decrypted; a sealed value's ciphertext is copied exactly as it was stored.
//
// This is the same observation AppendAdSubsetInline is built on, applied to merging rather than
// projecting.

// MergeScratch holds AppendAdMergedInline's per-call scratch: the overlay attributes' names and
// entry byte ranges. Reused across merges so a merge allocates only its output.
type MergeScratch struct {
	names [][]byte // overlay attribute name, as stored (compared case-insensitively)
	ents  [][]byte // the whole (nameLen, name, node) entry, for an attribute the base lacks
	nodes [][]byte // just the node, for substituting into a base entry (see set)
	used  []bool   // whether the base already consumed this overlay entry
}

// Reset clears the scratch, keeping its backing arrays.
func (s *MergeScratch) Reset() {
	s.names = s.names[:0]
	s.ents = s.ents[:0]
	s.nodes = s.nodes[:0]
	s.used = s.used[:0]
}

// Release is Reset plus dropping the references the backing arrays still hold. The names and
// entries are slices INTO the caller's ads, so a pooled scratch that keeps them keeps those ads
// alive; a caller returning a scratch to a pool must Release, not Reset.
func (s *MergeScratch) Release() {
	clear(s.names[:cap(s.names)])
	clear(s.ents[:cap(s.ents)])
	clear(s.nodes[:cap(s.nodes)])
	s.Reset()
}

// find returns the index of name in the scratch, or -1. Linear because an overlay is a delta --
// the attributes one write changed, a handful -- so a map would cost more to build than the scan
// costs to run.
func (s *MergeScratch) find(name []byte) int {
	for i, n := range s.names {
		if asciiFoldEqual(n, name) {
			return i
		}
	}
	return -1
}

// set records an overlay attribute, replacing any earlier one of the same name. Last write wins,
// within an overlay and across overlays, which is what merging a delta chain oldest-first means
// and what the decode-and-Insert path it replaces did.
//
// The node is kept apart from the whole entry because the two are needed in different places.
// Overwriting an attribute the base HAS substitutes only the node, leaving the base's name bytes
// in place: ClassAd.Insert replaces an existing attribute's value and keeps its name, so an
// overlay spelling a name with different case must not change how the name is stored. Emitting the
// overlay's whole entry instead made a scan and a point read of the same record report the same
// attribute under different casing.
func (s *MergeScratch) set(name, node, entry []byte) {
	if i := s.find(name); i >= 0 {
		s.ents[i] = entry
		s.nodes[i] = node
		return
	}
	s.names = append(s.names, name)
	s.ents = append(s.ents, entry)
	s.nodes = append(s.nodes, node)
	s.used = append(s.used, false)
}

// asciiFoldEqual reports whether a and b are equal ignoring ASCII case. ClassAd attribute names
// are ASCII, so this is the whole of case-insensitive comparison for them, and unlike
// strings.EqualFold it needs no conversion and no allocation.
func asciiFoldEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if ca == cb {
			continue
		}
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// AppendAdMergedInline appends to dst the ad formed by applying overlays, in order, over base,
// and returns the extended buffer. A later overlay's attribute wins over an earlier one's, and
// any overlay's wins over base's; an attribute no overlay mentions is copied from base byte for
// byte.
//
// Every participant must be an inline-names ad that is not standalone (no embedded intern table):
// entries are copied between ads, so the names have to be IN the entries. It reports false when
// that does not hold, or when any participant is malformed, so the caller can fall back to
// decoding -- which is the source of truth.
//
// The result carries NO hot header. The hot header stores absolute offsets into the entries
// region, which a merge invalidates, and every reader treats it as a guarded fast path that falls
// back to a linear walk when it comes up short (see rawprojected.go) or refuses a merged ad
// outright (hotClosureMatch requires flagHotClosure, which this does not set). AppendAdSubsetInline
// emits none for the same reason.
func AppendAdMergedInline(dst []byte, base Ad, overlays []Ad, sc *MergeScratch) ([]byte, bool) {
	sc.Reset()
	for _, ov := range overlays {
		c, ok := ov.bodyStart()
		if !ok || !c.inline || ov[2]&flagStandalone != 0 {
			return dst, false
		}
		hotCount := c.uvarint()
		for i := uint64(0); i < hotCount && c.ok; i++ {
			c.uvarint()
			c.uvarint()
		}
		n := c.uvarint()
		for i := uint64(0); i < n && c.ok; i++ {
			entStart := c.pos
			name := c.readNameBytes()
			if !c.ok {
				return dst, false
			}
			nodeStart := c.pos
			skipNode(c, 0)
			if !c.ok {
				return dst, false
			}
			sc.set(name, ov[nodeStart:c.pos], ov[entStart:c.pos])
		}
		if !c.ok {
			return dst, false
		}
	}

	bc, ok := base.bodyStart()
	if !ok || !bc.inline || base[2]&flagStandalone != 0 {
		return dst, false
	}
	hotCount := bc.uvarint()
	for i := uint64(0); i < hotCount && bc.ok; i++ {
		bc.uvarint()
		bc.uvarint()
	}
	baseCount := bc.uvarint()
	if !bc.ok {
		return dst, false
	}

	dst = append(dst, magicByte, formatVer, flagInlineNames)
	dst = append(dst, 0) // hotCount = 0
	countAt := len(dst)
	dst = append(dst, 0x80, 0x80, 0x80, 0x80, 0x00) // 5-byte uvarint slot, patched below
	emitted := uint64(0)

	// Base entries, in order, with overridden ones substituted. Runs of untouched entries are
	// copied in one append: the common shape is a wide ad with two changed attributes, so most of
	// the output is two or three contiguous memcpys.
	runStart := bc.pos
	flushRun := func(end int) {
		if end > runStart {
			dst = append(dst, base[runStart:end]...)
		}
	}
	for i := uint64(0); i < baseCount && bc.ok; i++ {
		entStart := bc.pos
		name := bc.readNameBytes()
		if !bc.ok {
			return dst, false
		}
		nodeStart := bc.pos
		skipNode(bc, 0)
		if !bc.ok {
			return dst, false
		}
		idx := sc.find(name)
		if idx < 0 {
			emitted++ // stays in the current run
			continue
		}
		flushRun(entStart)
		if !sc.used[idx] {
			// The BASE's name bytes with the OVERLAY's node: an overwrite replaces a value, not a
			// name. See MergeScratch.set.
			dst = append(dst, base[entStart:nodeStart]...)
			dst = append(dst, sc.nodes[idx]...)
			sc.used[idx] = true
			emitted++
		}
		// A later base entry with the same name is dropped rather than emitted: the overlay's
		// entry already stands at the first position, so a shadowed duplicate would only add
		// bytes. Stored records do not carry duplicate names, so this is a safety case.
		runStart = bc.pos
	}
	if !bc.ok {
		return dst, false
	}
	flushRun(bc.pos)

	// Attributes the overlays added, in the order they were first seen.
	for i := range sc.ents {
		if !sc.used[i] {
			dst = append(dst, sc.ents[i]...)
			emitted++
		}
	}
	patchUvarint5(dst[countAt:countAt+5], emitted)
	return dst, true
}
