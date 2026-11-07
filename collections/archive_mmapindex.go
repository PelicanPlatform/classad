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
	version uint16 // sidecar layout version it was read from (see sidecarMinVersion)
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
	// comps are the composite indexes this sidecar carries (v11), in tuple-identity order.
	// Empty for an older sidecar, which simply means the segment falls back to intersecting
	// the components' own postings -- correct, just the cost the composite was built to
	// avoid.
	comps []mmapComposite
}

// mmapComposite is one composite index's directory entry: the tuple it keys, and the file
// offset of its block.
type mmapComposite struct {
	spec compositeSpec
	off  uint32
}

// stalePostings reports whether the sidecar was built before negative numbers stored as unary minus
// over a literal were posted, and holds value exceptions that may be such numbers. Its answers are
// right either way (exceptions are re-verified); a rebuild only makes them cheaper, and is pointless
// for a sidecar with no value exception at all.
func (si *mmapSegIndex) stalePostings() bool {
	if si.version >= sidecarNegPostingVersion {
		return false
	}
	for _, st := range si.valStats {
		if st != nil && st.exc > 0 {
			return true
		}
	}
	return false
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
	v := c.u16()
	if v < sidecarMinVersion || v > sidecarVersion {
		return nil, fmt.Errorf("archive: unsupported sidecar version %d", v)
	}
	metaOff := c.u32()
	if c.err != nil {
		return nil, c.err
	}
	m := &cursor{b: data, i: int(metaOff)}
	si := &mmapSegIndex{
		version:  v,
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
	if v >= 11 {
		compN := m.u32()
		if compN > maxSidecarComposites {
			return nil, fmt.Errorf("archive: corrupt sidecar: %d composites", compN)
		}
		for i := uint32(0); i < compN; i++ {
			nAttrs := m.u32()
			if nAttrs < 2 || nAttrs > maxCompositeAttrs {
				return nil, fmt.Errorf("archive: corrupt sidecar: composite of %d attributes", nAttrs)
			}
			cs := compositeSpec{ids: make([]uint32, nAttrs), cats: make([]bool, nAttrs)}
			for j := range cs.ids {
				cs.ids[j] = m.u32()
			}
			mask := m.u32()
			for j := range cs.cats {
				cs.cats[j] = mask&(1<<uint(j)) != 0
			}
			cs.key = compositeKey(cs.ids)
			si.comps = append(si.comps, mmapComposite{spec: cs, off: m.u32()})
		}
	}
	if m.err != nil {
		return nil, fmt.Errorf("archive: corrupt sidecar directory: %w", m.err)
	}
	return si, nil
}

// maxSidecarComposites bounds the composite directory a sidecar may declare, so a corrupt
// length cannot make the parser allocate without limit before the cursor reports an error.
const maxSidecarComposites = 1024

// compositeMatch finds the composite covering usable's equality conjuncts and reads its
// block: the tuple's posting (binary search over the byte-sorted key blob, so only the
// touched pages are faulted in) and the composite's exception set.
func (si *mmapSegIndex) compositeMatch(usable []usableProbe) (compositeMatch, bool) {
	if len(si.comps) == 0 || len(usable) < 2 {
		return compositeMatch{}, false
	}
	var best compositeMatch
	var found bool
	for _, mc := range si.comps {
		key, used, ok := compositeTupleKey(mc.spec, usable)
		if !ok || (found && len(used) <= len(best.used)) {
			continue
		}
		cm := compositeMatch{spec: mc.spec, used: used, excN: le64(si.data, mc.off+4)}
		if off, hit := si.compFind(mc.off, key); hit {
			cm.post = si.bitmapAt(off)
		}
		if cm.excN > 0 {
			cm.exc = si.bitmapAt(le32(si.data, mc.off))
		}
		cm.certainWin = cm.excN == 0 && len(used) == len(usable)
		best, found = cm, true
	}
	return best, found
}

// compFind binary-searches a composite block's byte-sorted encoded tuple keys.
// Block layout: excOff u32; excN u64; n u32; keyOff[n+1] u32; bmOff[n] u32; keysBlob.
func (si *mmapSegIndex) compFind(blockOff uint32, key string) (bmOff uint32, ok bool) {
	d := si.data
	n := le32(d, blockOff+12)
	keyOffBase := blockOff + 16
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
func (si *mmapSegIndex) compositeCandidates(usable []usableProbe) (float64, string, bool) {
	return indexCompositeCandidates(si, usable)
}
func (si *mmapSegIndex) coveredUpto() uint32 { return si.upto }

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
			// Bloom fast path: if every probe value is definitely absent, skip the
			// per-value binary search over the (paged) sorted key blob entirely; only
			// the exceptional records can still match and are re-verified upstream.
			n := len(up.svals)
			if si.catBloomAllAbsent(attrOff, up.svals) {
				n = 0
			}
			bm := unionPostings(n, func(i int) *roaring.Bitmap {
				off, ok := si.catFindEq(attrOff, up.svals[i])
				if !ok {
					return nil
				}
				return si.bitmapAt(off)
			})
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
		case "prefix":
			// The sidecar already stores the folded keys sorted, so the prefix run is two
			// boundary searches over the mmapped blob; only the run's own postings are
			// paged in. The bloom is no help here -- it answers membership of a whole key,
			// not of a prefix -- so it is not consulted.
			from, to := si.catPrefixSpan(attrOff, up.svals[0])
			if n, _, _, _ := si.catKeyBases(attrOff); to-from == int(n) {
				bm := si.bitmapAt(postedOff).Clone() // every key: see index_prefix.go
				bm.Or(si.bitmapAt(excOff))
				return bm
			}
			bm := unionPostings(to-from, func(i int) *roaring.Bitmap {
				return si.bitmapAt(si.catBmOffAt(attrOff, from+i))
			})
			bm.Or(si.bitmapAt(excOff))
			return bm
		case "is": // =?= exact (case-sensitive) via the exact-case run, plus the exceptions (see
			// segIndex.probeOffsets)
			bm := roaring.New()
			if off, ok := si.catFindExact(attrOff, up.svals[0]); ok {
				bm.Or(si.bitmapAt(off))
			}
			bm.Or(si.bitmapAt(excOff))
			return bm
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
		bm := unionPostings(len(up.fvals), func(i int) *roaring.Bitmap {
			off, ok := si.valFind(attrOff, up.fvals[i])
			if !ok {
				return nil
			}
			return si.bitmapAt(off)
		})
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
		bm := si.valRange(attrOff, up)
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

// catKeyBases returns the three bases of a categorical attr block: the key-offset array,
// the bitmap-offset array, and the key blob, along with the key count.
func (si *mmapSegIndex) catKeyBases(attrOff uint32) (n, keyOffBase, bmOffBase, blobBase uint32) {
	n = le32(si.data, attrOff+8)
	keyOffBase = attrOff + 12
	bmOffBase = keyOffBase + (n+1)*4
	return n, keyOffBase, bmOffBase, bmOffBase + n*4
}

func (si *mmapSegIndex) catBmOffAt(attrOff uint32, i int) uint32 {
	_, _, bmOffBase, _ := si.catKeyBases(attrOff)
	return le32(si.data, bmOffBase+uint32(i)*4)
}

// catLowerBound returns the index of the first folded key not less than key.
func (si *mmapSegIndex) catLowerBound(attrOff uint32, key string) int {
	d := si.data
	n, keyOffBase, _, blobBase := si.catKeyBases(attrOff)
	lo, hi := 0, int(n)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		ks := blobBase + le32(d, keyOffBase+uint32(mid)*4)
		ke := blobBase + le32(d, keyOffBase+uint32(mid+1)*4)
		if cmpStrBytes(key, d[ks:ke]) <= 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// catPrefixSpan is strPrefixSpan over the sidecar's folded key run.
func (si *mmapSegIndex) catPrefixSpan(attrOff uint32, p string) (from, to int) {
	from = si.catLowerBound(attrOff, p)
	hi, ok := prefixUpperKey(p)
	if !ok {
		n, _, _, _ := si.catKeyBases(attrOff)
		return from, int(n)
	}
	return from, si.catLowerBound(attrOff, hi)
}

func (si *mmapSegIndex) prefixKeyCount(up usableProbe) (int, bool) {
	attrOff, ok := si.catDir[up.attrID]
	if !ok || !up.cat || len(up.svals) != 1 {
		return 0, false
	}
	from, to := si.catPrefixSpan(attrOff, up.svals[0])
	return to - from, true
}

// catExactRun locates the exact-case key run (=?=/=!=) of a categorical attr block. The run
// sits immediately after the folded keys blob, and since v12 a block whose run would restate
// that blob writes the exactRunMirrorsFolded marker in place of the four arrays: the folded
// run then IS the exact run, and the bases returned are its own. So every caller searches or
// walks the same three arrays without caring which layout it got. end is the first byte past
// the run, where the bloom begins.
//
// Deliberately the only place that knows the marker. The walk to the bloom, the =?= search
// and the canonical-value iteration each used to re-derive these bases from the block header,
// and three copies of a layout rule is three places for a format addition to be missed.
func (si *mmapSegIndex) catExactRun(attrOff uint32) (n, keyOffBase, bmOffBase, blobBase, end uint32) {
	d := si.data
	fn, fKeyOffBase, fBmOffBase, fBlobBase := si.catKeyBases(attrOff)
	exOff := fBlobBase + le32(d, fKeyOffBase+fn*4) // + folded blob length (keyOff[n])
	exN := le32(d, exOff)
	if exN == exactRunMirrorsFolded {
		return fn, fKeyOffBase, fBmOffBase, fBlobBase, exOff + 4
	}
	exKeyOffBase := exOff + 4
	exBmOffBase := exKeyOffBase + (exN+1)*4
	exBlobBase := exBmOffBase + exN*4
	return exN, exKeyOffBase, exBmOffBase, exBlobBase, exBlobBase + le32(d, exKeyOffBase+exN*4)
}

// catFindExact binary-searches the exact-case key run (=?=/=!=) for an exact spelling.
func (si *mmapSegIndex) catFindExact(attrOff uint32, key string) (bmOff uint32, ok bool) {
	d := si.data
	exN, exKeyOffBase, exBmOffBase, exBlobBase, _ := si.catExactRun(attrOff)
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
// cat attr block: it sits immediately after the exact-case run.
func (si *mmapSegIndex) catBloomOff(attrOff uint32) uint32 {
	_, _, _, _, end := si.catExactRun(attrOff)
	return end
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
	exN, exKeyOffBase, _, exBlobBase, _ := si.catExactRun(attrOff)
	for i := uint32(0); i < exN; i++ {
		ks := exBlobBase + le32(d, exKeyOffBase+i*4)
		ke := exBlobBase + le32(d, exKeyOffBase+(i+1)*4)
		if !add(string(d[ks:ke])) {
			return false
		}
	}
	return true
}

// catValueCount returns the number of records spelling categorical attribute id exactly
// key, read as the cardinality of that value's posting. roaring keeps per-container
// cardinalities in the serialized descriptive header, so this pages in the head of the
// bitmap rather than its containers.
func (si *mmapSegIndex) catValueCount(id uint32, key string) (uint64, bool) {
	attrOff, ok := si.catDir[id]
	if !ok {
		return 0, false
	}
	bmOff, ok := si.catFindExact(attrOff, key)
	if !ok {
		return 0, false
	}
	return si.bitmapAt(bmOff).GetCardinality(), true
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

// valRange unions the bitmaps of every key satisfying up's range bound(s). Keys are
// sorted, so a boundary search bounds the scan to the matching run — only those keys'
// pages and bitmaps are touched — and a two-sided probe bounds it at both ends, paging
// in just the band rather than everything above (or below) one threshold.
func (si *mmapSegIndex) valRange(attrOff uint32, up usableProbe) *roaring.Bitmap {
	d := si.data
	n := int(le32(d, attrOff+8))
	keyBase := attrOff + 12
	bmOffBase := keyBase + uint32(n)*8
	// [from, to) is the index range of matching keys.
	from, to := rangeSpan(up, n,
		func(t float64) int { return si.lowerBound(keyBase, n, t) },
		func(t float64) int { return si.upperBound(keyBase, n, t) })
	return unionPostings(to-from, func(i int) *roaring.Bitmap {
		return si.bitmapAt(le32(d, bmOffBase+uint32(from+i)*4))
	})
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

// recordCount is the number of records this segment's index covers. Unlike the per-attribute
// summaries -- parsed eagerly so the planner never pages the postings -- this reads the
// all-records bitmap out of the mapping, which is why indexEstCandidates calls it only for the
// presence probes. An `absent` probe's execution clones the same bitmap, so a probe that is
// estimated and then run pays for it once either way.
func (si *mmapSegIndex) recordCount() uint64 { return si.allBitmap().GetCardinality() }
