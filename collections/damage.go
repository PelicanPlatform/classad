package collections

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"sort"
	"sync/atomic"
)

// DAMAGED RECORDS.
//
// A record that fails its checksum cannot be trusted for anything, its header included. Its key,
// its seq, its supersededBySeq and its totalLen are bytes like any other, and most walks of a segment
// read exactly those fields without checking the CRC: the directory rebuild picks a winner per key
// from them and supersedes the losers on disk, the visibility check of every scan reads seq and
// supersededBySeq, and every walk steps by totalLen. A damaged record can therefore supersede a live
// record whose key it happens to match, appear live when it was not, or misframe the walk that steps
// over it.
//
// So a segment carries the byte ranges of the records known to be damaged (segment.damage), and every
// walk skips them: a span is stepped over to its end without reading anything inside it, and a row or
// key-index offset that lands in one is ignored. The end of a span is never taken on the damaged
// header's word alone. It comes from a proof of the framing:
//
//   - a verified columnar payload after the damage, whose offset map names every keyed record of the
//     segment (recoverPastDamage). Every row must be either a record that verifies and ends where the
//     next row begins, or the start of damage that ends there; and every record that verifies must be
//     a row. The payload therefore delimits the damage, and nothing a resync finds inside it is used;
//   - the segment's sidecar key index, which lists every record's offset as written at seal
//     (keyIndexDamage); or
//   - a header walk that lands exactly on a trusted extent, for damage the directory rebuild finds by
//     checksum. Every other walk of the segment steps the same way.
//
// The set is fixed when the segment is opened and persisted in its sidecar (see buildSegmentSidecar),
// so a later open that trusts the sidecar's extent also knows what it must skip. A rewrite of the
// segment (compaction, columnarization, a merge) leaves the damaged records out, and the new segment
// starts with no set.

// damageSpan is a damaged record's byte range [start, end) within its segment.
type damageSpan struct{ start, end uint32 }

// damageSet is a segment's damaged spans, sorted by start and disjoint. Immutable once published.
type damageSet struct {
	spans []damageSpan
	end   uint32 // the last span's end: no offset at or past it is damaged
}

// newDamageSet returns spans as a set, or nil when there are none. spans must be disjoint.
func newDamageSet(spans []damageSpan) *damageSet {
	if len(spans) == 0 {
		return nil
	}
	s := append([]damageSpan(nil), spans...)
	sort.Slice(s, func(i, j int) bool { return s[i].start < s[j].start })
	return &damageSet{spans: s, end: s[len(s)-1].end}
}

// skip reports whether o lies in a damaged span and, if so, the offset the walk resumes at. Nil-safe:
// a segment without damage pays one comparison per record.
func (d *damageSet) skip(o uint32) (uint32, bool) {
	if d == nil {
		return 0, false
	}
	return d.find(o)
}

// covers reports whether o lies in a damaged span. Nil-safe.
func (d *damageSet) covers(o uint32) bool {
	if d == nil {
		return false
	}
	_, ok := d.find(o)
	return ok
}

// holds is covers for segWindow.visible, which runs once per row of a columnar scan and must stay
// within the inliner's budget: covers' binary search is a call, which alone costs more than the budget
// leaves. d must be non-nil. An offset at or past the last span costs one comparison and one before
// the first span two; only an offset between them walks the spans that end before it.
func (d *damageSet) holds(o uint32) bool {
	if o >= d.end {
		return false
	}
	for _, s := range d.spans {
		if o < s.end {
			return o >= s.start
		}
	}
	return false
}

func (d *damageSet) find(o uint32) (uint32, bool) {
	i := sort.Search(len(d.spans), func(i int) bool { return d.spans[i].end > o })
	if i < len(d.spans) && d.spans[i].start <= o {
		return d.spans[i].end, true
	}
	return 0, false
}

// len is how many spans the set holds. Nil-safe.
func (d *damageSet) len() int {
	if d == nil {
		return 0
	}
	return len(d.spans)
}

// with returns d plus span, which must not overlap any span already in d.
func (d *damageSet) with(span damageSpan) *damageSet {
	var spans []damageSpan
	if d != nil {
		spans = d.spans
	}
	return newDamageSet(append(append([]damageSpan(nil), spans...), span))
}

// damaged returns the segment's damage set, nil when it has none. Nil-safe.
func (s *segment) damaged() *damageSet {
	if s == nil {
		return nil
	}
	return s.damage.Load()
}

// damageBlobMagic opens a persisted damage set ("DMG1").
const damageBlobMagic uint32 = 0x31474d44

// encodeDamage serializes a damage set for the sidecar: [magic u32][n u32] n x {start u32, end u32},
// then a CRC-32 (IEEE) over the preceding bytes. Nil encodes as nothing.
func encodeDamage(d *damageSet) []byte {
	if d.len() == 0 {
		return nil
	}
	b := make([]byte, 0, 8+8*len(d.spans)+4)
	b = binary.LittleEndian.AppendUint32(b, damageBlobMagic)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(d.spans)))
	for _, s := range d.spans {
		b = binary.LittleEndian.AppendUint32(b, s.start)
		b = binary.LittleEndian.AppendUint32(b, s.end)
	}
	return binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
}

// decodeDamage parses a persisted damage set. Spans must be 8-aligned, non-empty, ascending and
// disjoint; anything else is refused, and the sidecar with it.
func decodeDamage(b []byte) (*damageSet, bool) {
	if len(b) < 12 || binary.LittleEndian.Uint32(b) != damageBlobMagic {
		return nil, false
	}
	body := b[:len(b)-4]
	if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(b[len(b)-4:]) {
		return nil, false
	}
	n := int(binary.LittleEndian.Uint32(b[4:]))
	if n <= 0 || 8+8*n != len(body) {
		return nil, false
	}
	spans := make([]damageSpan, n)
	var prev uint32
	for i := range spans {
		s := damageSpan{binary.LittleEndian.Uint32(b[8+8*i:]), binary.LittleEndian.Uint32(b[12+8*i:])}
		if s.start%8 != 0 || s.end%8 != 0 || s.end <= s.start || (i > 0 && s.start < prev) {
			return nil, false
		}
		spans[i] = s
		prev = s.end
	}
	return &damageSet{spans: spans, end: prev}, true
}

// damageFits checks a persisted damage set against the segment it came with, in O(spans): every span
// lies inside the extent, and the record each one ends at (unless the extent or another span) frames
// and verifies. A sidecar whose set does not fit is not trusted, and recovery rewalks the segment.
func damageFits(data []byte, used int, d *damageSet) bool {
	if d == nil {
		return true
	}
	for i, s := range d.spans {
		e := int(s.end)
		if e > used {
			return false
		}
		if e == used || (i+1 < len(d.spans) && d.spans[i+1].start == s.end) {
			continue
		}
		if !recFramedVerified(data, e, used) {
			return false
		}
	}
	return true
}

// recFramedVerified reports whether a record starts at off whose framing fits [off, limit) and whose
// checksum verifies.
func recFramedVerified(data []byte, off, limit int) bool {
	if off < 0 || off+recHeaderSize > limit {
		return false
	}
	total := int(recTotalLen(data, uint32(off)))
	return total > 0 && total%8 == 0 && off+total <= limit && recVerifyCRC(data, uint32(off))
}

// recoverPastDamage tries to continue a recovery walk that stopped at a record failing its checksum,
// in a columnarized segment: [rows...][dictionary?][payload]. It resyncs forward from stop and
// collects the records that verify. The last one must be the columnar payload, with nothing written
// after it. Its offset map must then tile the segment from 0: every row is a verified keyed record
// ending where the next row begins, or damage that ends there (the last row's damage ends at the next
// verified record). After the rows comes the dictionary, verified or damaged, then the payload. A
// record that verifies but is not accounted for that way -- a keyed record no row names, or one
// beginning inside a damaged row -- refuses the proof, since only the payload says where records are.
//
// It returns the extent (the payload's end) and the damaged spans. ok is false when there is no such
// proof; the caller then stops at stop, as before.
func recoverPastDamage(data []byte, stop int) (used int, spans []damageSpan, ok bool) {
	type vrec struct{ off, total int }
	// Every record that verifies: the prefix the recovery walk already verified, then those past the
	// stop. Past the stop nothing is stepped over by its own totalLen unless it verifies.
	var recs []vrec
	for off := 0; off < stop; {
		total := int(recTotalLen(data, uint32(off)))
		recs = append(recs, vrec{off, total})
		off += total
	}
	for off := stop; off+recHeaderSize <= len(data); {
		if recFramedVerified(data, off, len(data)) {
			total := int(recTotalLen(data, uint32(off)))
			recs = append(recs, vrec{off, total})
			off += total
			continue
		}
		next := repairResync(data, off)
		if next < 0 {
			break
		}
		off = next
	}
	if len(recs) == 0 {
		return 0, nil, false
	}
	p := recs[len(recs)-1]
	if p.off < stop || !recIsCol(data, uint32(p.off)) || !recIsMarker(data, uint32(p.off)) {
		return 0, nil, false
	}
	end := p.off + p.total
	if end+recHeaderSize <= len(data) && recTotalLen(data, uint32(end)) != 0 {
		return 0, nil, false // something is written after the payload: it is not the segment's end
	}
	cs, err := unmarshalColPayload(recAd(data, uint32(p.off)), identityCodec{}, fsckInterner())
	if err != nil || cs.offsLen() == 0 {
		return 0, nil, false
	}
	n := cs.offsLen()
	pos, ri := 0, 0
	for i := 0; i < n; i++ {
		r := int(cs.offAt(i))
		if r != pos {
			return 0, nil, false // rows must tile the segment: each starts where the last ended
		}
		if ri < len(recs) && recs[ri].off == r && !recIsMarker(data, uint32(r)) {
			pos = r + recs[ri].total
			ri++
			continue
		}
		// A damaged row. It ends where the next row begins, or, for the last, at the next record that
		// verifies; nothing that verifies may begin inside it.
		e := -1
		switch {
		case i+1 < n:
			e = int(cs.offAt(i + 1))
		case ri < len(recs):
			e = recs[ri].off
		}
		if e <= r || (ri < len(recs) && recs[ri].off < e) {
			return 0, nil, false
		}
		spans = append(spans, damageSpan{uint32(r), uint32(e)})
		pos = e
	}
	// After the rows: the dictionary when it verifies, damage where it does not, then the payload.
	if ri < len(recs) && ri != len(recs)-1 {
		if recs[ri].off == pos && recIsDict(data, uint32(pos)) {
			pos += recs[ri].total
			ri++
		}
	}
	if ri == len(recs)-1 && recs[ri].off > pos {
		spans = append(spans, damageSpan{uint32(pos), uint32(recs[ri].off)})
		pos = recs[ri].off
	}
	if ri != len(recs)-1 || pos != p.off {
		return 0, nil, false
	}
	if len(spans) == 0 {
		return 0, nil, false // the stop is not accounted for; cannot happen when stop failed its CRC
	}
	return end, spans, true
}

// keyIndexDamage derives a segment's damaged spans from its sidecar key index, which lists the offset
// of every record (markers included) as the sidecar was written. Consecutive offsets delimit each
// record. A record that verifies carries a totalLen its checksum covers, so it must end at the next
// offset -- or, in a sidecar written with a damage set (which leaves damaged records out of its key
// index), at the start of a run of known spans that does. A record that does not verify is damaged up
// to the next offset. A verified record whose key does not hash to its entry means the index describes
// some other segment, and refuses the proof, as do offsets that are not ascending, a verified record
// running past the next offset, a gap no known span accounts for, or nothing that verifies.
func keyIndexDamage(data []byte, upto int, ki *mmapKeyIndex, h Hasher, known *damageSet) ([]damageSpan, bool) {
	if ki == nil || ki.count == 0 || upto <= 0 || upto > len(data) || int(ki.upto) != upto {
		return nil, false
	}
	type ent struct {
		off  uint32
		hash uint64
		tomb bool // filed under the key it DELETES, not under its own (empty) inline key
	}
	ents := make([]ent, ki.count)
	for i := uint32(0); i < ki.count; i++ {
		ents[i] = ent{ki.offAt(i), ki.hashAt(i), ki.isTombstoneAt(i)}
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].off < ents[j].off })
	var spans []damageSpan
	// gap accounts for [from, to) with known spans that tile it exactly.
	gap := func(from, to uint32) bool {
		for from < to {
			end, ok := known.skip(from)
			if !ok || end > to {
				return false
			}
			spans = append(spans, damageSpan{from, end})
			from = end
		}
		return true
	}
	if !gap(0, ents[0].off) {
		return nil, false
	}
	good := 0
	for i, e := range ents {
		end := uint32(upto)
		if i+1 < len(ents) {
			end = ents[i+1].off
		}
		if end <= e.off || e.off%8 != 0 {
			return nil, false
		}
		if !recFramedVerified(data, int(e.off), upto) {
			spans = append(spans, damageSpan{e.off, end})
			continue
		}
		// A delete marker is keyless, so its entry's hash is not the hash of its inline key:
		// since v2 it is the hash of the key it deletes, which is what lets recovery find a
		// delete from the key. Checking it against the inline key would make this refuse the
		// proof for every segment that holds a delete -- which is most of them.
		want := h.Hash(recKey(data, e.off))
		if e.tomb {
			k, ok := recDeleteMarkerKey(data, e.off)
			if !ok {
				return nil, false // flagged a tombstone but is not one: not this segment's index
			}
			want = h.Hash(k)
		}
		if want != e.hash {
			return nil, false
		}
		recEnd := e.off + recTotalLen(data, e.off)
		if recEnd > end || !gap(recEnd, end) {
			return nil, false
		}
		good++
	}
	if good == 0 {
		return nil, false
	}
	return spans, true
}

// persistSegDamage rewrites an adopted sidecar to record the segment's damage set (sidecar v5), which
// Open found after the sidecar was written. Its other sections are kept; the key index is rebuilt
// without the damaged records. Best effort: on failure the next open finds the damage again.
func (c *Collection) persistSegDamage(seg *segment) bool {
	path := snapshotPath(seg)
	if path == "" || !seg.persistent {
		return false
	}
	// Read rather than mapped: the sections are copied into the new container anyway.
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	attr, _, col, zone, ok := splitSegmentSidecar(data)
	if !ok {
		return false
	}
	container := buildSegmentSidecar(attr, c.segKeyIndex(seg), col,
		zone, seg.used, seg.dictRecOff(), seg.colRecOff(), encodeDamage(seg.damaged()))
	if writeFileAtomic(path, container) != nil {
		return false
	}
	seg.damageUnsaved = false
	return true
}

// verifyForRewrite checks a segment about to be rewritten (compacted, columnarized, merged or
// resealed): it walks the records the rewrite will copy and checks each against its CRC. A rewrite
// copies a record's key, seq and supersededBySeq and writes a fresh checksum over them, so a damaged
// record copied that way stops looking damaged, and its key and seq are believed from then on. Each
// record that fails is added to the segment's damage set, which the rewrite skips, so it is left out.
// It returns false, and the segment is not rewritten, when the walk does not land on the extent: some
// totalLen is damaged, and what lies past it cannot be told apart without a proof of the framing. Nor
// is a segment whose recovery stopped at damage with records past it: a rewrite would unlink the only
// copy of those records, which Repair can still recover. A RAM segment carries no checksums and is not
// checked.
func verifyForRewrite(seg *segment) bool {
	if seg == nil || !seg.persistent {
		return true
	}
	if seg.stoppedAtDamage {
		rewritesRefusedDamaged.Add(1)
		return false
	}
	dm := seg.damaged()
	var bad []damageSpan
	off := 0
	for off < seg.used {
		o := uint32(off)
		if end, ok := dm.skip(o); ok {
			off = int(end)
			continue
		}
		total := int(recTotalLen(seg.data, o))
		if total <= 0 || total%8 != 0 || off+total > seg.used {
			rewritesRefusedDamaged.Add(1)
			seg.framingLost.Store(true)
			return false
		}
		if !recVerifyCRC(seg.data, o) {
			bad = append(bad, damageSpan{o, o + uint32(total)})
		}
		off += total
	}
	if len(bad) > 0 {
		if dm != nil {
			bad = append(bad, dm.spans...)
		}
		seg.damage.Store(newDamageSet(bad))
	}
	return true
}

// rewritesRefusedDamaged counts rewrites refused because a source segment's framing was damaged.
var rewritesRefusedDamaged atomic.Int64

// RewritesRefusedDamaged reports how many times, since the process started, a rewrite of a segment
// (compaction, columnarization, a merge or a reseal) was not done because of damage: a header walk of
// the segment did not land on its extent, or Open's recovery stopped at damage in it and left records
// past the stop (OpenIndexDiag.DamageStops). The segment is left as it is for Fsck and Repair.
func RewritesRefusedDamaged() int64 { return rewritesRefusedDamaged.Load() }

// errRecordDamaged is a read of a record its segment holds as damaged.
var errRecordDamaged = errors.New("collections: record is damaged")

// damagedRecordsSkipped counts damaged records Open set aside, and damagedRecordsDropped those a rewrite
// left out.
var damagedRecordsSkipped, damagedRecordsDropped atomic.Int64

// DamagedRecordsSkipped reports how many damaged records, since the process started, Open found in the
// segments it loaded and set aside: every walk skips them, and none is read, superseded by, or
// supersedes another record. They are found by a recovery walk that continues past damage, by the
// directory rebuild, or from a damage set a sidecar recorded, and each open counts them again.
// OpenIndexDiag.DamageStops names the segments that recovery continued through.
func DamagedRecordsSkipped() int64 { return damagedRecordsSkipped.Load() }

// DamagedRecordsDropped reports how many damaged records, since the process started, left the store
// with their segment: a rewrite (compaction, columnarization, a merge or a reseal) leaves them out,
// and retention or Truncate removes them with the rest. It counts the segment's damage set when the
// segment is retired, including the records a rewrite found failing their checksum (verifyForRewrite).
func DamagedRecordsDropped() int64 { return damagedRecordsDropped.Load() }
