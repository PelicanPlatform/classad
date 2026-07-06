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
	}
	for id := range s.cat {
		n.cat[id] = struct{}{}
	}
	for id := range s.val {
		n.val[id] = struct{}{}
	}
	return n
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
	post map[string]*roaring.Bitmap
	exc  *roaring.Bitmap
}
type valPostings struct {
	post map[float64]*roaring.Bitmap
	exc  *roaring.Bitmap
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
// each record to read the indexed attributes.
func (c *Collection) buildSegIndex(data []byte, upto int, codec Codec, spec *indexSpec) *segIndex {
	si := &segIndex{
		upto:    uint32(upto),
		specGen: spec.gen,
		all:     roaring.New(),
		cat:     make(map[uint32]*catPostings, len(spec.catIDs)),
		val:     make(map[uint32]*valPostings, len(spec.valIDs)),
	}
	for _, id := range spec.catIDs {
		si.cat[id] = &catPostings{post: map[string]*roaring.Bitmap{}, exc: roaring.New()}
	}
	for _, id := range spec.valIDs {
		si.val[id] = &valPostings{post: map[float64]*roaring.Bitmap{}, exc: roaring.New()}
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
	return si
}

// indexRecord adds record offset o to the postings for each configured attribute,
// classifying the attribute value as indexable, exceptional, or absent.
func (si *segIndex) indexRecord(o uint32, ad wire.Ad, spec *indexSpec) {
	for _, id := range spec.catIDs {
		cp := si.cat[id]
		node, ok := ad.Lookup(id)
		if !ok {
			continue // absent
		}
		if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitString {
			// ClassAd string == is case-insensitive; index under a folded key.
			addOffset(cp.post, strings.ToLower(lit.Str), o)
		} else {
			cp.exc.Add(o)
		}
	}
	for _, id := range spec.valIDs {
		vp := si.val[id]
		node, ok := ad.Lookup(id)
		if !ok {
			continue
		}
		if lit, ok := wire.LiteralValue(node); ok {
			switch lit.Kind {
			case wire.LitInt:
				addOffset(vp.post, float64(lit.Int), o)
			case wire.LitReal:
				addOffset(vp.post, lit.Real, o)
			case wire.LitBool:
				f := 0.0
				if lit.Bool {
					f = 1
				}
				addOffset(vp.post, f, o)
			default: // string / undefined / error literal
				vp.exc.Add(o)
			}
		} else {
			vp.exc.Add(o) // list / record / computed expression
		}
	}
}

func addOffset[K comparable](m map[K]*roaring.Bitmap, k K, o uint32) {
	bm := m[k]
	if bm == nil {
		bm = roaring.New()
		m[k] = bm
	}
	bm.Add(o)
}
