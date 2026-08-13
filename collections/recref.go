package collections

// LOCATING A RECORD, RATHER THAN HANDING OUT ITS BYTES.
//
// The visible-record iterators historically passed a callback the record's compressed ad, its
// codec and its segment dictionary, and every callback decompressed for itself. That works only
// while a record's own bytes ARE its whole ad. In a columnarized segment they are not: the record
// carries the attributes the schema does not cover and the rest live in the segment's columnar
// payload (see colnative.go). A caller handed just those bytes decodes half an ad and cannot tell.
//
// So the iterators pass a recRef -- WHERE the record is -- and a caller that wants the whole ad
// asks for it. That keeps the reconciliation in one place instead of at every decompress site, and
// it makes the columnar case impossible to forget: there is no way to get bytes out of a recRef
// without going through a function that knows about it.

// recRef locates one visible record inside a frozen window.
//
// Everything is read through the window's data slice rather than the segment's own, because the
// window is what pins the mapping alive: a sealed segment can be retired and unmapped while a scan
// is still walking it, and only the window's reference keeps the bytes valid.
type recRef struct {
	w    segWindow
	off  uint32
	dict *segDictHandle // the window's dictionary, loaded once per window rather than per record
}

// key returns the record's key: a view into the frozen window, which the caller must not retain.
func (r recRef) key() []byte { return recKey(r.w.data, r.off) }

// stored returns the record's own stored bytes, still compressed. This is the remnant for a
// columnarized segment, so it is the whole ad only when the segment is not columnarized -- callers
// wanting an ad should use Collection.wire.
func (r recRef) stored() []byte { return recAd(r.w.data, r.off) }

// codec returns the codec the record's bytes were compressed with.
func (r recRef) codec() Codec { return r.w.codec }

// wire returns the record's full ad, decompressed into buf's backing where it fits.
//
// For an ordinary segment this is one Decompress. For a columnarized one it also splices in the
// attributes held in the columnar payload, and it reports an error rather than returning a partial
// ad if that payload cannot be trusted.
func (c *Collection) wire(r recRef, buf []byte) ([]byte, error) {
	if seg := r.w.seg; seg != nil && (seg.columnarized() || seg.colDamaged.Load()) {
		return c.recordWireIn(seg, r.w.data, r.off, buf)
	}
	return r.w.codec.Decompress(buf[:0], r.stored())
}

// forEachVisibleRef walks the frozen windows in append order and calls fn for each record visible
// at snapshot s0, skipping markers. It is the one place the visibility walk is written; the
// byte-passing iterators are expressed in terms of it.
func forEachVisibleRef(s0 uint64, wins []segWindow, fn func(recRef) bool) {
	for _, w := range wins {
		d := w.dict()
		for off := 0; off < w.used; {
			o := uint32(off)
			total := recTotalLen(w.data, o)
			if total == 0 {
				break
			}
			if !recIsMarker(w.data, o) && recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
				if !fn(recRef{w: w, off: o, dict: d}) {
					return
				}
			}
			off += int(total)
		}
	}
}

// forEachVisibleRefReverse is forEachVisibleRef in newest-first order: windows from the last
// (newest) segment backward and, within each, records back-to-front.
//
// Records are variable-length and self-describe only their forward length, so each window's
// offsets are collected in one forward pass and replayed in reverse -- the walk stays O(records)
// at the cost of a per-segment offset slice. A caller that stops after K records therefore
// receives the K most recently appended.
func forEachVisibleRefReverse(s0 uint64, wins []segWindow, fn func(recRef) bool) {
	var offs []uint32 // reused across windows
	for wi := len(wins) - 1; wi >= 0; wi-- {
		w := wins[wi]
		d := w.dict()
		offs = offs[:0]
		for off := 0; off < w.used; {
			o := uint32(off)
			total := recTotalLen(w.data, o)
			if total == 0 {
				break
			}
			offs = append(offs, o)
			off += int(total)
		}
		for i := len(offs) - 1; i >= 0; i-- {
			o := offs[i]
			if !recIsMarker(w.data, o) && recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
				if !fn(recRef{w: w, off: o, dict: d}) {
					return
				}
			}
		}
	}
}

// forEachVisibleWindowRef is forEachVisibleRef over a single window, for the per-segment parallel
// workers.
func forEachVisibleWindowRef(s0 uint64, w segWindow, fn func(recRef) bool) {
	d := w.dict()
	for off := 0; off < w.used; {
		o := uint32(off)
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(w.data, o) && recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
			if !fn(recRef{w: w, off: o, dict: d}) {
				return
			}
		}
		off += int(total)
	}
}

// adBytes presents a record as a whole ad plus the codec that decodes it, for the byte-passing
// iterators.
//
// An ordinary record is handed over exactly as stored, still compressed, so nothing changes for the
// shape that has always been there. A COLUMNARIZED record is reassembled first and handed over as
// plain wire bytes with an identity codec -- the callback's own Decompress then just copies, and it
// sees a complete ad rather than the fraction the record physically holds.
//
// scratch is reused across records; the returned bytes are valid only until the next call, which is
// already the contract for the compressed case (the window's mapping outlives neither).
func (c *Collection) adBytes(r recRef, scratch *[]byte) ([]byte, Codec, bool) {
	seg := r.w.seg
	if seg == nil || !(seg.columnarized() || seg.colDamaged.Load()) {
		return r.stored(), r.w.codec, true
	}
	full, err := c.recordWireIn(seg, r.w.data, r.off, *scratch)
	if err != nil {
		return nil, nil, false // skip a record we cannot reassemble rather than serve half of it
	}
	*scratch = full
	return full, identityCodec{}, true
}

// segStoredOrReassembled returns a record's bytes and the codec that decodes them, for the
// point-lookup paths that hold a segment and an offset rather than a window.
//
// The returned codec is the segment's for an ordinary record and the identity codec for a
// reassembled one, so a caller decodes the same way in both cases. The bytes do not alias the
// segment arena in the reassembled case, but the caller copies regardless -- it is holding a lock
// it is about to drop.
//
// A damaged payload reports "not found" rather than an ad, because the signature carries no error.
// That is a lie, but the alternative is an ad missing every schema'd attribute, which a caller
// cannot detect at all; ColNativeCRCFailures counts these so the cause is visible.
func segStoredOrReassembled(c *Collection, seg *segment, off uint32) ([]byte, Codec, bool) {
	if !(seg.columnarized() || seg.colDamaged.Load()) {
		return recAd(seg.data, off), seg.codec, true
	}
	full, err := c.recordWireIn(seg, seg.data, off, nil)
	if err != nil {
		return nil, nil, false
	}
	return full, identityCodec{}, true
}
