package collections

import (
	"encoding/binary"
	"errors"
	"math"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/collections/wire"
)

var errNotNumericField = errors.New("adschema: scanInt on a non-numeric field")

// errBadColBlock reports a block whose regions do not agree with its metadata -- a truncated string region,
// a code past the end of its dictionary. Derived state, so a caller can rebuild rather than fail the query.
var errBadColBlock = errors.New("adschema: columnar block region inconsistent with its schema")

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

	// colGroupAlign is the row-count multiple a byte-sealed row group is rounded UP to.
	//
	// Eight int64s are one cache line and one AVX-512 vector, and eight booleans are one byte of the
	// bool bitset, so an aligned block lets a column kernel run whole vectors with no tail. The value of
	// aligning is not the handful of records it shifts between blocks -- it is that a kernel's tail
	// handling then runs once per SEGMENT rather than once per block, because only a segment's FINAL
	// group is short. That last group cannot be aligned: it holds whatever records remain, and padding it
	// would mean storing records that do not exist.
	//
	// Applied to the byte rule only. An explicit maxRows policy seals exactly, so a test sweeping group
	// sizes still gets the size it asked for, and neither does an ad so large that the budget trips
	// within the first colGroupAlign records.
	colGroupAlign = 8

	// colGroupRows is the reference point the measurements above were taken at: what
	// colGroupTargetBytes works out to for OSPool slot ads (~8867 B/ad of regions). Kept as the
	// documented equivalence, and used by tests that need an exact row-count grouping.
	colGroupRows = 128
)

// colGrouping is the row-group sealing policy: seal a group once its records reach targetBytes of
// uncompressed row form, or maxRows records, whichever comes first. Where the byte rule trips, the block
// is sealed at the last colGroupAlign boundary that still fits the budget (see sealAt).
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

// sealAt returns how many of the group's rows to seal into a block now, given the row count and bytes
// that tripped full(), plus the row count and bytes as of the last colGroupAlign boundary.
//
// Alignment is a choice of SEAL POINT, not of when to notice the budget. Rounding UP to the next
// boundary would be simpler and is wrong: it puts the block OVER the byte budget by up to
// colGroupAlign-1 records, which for a 1 MiB budget and ordinary ads is a couple of percent but for a
// single pathological half-megabyte ad is several times over -- and the budget exists precisely so that
// no block is too big to cache. Sealing the last aligned PREFIX and carrying the remainder into the next
// group keeps both properties.
//
// Returns len(rows) when there is no aligned prefix to fall back to: an explicit maxRows policy (which
// seals exactly, so a test gets the group size it asked for), and ads so large that the budget trips
// within the first colGroupAlign records.
func (g colGrouping) sealAt(rows, bytes, alignedRows, alignedBytes int) (seal, sealBytes int) {
	if g.targetBytes <= 0 || bytes <= g.targetBytes {
		return rows, bytes // the row cap tripped, or the byte rule tripped exactly on budget
	}
	if alignedRows <= 0 || alignedRows >= rows {
		return rows, bytes
	}
	// Backing off to the aligned prefix must not produce a runt. Record sizes vary by orders of
	// magnitude -- a few small ads followed by one enormous one will trip the budget a record or two past
	// a boundary, and sealing the prefix would then emit a block holding almost nothing while the ad that
	// actually filled the budget carries forward. Below the floor, take the aligned-but-over block
	// instead: the overshoot is one record's worth and is caused by that record, which no split avoids.
	if alignedBytes < g.minGroupBytes() {
		return rows, bytes
	}
	return alignedRows, alignedBytes
}

// minGroupBytes is the smallest block the aligned-prefix back-off may produce, as a fraction of the byte
// budget rather than an absolute, so a test configuring a small budget still gets a proportionate floor.
func (g colGrouping) minGroupBytes() int { return g.targetBytes / 2 }

// columnarBlock is the PAX layout for a row-group of adSchema records (see adschema.go): the
// schema's popular ("hot") numeric fields plus the escape and bool bitsets stay uncompressed
// in a fixed-stride region (so an attribute scan reads a column with no decode), while the
// cold numeric fields (columnar), the string regions, and the cold tails are each a separately
// Codec-compressed stream. A numeric scan on a hot field touches only the uncompressed region;
// a scan on a cold field decompresses just that column group; a full-ad reconstruct
// decompresses the string + cold streams. This is the sealed-segment form; the active segment
// stays row-based (per-record).
//
// Uncompressed per record (stride = bitsStride): [escape bitmap][bool bitset]. Hot numerics and cold
// numerics are both COLUMNAR -- each field's values contiguous across the block -- differing only in that
// the cold ones are compressed. String/cold streams are per-record regions concatenated, addressed by
// strOff/coldOff.
type columnarBlock struct {
	id     uint64 // process-unique, the block-cache key
	schema *adSchema
	codec  Codec // the REGION codec (dictionary id 0), not the segment's record codec
	n      int

	// The uncompressed region, split by what each part is indexed BY. The bitsets are per-record -- one
	// bit per field -- so they stay row-major; the numerics are per-field, so they are columnar, each
	// field's values contiguous across the block exactly as the cold ones are.
	//
	// They shared one row-major region until v5, which made a hot column STRIDED. That costs nothing for
	// a scalar read -- a constant stride is what a prefetcher is for, and the dominant traffic is the
	// widening store into the caller's int64 buffer either way, both measured at 1.00x -- but a SIMD load
	// cannot address a stride AT ALL. This is a capability change, not a speed one on its own.
	bits        []byte // escape bitmap + bool bitset, bitsStride bytes per record
	bitsStride  int
	hotNum      []int       // hot int/real field indices (into schema.fields), layout order
	hotCol      []byte      // hot int/real fields, COLUMNAR: field i's values contiguous
	hotColStart map[int]int // field idx -> start offset in hotCol

	coldNum        []int       // cold int/real field indices, layout order
	coldFieldStart map[int]int // field idx -> start offset in the decompressed cold-numeric buffer
	coldNumComp    []byte      // cold numeric fields, columnar, Codec-compressed

	strComp, coldComp []byte // per-record string regions / cold tails, concatenated + compressed
	strOff, coldOff   []int  // per-record cumulative offsets into the decompressed streams

	// Per-field string dictionaries and their code columns (see strdict.go): a string predicate compares
	// codes instead of walking the positional string region. Empty when no string field in this block had
	// values repetitive enough to be worth one.
	strDict     map[int]strDictField
	strDictComp []byte // the distinct values, fold-sorted, compressed
	strCodeComp []byte // the code columns, columnar, compressed

	// zones is the per-field [min,max] of this block's NUMERIC columns, keyed by schema field index,
	// so a scan can skip the whole block when no value in it could satisfy a predicate. It is the
	// row-group equivalent of the per-segment zone map, and row groups are what make it worth having:
	// a segment-wide range over a 1 GiB segment rules out almost nothing, while a ~1 MiB group's does.
	//
	// Computed while encoding, where the values are already in hand -- a separate pass would cost the
	// scan it is meant to save.
	zones map[int]blockZone

	// escClass says, per schema field, whether this block's escapes of it are all MISSING (the
	// attribute is absent, so the escape bit proves it undefined), all EXCEPTIONAL (present but
	// out of slot, so always in the cold tail), or mixed. escExcRecs lists the exceptional records
	// of the mixed fields only. See colescclass.go for why this is per field rather than a second
	// per-record bitmap.
	escClass   []uint8
	escExcRecs map[int][]uint32
	// escAbsent has a bit per schema field, set when NO record in this block carries it -- an exact
	// whole-block proof that the attribute is undefined here.
	escAbsent []byte
}

// blockZone is one numeric column's range within a block.
//
// escaped matters for correctness, not for speed: min/max cover only values stored in the column, so
// if any record in the block has this field ESCAPED (missing, wrong type, or too wide) its value is
// not represented here and the range cannot rule the block out. A too-wide value in particular is by
// definition outside the column's range, so pruning on an inexact zone would drop records that match.
type blockZone struct {
	zoneRange
	escaped bool
}

// encodeColumnarBlock builds a columnar block from row-form records (each the output of
// adSchema.encode). hotNumFields lists the schema field indices (int/real) to keep uncompressed
// -- the popular ones, by query demand. Bools and the escape bitmap are always in the hot
// region (tiny, and bools may be scanned).
// layoutColumnar fills b's hot/cold field partition and their offsets from the schema and hot
// set: hot numerics columnar and uncompressed, cold numerics
// columnar (each field's n values contiguous). Deterministic in (schema, hotSet, n), so the
// persisted-block decoder reproduces the exact layout.
func layoutColumnar(b *columnarBlock, s *adSchema, hotNumFields []int, n int) {
	hotSet := make(map[int]bool, len(hotNumFields))
	for _, i := range hotNumFields {
		hotSet[i] = true
	}
	b.hotNum, b.coldNum = b.hotNum[:0], b.coldNum[:0]
	b.hotColStart, b.coldFieldStart = map[int]int{}, map[int]int{}
	for i := range s.fields {
		if k := s.fields[i].kind; k == akInt || k == akReal {
			if hotSet[i] {
				b.hotNum = append(b.hotNum, i)
			} else {
				b.coldNum = append(b.coldNum, i)
			}
		}
	}
	b.bitsStride = s.escBytes + s.boolBytes
	hp := 0
	for _, i := range b.hotNum {
		b.hotColStart[i] = hp
		hp += s.fields[i].width * n
	}
	cp := 0
	for _, i := range b.coldNum {
		b.coldFieldStart[i] = cp
		cp += s.fields[i].width * n
	}
}

func encodeColumnarBlock(s *adSchema, recs [][]byte, hotNumFields []int, regionCodec Codec) *columnarBlock {
	b := &columnarBlock{id: colBlockSeq.Add(1), schema: s, codec: regionCodec, n: len(recs)}
	layoutColumnar(b, s, hotNumFields, len(recs))
	cp := 0
	for _, i := range b.coldNum {
		cp += s.fields[i].width * len(recs)
	}
	hp := 0
	for _, i := range b.hotNum {
		hp += s.fields[i].width * len(recs)
	}
	b.bits = make([]byte, 0, b.bitsStride*len(recs))
	b.hotCol = make([]byte, hp)
	coldRaw := make([]byte, cp)
	// Decided BEFORE the record loop, because the positional region omits whatever the dictionary owns and
	// the loop is what writes that region.
	dicts, dictRaw, codeRaw := buildStrDicts(s, recs, len(recs))
	var strCat, coldCat []byte
	b.strOff = []int{0}
	b.coldOff = []int{0}
	for k, r := range recs {
		b.bits = append(b.bits, r[:s.escBytes+s.boolBytes]...) // escape bitmap then bool bitset
		for _, i := range b.hotNum {
			off := s.escBytes + s.fields[i].off
			copy(b.hotCol[b.hotColStart[i]+k*s.fields[i].width:], r[off:off+s.fields[i].width])
		}
		for _, i := range b.coldNum {
			off := s.escBytes + s.fields[i].off
			copy(coldRaw[b.coldFieldStart[i]+k*s.fields[i].width:], r[off:off+s.fields[i].width])
		}
		_, str, cold := s.splitRecord(r)
		if len(dicts) == 0 {
			strCat = append(strCat, str...)
		} else {
			var ok bool
			if strCat, ok = appendNonDictStrings(s, r, dicts, strCat); !ok {
				strCat = append(strCat, str...) // malformed record: keep it whole rather than lose it
			}
		}
		coldCat = append(coldCat, cold...)
		b.strOff = append(b.strOff, len(strCat))
		b.coldOff = append(b.coldOff, len(coldCat))
	}
	b.zones = numericZones(s, b, recs)
	b.escClass, b.escExcRecs, b.escAbsent = classifyEscapes(s, recs)
	if dicts != nil {
		b.strDict = dicts
		b.strDictComp = regionCodec.Compress(nil, dictRaw)
		b.strCodeComp = regionCodec.Compress(nil, codeRaw)
	}
	b.coldNumComp = regionCodec.Compress(nil, coldRaw)
	b.strComp = regionCodec.Compress(nil, strCat)
	b.coldComp = regionCodec.Compress(nil, coldCat)
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
// The two codecs are deliberately different. arenaCodec decompresses the stored records and must be
// the segment's own codec (whatever dictionary they were written under); regionCodec compresses the
// block's regions and is the dictionary-less base codec (see Collection.regionCodec).
//
// toInterned canonicalizes a decompressed record into interned wire (identity for an already-
// interned collection; a decode/re-encode for an inline/persistent one). encode reads the
// id-keyed form, so a non-interned record must be converted first or the block would be empty.
func buildColumnarFromSegment(data []byte, upto int, arenaCodec, regionCodec Codec, s *adSchema, hot []int, g colGrouping, toInterned func(dst, w []byte) ([]byte, bool)) ([]*columnarBlock, []uint32) {
	blocks, _, offs := buildColumnarFromSegmentGrouped(data, upto, arenaCodec, regionCodec, s, hot, nil, g, toInterned)
	return blocks, offs
}

// buildColumnarFromSegmentGrouped is buildColumnarFromSegment plus the group schemas' selections.
// groups may be nil, which reproduces the ungrouped build exactly.
//
// It keeps each pending record's INTERNED WIRE alongside its base row, because group membership is
// a question the base row can no longer answer (see buildGroupBlocks). The retained wire is bounded
// by the same group budget that bounds the pending rows.
func buildColumnarFromSegmentGrouped(data []byte, upto int, arenaCodec, regionCodec Codec, s *adSchema, hot []int, groups []*colGroup, g colGrouping, toInterned func(dst, w []byte) ([]byte, bool)) ([]*columnarBlock, [][]*colGroupBlock, []uint32) {
	var blocks []*columnarBlock
	var groupBlocks [][]*colGroupBlock
	var recs [][]byte
	var iws [][]byte
	var offs []uint32
	var buf []byte
	groupBytes := 0
	// The group's extent as of the last colGroupAlign boundary, so an over-budget group can be sealed
	// there rather than past it. See sealAt.
	alignedRows, alignedBytes := 0, 0
	for off := 0; off < upto; {
		o := uint32(off)
		total := recTotalLen(data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(data, o) {
			if w, err := arenaCodec.Decompress(buf[:0], recAd(data, o)); err == nil {
				buf = w
				if iw, ok := toInterned(nil, w); ok {
					// A record that holds a whole group has those attributes stored by the
					// group's column, so they are left out of the base row entirely rather
					// than written to its cold tail as a second copy.
					var skip map[uint32]struct{}
					if len(groups) > 0 {
						skip = groupSkipSet(groups, iw)
					}
					rec := s.encodeExcept(wire.Ad(iw), skip)
					recs = append(recs, rec)
					if len(groups) > 0 {
						iws = append(iws, append([]byte(nil), iw...))
					}
					offs = append(offs, o)
					groupBytes += len(rec)
					if len(recs)%colGroupAlign == 0 {
						alignedRows, alignedBytes = len(recs), groupBytes
					}
					if g.full(len(recs), groupBytes) {
						seal, sealBytes := g.sealAt(len(recs), groupBytes, alignedRows, alignedBytes)
						blocks = append(blocks, encodeColumnarBlock(s, recs[:seal], hot, regionCodec))
						if len(groups) > 0 {
							// Slice the retained wire at the SAME point, so a group's membership
							// bitmap is indexed by the base block's own record numbering.
							groupBlocks = append(groupBlocks, buildGroupBlocks(groups, iws[:seal], regionCodec))
							m := copy(iws, iws[seal:])
							iws = iws[:m]
						}
						// Carry the records past the seal point into the next group. encodeColumnarBlock
						// has already copied what it needs out of the record slices, so moving their
						// headers down is safe; offs is per SEGMENT and is not reset.
						n := copy(recs, recs[seal:])
						recs, groupBytes = recs[:n], groupBytes-sealBytes
						alignedRows, alignedBytes = 0, 0
						if n > 0 && n%colGroupAlign == 0 {
							alignedRows, alignedBytes = n, groupBytes
						}
					}
				}
			}
		}
		off += int(total)
	}
	if len(recs) > 0 {
		blocks = append(blocks, encodeColumnarBlock(s, recs, hot, regionCodec)) // short final group
		if len(groups) > 0 {
			groupBlocks = append(groupBlocks, buildGroupBlocks(groups, iws, regionCodec))
		}
	}
	if len(blocks) == 0 {
		// A segment with no encodable records still gets one empty block, so a colSegment always
		// carries the schema it was built under (which persistence and the scan's field resolution
		// both read from the first block).
		blocks = append(blocks, encodeColumnarBlock(s, nil, hot, regionCodec))
	}
	return blocks, groupBlocks, offs
}

// numericZones computes each numeric field's [min,max] over the records being encoded, and whether
// any record escaped that field. Reads the row-form records directly, since encodeColumnarBlock has
// them in hand.
func numericZones(s *adSchema, b *columnarBlock, recs [][]byte) map[int]blockZone {
	if len(recs) == 0 {
		return nil
	}
	out := make(map[int]blockZone, len(b.hotNum)+len(b.coldNum))
	for _, idx := range append(append([]int(nil), b.hotNum...), b.coldNum...) {
		f := s.fields[idx]
		z := blockZone{zoneRange: zoneRange{Min: math.Inf(1), Max: math.Inf(-1)}}
		n := 0
		for _, r := range recs {
			if testBit(r[:s.escBytes], idx) {
				z.escaped = true
				continue
			}
			raw := readIntLE(r[s.escBytes+f.off:], f.width, f.unsigned)
			v := float64(raw)
			if f.kind == akReal {
				v = math.Float64frombits(uint64(raw))
			}
			if v < z.Min {
				z.Min = v
			}
			if v > z.Max {
				z.Max = v
			}
			n++
		}
		if n == 0 {
			// Every record escaped: there is no range, and the block can never be pruned on it.
			z.Min, z.Max, z.escaped = math.Inf(-1), math.Inf(1), true
		}
		out[idx] = z
	}
	return out
}

// mayMatch reports whether this block could hold a record satisfying every test on field idx. A block
// with no zone for the field, or an inexact one (some record escaped), is never ruled out.
func (b *columnarBlock) mayMatch(idx int, tests []zoneTest) bool {
	z, ok := b.zones[idx]
	if !ok || z.escaped {
		return true
	}
	for _, t := range tests {
		if !zoneMayMatch(z.zoneRange, t.op, t.vals) {
			return false
		}
	}
	return true
}

// escapeFree reports whether NO record in this block escaped fieldIdx, so a reader can skip the
// per-record escape test entirely.
//
// The answer is already recorded: numericZones sets blockZone.escaped while encoding, for the zone
// pruning that came with row groups. Spending it here costs nothing and removes a slice construction
// plus a bit test from every value read. Only numeric fields carry a zone, which is where the hot
// scans are.
func (b *columnarBlock) escapeFree(fieldIdx int) bool {
	z, ok := b.zones[fieldIdx]
	return ok && !z.escaped
}

// loadIntBatch fills dst[0:b.n] with a column's raw slot values, or reports false when the field is
// not a numeric column here.
//
// It does NOT require the block to be escape-free, and that is deliberate. An earlier version bailed
// when any record had escaped, which sounds cheap and is not: with ~1000-record row groups, even a
// 0.1% escape rate leaves only about a third of blocks eligible, and 1% leaves almost none -- so the
// fast path would essentially never fire on real data. Instead the load runs unconditionally and the
// CALLER checks the escape bit as it walks, reading those few records from the cold tail. Escaped
// slots hold undefined bytes, so a caller must not use dst[k] without that check (see escapeFree,
// which lets it skip the check entirely when the block has none).
//
// This is the batch form of scanInt, and it is where the measured cost actually was. Removing the
// per-record escape test and readIntLE's byte-at-a-time read left the callback itself as the
// bottleneck: a closure call per record. Separating "load the column" from "apply the predicate" lets
// both be tight loops over contiguous memory, which is what the vectorization spike measured as the
// win -- applied to the paths that already exist rather than to a new engine.
func (b *columnarBlock) loadIntBatch(fieldIdx int, bc *blockCache, dst []int64) bool {
	f := b.schema.fields[fieldIdx]
	if !numericKind(f.kind) {
		return false
	}
	if start, hot := b.hotColStart[fieldIdx]; hot {
		// Contiguous, so the stride is the field width -- the same call the cold path makes.
		loadIntsTyped(dst, b.hotCol[start:], f.width, 0, f.width, f.unsigned, b.n)
		return true
	}
	start, ok := b.coldFieldStart[fieldIdx]
	if !ok {
		return false
	}
	coldNum, err := bc.stream(b, kindColdNum)
	if err != nil {
		return false
	}
	loadIntsTyped(dst, coldNum[start:], f.width, 0, f.width, f.unsigned, b.n)
	return true
}

// loadIntsTyped fills dst from a strided column with the width decision hoisted OUT of the loop.
//
// readIntLE reads a value byte at a time and then branches to sign-extend, so a generic loop pays a
// loop and a branch per VALUE. A column has one width for the whole block by construction -- widths
// are per-field and fitted by chooseIntWidth -- so the switch belongs outside, and each case is a
// single machine load.
func loadIntsTyped(dst []int64, src []byte, stride, off, width int, unsigned bool, n int) {
	switch {
	case width == 1 && unsigned:
		for k := 0; k < n; k++ {
			dst[k] = int64(src[k*stride+off])
		}
	case width == 1:
		for k := 0; k < n; k++ {
			dst[k] = int64(int8(src[k*stride+off]))
		}
	case width == 2 && unsigned:
		for k := 0; k < n; k++ {
			dst[k] = int64(binary.LittleEndian.Uint16(src[k*stride+off:]))
		}
	case width == 2:
		for k := 0; k < n; k++ {
			dst[k] = int64(int16(binary.LittleEndian.Uint16(src[k*stride+off:])))
		}
	case width == 4 && unsigned:
		for k := 0; k < n; k++ {
			dst[k] = int64(binary.LittleEndian.Uint32(src[k*stride+off:]))
		}
	case width == 4:
		for k := 0; k < n; k++ {
			dst[k] = int64(int32(binary.LittleEndian.Uint32(src[k*stride+off:])))
		}
	case width == 8:
		for k := 0; k < n; k++ {
			dst[k] = int64(binary.LittleEndian.Uint64(src[k*stride+off:]))
		}
	default: // a 6-byte or otherwise unusual fitted width
		for k := 0; k < n; k++ {
			dst[k] = readIntLE(src[k*stride+off:], width, unsigned)
		}
	}
}

// rawColumn returns a numeric field's values as the contiguous bytes the block stores, at their stored width.
//
// Both numeric regions are columnar, so this is a subslice either way -- for a hot field, a span of the
// uncompressed region and therefore of the mmap. Handing that to a comparison kernel is what makes a
// width-native compare possible: 8 lanes of a 128-bit vector at a 2-byte width where int64 gives 2.
func (b *columnarBlock) rawColumn(fieldIdx int, bc *blockCache) ([]byte, bool) {
	f := b.schema.fields[fieldIdx]
	if !numericKind(f.kind) {
		return nil, false
	}
	need := b.n * f.width
	if start, hot := b.hotColStart[fieldIdx]; hot {
		if start+need > len(b.hotCol) {
			return nil, false
		}
		return b.hotCol[start : start+need], true
	}
	start, ok := b.coldFieldStart[fieldIdx]
	if !ok {
		return nil, false
	}
	coldNum, err := bc.stream(b, kindColdNum)
	if err != nil || start+need > len(coldNum) {
		return nil, false
	}
	return coldNum[start : start+need], true
}

// scanInt calls fn for each record's value of a numeric (int/real read as int bits) field: a
// hot field reads the uncompressed region directly (no decode); a cold field decompresses its
// column group once (via bc, nil for no cache). present is false for a missing/exceptional
// record (its value is in the cold tail).
func (b *columnarBlock) scanInt(fieldIdx int, bc *blockCache, fn func(rec int, present bool, v int64)) error {
	f := b.schema.fields[fieldIdx]
	if start, hot := b.hotColStart[fieldIdx]; hot {
		for k := 0; k < b.n; k++ {
			if testBit(b.escapeAt(k), fieldIdx) {
				fn(k, false, 0)
				continue
			}
			fn(k, true, readIntLE(b.hotCol[start+k*f.width:], f.width, f.unsigned))
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

// appendStrings rebuilds record k's full positional string region, interleaving the fields the dictionary
// owns with the ones still stored positionally -- in schema order, which is the order a reader expects.
func (b *columnarBlock) appendStrings(rec []byte, k int, strRaw []byte, bc *blockCache) ([]byte, error) {
	s := b.schema
	esc := b.escapeAt(k)
	pos := strRaw[b.strOff[k]:b.strOff[k+1]]
	for i := range s.fields {
		if s.fields[i].kind != akString || testBit(esc, i) {
			continue
		}
		if !b.dictOwns(i) {
			l, m := binary.Uvarint(pos)
			if m <= 0 || m+int(l) > len(pos) {
				return nil, errBadColBlock
			}
			rec = append(rec, pos[:m+int(l)]...)
			pos = pos[m+int(l):]
			continue
		}
		var buf [][]byte
		entries, ok := b.dictEntries(i, bc, &buf)
		if !ok {
			return nil, errBadColBlock
		}
		codes, w, ok := b.dictCodes(i, bc)
		if !ok {
			return nil, errBadColBlock
		}
		c := dictCodeAt(codes, w, k)
		if c >= len(entries) {
			return nil, errBadColBlock
		}
		rec = binary.AppendUvarint(rec, uint64(len(entries[c])))
		rec = append(rec, entries[c]...)
	}
	return rec, nil
}

// escapeAt returns record k's escape bitmap, from the per-record bitsets region.
func (b *columnarBlock) escapeAt(k int) []byte {
	base := k * b.bitsStride
	return b.bits[base : base+b.schema.escBytes]
}

// reconstruct rebuilds record k's row form (identical to the adSchema.encode input), so
// schema.forEach reconstructs the full ad. Decompresses the cold-numeric, string, and cold
// streams (via bc, nil for no cache) -- the full-ad-read cost, amortized by the cache.
// reconstruct is reconstructInto with a fresh buffer, for callers not on a hot path.
func (b *columnarBlock) reconstruct(k int, bc *blockCache) ([]byte, error) {
	return b.reconstructInto(nil, k, bc)
}

// reconstructInto rebuilds record k's canonical record form into dst's backing array where it
// fits, returning the result.
//
// dst exists because this is called once per record on every columnar full-ad read, and a fresh
// allocation per record puts the whole scan's working set through the garbage collector -- the
// profile of a columnar scan was dominated by page management rather than by any of the work
// here. The caller retains the returned slice for the next record.
func (b *columnarBlock) reconstructInto(dst []byte, k int, bc *blockCache) ([]byte, error) {
	s := b.schema
	ds, err := bc.streams(b)
	if err != nil {
		return nil, err
	}
	coldRaw, strRaw, tailRaw := ds.coldNum, ds.str, ds.cold
	need := s.escBytes + s.fixedLen + (b.strOff[k+1] - b.strOff[k]) + (b.coldOff[k+1] - b.coldOff[k])
	if cap(dst) < need {
		dst = make([]byte, 0, need)
	}
	rec := dst[:s.escBytes+s.fixedLen]
	clear(rec)
	base := k * b.bitsStride
	copy(rec[:s.escBytes+s.boolBytes], b.bits[base:base+s.escBytes+s.boolBytes]) // escape then bools
	// The cost of a columnar hot region: one gather per field rather than one span. Identical in shape to
	// the cold loop below, which has always done this.
	for _, i := range b.hotNum {
		w := s.fields[i].width
		st := b.hotColStart[i] + k*w
		copy(rec[s.escBytes+s.fields[i].off:], b.hotCol[st:st+w])
	}
	for _, i := range b.coldNum {
		w := s.fields[i].width
		copy(rec[s.escBytes+s.fields[i].off:], coldRaw[b.coldFieldStart[i]+k*w:b.coldFieldStart[i]+k*w+w])
	}
	// The positional region omits whatever a dictionary owns, so rebuilding the canonical record form means
	// walking the schema's string fields in order and taking each from whichever side holds it.
	if len(b.strDict) == 0 {
		rec = append(rec, strRaw[b.strOff[k]:b.strOff[k+1]]...)
	} else {
		var err error
		if rec, err = b.appendStrings(rec, k, strRaw, bc); err != nil {
			return nil, err
		}
	}
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

// escapedNode returns the raw wire node an ESCAPED field holds in record k's cold tail, without
// interpreting it. found=false means the attribute is not in the tail at all, i.e. genuinely absent
// from the record.
//
// Presence needs the node rather than a value: whether an attribute is UNDEFINED depends on which
// literal it is (or on evaluating it, if it is an expression), and a value-returning accessor throws
// exactly that away. Reads only this field, not the whole record.
func (b *columnarBlock) escapedNode(k int, fieldID uint32, bc *blockCache) (node []byte, found bool, err error) {
	tail, err := bc.stream(b, kindCold)
	if err != nil {
		return nil, false, err
	}
	cold := tail[b.coldOff[k]:b.coldOff[k+1]]
	for len(cold) > 0 {
		id, m := binary.Uvarint(cold)
		if m <= 0 {
			return nil, false, nil // malformed tail: treat fieldID as absent
		}
		cold = cold[m:]
		nl, ok := wire.NodeLen(cold)
		if !ok || nl > len(cold) {
			return nil, false, nil
		}
		if uint32(id) == fieldID {
			return cold[:nl], true, nil
		}
		cold = cold[nl:]
	}
	return nil, false, nil
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
