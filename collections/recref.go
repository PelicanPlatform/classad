package collections

import (
	"errors"
	"sync/atomic"
)

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

// errRecordCRC is returned when read verification is on and a record's stored checksum does
// not match its bytes. Callers of wire/wireAt already skip a record they cannot
// complete rather than serve half an ad; a record that fails its own checksum is the
// same decision for the same reason.
var errRecordCRC = errors.New("collections: record failed its checksum")

// verifyReadSkips counts records read verification refused. A skipped record silently
// shrinks a query's answer, so this has to be observable: "the query returned fewer
// rows" is not something a caller can otherwise distinguish from "there were fewer
// rows".
var verifyReadSkips atomic.Int64

// VerifyReadSkips reports how many records have been refused because their checksum did
// not match, across every collection in this process. It is zero unless some collection
// was opened with DisableReadVerification.
//
// Non-zero means reads are quietly incomplete, and fsck is what says where.
func VerifyReadSkips() int64 { return verifyReadSkips.Load() }

// VerifyingReads reports whether this collection checks each record's stored checksum
// before using its bytes. It is true for a persistent collection unless
// Options.DisableReadVerification was set, and always false in memory.
//
// Exported so a daemon can state which mode it is running in rather than inferring it
// from its own configuration parsing, which is the sort of thing that drifts.
func (c *Collection) VerifyingReads() bool { return c != nil && c.verifyReads }

// recordFailsCRC reports whether verification is on and this record does not verify.
// It is the one place the condition is written, so the iterator paths and the point-read
// path cannot drift apart on what "verified" means.
func (c *Collection) recordFailsCRC(seg *segment, data []byte, off uint32) bool {
	if c == nil || !c.verifyReads || seg == nil || !seg.persistent {
		return false
	}
	if recVerifyCRC(data, off) {
		return false
	}
	verifyReadSkips.Add(1)
	return true
}

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
	return c.wireAt(r, seqMax, buf)
}

// wireAt is wire for a reader that knows the snapshot it is reading at, which a DELTA record
// needs: merging one means selecting the versions visible at that snapshot.
//
// Every reader that goes through here gets whole ads, which is the point -- this and adBytes
// are the two places the package already reassembles a partial record (the columnar case), and
// teaching readers one at a time is how roughly ten of them ended up serving fragments.
// Callers that genuinely have no snapshot pass seqMax and get the current version.
//
// Every failure is counted by reason (RecordReadSkips): the callers skip the record, so the count is
// the only trace that an answer came back short.
func (c *Collection) wireAt(r recRef, s0 uint64, buf []byte) ([]byte, error) {
	w, err := c.wireAtUncounted(r, s0, buf)
	if err != nil {
		noteReadSkip(err)
	}
	return w, err
}

// wireAtUncounted is wireAt without the count, so each failure is counted once, in wireAt.
func (c *Collection) wireAtUncounted(r recRef, s0 uint64, buf []byte) ([]byte, error) {
	if err := c.recordRefused(r.w.seg, r.w.data, r.off); err != nil {
		return nil, err
	}
	var raw []byte
	var err error
	if seg := r.w.seg; seg != nil && (seg.columnarized() || seg.colDamaged.Load() || recIsStripped(r.w.data, r.off)) {
		raw, err = c.recordWireIn(seg, r.w.data, r.off, buf)
	} else {
		raw, err = r.w.codec.Decompress(buf[:0], r.stored())
	}
	if err != nil || !c.deltaRead {
		return raw, err
	}
	if deltaHdrDispatch {
		if !recIsDelta(r.w.data, r.off) {
			return raw, err
		}
	} else if !isDeltaRecord(raw) { // see deltaHdrDispatch: the pre-header dispatch
		return raw, err
	}
	key := r.key()
	h := c.h.Hash(key)
	sh := c.shards[c.shardOf(key, h)]
	sh.mu.RLock()
	merged, _, ok := sh.materializeAt(c, key, h, s0, mWire, buf[:0])
	sh.mu.RUnlock()
	if !ok {
		return nil, errDeltaUnresolved // a fragment we cannot complete: skip, never serve half an ad
	}
	return merged, nil
}

// errDeltaUnresolved is a delta record whose chain could not be merged into a whole ad.
var errDeltaUnresolved = errors.New("collections: delta record could not be merged into a whole ad")

// recordRefused reports why a record must not be read at all, before any of its bytes are used:
// errRecordDamaged when its segment holds it as damaged, errRecordCRC when verification is on and its checksum does not match, errSegDictUnusable when its
// segment's attribute dictionary is unusable. nil otherwise.
func (c *Collection) recordRefused(seg *segment, data []byte, off uint32) error {
	if seg.damaged().covers(off) {
		return errRecordDamaged
	}
	if c.recordFailsCRC(seg, data, off) {
		return errRecordCRC
	}
	if seg != nil && seg.dictUnusable.Load() {
		return errSegDictUnusable
	}
	return nil
}

// forEachVisibleRef walks the frozen windows in append order and calls fn for each record visible
// at snapshot s0, skipping markers. It is the one place the visibility walk is written; the
// byte-passing iterators are expressed in terms of it.
func forEachVisibleRef(s0 uint64, wins []segWindow, fn func(recRef) bool) {
	for _, w := range wins {
		d := w.dict()
		off := 0
		for off < w.used {
			o := uint32(off)
			if end, ok := w.dmg.skip(o); ok {
				off = int(end)
				continue
			}
			total := recTotalLen(w.data, o)
			if total == 0 {
				break
			}
			if !recIsMarker(w.data, o) && w.visible(o, s0) {
				if !fn(recRef{w: w, off: o, dict: d}) {
					return
				}
			}
			off += int(total)
		}
		noteFramingLost(off, w.used)
	}
}

// noteFramingLost counts a walk of a window that did not land on its end: a damaged totalLen the
// segment's damage set does not know misframed it, and the records past that point were not read.
// That is damage inside a trusted extent, which only a walk finds (Fsck names the segment).
func noteFramingLost(off, used int) {
	if off != used {
		noteReadSkip(errFramingLost)
	}
}

// errFramingLost is a walk that lost the records' framing at a damaged length.
var errFramingLost = errors.New("collections: a walk lost the records' framing at a damaged length")

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
		off := 0
		for off < w.used {
			o := uint32(off)
			if end, ok := w.dmg.skip(o); ok {
				off = int(end)
				continue
			}
			total := recTotalLen(w.data, o)
			if total == 0 {
				break
			}
			offs = append(offs, o)
			off += int(total)
		}
		noteFramingLost(off, w.used)
		for i := len(offs) - 1; i >= 0; i-- {
			o := offs[i]
			if !recIsMarker(w.data, o) && w.visible(o, s0) {
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
	off := 0
	for off < w.used {
		o := uint32(off)
		if end, ok := w.dmg.skip(o); ok {
			off = int(end)
			continue
		}
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(w.data, o) && w.visible(o, s0) {
			if !fn(recRef{w: w, off: o, dict: d}) {
				return
			}
		}
		off += int(total)
	}
	noteFramingLost(off, w.used)
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
//
// A false return is counted by reason (RecordReadSkips), since every caller skips the record.
func (c *Collection) adBytes(r recRef, s0 uint64, scratch *[]byte) ([]byte, Codec, bool) {
	ad, codec, err := c.adBytesErr(r, s0, scratch)
	if err != nil {
		noteReadSkip(err)
		return nil, nil, false
	}
	return ad, codec, true
}

// adBytesErr is adBytes reporting why a record could not be presented.
func (c *Collection) adBytesErr(r recRef, s0 uint64, scratch *[]byte) ([]byte, Codec, error) {
	seg := r.w.seg
	if err := c.recordRefused(seg, r.w.data, r.off); err != nil {
		return nil, nil, err
	}
	stored, codec := r.stored(), r.w.codec
	if seg != nil && (seg.columnarized() || seg.colDamaged.Load() || recIsStripped(r.w.data, r.off)) {
		full, err := c.recordWireIn(seg, r.w.data, r.off, *scratch)
		if err != nil {
			return nil, nil, err // skip a record we cannot reassemble rather than serve half of it
		}
		*scratch = full
		stored, codec = full, identityCodec{}
	}
	// A delta record holds only the attributes one write changed -- the same hazard the
	// columnar branch above exists for: half an ad is indistinguishable from an ad whose
	// attributes really were removed. Resolving it HERE covers every reader that goes through
	// this primitive (the visible-record walks behind serial, reverse and chained scans, the
	// ordered-index rebuild, ForEachAd, and the watch catch-up) rather than requiring each to
	// be taught separately -- which is how about ten of them ended up serving fragments.
	//
	// The test is the record HEADER, so an ordinary record in a delta-enabled collection leaves
	// here on the same line it always did, still compressed, with the callback decompressing it
	// once as before. Deciding this from the payload instead meant decompressing every record
	// scanned -- and then the callback's identity Decompress copied the result again, so a scan
	// of a store of whole ads paid a decompression and a full-ad memcpy per record for a feature
	// none of its records used.
	if !c.deltaRead {
		return stored, codec, nil
	}
	if deltaHdrDispatch {
		if !recIsDelta(r.w.data, r.off) {
			return stored, codec, nil
		}
	} else {
		// The pre-header dispatch (see deltaHdrDispatch), reproduced exactly so the A/B compares
		// implementations and not incidental extra work: one decompression, reused by the caller
		// through the identity codec when the record turns out to be an ordinary one.
		raw, err := codec.Decompress((*scratch)[:0], stored)
		if err != nil {
			return nil, nil, err
		}
		*scratch = raw
		if !isDeltaRecord(raw) {
			return raw, identityCodec{}, nil
		}
	}
	key := r.key()
	h := c.h.Hash(key)
	sh := c.shards[c.shardOf(key, h)]
	sh.mu.RLock()
	merged, _, ok := sh.materializeAt(c, key, h, s0, mWire, (*scratch)[:0])
	sh.mu.RUnlock()
	if !ok {
		return nil, nil, errDeltaUnresolved
	}
	*scratch = merged
	return merged, identityCodec{}, nil
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
	ad, codec, ok, _ := segStoredOrReassembledWhy(c, seg, off)
	return ad, codec, ok
}

// segStoredOrReassembledWhy is segStoredOrReassembled carrying the reason for a miss, so a
// caller that has to explain a failed read (encodePatchOnly) can say which condition it hit
// rather than reporting every one of them as the same opaque "unreadable base".
func segStoredOrReassembledWhy(c *Collection, seg *segment, off uint32) ([]byte, Codec, bool, readFail) {
	// Every record carries a checksum over its own bytes, but nothing on the read path
	// looks at it -- it is verified at recovery and at columnarization, and in between a
	// bit flip inside an encoded ad comes back as a plausible wrong value. Checking here
	// turns that into a miss. On by default; see Options.DisableReadVerification.
	if seg.damaged().covers(off) {
		noteReadSkip(errRecordDamaged)
		return nil, nil, false, failRecordCRC
	}
	if c != nil && c.verifyReads && seg.persistent && !recVerifyCRC(seg.data, off) {
		noteReadSkip(errRecordCRC)
		return nil, nil, false, failRecordCRC
	}
	if seg.dictUnusable.Load() {
		noteReadSkip(errSegDictUnusable)
		return nil, nil, false, failDecode
	}
	if !(seg.columnarized() || seg.colDamaged.Load() || recIsStripped(seg.data, off)) {
		return recAd(seg.data, off), seg.codec, true, failNone
	}
	full, err := c.recordWireIn(seg, seg.data, off, nil)
	if err != nil {
		noteReadSkip(err)
		return nil, nil, false, failReassemble
	}
	return full, identityCodec{}, true, failNone
}
