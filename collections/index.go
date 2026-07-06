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

// indexSpec is the collection's configured indexes, resolved once at New: interned
// attribute ids partitioned into categorical (string) and value (numeric). Shared
// read-only across shards and segments.
type indexSpec struct {
	catIDs []uint32
	valIDs []uint32
	cat    map[uint32]struct{}
	val    map[uint32]struct{}
}

func (s *indexSpec) any() bool { return s != nil && (len(s.catIDs) > 0 || len(s.valIDs) > 0) }

// newIndexSpec resolves configured attribute names to interned ids.
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
// [0, upto); the query full-scans anything at or beyond upto.
type segIndex struct {
	upto uint32
	all  *roaring.Bitmap         // offsets of all covered records (for `!=` = all minus value)
	cat  map[uint32]*catPostings // interned attr id -> postings
	val  map[uint32]*valPostings
}

// buildSegIndex scans a segment's records in [0, upto) and returns its index.
// Reads immutable segment bytes only (no lock needed); decompresses each record to
// read the indexed attributes.
func (c *Collection) buildSegIndex(data []byte, upto int, codec Codec) *segIndex {
	si := &segIndex{
		upto: uint32(upto),
		all:  roaring.New(),
		cat:  make(map[uint32]*catPostings, len(c.spec.catIDs)),
		val:  make(map[uint32]*valPostings, len(c.spec.valIDs)),
	}
	for _, id := range c.spec.catIDs {
		si.cat[id] = &catPostings{post: map[string]*roaring.Bitmap{}, exc: roaring.New()}
	}
	for _, id := range c.spec.valIDs {
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
			si.indexRecord(o, wire.Ad(w), c.spec)
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
