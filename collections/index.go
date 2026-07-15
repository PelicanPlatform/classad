package collections

import (
	"strings"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// Value/categorical indexing is per SEGMENT. Segments are append-only and
// immutable once written, so a segment's index is built by scanning its records
// once (extracting each configured attribute's value and recording the record's
// offset in a roaring bitmap keyed by value) and then never mutated. Supersession
// and scan-exactly-once fall out of the existing MVCC visibility check
// (seq <= S0 < supersededBySeq): an update writes a new record (indexed in a
// newer segment) and the stale version is filtered out at query time, so the
// index needs no per-update maintenance.
//
// An index need not cover every record: it covers offsets [0, upto). A query
// consults the index for [0, upto) and full-scans the remaining tail, so a stale
// or partial index is always correct, just less selective. Indexes are (re)built
// on a schedule the caller controls (Reindex), independently of writes and
// compaction — new/active/compacted segments are simply full-scanned until their
// next reindex.

// indexSpec is the collection's configured indexes: interned attribute ids
// partitioned into categorical (string) and value (numeric). It is immutable once
// built and published via Collection.spec (an atomic.Pointer); AddIndex/DropIndex
// swap in a new spec with an incremented gen. A segIndex records the gen it was
// built under, so Reindex can tell a stale index (built before an add/drop) from a
// current one. Shared read-only across shards and segments.
type indexSpec struct {
	gen    uint64 // bumped on every add/drop, so segIndexes can detect staleness
	catIDs []uint32
	valIDs []uint32
	cat    map[uint32]struct{}
	val    map[uint32]struct{}

	// Inline (persistent) collections have no intern table, so attribute ids are
	// assigned synthetically here and records' values are looked up by name. In
	// interned mode these are zero/nil and id == the global intern id. Indexes on a
	// persistent collection are in-memory only and rebuilt on recovery.
	inline   bool
	nextID   uint32            // next synthetic id to assign (inline only)
	names    map[uint32]string // id -> attribute name (inline only)
	nameToID map[string]uint32 // folded name -> id (inline only)
}

// inlineID returns the synthetic id for name, assigning a fresh one on first use.
// Only valid on a freshly built or cloned (unpublished) inline spec.
func (s *indexSpec) inlineID(name string) uint32 {
	fold := strings.ToLower(name)
	if id, ok := s.nameToID[fold]; ok {
		return id
	}
	id := s.nextID
	s.nextID++
	s.nameToID[fold] = id
	s.names[id] = name
	return id
}

// attrNode returns the value node for the attribute with id id in ad, reading it by
// name for an inline record or by interned id otherwise.
func (s *indexSpec) attrNode(ad wire.Ad, id uint32) ([]byte, bool) {
	if s.inline {
		return ad.LookupByName(s.names[id])
	}
	return ad.Lookup(id)
}

func (s *indexSpec) any() bool { return s != nil && (len(s.catIDs) > 0 || len(s.valIDs) > 0) }

// newIndexSpec resolves configured attribute names to interned ids (gen 0).
func newIndexSpec(intern *wire.InternTable, catNames, valNames []string) *indexSpec {
	s := &indexSpec{cat: map[uint32]struct{}{}, val: map[uint32]struct{}{}}
	for _, name := range catNames {
		id := intern.Intern(name)
		if _, dup := s.cat[id]; !dup {
			s.cat[id] = struct{}{}
			s.catIDs = append(s.catIDs, id)
		}
	}
	for _, name := range valNames {
		id := intern.Intern(name)
		if _, dup := s.val[id]; !dup {
			s.val[id] = struct{}{}
			s.valIDs = append(s.valIDs, id)
		}
	}
	return s
}

// clone returns a deep copy carrying the same gen (callers bump it after mutating).
func (s *indexSpec) clone() *indexSpec {
	n := &indexSpec{
		gen:    s.gen,
		catIDs: append([]uint32(nil), s.catIDs...),
		valIDs: append([]uint32(nil), s.valIDs...),
		cat:    make(map[uint32]struct{}, len(s.cat)),
		val:    make(map[uint32]struct{}, len(s.val)),
		inline: s.inline,
		nextID: s.nextID,
	}
	for id := range s.cat {
		n.cat[id] = struct{}{}
	}
	for id := range s.val {
		n.val[id] = struct{}{}
	}
	if s.inline {
		n.names = make(map[uint32]string, len(s.names))
		n.nameToID = make(map[string]uint32, len(s.nameToID))
		for id, name := range s.names {
			n.names[id] = name
		}
		for name, id := range s.nameToID {
			n.nameToID[name] = id
		}
	}
	return n
}

// newInlineIndexSpec builds an inline (persistent) index spec, assigning each
// distinct attribute a synthetic id and recording its name for by-name value lookup.
// Mirrors newIndexSpec's categorical/value partitioning.
func newInlineIndexSpec(catNames, valNames []string) *indexSpec {
	s := &indexSpec{
		inline:   true,
		cat:      map[uint32]struct{}{},
		val:      map[uint32]struct{}{},
		names:    map[uint32]string{},
		nameToID: map[string]uint32{},
	}
	for _, name := range catNames {
		id := s.inlineID(name)
		if _, dup := s.cat[id]; !dup {
			s.cat[id] = struct{}{}
			s.catIDs = append(s.catIDs, id)
		}
	}
	for _, name := range valNames {
		id := s.inlineID(name)
		if _, isCat := s.cat[id]; isCat {
			continue // an attr indexed as both: categorical wins (matches AddIndex)
		}
		if _, dup := s.val[id]; !dup {
			s.val[id] = struct{}{}
			s.valIDs = append(s.valIDs, id)
		}
	}
	return s
}

// equalIDs reports whether two specs index exactly the same attribute ids in the
// same buckets (order-insensitive), i.e. an add/drop would be a no-op.
func (s *indexSpec) equalIDs(o *indexSpec) bool {
	if len(s.cat) != len(o.cat) || len(s.val) != len(o.val) {
		return false
	}
	for id := range s.cat {
		if _, ok := o.cat[id]; !ok {
			return false
		}
	}
	for id := range s.val {
		if _, ok := o.val[id]; !ok {
			return false
		}
	}
	return true
}

// catPostings / valPostings map an attribute value to the bitmap of record offsets
// (within one segment) that hold it, plus an exceptions bitmap for records whose
// value is present but not the expected literal type.
type catPostings struct {
	post map[string]*roaring.Bitmap // folded (lower-cased) value -> offsets; serves ==/!=/in
	// exact / exactCase together answer =?= and =!= (case-sensitive) without doubling
	// memory in the common case. exactCase maps a folded key to its single exact-case
	// spelling for a *case-uniform* bucket (all its records share one spelling); the
	// empty string marks "spelling == folded key" (already lower-case), so post's bitmap
	// is reused directly. A folded key absent from exactCase is a multi-variant bucket,
	// and exact holds a per-exact-spelling bitmap for it. So exact is populated only for
	// the rare buckets that mix case; case-uniform buckets store just a small string.
	exact     map[string]*roaring.Bitmap
	exactCase map[string]string
	exc       *roaring.Bitmap
	posted    *roaring.Bitmap // records that posted a literal value (for presence probes)
	stats     segStats        // filled by finishStats after all records are indexed
}
type valPostings struct {
	post map[float64]*roaring.Bitmap
	// sortedKeys is post's keys in ascending order (filled by finishStats), so a range
	// probe boundary-searches to the matching run instead of scanning every key.
	sortedKeys []float64
	exc        *roaring.Bitmap
	posted     *roaring.Bitmap // records that posted a literal value (for presence probes)
	stats      segStats        // filled by finishStats after all records are indexed
}

// segIndex is one segment's immutable index. It covers records at offsets in
// [0, upto); the query full-scans anything at or beyond upto. specGen is the
// indexSpec generation it was built under, so Reindex can rebuild it when an
// add/drop has since changed the configured attributes.
type segIndex struct {
	upto    uint32
	specGen uint64
	all     *roaring.Bitmap         // offsets of all covered records (for `!=` = all minus value)
	cat     map[uint32]*catPostings // interned attr id -> postings
	val     map[uint32]*valPostings
}

// buildSegIndex scans a segment's records in [0, upto) and returns its index for
// the given spec. Reads immutable segment bytes only (no lock needed); decompresses
// each record to read the indexed attributes. It has no Collection dependency, so
// both the live store (Reindex) and the Archive build indexes with it.
func buildSegIndex(data []byte, upto int, codec Codec, spec *indexSpec) *segIndex {
	si := &segIndex{
		upto:    uint32(upto),
		specGen: spec.gen,
		all:     roaring.New(),
		cat:     make(map[uint32]*catPostings, len(spec.catIDs)),
		val:     make(map[uint32]*valPostings, len(spec.valIDs)),
	}
	for _, id := range spec.catIDs {
		si.cat[id] = &catPostings{
			post:   map[string]*roaring.Bitmap{},
			exact:  map[string]*roaring.Bitmap{},
			exc:    roaring.New(),
			posted: roaring.New(),
		}
	}
	for _, id := range spec.valIDs {
		si.val[id] = &valPostings{post: map[float64]*roaring.Bitmap{}, exc: roaring.New(), posted: roaring.New()}
	}
	var buf []byte
	for off := 0; off < upto; {
		o := uint32(off)
		total := recTotalLen(data, o)
		if total == 0 {
			break
		}
		si.all.Add(o)
		if w, err := codec.Decompress(buf[:0], recAd(data, o)); err == nil {
			buf = w
			si.indexRecord(o, wire.Ad(w), spec)
		}
		off += int(total)
	}
	// Summarize each attribute's completed postings for segment-skip and
	// selectivity ordering (see segstats.go). One pass over the distinct values.
	for _, cp := range si.cat {
		cp.finalizeExact()
		cp.finishStats()
	}
	for _, vp := range si.val {
		vp.finishStats()
	}
	return si
}

// indexRecord adds record offset o to the postings for each configured attribute,
// classifying the attribute value as indexable, exceptional, or absent.
func (si *segIndex) indexRecord(o uint32, ad wire.Ad, spec *indexSpec) {
	for _, id := range spec.catIDs {
		cp := si.cat[id]
		node, ok := spec.attrNode(ad, id)
		if !ok {
			continue // absent
		}
		if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitString {
			// ClassAd string == is case-insensitive; index under a folded key for
			// ==/!=/in, and also under the exact spelling for =?=/=!= (finalizeExact
			// later drops the exact bitmap wherever a bucket turns out case-uniform).
			addOffset(cp.post, strings.ToLower(lit.Str), o)
			addOffset(cp.exact, lit.Str, o)
			cp.posted.Add(o)
		} else {
			cp.exc.Add(o)
		}
	}
	for _, id := range spec.valIDs {
		vp := si.val[id]
		node, ok := spec.attrNode(ad, id)
		if !ok {
			continue
		}
		if lit, ok := wire.LiteralValue(node); ok {
			switch lit.Kind {
			case wire.LitInt:
				addOffset(vp.post, float64(lit.Int), o)
				vp.posted.Add(o)
			case wire.LitReal:
				addOffset(vp.post, lit.Real, o)
				vp.posted.Add(o)
			case wire.LitBool:
				f := 0.0
				if lit.Bool {
					f = 1
				}
				addOffset(vp.post, f, o)
				vp.posted.Add(o)
			default: // string / undefined / error literal
				vp.exc.Add(o)
			}
		} else {
			vp.exc.Add(o) // list / record / computed expression
		}
	}
}

// finalizeExact collapses case-uniform buckets: for every folded key whose records
// all share one exact spelling, it records that spelling in exactCase (empty when it
// equals the folded key) and drops the now-redundant exact bitmap, so exact retains
// only genuinely mixed-case buckets. The common all-lower-case index ends with an
// empty exact map and a small exactCase of empty strings.
func (cp *catPostings) finalizeExact() {
	cp.exactCase = make(map[string]string, len(cp.post))
	byFold := make(map[string][]string, len(cp.exact))
	for e := range cp.exact {
		f := strings.ToLower(e)
		byFold[f] = append(byFold[f], e)
	}
	for f, es := range byFold {
		if len(es) != 1 {
			continue // mixed case: keep per-spelling bitmaps in exact
		}
		e := es[0]
		if e == f {
			cp.exactCase[f] = "" // already lower-case: reuse post[f]
		} else {
			cp.exactCase[f] = e
		}
		delete(cp.exact, e)
	}
}

// exactBitmap returns the offsets of records whose categorical value is exactly key
// (case-sensitive), or nil if none. It reuses the folded bitmap for case-uniform
// buckets and consults exact only for mixed-case ones.
func (cp *catPostings) exactBitmap(key string) *roaring.Bitmap {
	f := strings.ToLower(key)
	fb := cp.post[f]
	if fb == nil {
		return nil // no record has this value under any casing
	}
	if ec, ok := cp.exactCase[f]; ok {
		canon := f
		if ec != "" {
			canon = ec
		}
		if canon == key {
			return fb
		}
		return nil // records exist, but spelled with a different case
	}
	return cp.exact[key] // mixed-case bucket; nil if this exact spelling is absent
}

func addOffset[K comparable](m map[K]*roaring.Bitmap, k K, o uint32) {
	bm := m[k]
	if bm == nil {
		bm = roaring.New()
		m[k] = bm
	}
	bm.Add(o)
}
