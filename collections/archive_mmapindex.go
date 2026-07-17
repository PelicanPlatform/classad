package collections

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/RoaringBitmap/roaring/v2"
)

// mmapSegIndex is a read-only view of a v2 sidecar index directly over its mmap'd
// (or heap-read) bytes. Unlike the live store's in-RAM segIndex, it materializes no
// per-value map: equality is a binary search over the sorted key run and range is a
// boundary search plus a scan of only the matching keys, so resident memory is
// O(#indexed attributes) regardless of a segment's value cardinality — the keys,
// bitmap-offset arrays, and bitmap payloads all stay in the demand-paged mapping.
// Bitmaps are built lazily via roaring FromBuffer (zero-copy) only for the postings
// a probe actually touches.
type mmapSegIndex struct {
	data    []byte
	upto    uint32
	specGen uint64
	allOff  uint32
	catDir  map[uint32]uint32 // attr id -> file offset of its cat attr block
	valDir  map[uint32]uint32 // attr id -> file offset of its val attr block
	// Per-attribute summaries, parsed eagerly (O(#attrs), resident) so the query planner's
	// skip/selectivity paths can read them without paging the postings (v7).
	catStats map[uint32]*segStats
	valStats map[uint32]*segStats
}

// statsFor returns the parsed per-segment summary for a probe's attribute (nil if the
// attribute is not indexed in this segment).
func (si *mmapSegIndex) statsFor(up usableProbe) *segStats {
	if up.cat {
		return si.catStats[up.attrID]
	}
	return si.valStats[up.attrID]
}

// parseMmapSidecar reads a v2 sidecar's fixed header and meta directory (a few
// entries — one per indexed attribute), leaving keys, offsets, and bitmaps in the
// mapping to be paged in on demand.
func parseMmapSidecar(data []byte) (*mmapSegIndex, error) {
	c := &cursor{b: data}
	if c.u32() != sidecarMagic {
		return nil, fmt.Errorf("archive: bad sidecar magic")
	}
	if v := c.u16(); v != sidecarVersion {
		return nil, fmt.Errorf("archive: unsupported sidecar version %d", v)
	}
	metaOff := c.u32()
	if c.err != nil {
		return nil, c.err
	}
	m := &cursor{b: data, i: int(metaOff)}
	si := &mmapSegIndex{
		data:     data,
		catDir:   map[uint32]uint32{},
		valDir:   map[uint32]uint32{},
		catStats: map[uint32]*segStats{},
		valStats: map[uint32]*segStats{},
	}
	si.upto = m.u32()
	si.specGen = m.u64()
	si.allOff = m.u32()
	catN := m.u32()
	for i := uint32(0); i < catN; i++ {
		id := m.u32()
		si.catDir[id] = m.u32()
		si.catStats[id] = readSegStats(data, m.u32())
	}
	valN := m.u32()
	for i := uint32(0); i < valN; i++ {
		id := m.u32()
		si.valDir[id] = m.u32()
		si.valStats[id] = readSegStats(data, m.u32())
	}
	if m.err != nil {
		return nil, fmt.Errorf("archive: corrupt sidecar directory: %w", m.err)
	}
	return si, nil
}

func le32(b []byte, off uint32) uint32 { return binary.LittleEndian.Uint32(b[off:]) }
func le64(b []byte, off uint32) uint64 { return binary.LittleEndian.Uint64(b[off:]) }

// bitmapAt returns the length-prefixed roaring bitmap at file offset off as a
// zero-copy view into the mapping.
func (si *mmapSegIndex) bitmapAt(off uint32) *roaring.Bitmap {
	c := &cursor{b: si.data, i: int(off), zeroCopy: true}
	bm, err := c.bitmap()
	if err != nil {
		return roaring.New()
	}
	return bm
}

func (si *mmapSegIndex) allBitmap() *roaring.Bitmap { return si.bitmapAt(si.allOff) }

// The readIndex surface: thin delegates to the shared planner logic (readindex.go), so this
// tier cannot diverge from the in-RAM segIndex.
func (si *mmapSegIndex) covers(usable []usableProbe) bool { return indexCovers(si, usable) }
func (si *mmapSegIndex) coversGroups(groups [][]usableProbe) bool {
	return indexCoversGroups(si, groups)
}
func (si *mmapSegIndex) candidateOffsetsGroups(groups [][]usableProbe) *roaring.Bitmap {
	return indexCandidateOffsetsGroups(si, groups)
}
func (si *mmapSegIndex) skipsPrefix(usable []usableProbe) bool { return indexSkipsPrefix(si, usable) }
func (si *mmapSegIndex) estCandidates(up usableProbe) float64  { return indexEstCandidates(si, up) }
func (si *mmapSegIndex) coveredUpto() uint32                   { return si.upto }

// coversProbe reports whether this segment indexes one probe's attribute.
func (si *mmapSegIndex) coversProbe(up usableProbe) bool {
	if up.cat {
		_, ok := si.catDir[up.attrID]
		return ok
	}
	_, ok := si.valDir[up.attrID]
	return ok
}

// bloomAbsent consults the on-disk categorical bloom (v5) for a ==/in probe: true iff every
// probe value is definitely absent.
func (si *mmapSegIndex) bloomAbsent(up usableProbe) bool {
	if !up.cat || (up.op != "==" && up.op != "in") {
		return false
	}
	attrOff, ok := si.catDir[up.attrID]
	if !ok {
		return false
	}
	return si.catBloomAllAbsent(attrOff, up.svals)
}

// candidateOffsets returns the offsets satisfying every usable probe (a superset the caller
// re-verifies), most-selective probe first via the shared planner logic. nil = no probe.
func (si *mmapSegIndex) candidateOffsets(usable []usableProbe) *roaring.Bitmap {
	return indexCandidateOffsets(si, usable)
}

// probeOffsets returns a fresh, mutable offset bitmap for one probe, reading only the
// touched postings from the mapping.
func (si *mmapSegIndex) probeOffsets(up usableProbe) *roaring.Bitmap {
	if up.cat {
		attrOff, ok := si.catDir[up.attrID]
		if !ok {
			return roaring.New()
		}
		excOff := le32(si.data, attrOff)
		postedOff := le32(si.data, attrOff+4)
		switch up.op {
		case "==", "in":
			bm := roaring.New()
			// Bloom fast path: if every probe value is definitely absent, skip the
			// per-value binary search over the (paged) sorted key blob entirely; only
			// the exceptional records can still match and are re-verified upstream.
			if !si.catBloomAllAbsent(attrOff, up.svals) {
				for _, s := range up.svals {
					if off, ok := si.catFindEq(attrOff, s); ok {
						bm.Or(si.bitmapAt(off))
					}
				}
			}
			bm.Or(si.bitmapAt(excOff))
			return bm
		case "!=":
			bm := si.allBitmap().Clone()
			if off, ok := si.catFindEq(attrOff, up.svals[0]); ok {
				bm.AndNot(si.bitmapAt(off))
			}
			return bm
		case "present": // attr isnt undefined: posted a value, or present-but-exceptional
			bm := si.bitmapAt(postedOff).Clone()
			bm.Or(si.bitmapAt(excOff))
			return bm
		case "absent": // attr is undefined: everything but the definitely-posted records
			bm := si.allBitmap().Clone()
			bm.AndNot(si.bitmapAt(postedOff))
			return bm
		case "is": // =?= exact (case-sensitive) via the exact-case run
			if off, ok := si.catFindExact(attrOff, up.svals[0]); ok {
				return si.bitmapAt(off)
			}
			return roaring.New()
		case "isnt": // =!= exact: everything but the exact-case matches
			bm := si.allBitmap().Clone()
			if off, ok := si.catFindExact(attrOff, up.svals[0]); ok {
				bm.AndNot(si.bitmapAt(off))
			}
			return bm
		}
		return roaring.New()
	}

	attrOff, ok := si.valDir[up.attrID]
	if !ok {
		return roaring.New()
	}
	excOff := le32(si.data, attrOff)
	postedOff := le32(si.data, attrOff+4)
	switch up.op {
	case "==", "in":
		bm := roaring.New()
		for _, f := range up.fvals {
			if off, ok := si.valFind(attrOff, f); ok {
				bm.Or(si.bitmapAt(off))
			}
		}
		bm.Or(si.bitmapAt(excOff))
		return bm
	case "!=":
		bm := si.allBitmap().Clone()
		if off, ok := si.valFind(attrOff, up.fvals[0]); ok {
			bm.AndNot(si.bitmapAt(off))
		}
		return bm
	case "present":
		bm := si.bitmapAt(postedOff).Clone()
		bm.Or(si.bitmapAt(excOff))
		return bm
	case "absent":
		bm := si.allBitmap().Clone()
		bm.AndNot(si.bitmapAt(postedOff))
		return bm
	case "<", "<=", ">", ">=":
		bm := si.valRange(attrOff, up.op, up.fvals[0])
		bm.Or(si.bitmapAt(excOff))
		return bm
	}
	return roaring.New()
}

// --- categorical attr block: excOff u32; postedOff u32; n u32; keyOff[n+1] u32; bmOff[n] u32; keysBlob ---

func (si *mmapSegIndex) catFind(attrOff uint32, key string) (bmOff uint32, ok bool) {
	d := si.data
	n := le32(d, attrOff+8)
	keyOffBase := attrOff + 12
	bmOffBase := keyOffBase + (n+1)*4
	blobBase := bmOffBase + n*4
	lo, hi := 0, int(n)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		ks := blobBase + le32(d, keyOffBase+uint32(mid)*4)
		ke := blobBase + le32(d, keyOffBase+uint32(mid+1)*4)
		switch cmpStrBytes(key, d[ks:ke]) {
		case 0:
			return le32(d, bmOffBase+uint32(mid)*4), true
		case -1:
			hi = mid
		default:
			lo = mid + 1
		}
	}
	return 0, false
}

// catFindExact binary-searches the exact-case key run (=?=/=!=) for an exact spelling.
// The run sits immediately after the folded keys blob: its offset is blobBase plus the
// folded blob's length (the last folded keyOff).
func (si *mmapSegIndex) catFindExact(attrOff uint32, key string) (bmOff uint32, ok bool) {
	d := si.data
	n := le32(d, attrOff+8)
	keyOffBase := attrOff + 12
	bmOffBase := keyOffBase + (n+1)*4
	blobBase := bmOffBase + n*4
	foldedBlobLen := le32(d, keyOffBase+n*4) // keyOff[n]
	exOff := blobBase + foldedBlobLen

	exN := le32(d, exOff)
	exKeyOffBase := exOff + 4
	exBmOffBase := exKeyOffBase + (exN+1)*4
	exBlobBase := exBmOffBase + exN*4
	lo, hi := 0, int(exN)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		ks := exBlobBase + le32(d, exKeyOffBase+uint32(mid)*4)
		ke := exBlobBase + le32(d, exKeyOffBase+uint32(mid+1)*4)
		switch cmpStrBytes(key, d[ks:ke]) {
		case 0:
			return le32(d, exBmOffBase+uint32(mid)*4), true
		case -1:
			hi = mid
		default:
			lo = mid + 1
		}
	}
	return 0, false
}

// catBloomOff returns the file offset of the categorical bloom block (bloomK) within a
// cat attr block: it sits immediately after the exact-case run. Mirrors catFindExact's
// walk to the exact run, then skips the exact blob.
func (si *mmapSegIndex) catBloomOff(attrOff uint32) uint32 {
	d := si.data
	n := le32(d, attrOff+8)
	keyOffBase := attrOff + 12
	bmOffBase := keyOffBase + (n+1)*4
	blobBase := bmOffBase + n*4
	exOff := blobBase + le32(d, keyOffBase+n*4) // + folded blob length (keyOff[n])
	exN := le32(d, exOff)
	exKeyOffBase := exOff + 4
	exBmOffBase := exKeyOffBase + (exN+1)*4
	exBlobBase := exBmOffBase + exN*4
	return exBlobBase + le32(d, exKeyOffBase+exN*4) // + exact blob length (exKeyOff[exN])
}

// catBloomAllAbsent reports whether the categorical bloom proves EVERY key definitely
// absent (so a query can skip binary-searching the sorted key blob). Returns false when
// there is no filter (bloomM==0), so the caller falls back to the exact lookup. Replays
// the same double-hash probe as bloomFilter.mayContain directly over the mapped words.
func (si *mmapSegIndex) catBloomAllAbsent(attrOff uint32, keys []string) bool {
	d := si.data
	bloomOff := si.catBloomOff(attrOff)
	k := le32(d, bloomOff)
	m := le32(d, bloomOff+4)
	if m == 0 || k == 0 {
		return false // no filter: cannot prove absence
	}
	wordsBase := bloomOff + 8
	mayContain := func(h uint64) bool {
		h1, h2 := uint32(h), uint32(h>>32)
		for i := uint32(0); i < k; i++ {
			p := (h1 + i*h2) & (m - 1)
			if le64(d, wordsBase+(p>>6)*8)&(1<<(p&63)) == 0 {
				return false
			}
		}
		return true
	}
	for _, s := range keys {
		if mayContain(hashString(s)) {
			return false // a value might be present: cannot skip
		}
	}
	return true
}

// catMPHOff returns the file offset of the categorical MPH block (mphBlockLen), which sits
// immediately after the bloom: bloomK u32; bloomM u32; bloom [bloomM/64] u64.
func (si *mmapSegIndex) catMPHOff(attrOff uint32) uint32 {
	bloomOff := si.catBloomOff(attrOff)
	bloomM := le32(si.data, bloomOff+4)
	return bloomOff + 8 + (bloomM/64)*8
}

// catFindEq resolves a categorical equality probe, using the MPH fast path when present and
// falling back to the sorted-run binary search otherwise (or on an MPH miss / false hit).
// The binary search is authoritative, so the MPH can only make a hit faster, never wrong.
func (si *mmapSegIndex) catFindEq(attrOff uint32, key string) (bmOff uint32, ok bool) {
	if off, hit := si.catFindMPH(attrOff, key); hit {
		return off, true
	}
	return si.catFind(attrOff, key)
}

// catCanonicalValues emits each distinct canonical spelling of categorical attribute id by
// iterating the sidecar's exact-case run, which writeSidecarIndex populated with exactly the
// canonical spellings (a case-uniform bucket contributes its spelling; a mixed-case bucket
// contributes each exact spelling) -- the same set segIndex.catCanonicalValues produces.
func (si *mmapSegIndex) catCanonicalValues(id uint32, add func(string) bool) bool {
	d := si.data
	attrOff, ok := si.catDir[id]
	if !ok {
		return true
	}
	n := le32(d, attrOff+8)
	keyOffBase := attrOff + 12
	bmOffBase := keyOffBase + (n+1)*4
	blobBase := bmOffBase + n*4
	exOff := blobBase + le32(d, keyOffBase+n*4) // + folded blob length
	exN := le32(d, exOff)
	exKeyOffBase := exOff + 4
	exBmOffBase := exKeyOffBase + (exN+1)*4
	exBlobBase := exBmOffBase + exN*4
	for i := uint32(0); i < exN; i++ {
		ks := exBlobBase + le32(d, exKeyOffBase+i*4)
		ke := exBlobBase + le32(d, exKeyOffBase+(i+1)*4)
		if !add(string(d[ks:ke])) {
			return false
		}
	}
	return true
}

// catFindMPH probes the categorical MPH (v6). It returns (bmOff, true) only for a key that
// the MPH resolves AND whose folded spelling verifies at the resolved slot; any other case
// -- no MPH, an unresolved key (unassigned member or non-member), or a verify mismatch (an
// MPH false hit) -- returns ok=false so catFindEq falls back to binary search.
func (si *mmapSegIndex) catFindMPH(attrOff uint32, key string) (bmOff uint32, ok bool) {
	d := si.data
	mphOff := si.catMPHOff(attrOff)
	mphLen := le32(d, mphOff)
	if mphLen == 0 {
		return 0, false
	}
	mphStart := mphOff + 4
	nAssigned := le32(d, mphStart)
	slot, resolved := mphLookupBytes(d, mphStart, key)
	if !resolved || slot >= nAssigned {
		return 0, false
	}
	j := le32(d, mphStart+mphLen+slot*4) // slotSortedIdx[slot]: index into the folded run
	n := le32(d, attrOff+8)
	if j >= n {
		return 0, false // defensive: corrupt permutation -> binary search
	}
	keyOffBase := attrOff + 12
	bmOffBase := keyOffBase + (n+1)*4
	blobBase := bmOffBase + n*4
	ks := blobBase + le32(d, keyOffBase+j*4)
	ke := blobBase + le32(d, keyOffBase+(j+1)*4)
	if cmpStrBytes(key, d[ks:ke]) != 0 {
		return 0, false // MPH false hit: the slot holds a different key
	}
	return le32(d, bmOffBase+j*4), true
}

// --- value attr block: excOff u32; postedOff u32; n u32; key[n] f64; bmOff[n] u32 ---

func (si *mmapSegIndex) valKeyAt(keyBase uint32, i int) float64 {
	return math.Float64frombits(le64(si.data, keyBase+uint32(i)*8))
}

func (si *mmapSegIndex) valFind(attrOff uint32, f float64) (bmOff uint32, ok bool) {
	d := si.data
	n := int(le32(d, attrOff+8))
	keyBase := attrOff + 12
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		k := si.valKeyAt(keyBase, mid)
		if k == f {
			bmOffBase := keyBase + uint32(n)*8
			return le32(d, bmOffBase+uint32(mid)*4), true
		}
		if f < k {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return 0, false
}

// valRange ORs the bitmaps of every key satisfying (op, t). Keys are sorted, so a
// boundary search bounds the scan to the matching run — only those keys' pages and
// bitmaps are touched.
func (si *mmapSegIndex) valRange(attrOff uint32, op string, t float64) *roaring.Bitmap {
	d := si.data
	n := int(le32(d, attrOff+8))
	keyBase := attrOff + 12
	bmOffBase := keyBase + uint32(n)*8
	bm := roaring.New()
	// [from, to) is the index range of matching keys.
	var from, to int
	switch op {
	case ">":
		from, to = si.upperBound(keyBase, n, t), n
	case ">=":
		from, to = si.lowerBound(keyBase, n, t), n
	case "<":
		from, to = 0, si.lowerBound(keyBase, n, t)
	case "<=":
		from, to = 0, si.upperBound(keyBase, n, t)
	}
	for i := from; i < to; i++ {
		bm.Or(si.bitmapAt(le32(d, bmOffBase+uint32(i)*4)))
	}
	return bm
}

// lowerBound returns the first index i with key[i] >= t (or n).
func (si *mmapSegIndex) lowerBound(keyBase uint32, n int, t float64) int {
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if si.valKeyAt(keyBase, mid) < t {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// upperBound returns the first index i with key[i] > t (or n).
func (si *mmapSegIndex) upperBound(keyBase uint32, n int, t float64) int {
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if si.valKeyAt(keyBase, mid) <= t {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// cmpStrBytes compares a string to a byte slice without allocating, returning -1, 0,
// or 1.
func cmpStrBytes(s string, b []byte) int {
	n := len(s)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if s[i] != b[i] {
			if s[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(s) < len(b):
		return -1
	case len(s) > len(b):
		return 1
	}
	return 0
}
