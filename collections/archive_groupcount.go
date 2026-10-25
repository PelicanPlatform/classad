package collections

import (
	"math"
	"strings"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// Index-resident GROUP BY counts.
//
// `SELECT attr, COUNT(*) ... GROUP BY attr` over an archive is normally a scan: every
// record in every unpruned segment is decompressed so the grouping attribute can be read
// out of it. But for a categorically indexed attribute the answer is already in the index
// -- each distinct value has a posting listing the records that carry it, and the size of
// that posting IS the count. Summing posting cardinalities across segments answers the
// query without reading a single record, turning a scan of the whole archive into a walk
// of its per-segment indexes.
//
// The saving grows with the archive: at history scale a scan is minutes to tens of minutes
// and this is milliseconds, because it reads index metadata (kilobytes per segment) rather
// than record bytes (megabytes to gigabytes per segment).
//
// Correctness rests on one self-validating check rather than on enumerating every way the
// index could be an incomplete description of the data. A record contributes to a posting
// only if it carries the attribute as an indexed literal, so records that are missing the
// attribute, hold a non-literal expression for it, or are of an unindexed type are absent
// from every posting. Rather than test for each of those, the caller compares the summed
// total against the archive's record count: if they disagree, some record was not
// attributable to a value and the fast path declines. That is conservative in the safe
// direction -- it can only refuse an answer it could have given, never give a wrong one.

// CategoricalGroupCounts returns the exact number of records carrying each distinct value
// of a categorically indexed attribute, derived from the per-segment indexes instead of by
// scanning records. ok is false when the answer cannot be established from the indexes
// alone, in which case the caller must scan; see the package comment above for what makes
// it decline.
//
// The counts are keyed by the value's exact spelling, matching what a scan would group by:
// ClassAd string comparison folds case, but two spellings of the same value are distinct
// groups, and the index keeps its exact-case run for precisely that reason.
func (a *Archive) CategoricalGroupCounts(attr string) (map[string]int64, bool) {
	return a.c.categoricalGroupCounts(attr)
}

// CategoricalGroupCountsBucketed is CategoricalGroupCounts split by a second, numeric
// dimension: it returns per-value counts keyed by the bucket floor(bucketAttr/width)*width.
// This is the "per group per day" shape, with width the bucket size in the attribute's own
// units (86400 for daily buckets over a unix-seconds attribute).
//
// It is cheap for the same reason the ungrouped form is, plus one more: an archive is
// append-ordered, so a segment's values for a monotonically advancing attribute like a
// completion time span a narrow range. When a segment's zone map for bucketAttr falls
// wholly inside one bucket and every record holds a number for it -- the common case --
// every record in it belongs to that bucket
// and its per-value counts are attributed wholesale, with no record read. Only the segments
// straddling a bucket boundary are scanned, and with buckets much wider than a segment's
// time span those are a small minority.
//
// bucketAttr must carry a zone map (ZoneAttrs at creation); without one there is no way to
// place a segment in a bucket without reading it, and the whole point is lost, so this
// declines. It also declines for the same completeness reasons as CategoricalGroupCounts.
func (a *Archive) CategoricalGroupCountsBucketed(attr, bucketAttr string, width int64) (map[int64]map[string]int64, bool) {
	return a.c.categoricalGroupCountsBucketed(attr, bucketAttr, width)
}

func (c *Collection) categoricalGroupCountsBucketed(attr, bucketAttr string, width int64) (map[int64]map[string]int64, bool) {
	if width <= 0 {
		return nil, false
	}
	spec := c.spec.Load()
	id, ok := c.categoricalAttrID(spec, attr)
	if !ok {
		return nil, false
	}
	// Zone maps are keyed by interned attribute id, a different id space from the (possibly
	// inline) index spec above -- resolve this one through the intern table.
	zoneID, ok := c.intern.LookupID(bucketAttr)
	if !ok {
		return nil, false
	}

	out := make(map[int64]map[string]int64)
	addTo := func(bucket int64, v string, n int64) {
		m := out[bucket]
		if m == nil {
			m = make(map[string]int64)
			out[bucket] = m
		}
		m[v] += n
	}
	var total int64
	var dbuf []byte
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		ok := func() bool {
			defer releaseWindows(wins)
			for _, w := range wins {
				z, hasZone := w.zones[zoneID]
				idx := w.seg.readIdx()
				covered := 0
				// Whole-segment attribution: every record holds a number for bucketAttr (Full)
				// and the segment's entire range sits in one bucket, so every record lands there.
				// Without Full a record with no bucket value would be counted in one.
				b, oneBucket := int64(0), false
				if hasZone && z.Full {
					b, oneBucket = zoneBucket(z, width)
				}
				if idx != nil && oneBucket {
					covered = min(int(idx.coveredUpto()), w.used)
					complete := idx.catCanonicalValues(id, func(v string) bool {
						n, ok := idx.catValueCount(id, v)
						if !ok {
							return false
						}
						addTo(b, v, int64(n))
						total += int64(n)
						return true
					})
					if !complete {
						return false
					}
				}
				// Everything the index could not account for -- a straddling segment, the
				// open segment, or an unindexed tail -- is read record by record.
				if covered < w.used {
					n, ok := countAttrBucketRange(c, w, attr, bucketAttr, width, covered, w.used, &dbuf, addTo)
					if !ok {
						return false
					}
					total += n
				}
			}
			return true
		}()
		if !ok {
			return nil, false
		}
	}
	if total != int64(c.Len()) {
		return nil, false
	}
	return out, true
}

// bucketOf floors v into a width-aligned bucket. It must agree exactly with the aggregate
// engine's bucketKeyText, or the fast path would label rows differently from the scan.
//
// ok is false when v is not finite, which means it is in NO bucket: a bucket is the half-open
// interval [b, b+width) on the number line, and neither a NaN nor an infinity lies in one of
// those -- there is nothing to floor. Going ahead anyway is worse than useless, because
// int64() of a non-finite float is implementation-defined in Go: on darwin/arm64 with width
// 86400 a NaN comes out as bucket 0, right beside genuine epoch-0 records, and a +Inf as
// bucket -86400. Those are legitimate-looking bucket keys that differ by architecture, so the
// wrong answer is silent. Declining is the only answer that is the same everywhere, and it is
// what the bucketed path already does with every other value it cannot floor -- a string, a
// boolean, an absent attribute: the record leaves the series.
//
// Finiteness is the whole test: a finite value whose bucket start does not fit in an int64 still
// gets whatever the cast invents for it, because narrowing the accepted range would change where
// ordinary values bucket. Nothing within 9.2e18 of zero is affected, which is every timestamp and
// every counter a bucket is asked for.
func bucketOf(v float64, width int64) (int64, bool) {
	// Rejecting non-finite values rather than clamping them: a distinguished bucket would have
	// to be an int64 on the wire like any other group key, so it would collide with whatever
	// legitimate bucket that integer names -- which is exactly how the unguarded cast goes wrong.
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return int64(math.Floor(v/float64(width))) * width, true
}

// zoneBucket returns the single bucket a segment's whole zone range sits in. ok is false when
// the range straddles a bucket boundary, and when an edge is in no bucket at all: a NaN never
// reaches a zone edge (it clears Full, which the caller requires), but an infinity does, and
// Min == Max == +Inf would otherwise attribute every record in the segment to one bogus bucket.
func zoneBucket(z zoneRange, width int64) (int64, bool) {
	lo, ok := bucketOf(z.Min, width)
	if !ok {
		return 0, false
	}
	hi, ok := bucketOf(z.Max, width)
	if !ok || lo != hi {
		return 0, false
	}
	return lo, true
}

func (c *Collection) categoricalGroupCounts(attr string) (map[string]int64, bool) {
	spec := c.spec.Load()
	id, ok := c.categoricalAttrID(spec, attr)
	if !ok {
		return nil, false // not categorically indexed: nothing to read counts from
	}

	counts := make(map[string]int64)
	var total int64
	var dbuf []byte
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		ok := func() bool {
			defer releaseWindows(wins)
			for _, w := range wins {
				// An archive keeps no in-RAM index for its open segment -- the transient
				// build is discarded when the segment seals and its sidecar is written -- so
				// there is always one unindexed segment at the head, plus (for a sealed
				// segment reindexed under an older spec) a possible unindexed tail. Read
				// counts from the index where there is one and scan the remainder, which is
				// bounded by the segment size rather than by the archive.
				covered := 0
				if idx := w.seg.readIdx(); idx != nil {
					covered = min(int(idx.coveredUpto()), w.used)
					complete := idx.catCanonicalValues(id, func(v string) bool {
						n, ok := idx.catValueCount(id, v)
						if !ok {
							return false // enumerated a value it cannot count: give up
						}
						counts[v] += int64(n)
						total += int64(n)
						return true
					})
					if !complete {
						return false
					}
				}
				if covered < w.used {
					n, ok := countAttrRange(c, w, attr, covered, w.used, &dbuf, counts)
					if !ok {
						return false
					}
					total += n
				}
			}
			return true
		}()
		if !ok {
			return nil, false
		}
	}

	// The self-validating check: every record must have been attributed to exactly one
	// value. Any shortfall means the index is not a complete description of the data here.
	if total != int64(c.Len()) {
		return nil, false
	}
	return counts, true
}

// countAttrRange counts attr's string values over the records in [from, to) of one segment
// window, adding them to counts. It reports how many records it attributed, and ok=false as
// soon as a record does not carry the attribute as a string literal -- the same bar the
// indexed path holds, so a mixed archive declines rather than half-counting.
//
// Attributes are matched by name rather than by the index's attribute id: an index spec may
// be inline, in which case its ids are synthetic and share no id space with the encoded
// records. Name matching is the one bridge that holds in either regime. It costs a walk of
// each record's attributes, which is affordable because this only ever runs over the
// segment the index does not cover -- bounded by the segment size, not the archive.
func countAttrRange(c *Collection, w segWindow, attr string, from, to int, dbuf *[]byte, counts map[string]int64) (int64, bool) {
	var n int64
	for off := from; off < to; {
		o := uint32(off)
		if end, ok := w.dmg.skip(o); ok {
			off = int(end)
			continue
		}
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		off += int(total)
		if isSystemKeyBytes(recKey(w.data, o)) {
			continue // internal record, not part of the user's data (nor of Len)
		}
		raw, err := c.wire(recRef{w: w, off: o, dict: w.dict()}, *dbuf)
		if err != nil {
			return 0, false
		}
		*dbuf = raw
		var val string
		found := false
		wire.Ad(raw).ForEachNamed(c.intern, func(name string, node []byte) bool {
			if !strings.EqualFold(name, attr) {
				return true
			}
			if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitString {
				val, found = lit.Str, true
			}
			return false // stop at the attribute, however it turned out
		})
		if !found {
			return 0, false // absent, or present but not an indexable string literal
		}
		counts[val]++
		n++
	}
	return n, true
}

// countAttrBucketRange is countAttrRange for the bucketed form: it reads both the grouping
// attribute (a string) and the bucketing attribute (a number) out of each record in one
// pass, and reports ok=false if either is missing or of the wrong kind -- the same bar the
// indexed path holds.
func countAttrBucketRange(c *Collection, w segWindow, attr, bucketAttr string, width int64,
	from, to int, dbuf *[]byte, addTo func(bucket int64, v string, n int64)) (int64, bool) {
	var n int64
	for off := from; off < to; {
		o := uint32(off)
		if end, ok := w.dmg.skip(o); ok {
			off = int(end)
			continue
		}
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		off += int(total)
		if isSystemKeyBytes(recKey(w.data, o)) {
			continue
		}
		raw, err := c.wire(recRef{w: w, off: o, dict: w.dict()}, *dbuf)
		if err != nil {
			return 0, false
		}
		*dbuf = raw
		var val string
		var bucket int64
		gotVal, gotBucket, inBucket := false, false, false
		wire.Ad(raw).ForEachNamed(c.intern, func(name string, node []byte) bool {
			switch {
			case !gotVal && strings.EqualFold(name, attr):
				if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitString {
					val, gotVal = lit.Str, true
				} else {
					return false // present but not indexable: decline
				}
			case !gotBucket && strings.EqualFold(name, bucketAttr):
				f, ok := literalFloat(node)
				if !ok {
					return false // present but not a number: decline
				}
				// A non-finite number is READ but belongs to no bucket, so the record is
				// accounted for (it counts toward the completeness total below) while
				// contributing to no group -- which is what the scan does with it too.
				bucket, inBucket = bucketOf(f, width)
				gotBucket = true
			}
			return !(gotVal && gotBucket)
		})
		if !gotVal || !gotBucket {
			return 0, false
		}
		if inBucket {
			addTo(bucket, val, 1)
		}
		n++
	}
	return n, true
}

// categoricalAttrID resolves an attribute name to its id among the spec's categorical
// indexes. Names are matched case-insensitively, as ClassAd attribute names are, and the
// id is resolved the same way IndexedAttrs renders it so the two always agree on which
// attributes are categorically indexed.
func (c *Collection) categoricalAttrID(spec *indexSpec, attr string) (uint32, bool) {
	for _, id := range spec.catIDs {
		var name string
		var ok bool
		if spec.inline {
			name, ok = spec.names[id]
		} else {
			name, ok = c.intern.Name(id)
		}
		if ok && strings.EqualFold(name, attr) {
			return id, true
		}
	}
	return 0, false
}
