package collections

import (
	"math"
	"os"
	"sort"

	"github.com/RoaringBitmap/roaring/v2"
)

// Live index snapshot ("CLIX"): a round-trip serialization of a built in-RAM segIndex for
// a SEALED persistent segment, so a reopened collection restores its indexes by
// deserializing postings instead of re-decompressing and re-indexing every record (the
// dominant cost of Open on a large store). Unlike the archive sidecar (mmap, pageable,
// O(#attrs) resident), this restores the full in-RAM segIndex the live query path uses.
//
// Only the postings are serialized (the per-value roaring bitmaps, the exact-case run for
// =?=/=!=, and the exception/posted bitmaps). All DERIVED state -- the value index's
// sortedKeys and every segStats sketch (min/max, bloom, HLL, top-N, ndv, covered) -- is
// recomputed on load by finishStats, which builds it purely from the postings, so a decode
// reproduces a byte-identical segIndex to buildSegIndex. finalizeExact is NOT re-run: the
// snapshot already holds the finalized exact/exactCase split.
//
// Safety: a snapshot is bound to its segment by living beside the segment file, and is
// validated on load against the index spec generation (and, by the caller, the segment's
// write extent). Any structural mismatch -- wrong magic/version or a different spec gen --
// is a SOFT miss (nil, nil): the caller rebuilds from the records. A snapshot can therefore
// only ever be a faster path to the same index, never a wrong one.
const (
	liveIndexMagic   = 0x434C4958 // "CLIX"
	liveIndexVersion = 1
)

func appendStr(b []byte, s string) []byte { return appendBytes(b, []byte(s)) }

func appendRoaring(b []byte, bm *roaring.Bitmap) ([]byte, error) {
	if bm == nil {
		bm = roaring.New()
	}
	p, err := bm.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return appendBytes(b, p), nil
}

// encodeLiveIndex serializes a built segIndex. Attribute ids and keys are emitted in
// sorted order so the encoding is deterministic (a rebuilt+re-encoded index is
// byte-identical), which keeps round-trip tests and any future content hashing stable.
func encodeLiveIndex(si *segIndex) ([]byte, error) {
	b := make([]byte, 0, 1024)
	b = appendU32(b, liveIndexMagic)
	b = appendU16(b, liveIndexVersion)
	b = appendU32(b, si.upto)
	b = appendU64(b, si.specGen)
	var err error
	if b, err = appendRoaring(b, si.all); err != nil {
		return nil, err
	}

	catIDs := sortedU32Keys(si.cat)
	b = appendU32(b, uint32(len(catIDs)))
	for _, id := range catIDs {
		cp := si.cat[id]
		b = appendU32(b, id)
		if b, err = encodeStrPostings(b, cp.post); err != nil {
			return nil, err
		}
		if b, err = encodeStrPostings(b, cp.exact); err != nil {
			return nil, err
		}
		ecKeys := sortedStrMapKeys(cp.exactCase)
		b = appendU32(b, uint32(len(ecKeys)))
		for _, k := range ecKeys {
			b = appendStr(b, k)
			b = appendStr(b, cp.exactCase[k])
		}
		if b, err = appendRoaring(b, cp.exc); err != nil {
			return nil, err
		}
		if b, err = appendRoaring(b, cp.posted); err != nil {
			return nil, err
		}
	}

	valIDs := sortedU32Keys(si.val)
	b = appendU32(b, uint32(len(valIDs)))
	for _, id := range valIDs {
		vp := si.val[id]
		b = appendU32(b, id)
		fkeys := make([]float64, 0, len(vp.post))
		for k := range vp.post {
			fkeys = append(fkeys, k)
		}
		sort.Float64s(fkeys)
		b = appendU32(b, uint32(len(fkeys)))
		for _, k := range fkeys {
			b = appendU64(b, math.Float64bits(k))
			if b, err = appendRoaring(b, vp.post[k]); err != nil {
				return nil, err
			}
		}
		if b, err = appendRoaring(b, vp.exc); err != nil {
			return nil, err
		}
		if b, err = appendRoaring(b, vp.posted); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// encodeStrPostings emits a folded/exact string->bitmap map as n × { key; roaring }.
func encodeStrPostings(b []byte, m map[string]*roaring.Bitmap) ([]byte, error) {
	keys := sortedStrKeys(m)
	b = appendU32(b, uint32(len(keys)))
	var err error
	for _, k := range keys {
		b = appendStr(b, k)
		if b, err = appendRoaring(b, m[k]); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// decodeLiveIndex deserializes a segIndex snapshot and recomputes its derived stats.
// Returns (nil, nil) -- a soft miss telling the caller to rebuild -- when the blob is not a
// current-version snapshot or was built under a different index spec generation. Returns a
// non-nil error only on a corrupt/truncated blob.
func decodeLiveIndex(data []byte, spec *indexSpec) (*segIndex, error) {
	c := &cursor{b: data}
	if c.u32() != liveIndexMagic || c.u16() != liveIndexVersion {
		return nil, nil // not a snapshot we understand: rebuild
	}
	si := &segIndex{cat: map[uint32]*catPostings{}, val: map[uint32]*valPostings{}}
	si.upto = c.u32()
	si.specGen = c.u64()
	if spec != nil && si.specGen != spec.gen {
		return nil, nil // built under a different spec (attrs added/dropped): rebuild
	}
	var err error
	if si.all, err = c.bitmap(); err != nil {
		return nil, err
	}

	catN := c.u32()
	for i := uint32(0); i < catN; i++ {
		id := c.u32()
		cp := &catPostings{
			post:      map[string]*roaring.Bitmap{},
			exact:     map[string]*roaring.Bitmap{},
			exactCase: map[string]string{},
		}
		if err = decodeStrPostings(c, cp.post); err != nil {
			return nil, err
		}
		if err = decodeStrPostings(c, cp.exact); err != nil {
			return nil, err
		}
		ecN := c.u32()
		for j := uint32(0); j < ecN; j++ {
			k := string(c.bytes())
			cp.exactCase[k] = string(c.bytes())
		}
		if cp.exc, err = c.bitmap(); err != nil {
			return nil, err
		}
		if cp.posted, err = c.bitmap(); err != nil {
			return nil, err
		}
		cp.finishStats() // recompute stats from the restored postings (NOT finalizeExact)
		si.cat[id] = cp
	}

	valN := c.u32()
	for i := uint32(0); i < valN; i++ {
		id := c.u32()
		vp := &valPostings{post: map[float64]*roaring.Bitmap{}}
		pn := c.u32()
		for j := uint32(0); j < pn; j++ {
			k := math.Float64frombits(c.u64())
			bm, e := c.bitmap()
			if e != nil {
				return nil, e
			}
			vp.post[k] = bm
		}
		if vp.exc, err = c.bitmap(); err != nil {
			return nil, err
		}
		if vp.posted, err = c.bitmap(); err != nil {
			return nil, err
		}
		vp.finishStats() // recomputes sortedKeys + stats
		si.val[id] = vp
	}
	if c.err != nil {
		return nil, c.err
	}
	return si, nil
}

func decodeStrPostings(c *cursor, m map[string]*roaring.Bitmap) error {
	n := c.u32()
	for j := uint32(0); j < n; j++ {
		k := string(c.bytes())
		bm, err := c.bitmap()
		if err != nil {
			return err
		}
		m[k] = bm
	}
	return nil
}

// --- deterministic key ordering helpers ---

func sortedStrKeys(m map[string]*roaring.Bitmap) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedStrMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedU32Keys[V any](m map[uint32]V) []uint32 {
	keys := make([]uint32, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// --- segment snapshot files (persistent collections only) ---

// snapshotPath is where a persistent segment's index snapshot lives: beside the segment
// file, sharing its name so compaction/rotation drops it with the segment (a compacted
// segment is a new file with no snapshot, so it is rebuilt). "" for a RAM segment.
func snapshotPath(seg *segment) string {
	if seg.path == "" {
		return ""
	}
	return seg.path + ".idx"
}

// writeIndexSnapshot persists a built segIndex beside its segment so a reopen restores it
// without re-indexing. Best-effort: a write failure is swallowed (a missing snapshot only
// costs a rebuild next Open). No-op for a RAM segment. The active (still-growing) segment
// is snapshotted too, but reload validates coverage and rebuilds it if it has since grown,
// so a stale active-segment snapshot is harmless.
func (c *Collection) writeIndexSnapshot(seg *segment, si *segIndex) {
	path := snapshotPath(seg)
	if path == "" || si == nil {
		return
	}
	if blob, err := encodeLiveIndex(si); err == nil {
		_ = writeFileSync(path, blob)
	}
}

// loadIndexSnapshot restores seg's index from its on-disk snapshot, returning true iff a
// valid snapshot that fully covers the segment's recovered write extent was installed. Any
// miss -- no file, unreadable, wrong version, a different index-spec generation, or a
// coverage that does not match the recovered extent -- leaves idx nil so Reindex rebuilds
// it. A snapshot is therefore only ever a faster path to the same index, never a wrong one.
func (c *Collection) loadIndexSnapshot(seg *segment, spec *indexSpec) bool {
	path := snapshotPath(seg)
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	si, err := decodeLiveIndex(data, spec)
	if err != nil || si == nil {
		return false
	}
	if int(si.upto) != seg.used {
		return false // snapshot doesn't match the recovered extent: rebuild
	}
	seg.idx.Store(si)
	return true
}
