package collections

import (
	"encoding/binary"
	"errors"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/collections/wire"
)

var errNotNumericField = errors.New("adschema: scanInt on a non-numeric field")

// colBlockSeq assigns each built block a process-unique id, used as the block-cache key.
var colBlockSeq atomic.Uint64

// columnarBlock is the PAX layout for a row-group of adSchema records (see adschema.go): the
// schema's popular ("hot") numeric fields plus the escape and bool bitsets stay uncompressed
// in a fixed-stride region (so an attribute scan reads a column with no decode), while the
// cold numeric fields (columnar), the string regions, and the cold tails are each a separately
// Codec-compressed stream. A numeric scan on a hot field touches only the uncompressed region;
// a scan on a cold field decompresses just that column group; a full-ad reconstruct
// decompresses the string + cold streams. This is the sealed-segment form; the active segment
// stays row-based (per-record).
//
// Hot region per record (stride = hotStride): [escape bitmap][bool bitset][hot int/real,
// packed]. Cold numeric buffer is columnar: each cold field's values contiguous across the
// block. String/cold streams are per-record regions concatenated, addressed by strOff/coldOff.
type columnarBlock struct {
	id     uint64 // process-unique, the block-cache key
	schema *adSchema
	codec  Codec
	n      int

	hot         []byte // uncompressed region, hotStride bytes per record
	hotStride   int
	hotNum      []int       // hot int/real field indices (into schema.fields), layout order
	hotFieldOff map[int]int // field idx -> byte offset within a record's hot region

	coldNum        []int       // cold int/real field indices, layout order
	coldFieldStart map[int]int // field idx -> start offset in the decompressed cold-numeric buffer
	coldNumComp    []byte      // cold numeric fields, columnar, Codec-compressed

	strComp, coldComp []byte // per-record string regions / cold tails, concatenated + compressed
	strOff, coldOff   []int  // per-record cumulative offsets into the decompressed streams
}

// encodeColumnarBlock builds a columnar block from row-form records (each the output of
// adSchema.encode). hotNumFields lists the schema field indices (int/real) to keep uncompressed
// -- the popular ones, by query demand. Bools and the escape bitmap are always in the hot
// region (tiny, and bools may be scanned).
// layoutColumnar fills b's hot/cold field partition and their offsets from the schema and hot
// set: hot numerics packed after the escape+bool bitsets (stride hotStride), cold numerics
// columnar (each field's n values contiguous). Deterministic in (schema, hotSet, n), so the
// persisted-block decoder reproduces the exact layout.
func layoutColumnar(b *columnarBlock, s *adSchema, hotNumFields []int, n int) {
	hotSet := make(map[int]bool, len(hotNumFields))
	for _, i := range hotNumFields {
		hotSet[i] = true
	}
	b.hotNum, b.coldNum = b.hotNum[:0], b.coldNum[:0]
	b.hotFieldOff, b.coldFieldStart = map[int]int{}, map[int]int{}
	for i := range s.fields {
		if k := s.fields[i].kind; k == akInt || k == akReal {
			if hotSet[i] {
				b.hotNum = append(b.hotNum, i)
			} else {
				b.coldNum = append(b.coldNum, i)
			}
		}
	}
	hp := s.escBytes + s.boolBytes
	for _, i := range b.hotNum {
		b.hotFieldOff[i] = hp
		hp += s.fields[i].width
	}
	b.hotStride = hp
	cp := 0
	for _, i := range b.coldNum {
		b.coldFieldStart[i] = cp
		cp += s.fields[i].width * n
	}
}

func encodeColumnarBlock(s *adSchema, recs [][]byte, hotNumFields []int, codec Codec) *columnarBlock {
	b := &columnarBlock{id: colBlockSeq.Add(1), schema: s, codec: codec, n: len(recs)}
	layoutColumnar(b, s, hotNumFields, len(recs))
	cp := 0
	for _, i := range b.coldNum {
		cp += s.fields[i].width * len(recs)
	}
	b.hot = make([]byte, 0, b.hotStride*len(recs))
	coldRaw := make([]byte, cp)
	var strCat, coldCat []byte
	b.strOff = []int{0}
	b.coldOff = []int{0}
	for k, r := range recs {
		b.hot = append(b.hot, r[:s.escBytes]...)                       // escape
		b.hot = append(b.hot, r[s.escBytes:s.escBytes+s.boolBytes]...) // bool bitset
		for _, i := range b.hotNum {
			off := s.escBytes + s.fields[i].off
			b.hot = append(b.hot, r[off:off+s.fields[i].width]...)
		}
		for _, i := range b.coldNum {
			off := s.escBytes + s.fields[i].off
			copy(coldRaw[b.coldFieldStart[i]+k*s.fields[i].width:], r[off:off+s.fields[i].width])
		}
		_, str, cold := s.splitRecord(r)
		strCat = append(strCat, str...)
		coldCat = append(coldCat, cold...)
		b.strOff = append(b.strOff, len(strCat))
		b.coldOff = append(b.coldOff, len(coldCat))
	}
	b.coldNumComp = codec.Compress(nil, coldRaw)
	b.strComp = codec.Compress(nil, strCat)
	b.coldComp = codec.Compress(nil, coldCat)
	return b
}

// buildColumnarFromSegment transcodes a segment's live-arena records in [0, upto) into a
// columnar block, mirroring buildSegIndex: it walks the records, decompresses each (skipping
// time-travel markers), re-encodes it in the adSchema row form, and builds the block. It
// returns the block and offs, where offs[k] is the segment offset of block record k -- the map
// a scan uses to recover each record's MVCC seq/sup (for visibility) and its key. Reads
// immutable segment bytes only.
// toInterned canonicalizes a decompressed record into interned wire (identity for an already-
// interned collection; a decode/re-encode for an inline/persistent one). encode reads the
// id-keyed form, so a non-interned record must be converted first or the block would be empty.
func buildColumnarFromSegment(data []byte, upto int, codec Codec, s *adSchema, hot []int, toInterned func(dst, w []byte) ([]byte, bool)) (*columnarBlock, []uint32) {
	var recs [][]byte
	var offs []uint32
	var buf []byte
	for off := 0; off < upto; {
		o := uint32(off)
		total := recTotalLen(data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(data, o) {
			if w, err := codec.Decompress(buf[:0], recAd(data, o)); err == nil {
				buf = w
				if iw, ok := toInterned(nil, w); ok {
					recs = append(recs, s.encode(wire.Ad(iw)))
					offs = append(offs, o)
				}
			}
		}
		off += int(total)
	}
	return encodeColumnarBlock(s, recs, hot, codec), offs
}

// scanInt calls fn for each record's value of a numeric (int/real read as int bits) field: a
// hot field reads the uncompressed region directly (no decode); a cold field decompresses its
// column group once (via bc, nil for no cache). present is false for a missing/exceptional
// record (its value is in the cold tail).
func (b *columnarBlock) scanInt(fieldIdx int, bc *blockCache, fn func(rec int, present bool, v int64)) error {
	f := b.schema.fields[fieldIdx]
	if _, hot := b.hotFieldOff[fieldIdx]; hot {
		off := b.hotFieldOff[fieldIdx]
		for k := 0; k < b.n; k++ {
			base := k * b.hotStride
			if testBit(b.hot[base:base+b.schema.escBytes], fieldIdx) {
				fn(k, false, 0)
				continue
			}
			fn(k, true, readIntLE(b.hot[base+off:], f.width, f.unsigned))
		}
		return nil
	}
	start, ok := b.coldFieldStart[fieldIdx]
	if !ok {
		return errNotNumericField
	}
	ds, err := bc.streams(b)
	if err != nil {
		return err
	}
	for k := 0; k < b.n; k++ {
		if testBit(b.escapeAt(k), fieldIdx) {
			fn(k, false, 0)
			continue
		}
		fn(k, true, readIntLE(ds.coldNum[start+k*f.width:], f.width, f.unsigned))
	}
	return nil
}

// escapeAt returns record k's escape bitmap (in the uncompressed hot region).
func (b *columnarBlock) escapeAt(k int) []byte {
	base := k * b.hotStride
	return b.hot[base : base+b.schema.escBytes]
}

// reconstruct rebuilds record k's row form (identical to the adSchema.encode input), so
// schema.forEach reconstructs the full ad. Decompresses the cold-numeric, string, and cold
// streams (via bc, nil for no cache) -- the full-ad-read cost, amortized by the cache.
func (b *columnarBlock) reconstruct(k int, bc *blockCache) ([]byte, error) {
	s := b.schema
	ds, err := bc.streams(b)
	if err != nil {
		return nil, err
	}
	coldRaw, strRaw, tailRaw := ds.coldNum, ds.str, ds.cold
	rec := make([]byte, s.escBytes+s.fixedLen, s.escBytes+s.fixedLen+(b.strOff[k+1]-b.strOff[k])+(b.coldOff[k+1]-b.coldOff[k]))
	base := k * b.hotStride
	copy(rec[:s.escBytes], b.hot[base:base+s.escBytes])                                              // escape
	copy(rec[s.escBytes:s.escBytes+s.boolBytes], b.hot[base+s.escBytes:base+s.escBytes+s.boolBytes]) // bools
	for _, i := range b.hotNum {
		w := s.fields[i].width
		copy(rec[s.escBytes+s.fields[i].off:], b.hot[base+b.hotFieldOff[i]:base+b.hotFieldOff[i]+w])
	}
	for _, i := range b.coldNum {
		w := s.fields[i].width
		copy(rec[s.escBytes+s.fields[i].off:], coldRaw[b.coldFieldStart[i]+k*w:b.coldFieldStart[i]+k*w+w])
	}
	rec = append(rec, strRaw[b.strOff[k]:b.strOff[k+1]]...)
	rec = append(rec, tailRaw[b.coldOff[k]:b.coldOff[k+1]]...)
	return rec, nil
}

// escapedNumField reads the numeric value of an ESCAPED schema field (fieldID) for record k
// straight from its cold-tail slice, skipping the whole-record reconstruct. A field is escaped
// when it is missing or exceptional (out of the fitted width, or a type exception); an exceptional
// value lives in the cold tail as a uvarint(id)+node entry alongside the record's non-schema
// attributes. Returns (value, true) if fieldID has a numeric value here, or (0, false) if it is
// missing or non-numeric. bc caches the decompressed cold stream, so a scan's many escaped reads
// on one block share a single decompression. Callers use this only for records the scan has
// already found escaped for fieldID; a hot/cold-column value never reaches here.
func (b *columnarBlock) escapedNumField(k int, fieldID uint32, bc *blockCache) (float64, bool, error) {
	ds, err := bc.streams(b)
	if err != nil {
		return 0, false, err
	}
	cold := ds.cold[b.coldOff[k]:b.coldOff[k+1]]
	for len(cold) > 0 {
		id, m := binary.Uvarint(cold)
		if m <= 0 {
			return 0, false, nil // malformed tail: treat fieldID as absent
		}
		cold = cold[m:]
		nl, ok := wire.NodeLen(cold)
		if !ok || nl > len(cold) {
			return 0, false, nil
		}
		if uint32(id) == fieldID {
			f, found := literalFloat(cold[:nl])
			return f, found, nil
		}
		cold = cold[nl:]
	}
	return 0, false, nil
}
