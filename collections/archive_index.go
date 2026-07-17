package collections

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/RoaringBitmap/roaring/v2"
)

// Sidecar index file format v2 (per sealed segment, "seg-<id>.idx"). It serializes
// the immutable segIndex as sorted key runs, so a query reads it directly from an
// mmap — binary search for equality, boundary scan for range — without ever
// materializing a per-value map in RAM (see mmapSegIndex). Only the bitmap postings
// a query actually touches are paged in. All integers are little-endian.
//
//	[0:4]    magic   = 'A','R','C','X'
//	[4:6]    version = 2
//	[6:10]   metaOff uint32     -> offset of the meta section
//	[10:..]  bitmaps region: a run of { len uint32; roaring bytes } blocks, each
//	         referenced by its absolute file offset (FromBuffer-able zero-copy).
//	[metaOff:] meta:
//	         upto uint32; specGen uint64; allOff uint32
//	         catN uint32; catN × { id uint32; attrOff uint32 }
//	         valN uint32; valN × { id uint32; attrOff uint32 }
//	  cat attr block @attrOff: excOff uint32; postedOff uint32; n uint32;
//	         keyOff [n+1] uint32 (delimit sorted folded keys in keysBlob); bmOff [n] uint32;
//	         keysBlob;
//	         exactN uint32; exactKeyOff [exactN+1] uint32; exactBmOff [exactN] uint32;
//	         exactKeysBlob   -- exact-case keys (sorted) for =?=/=!=; a case-uniform
//	         bucket's exact key reuses its folded bmOff (no duplicate payload);
//	         bloomK uint32; bloomM uint32; bloom [bloomM/64] uint64  (v5) -- categorical
//	         membership filter over the folded keys. A miss on every probe value lets a
//	         query skip binary-searching (and paging) the sorted key blob for that probe;
//	         bloomM==0 means no filter (empty index);
//	         mphBlockLen uint32; [MPH bytes]; slotSortedIdx [nAssigned] uint32  (v6) --
//	         a minimal perfect hash over the folded keys for O(1) equality on a
//	         high-cardinality attr (built only when NDV >= mphNDVThreshold; mphBlockLen==0
//	         means none). mphLookupBytes -> slot -> slotSortedIdx[slot] indexes the folded
//	         keyOff/bmOff arrays; the caller verifies the key and falls back to binary
//	         search, so the MPH is a fast path, never authoritative.
//	  val attr block @attrOff: excOff uint32; postedOff uint32; n uint32;
//	         key [n] float64 (sorted asc); bmOff [n] uint32
//
// postedOff is the bitmap of records that posted a literal value, for the presence
// probes: present (isnt undefined) = posted OR exc, absent (is undefined) = all AND-NOT
// posted. v3 added the exact-case run; v4 added postedOff; v5 added the categorical
// bloom. There is no migration (indexes are rebuilt at seal), so an older sidecar is
// simply rejected and reindexed.
const (
	sidecarMagic   = 0x41524358 // "ARCX"
	sidecarVersion = 8          // v8 appended a CRC-32C footer over the whole sidecar

	// mphNDVThreshold gates the categorical minimal-perfect-hash: only an attribute with
	// at least this many distinct values in a segment gets one. Below it, the sorted key
	// blob is small enough that binary search (two or three comparisons, one or two pages)
	// plus the bloom already resolve equality cheaply, and the MPH's per-key overhead would
	// not pay for itself.
	mphNDVThreshold = 4096
)

// writeSidecarIndex serializes si to path (v2 sorted runs) and fsyncs it. si is
// built transiently at seal and discarded after — the archive never retains an
// in-RAM index; queries mmap the sidecar (mmapSegIndex).
func writeSidecarIndex(path string, si *segIndex) error {
	b, err := buildSidecarIndex(si)
	if err != nil {
		return err
	}
	return writeFileSync(path, b)
}

// buildSidecarIndex serializes si to the sidecar byte layout (documented on
// writeSidecarIndex). The bytes back either a file mmap (a persistent sealed segment) or an
// anonymous region (an in-memory one), so the same immutable index representation serves
// both tiers.
func buildSidecarIndex(si *segIndex) ([]byte, error) {
	b := make([]byte, 0, 256)
	b = appendU32(b, sidecarMagic)
	b = appendU16(b, sidecarVersion)
	metaOffPos := len(b)
	b = appendU32(b, 0) // metaOff placeholder; bitmaps region begins here (offset 10)

	// emit appends a length-prefixed bitmap to the bitmaps region and returns its
	// absolute file offset.
	emit := func(bm *roaring.Bitmap) (uint32, error) {
		if bm == nil {
			bm = roaring.New()
		}
		p, err := bm.MarshalBinary()
		if err != nil {
			return 0, err
		}
		off := uint32(len(b))
		b = appendBytes(b, p)
		return off, nil
	}

	allOff, err := emit(si.all)
	if err != nil {
		return nil, err
	}

	type catBlk struct {
		id, excOff, postedOff uint32
		keys                  []string // folded keys (sorted) -> bmOffs
		bmOffs                []uint32
		exKeys                []string // exact-case keys (sorted) -> exBmOffs (for =?=/=!=)
		exBmOffs              []uint32
		stats                 *segStats // full summary (bloom, min/max, top-N, ndv, HLL)
	}
	type valBlk struct {
		id, excOff, postedOff uint32
		keys                  []float64
		bmOffs                []uint32
		stats                 *segStats
	}
	var catBlks []catBlk
	for id, cp := range si.cat {
		keys := make([]string, 0, len(cp.post))
		for k := range cp.post {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		bmOffs := make([]uint32, len(keys))
		foldedOff := make(map[string]uint32, len(keys))
		for i, k := range keys {
			if bmOffs[i], err = emit(cp.post[k]); err != nil {
				return nil, err
			}
			foldedOff[k] = bmOffs[i]
		}
		// Exact-case run for =?=/=!=. A case-uniform bucket (in exactCase) reuses its
		// folded bitmap offset -- no duplicate payload on disk; a mixed-case bucket
		// emits one bitmap per exact spelling. Group the retained exact spellings by
		// their folded key so each folded bucket contributes its exact entries.
		exByFold := map[string][]string{}
		for e := range cp.exact {
			exByFold[strings.ToLower(e)] = append(exByFold[strings.ToLower(e)], e)
		}
		type exPair struct {
			key string
			off uint32
		}
		var exPairs []exPair
		for _, f := range keys {
			if ec, ok := cp.exactCase[f]; ok {
				spelling := f
				if ec != "" {
					spelling = ec
				}
				exPairs = append(exPairs, exPair{spelling, foldedOff[f]})
				continue
			}
			for _, e := range exByFold[f] {
				off, emitErr := emit(cp.exact[e])
				if emitErr != nil {
					return nil, emitErr
				}
				exPairs = append(exPairs, exPair{e, off})
			}
		}
		sort.Slice(exPairs, func(i, j int) bool { return exPairs[i].key < exPairs[j].key })
		exKeys := make([]string, len(exPairs))
		exBmOffs := make([]uint32, len(exPairs))
		for i, p := range exPairs {
			exKeys[i], exBmOffs[i] = p.key, p.off
		}
		excOff, err := emit(cp.exc)
		if err != nil {
			return nil, err
		}
		postedOff, err := emit(cp.posted)
		if err != nil {
			return nil, err
		}
		catBlks = append(catBlks, catBlk{id, excOff, postedOff, keys, bmOffs, exKeys, exBmOffs, &cp.stats})
	}
	sort.Slice(catBlks, func(i, j int) bool { return catBlks[i].id < catBlks[j].id })

	var valBlks []valBlk
	for id, vp := range si.val {
		keys := make([]float64, 0, len(vp.post))
		for k := range vp.post {
			keys = append(keys, k)
		}
		sort.Float64s(keys)
		bmOffs := make([]uint32, len(keys))
		for i, k := range keys {
			if bmOffs[i], err = emit(vp.post[k]); err != nil {
				return nil, err
			}
		}
		excOff, err := emit(vp.exc)
		if err != nil {
			return nil, err
		}
		postedOff, err := emit(vp.posted)
		if err != nil {
			return nil, err
		}
		valBlks = append(valBlks, valBlk{id, excOff, postedOff, keys, bmOffs, &vp.stats})
	}
	sort.Slice(valBlks, func(i, j int) bool { return valBlks[i].id < valBlks[j].id })

	// Meta section: directory (with attrOff placeholders backpatched as each block is
	// written) followed by the sorted-key attr blocks.
	metaOff := uint32(len(b))
	b = appendU32(b, si.upto)
	b = appendU64(b, si.specGen)
	b = appendU32(b, allOff)

	// Each directory entry is {id; attrOff; statsOff} (v7 added statsOff -> the per-attr
	// segStats block), both offsets backpatched as the block is written.
	b = appendU32(b, uint32(len(catBlks)))
	catSlot := make([]int, len(catBlks))
	catStatsSlot := make([]int, len(catBlks))
	for i, cb := range catBlks {
		b = appendU32(b, cb.id)
		catSlot[i] = len(b)
		b = appendU32(b, 0)
		catStatsSlot[i] = len(b)
		b = appendU32(b, 0)
	}
	b = appendU32(b, uint32(len(valBlks)))
	valSlot := make([]int, len(valBlks))
	valStatsSlot := make([]int, len(valBlks))
	for i, vb := range valBlks {
		b = appendU32(b, vb.id)
		valSlot[i] = len(b)
		b = appendU32(b, 0)
		valStatsSlot[i] = len(b)
		b = appendU32(b, 0)
	}
	for i, cb := range catBlks {
		binary.LittleEndian.PutUint32(b[catSlot[i]:], uint32(len(b)))
		b = appendU32(b, cb.excOff)
		b = appendU32(b, cb.postedOff) // presence: is/isnt undefined
		b = appendU32(b, uint32(len(cb.keys)))
		var blob []byte
		keyOffs := make([]uint32, len(cb.keys)+1)
		for j, k := range cb.keys {
			keyOffs[j] = uint32(len(blob))
			blob = append(blob, k...)
		}
		keyOffs[len(cb.keys)] = uint32(len(blob))
		for _, o := range keyOffs {
			b = appendU32(b, o)
		}
		for _, o := range cb.bmOffs {
			b = appendU32(b, o)
		}
		b = append(b, blob...)
		// Exact-case run immediately after the folded keys blob: exactN;
		// exactKeyOff[exactN+1]; exactBmOff[exactN]; exactKeysBlob.
		b = appendU32(b, uint32(len(cb.exKeys)))
		var exBlob []byte
		exKeyOffs := make([]uint32, len(cb.exKeys)+1)
		for j, k := range cb.exKeys {
			exKeyOffs[j] = uint32(len(exBlob))
			exBlob = append(exBlob, k...)
		}
		exKeyOffs[len(cb.exKeys)] = uint32(len(exBlob))
		for _, o := range exKeyOffs {
			b = appendU32(b, o)
		}
		for _, o := range cb.exBmOffs {
			b = appendU32(b, o)
		}
		b = append(b, exBlob...)
		// Categorical bloom (v5), immediately after the exact-case run: bloomK; bloomM;
		// bloom words. bloomM==0 marks "no filter". Reuses the filter built at seal.
		if cb.stats.bloom != nil && cb.stats.bloom.m > 0 {
			b = appendU32(b, cb.stats.bloom.k)
			b = appendU32(b, cb.stats.bloom.m)
			for _, w := range cb.stats.bloom.bits {
				b = appendU64(b, w)
			}
		} else {
			b = appendU32(b, 0) // bloomK
			b = appendU32(b, 0) // bloomM == 0: no filter
		}
		// Categorical MPH (v6), immediately after the bloom. Gated on NDV: a high-cardinality
		// attribute gets O(1) equality; a small one keeps bloom + binary search. mphBlockLen==0
		// marks "no MPH". slotSortedIdx maps an MPH slot to the sorted index, so the reader
		// reuses the folded keyOff/bmOff arrays (no key or bitmap-offset duplication).
		if len(cb.keys) >= mphNDVThreshold {
			m, slots := buildMPH(cb.keys)
			mphBytes := appendMPH(nil, m)
			b = appendU32(b, uint32(len(mphBytes)))
			b = append(b, mphBytes...)
			slotIdx := make([]uint32, m.nAssigned)
			for sortedIdx, s := range slots {
				if s >= 0 {
					slotIdx[s] = uint32(sortedIdx)
				}
			}
			for _, idx := range slotIdx {
				b = appendU32(b, idx)
			}
		} else {
			b = appendU32(b, 0) // mphBlockLen == 0: no MPH
		}
		// Per-attribute stats block (v7): min/max, top-N, ndv, HLL — everything the query
		// planner's skip/selectivity paths read (the mmap reader can't recompute it without
		// paging every bitmap). Backpatch the directory's statsOff, then serialize.
		binary.LittleEndian.PutUint32(b[catStatsSlot[i]:], uint32(len(b)))
		b = appendSegStats(b, cb.stats)
	}
	for i, vb := range valBlks {
		binary.LittleEndian.PutUint32(b[valSlot[i]:], uint32(len(b)))
		b = appendU32(b, vb.excOff)
		b = appendU32(b, vb.postedOff) // presence: is/isnt undefined
		b = appendU32(b, uint32(len(vb.keys)))
		for _, k := range vb.keys {
			b = appendU64(b, math.Float64bits(k))
		}
		for _, o := range vb.bmOffs {
			b = appendU32(b, o)
		}
		binary.LittleEndian.PutUint32(b[valStatsSlot[i]:], uint32(len(b)))
		b = appendSegStats(b, vb.stats)
	}

	binary.LittleEndian.PutUint32(b[metaOffPos:], metaOff)
	// CRC-32C footer over all preceding bytes (v8). The sidecar is derived state, so a
	// mismatch on load is a rebuild trigger, never fatal. parseMmapSidecar reads via metaOff
	// and never touches this tail, so the footer is transparent to the lazy mmap reader.
	b = appendU32(b, crc32.Checksum(b, crcTable))
	return b, nil
}

// sidecarCRCValid recomputes the CRC-32C footer and reports whether it matches. A false
// result means the sidecar is corrupt (bit-rot or a torn write) and must be rebuilt from the
// data. The live tier checks this on load; the archive's lazy reader does not (a full verify
// would page the whole sidecar and defeat its >RAM design).
func sidecarCRCValid(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	want := binary.LittleEndian.Uint32(data[len(data)-4:])
	return crc32.Checksum(data[:len(data)-4], crcTable) == want
}

// --- little-endian append helpers ---

func appendU16(b []byte, v uint16) []byte { return binary.LittleEndian.AppendUint16(b, v) }
func appendU32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }
func appendU64(b []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(b, v) }

func appendBytes(b, p []byte) []byte {
	b = appendU32(b, uint32(len(p)))
	return append(b, p...)
}

// appendSegStats serializes a segment's per-attribute summary (v7). The bloom is NOT here
// (categorical blocks already carry it since v5, and the mmap reader consults it in place);
// everything the query planner's skip/selectivity paths need that cannot be recomputed
// without paging every bitmap is: covered/exc/ndv, the numeric range, the top-N heavy
// hitters, and the HLL registers.
func appendSegStats(b []byte, s *segStats) []byte {
	b = appendU64(b, s.covered)
	b = appendU64(b, s.exc)
	b = appendU64(b, s.ndv)
	var hasRange uint32
	if s.hasRange {
		hasRange = 1
	}
	b = appendU32(b, hasRange)
	b = appendU64(b, math.Float64bits(s.min))
	b = appendU64(b, math.Float64bits(s.max))
	b = appendU32(b, uint32(len(s.top)))
	for _, e := range s.top {
		b = appendBytes(b, []byte(e.skey))
		b = appendU64(b, math.Float64bits(e.fkey))
		b = appendU32(b, e.count)
	}
	if s.hll != nil {
		b = appendBytes(b, s.hll.reg)
	} else {
		b = appendBytes(b, nil)
	}
	return b
}

// readSegStats reconstructs a segStats from the block at off. The bloom stays nil (the mmap
// reader consults the on-disk bloom directly); the rest is faithful to the build-time stats.
func readSegStats(d []byte, off uint32) *segStats {
	c := &cursor{b: d, i: int(off)}
	s := &segStats{}
	s.covered = c.u64()
	s.exc = c.u64()
	s.ndv = c.u64()
	s.hasRange = c.u32() != 0
	s.min = math.Float64frombits(c.u64())
	s.max = math.Float64frombits(c.u64())
	if n := c.u32(); n > 0 && n < 1<<20 {
		s.top = make([]topEntry, n)
		for i := range s.top {
			s.top[i].skey = string(c.bytes())
			s.top[i].fkey = math.Float64frombits(c.u64())
			s.top[i].count = c.u32()
		}
	}
	if reg := c.bytes(); len(reg) > 0 {
		s.hll = &hyperLogLog{reg: append([]uint8(nil), reg...)}
	}
	return s
}

// cursor reads little-endian fields from a byte slice, latching the first error so
// callers can check once at the end (an out-of-range read yields zero and sets err).
// When zeroCopy is set, bitmap() builds roaring bitmaps as views into b (FromBuffer)
// instead of copying — used when b is an mmap'd sidecar that outlives the index.
type cursor struct {
	b        []byte
	i        int
	err      error
	zeroCopy bool
}

func (c *cursor) need(n int) bool {
	if c.err != nil {
		return false
	}
	if c.i+n > len(c.b) {
		c.err = fmt.Errorf("unexpected end of data")
		return false
	}
	return true
}

func (c *cursor) u16() uint16 {
	if !c.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(c.b[c.i:])
	c.i += 2
	return v
}

func (c *cursor) u32() uint32 {
	if !c.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(c.b[c.i:])
	c.i += 4
	return v
}

func (c *cursor) u64() uint64 {
	if !c.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(c.b[c.i:])
	c.i += 8
	return v
}

func (c *cursor) bytes() []byte {
	n := int(c.u32())
	if !c.need(n) {
		return nil
	}
	p := c.b[c.i : c.i+n]
	c.i += n
	return p
}

func (c *cursor) bitmap() (*roaring.Bitmap, error) {
	p := c.bytes()
	if c.err != nil {
		return nil, c.err
	}
	bm := roaring.New()
	if c.zeroCopy {
		// FromBuffer reads the same portable format MarshalBinary writes, referencing
		// p directly (best-effort, copy-on-write). p must stay immutable and mapped.
		if _, err := bm.FromBuffer(p); err != nil {
			return nil, err
		}
		return bm, nil
	}
	if err := bm.UnmarshalBinary(p); err != nil {
		return nil, err
	}
	return bm, nil
}

// writeFileSync writes data to path (truncating) and fsyncs it before returning.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
