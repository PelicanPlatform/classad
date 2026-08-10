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

// Row groups: a segment is split into a series of columnar blocks rather than being one block, which
// is what makes this layout PAX (a hybrid) instead of fully columnar.
//
// WHY a bound is needed. Reading a value that ESCAPED its fixed slot means decompressing its
// block's cold tail, and reconstructing a full ad means decompressing its block's string + cold
// regions. Both costs are per-BLOCK, so they scale with the block. Measured on the OSPool slot
// corpus (1500 ads, 561 attrs/ad; schema_fullread_test.go), point-reading one ad:
//
//	group  point read   vs row   full read   size
//	row      14.4µs       1x      23.0µs/ad  4078 B/ad
//	128       246µs      17x      11.3µs/ad   858 B/ad
//	256       563µs      39x      11.5µs/ad   839 B/ad
//	512      1.32ms      91x      11.3µs/ad   828 B/ad
//	1500     4.59ms     319x      20.6µs/ad   824 B/ad
//
// Linear in the group size, while full-read is already saturated at 128 (and ~2x faster than the
// row form, since a group's decompression amortizes over its records) and size barely moves -- 858
// vs 824 B/ad is 0.8 points against the row baseline. The harder bound is the block cache: those
// blocks' decompressed regions run ~8867 B/ad, so a block past ~30k records exceeds the cache's
// whole 256 MiB budget, can never be admitted, and re-decompresses on EVERY read -- permanently,
// not as a cold start. One block per segment sat exactly there: at defaultMaxSegmentBytes (1 GiB) a
// segment holds far more than 30k ads.
//
// WHAT IT BUYS on the real store, over a full-column aggregate on the OSPool corpus in 8 MiB
// segments (~800 ads per segment; colgroup_bench_test.go, medians of 3):
//
//	                          cold      warm    decompression (cold-warm)
//	one block per segment    4.02ms    2.70ms      1.32ms
//	~1 MiB groups            3.06ms    2.70ms      0.36ms
//
// So ~24% off the whole query and ~4x off the part the block size actually governs. That is the
// MODEST end of the range by construction: an aggregate reads the column for every record, so a
// block's decompression is amortized over all of it. The large end is the sparse case -- a point
// read, or a scan where only a few records escape and each one drags in its block's whole tail --
// which is the 17x-vs-319x in the table above, and which is what made MAX(ProcId) take 60s.
//
// WHY THE BOUND IS IN BYTES, not rows. 128 rows of those ads is ~1.1 MiB of regions: the row count
// was a byte budget in disguise, and it does not travel to other ad shapes. What a block costs to
// touch, and whether it fits the cache at all, are properties of its BYTES; rows are only a proxy,
// and a bad one across a 300x range in ad width (a 5-attribute job ad vs a 561-attribute slot ad).
// Budgeting the real quantity means a table of fat ads lands near the measured 128-row sweet spot on
// its own, while a table of small ads gets thousands of rows per group instead of blocks so small
// that splitting them accomplishes nothing.
//
// On that second case, note that a row-count rule is not measurably HARMFUL -- on a 5-attribute
// fixture (~27 B/record) 128-row groups came out within ~3% of whole-segment blocks, i.e. noise, at
// identical stored size. It is simply arbitrary there, and a bound that means nothing on half the
// tables it applies to is one nobody can reason about later. The byte budget is the same decision
// expressed in the units that caused it.
//
// Splitting is not free in principle: each block is one decompress call and one cache entry per
// scan, and compressing groups independently gives up cross-group redundancy. Both measured small --
// the warm column above is flat, and stored size moved 0.8 points on the corpus sweep.
const (
	// colGroupTargetBytes is the uncompressed record-bytes budget for one row group. ~1 MiB keeps a
	// block's decompressed regions two orders of magnitude under the 256 MiB cache budget (so
	// hundreds cache at once and nothing is uncacheable) while staying large enough to compress
	// well and to amortize a decompress call.
	colGroupTargetBytes = 1 << 20

	// colGroupMaxRows caps rows per group regardless of size, so a table of very small ads still
	// gets bounded blocks -- and bounded point reads -- rather than one block per segment by
	// default. Well above the byte budget's reach for any ad fat enough for the byte rule to bind.
	colGroupMaxRows = 8192

	// colGroupRows is the reference point the measurements above were taken at: what
	// colGroupTargetBytes works out to for OSPool slot ads (~8867 B/ad of regions). Kept as the
	// documented equivalence, and used by tests that need an exact row-count grouping.
	colGroupRows = 128
)

// colGrouping is the row-group sealing policy: seal a group once its records reach targetBytes of
// uncompressed row form, or maxRows records, whichever comes first.
//
// A zero targetBytes means "no byte rule" -- group purely by row count, which is what a test
// sweeping exact group sizes wants. A zero maxRows means colGroupMaxRows.
type colGrouping struct {
	targetBytes int
	maxRows     int
}

// defaultColGrouping is the production policy.
func defaultColGrouping() colGrouping {
	return colGrouping{targetBytes: colGroupTargetBytes, maxRows: colGroupMaxRows}
}

// byRows is a pure row-count policy, for tests and benchmarks sweeping the group size.
func byRows(n int) colGrouping { return colGrouping{maxRows: n} }

// full reports whether a group holding rows records of bytes total should be sealed now.
func (g colGrouping) full(rows, bytes int) bool {
	maxRows := g.maxRows
	if maxRows <= 0 {
		maxRows = colGroupMaxRows
	}
	if rows >= maxRows {
		return true
	}
	return g.targetBytes > 0 && bytes >= g.targetBytes
}

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

// buildColumnarFromSegment transcodes a segment's live-arena records in [0, upto) into a series of
// columnar ROW-GROUP blocks of groupRows records each, mirroring buildSegIndex: it walks the
// records, decompresses each (skipping time-travel markers), re-encodes it in the adSchema row
// form, and seals a block every groupRows records. It returns the blocks in record order and offs,
// where offs[k] is the segment offset of the k'th record COUNTING ACROSS BLOCKS -- the map a scan
// uses to recover each record's MVCC seq/sup (for visibility) and its key. Reads immutable segment
// bytes only.
//
// Encoding a group as soon as it fills also bounds peak transcode memory at one group's row
// records instead of the whole segment's, which for a 1 GiB segment was the same order as the
// segment itself.
//
// toInterned canonicalizes a decompressed record into interned wire (identity for an already-
// interned collection; a decode/re-encode for an inline/persistent one). encode reads the
// id-keyed form, so a non-interned record must be converted first or the block would be empty.
func buildColumnarFromSegment(data []byte, upto int, codec Codec, s *adSchema, hot []int, g colGrouping, toInterned func(dst, w []byte) ([]byte, bool)) ([]*columnarBlock, []uint32) {
	var blocks []*columnarBlock
	var recs [][]byte
	var offs []uint32
	var buf []byte
	groupBytes := 0
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
					rec := s.encode(wire.Ad(iw))
					recs = append(recs, rec)
					offs = append(offs, o)
					groupBytes += len(rec)
					if g.full(len(recs), groupBytes) {
						blocks = append(blocks, encodeColumnarBlock(s, recs, hot, codec))
						recs, groupBytes = recs[:0], 0
					}
				}
			}
		}
		off += int(total)
	}
	if len(recs) > 0 {
		blocks = append(blocks, encodeColumnarBlock(s, recs, hot, codec)) // short final group
	}
	if len(blocks) == 0 {
		// A segment with no encodable records still gets one empty block, so a colSegment always
		// carries the schema it was built under (which persistence and the scan's field resolution
		// both read from the first block).
		blocks = append(blocks, encodeColumnarBlock(s, nil, hot, codec))
	}
	return blocks, offs
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
	coldNum, err := bc.stream(b, kindColdNum)
	if err != nil {
		return err
	}
	for k := 0; k < b.n; k++ {
		if testBit(b.escapeAt(k), fieldIdx) {
			fn(k, false, 0)
			continue
		}
		fn(k, true, readIntLE(coldNum[start+k*f.width:], f.width, f.unsigned))
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
	tail, err := bc.stream(b, kindCold)
	if err != nil {
		return 0, false, err
	}
	cold := tail[b.coldOff[k]:b.coldOff[k+1]]
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

// escapedNumVal is escapedNumField returning the value WITH its ClassAd kind. A caller that
// renders or sums the value needs the kind, and the cold tail carries the wire node, so it is read
// from the node rather than assumed from the segment's schema (an escaped value is by definition
// one the schema field did not fit).
func (b *columnarBlock) escapedNumVal(k int, fieldID uint32, bc *blockCache) (colVal, bool) {
	tail, err := bc.stream(b, kindCold)
	if err != nil {
		return colVal{}, false
	}
	cold := tail[b.coldOff[k]:b.coldOff[k+1]]
	for len(cold) > 0 {
		id, m := binary.Uvarint(cold)
		if m <= 0 {
			return colVal{}, false // malformed tail: treat fieldID as absent
		}
		cold = cold[m:]
		nl, ok := wire.NodeLen(cold)
		if !ok || nl > len(cold) {
			return colVal{}, false
		}
		if uint32(id) == fieldID {
			return nodeColVal(cold[:nl])
		}
		cold = cold[nl:]
	}
	return colVal{}, false
}
