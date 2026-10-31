package collections

import (
	"math"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// Zone maps for an append-only collection: a sealed segment records the numeric
// [min,max] span of each configured ZoneAttr across its records, so a query with a
// range/equality constraint on such an attribute can skip whole segments whose span
// cannot contain a match (the archive's per-segment pruning, expressed as a Collection
// rule). The zoneRange type and the zonePrune/zoneMayMatch predicates are shared with
// the archive (archive.go / archive_query.go).
//
// A missing entry means "nothing known" to every reader: no pruning, no age retention, no
// wholesale attribution. That is how an attribute some record holds an expression for is
// represented, since the expression's value is unknown until the record is evaluated.
//
// Only sealed segments carry zones; the active (still-appended) segment has nil zones
// and is always scanned, so an in-flight record is never pruned away. Zones are
// computed once when a segment stops being active (seal) and, on reopen, for every
// non-active segment (loadShard).

// zoneAttr is one zone-mapped attribute: its (folded) name for inline-encoded records
// and its interned id for id-encoded records. The computed zone map is keyed by id
// regardless of encoding, since zone pruning (zonePrune) and age retention resolve a
// query/config attribute name to the same interned id via the shared intern table.
type zoneAttr struct {
	name string
	id   uint32
}

// zoneCell is what one record's value for a zone attribute means to the zone map.
type zoneCell uint8

const (
	// zoneNumber: a number, which the range must contain.
	zoneNumber zoneCell = iota
	// zoneNever: a value no numeric comparison can be true of. Ordering and equality against a
	// number give UNDEFINED for undefined and ERROR for an error, a string, a list or a nested ad;
	// every comparison with NaN is false. The range can leave these out and still prune soundly.
	zoneNever
	// zoneOpaque: a value only evaluating the record can give -- an expression, or an encrypted
	// value. The range cannot bound it, so the attribute gets no zone in this segment.
	zoneOpaque
)

// zoneValue classifies a record's value node for a zone map, returning the number for a
// zoneNumber. A boolean is a number here (1 or 0), as it is to the comparison operators. A
// negative constant written before the encoder folded one into a signed literal is stored as
// unary minus over a literal, and counts as the number it is either way; a list or nested ad is
// opaque only because it is not worth decoding to prove otherwise.
func zoneValue(node []byte) (float64, zoneCell) {
	lit, ok := scalarValue(node)
	if !ok {
		return 0, zoneOpaque
	}
	switch lit.Kind {
	case wire.LitInt:
		return float64(lit.Int), zoneNumber
	case wire.LitReal:
		if math.IsNaN(lit.Real) {
			return 0, zoneNever
		}
		return lit.Real, zoneNumber
	case wire.LitBool:
		if lit.Bool {
			return 1, zoneNumber
		}
		return 0, zoneNumber
	}
	return 0, zoneNever // string, undefined, error
}

// computeSegZones scans a sealed segment's records and returns the per-attribute numeric
// min/max for the given zone attributes. An attribute any record holds an expression for gets no
// entry: the zone could not bound that record, and pruning on it would drop the record whenever
// the expression evaluates into the queried range. Values no numeric comparison can be true of
// (absent, string, undefined) are left out of the range and clear Full (see zoneRange).
// Returns nil when no zone attributes are configured, or when a record cannot be read: a record
// the zone never saw could hold anything.
//
// inline selects name-based vs id-based attribute lookup, matching how the collection
// encodes records (persistent collections store inline names; in-memory ones store
// interned ids). Caller holds the shard write lock (seal) or runs single-threaded during
// Open (reopen).
//
// computeSegZones takes the SEGMENT rather than its bytes for the same reason buildSegIndex does: a
// zone map built from a columnarized segment's stored bytes would cover only the attributes its
// schema does not cover, and a zone map that under-reports a range makes the scan SKIP segments that
// contain matches. That is the worst shape of wrong answer here -- silent, and faster than the
// correct one.
func computeSegZones(c *Collection, seg *segment, upto int, attrs []zoneAttr, inline bool) map[uint32]zoneRange {
	data, dict := seg.data, seg.dict.Load()
	// c is nil only from the seal path, whose subject is the ACTIVE segment -- never columnarized,
	// since only a sealed segment is rewritten. Refuse rather than assume if that ever changes: no
	// zone map means no pruning, which is slow, while a partial one means wrong answers.
	if c == nil && seg.columnarized() {
		return nil
	}
	if len(attrs) == 0 {
		return nil
	}
	type zoneAcc struct {
		z      zoneRange
		seen   bool // some record held a number
		sparse bool // some record held no number
		opaque bool // some record held an expression
	}
	accs := make([]zoneAcc, len(attrs))
	var buf []byte
	dm := seg.damaged()
	walked := 0
	if c != nil {
		defer func() { c.recWalked.Add(int64(walked)) }()
	}
	for off := 0; off < upto; {
		o := uint32(off)
		if end, ok := dm.skip(o); ok {
			off = int(end)
			continue
		}
		total := recTotalLen(data, o)
		if total == 0 {
			break
		}
		if recIsMarker(data, o) {
			off += int(total)
			continue
		}
		w, err := c.recordWireIn(seg, data, o, buf)
		if err != nil {
			return nil
		}
		buf = w
		ad := wire.Ad(w)
		for i, za := range attrs {
			a := &accs[i]
			if a.opaque {
				continue
			}
			var node []byte
			var ok bool
			switch {
			case dict != nil: // interned segment: resolve the zone attr's segment-local id
				if id, has := dict.segIDOf(za.name); has {
					node, ok = ad.Lookup(uint32(id)) //idspace:translate a dictionary-keyed record is keyed by the segID
				}
			case inline:
				node, ok = ad.LookupByName(za.name)
			default:
				node, ok = ad.Lookup(za.id)
			}
			if !ok {
				a.sparse = true
				continue
			}
			f, cell := zoneValue(node)
			switch cell {
			case zoneOpaque:
				a.opaque = true
			case zoneNever:
				a.sparse = true
			default:
				if !a.seen {
					a.z, a.seen = zoneRange{Min: f, Max: f}, true
				} else if f < a.z.Min {
					a.z.Min = f
				} else if f > a.z.Max {
					a.z.Max = f
				}
			}
		}
		walked++
		off += int(total)
	}
	zones := make(map[uint32]zoneRange, len(attrs))
	for i, za := range attrs {
		a := accs[i]
		if a.opaque || !a.seen {
			continue
		}
		a.z.Full = !a.sparse
		zones[za.id] = a.z
	}
	return zones
}

// ZoneAttrs returns the attributes this collection keeps per-segment [min,max] zone maps
// on, in configuration order (explicit ZoneAttrs first, then the value-indexed attributes
// that are zoned automatically). Empty for a mutable collection, which has no zone maps.
// Diagnostic only -- it is how `.indexes` on an archive can show which range queries prune
// whole segments rather than only postings.
func (c *Collection) ZoneAttrs() []string {
	if !c.hasZones.Load() || len(c.shards) == 0 {
		return nil
	}
	sh := c.shards[0] // every shard carries the same zone-attribute list
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	names := make([]string, 0, len(sh.zoneAttrs))
	for _, za := range sh.zoneAttrs {
		names = append(names, za.name)
	}
	return names
}

// addZoneAttrs extends the zone-mapped attribute set of an append-only collection with
// names not already covered, so a value index added at RUNTIME gets whole-segment pruning
// too (construction wires ZoneAttrs + ValueAttrs together; see New). Segments already
// sealed keep the zone map they were sealed with -- a nil entry simply never prunes -- so
// the new attribute starts pruning from the next segment sealed, or across the whole
// archive after a Rewrite. No-op unless the collection is append-only.
func (c *Collection) addZoneAttrs(names []string) {
	if !c.appendOnly() || len(names) == 0 {
		return
	}
	for _, sh := range c.shards {
		sh.mu.Lock()
		seen := make(map[uint32]struct{}, len(sh.zoneAttrs))
		for _, za := range sh.zoneAttrs {
			seen[za.id] = struct{}{}
		}
		for _, n := range names {
			id := c.intern.Intern(n)
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			sh.zoneAttrs = append(sh.zoneAttrs, zoneAttr{name: n, id: id})
		}
		sh.mu.Unlock()
	}
	c.hasZones.Store(true)
}

// sealZones computes and installs zones for a segment that has just stopped being the
// active append target. No-op unless the shard is append-only with zone attributes
// configured. Caller holds the shard write lock.
func (sh *shard) sealZones(seg *segment) {
	if seg == nil || !sh.appendOnly || len(sh.zoneAttrs) == 0 || seg.zones != nil {
		return
	}
	// nil collection: the segment being sealed is the active append target, which is whole-record by
	// construction (only sealed segments are columnarized), so no reassembly can be needed here.
	seg.zones = computeSegZones(nil, seg, seg.used, sh.zoneAttrs, sh.zoneInline)
}
