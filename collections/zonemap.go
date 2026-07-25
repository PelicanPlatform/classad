package collections

import "github.com/PelicanPlatform/classad/collections/wire"

// Zone maps for an append-only collection: a sealed segment records the numeric
// [min,max] span of each configured ZoneAttr across its records, so a query with a
// range/equality constraint on such an attribute can skip whole segments whose span
// cannot contain a match (the archive's per-segment pruning, expressed as a Collection
// rule). The zoneRange type and the zonePrune/zoneMayMatch predicates are shared with
// the archive (archive.go / archive_query.go).
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

// computeSegZones scans a sealed segment's records and returns the per-attribute
// numeric min/max for the given zone attributes (absent/non-numeric values ignored).
// inline selects name-based vs id-based attribute lookup, matching how the collection
// encodes records (persistent collections store inline names; in-memory ones store
// interned ids). Returns nil when no zone attributes are configured. Caller holds the
// shard write lock (seal) or runs single-threaded during Open (reopen).
func computeSegZones(data []byte, upto int, attrs []zoneAttr, inline bool, codec Codec) map[uint32]zoneRange {
	if len(attrs) == 0 {
		return nil
	}
	zones := make(map[uint32]zoneRange, len(attrs))
	var buf []byte
	for off := 0; off < upto; {
		o := uint32(off)
		total := recTotalLen(data, o)
		if total == 0 {
			break
		}
		if recIsMarker(data, o) {
			off += int(total)
			continue
		}
		if w, err := codec.Decompress(buf[:0], recAd(data, o)); err == nil {
			buf = w
			ad := wire.Ad(w)
			for _, za := range attrs {
				var node []byte
				var ok bool
				if inline {
					node, ok = ad.LookupByName(za.name)
				} else {
					node, ok = ad.Lookup(za.id)
				}
				if !ok {
					continue
				}
				f, ok := literalFloat(node)
				if !ok {
					continue
				}
				z, seen := zones[za.id]
				if !seen {
					zones[za.id] = zoneRange{Min: f, Max: f}
				} else {
					if f < z.Min {
						z.Min = f
					}
					if f > z.Max {
						z.Max = f
					}
					zones[za.id] = z
				}
			}
		}
		off += int(total)
	}
	return zones
}

// sealZones computes and installs zones for a segment that has just stopped being the
// active append target. No-op unless the shard is append-only with zone attributes
// configured. Caller holds the shard write lock.
func (sh *shard) sealZones(seg *segment) {
	if seg == nil || !sh.appendOnly || len(sh.zoneAttrs) == 0 || seg.zones != nil {
		return
	}
	seg.zones = computeSegZones(seg.data, seg.used, sh.zoneAttrs, sh.zoneInline, seg.codec)
}
