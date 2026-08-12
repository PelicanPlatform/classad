package collections

import (
	"math"
	"sort"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// GROUP BY one numeric column with COUNT(*), answered from the columns.
//
// The ungrouped count already reads a column and never decodes a record:
//
//	count(*) where NumJobStarts < 10                                        38ms
//	NumJobStarts, count(*) where NumJobStarts < 10 GROUP BY NumJobStarts   8112ms
//
// 210x apart on the same predicate over the same 631,983 matching records, with the group column HOT. The
// grouped form declined every columnar path -- an index answers only the unconstrained case, the count fast
// path is gated on there being no grouping, and the general columnar aggregate has no attribute to aggregate
// for COUNT(*) -- and fell through to projecting every matching record one at a time.
//
// But a GROUP BY on one numeric column whose only aggregate is COUNT(*) IS a histogram of that column, and the
// count scan is already walking it. This does the same narrowing passes and then buckets the group column's
// values for the survivors instead of only counting them.

// GroupCount is one group of a GROUP BY: the group column's value and how many records held it.
type GroupCount struct {
	Value classad.Value
	Count int
}

// GroupCountConstraint parses a constraint string and, if the shape is columnar-eligible, answers
// the per-value counts of groupAttr over the matching records (see GroupCountQuery). ok=false ⇒
// use the normal grouping path.
func (c *Collection) GroupCountConstraint(constraint, groupAttr string) ([]GroupCount, bool) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, false
	}
	return c.GroupCountQuery(q, groupAttr)
}

// GroupCountQuery answers `SELECT groupAttr, COUNT(*) ... WHERE q GROUP BY groupAttr` from the columnar
// blocks, returning the groups sorted by value.
//
// ok=false when the shape is not columnar-servable -- no accelerator, a non-native constraint, a predicate
// that is not a conjunction of scalar numeric comparisons, or a group attribute the schema does not carry as
// a numeric field -- and the caller then scans as before. It also declines mid-flight if a matching record's
// group value is not a scalar number, because such a record still forms a GROUP: the row path emits a group
// per distinct rendered value (`undefined` for an absent attribute, the text for a string), and a column read
// cannot tell absent from present-but-non-numeric, let alone render one. Dropping those records instead would
// return a result that is silently missing groups, so the query goes back to the scan that can render them.
//
// Segments whose own schema lacks a field fall back to a row walk that reads only the attributes involved, so
// a schema change costs those segments and not the query.
func (c *Collection) GroupCountQuery(q *vm.Query, groupAttr string) ([]GroupCount, bool) {
	st := c.schemaScan.Load()
	if st == nil || c.intern == nil || q == nil {
		return nil, false
	}
	groupID, ok := c.intern.LookupID(groupAttr)
	if !ok {
		return nil, false
	}
	idx, ok := st.schema.byID[groupID]
	if !ok || !numericKind(st.schema.fields[idx].kind) {
		return nil, false
	}
	// The predicate has to be one the column scan can apply -- the same analysis the ungrouped count uses, so
	// the two paths serve exactly the same shapes.
	preds, ok := c.numPredsOnFields(q, st.schema)
	if !ok {
		return nil, false
	}
	return c.groupCount(groupID, preds, st.cache)
}

// GroupCountAll is GroupCountQuery with no constraint: the histogram of the whole column. The caller is
// asserting the query matches every record, which only it can know (see db.IsMatchAll) -- there is no
// predicate here to check that against.
//
// This is not reachable through GroupCountQuery: an unconstrained query has no probes to analyze, so the
// predicate analysis declines it. And the index path that answers other unconstrained grouped counts only
// covers CATEGORICALLY indexed attributes, so `GROUP BY <numeric>` with no WHERE fell to a record scan the
// same way the constrained form did.
func (c *Collection) GroupCountAll(groupAttr string) ([]GroupCount, bool) {
	st := c.schemaScan.Load()
	if st == nil || c.intern == nil {
		return nil, false
	}
	groupID, ok := c.intern.LookupID(groupAttr)
	if !ok {
		return nil, false
	}
	idx, ok := st.schema.byID[groupID]
	if !ok || !numericKind(st.schema.fields[idx].kind) {
		return nil, false
	}
	return c.groupCount(groupID, nil, st.cache)
}

// groupCount runs the scan and shapes its result: the part GroupCountQuery and GroupCountAll share once
// the group column and the predicate set are resolved. A nil preds means every visible record survives.
func (c *Collection) groupCount(groupID uint32, preds []fieldPred, bc *blockCache) ([]GroupCount, bool) {
	counts, ok := c.schemaScanGroupCount(groupID, preds, bc)
	if !ok {
		return nil, false
	}
	out := make([]GroupCount, 0, len(counts))
	for raw, n := range counts {
		out = append(out, GroupCount{Value: raw.value(), Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return groupLess(out[i].Value, out[j].Value) })
	if name, ok := c.schemaFieldName(groupID); ok {
		c.demand.recordReads([]string{name})
	}
	return out, true
}

// groupKey is a group column's raw slot value plus its kind, so an int and a real that share a bit pattern do
// not collide and each renders as the type it was stored as.
type groupKey struct {
	raw  int64
	kind adKind
}

// groupKeyOf turns a column value into a map key: the raw bits plus the kind, so an int and a real sharing a
// bit pattern do not merge and each renders as the type it was stored as.
func groupKeyOf(nv colVal) groupKey {
	if nv.kind == akReal {
		return groupKey{raw: int64(math.Float64bits(nv.f)), kind: akReal}
	}
	return groupKey{raw: nv.i, kind: nv.kind}
}

func (k groupKey) value() classad.Value {
	if k.kind == akReal {
		return classad.NewRealValue(math.Float64frombits(uint64(k.raw)))
	}
	return classad.NewIntValue(k.raw)
}

// groupLess orders groups by value, ints before reals only when they are equal, so ORDER BY on the group
// column gets the order it expects without the caller re-deriving it.
func groupLess(a, b classad.Value) bool {
	af, aerr := a.NumberValue()
	bf, berr := b.NumberValue()
	if aerr == nil && berr == nil {
		// NaN sorts last rather than comparing false against everything, which would leave it
		// anywhere and stop equal values from landing next to each other.
		if an, bn := af != af, bf != bf; an || bn {
			return !an && bn
		}
		return af < bf
	}
	return a.String() < b.String()
}

// schemaScanGroupCount buckets the group column for every record satisfying preds, one narrowing pass per
// predicate column and then one read of the group column for the survivors.
//
// ok=false means a surviving record's group value was not a scalar number, which this cannot turn into a
// group (see GroupCountQuery); the partial counts are then discarded and the caller scans.
func (c *Collection) schemaScanGroupCount(groupID uint32, preds []fieldPred, bc *blockCache) (map[groupKey]int, bool) {
	counts := map[groupKey]int{}
	lookups := make([]func(wire.Ad) ([]byte, bool), len(preds))
	for i, p := range preds {
		lookups[i] = c.attrLookup(p.fieldID)
	}
	groupLookup := c.attrLookup(groupID)

	var keep []bool
	var scratch []int64
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			groupIdx, predIdxs, ok := resolveStatsFields(cs, groupID, preds)
			if !ok {
				if !bruteGroupCount(w, s0, groupLookup, preds, lookups, counts) {
					releaseWindows(wins)
					return nil, false
				}
				continue
			}
			base := 0
			for _, blk := range cs.blocks {
				// Only the PREDICATE columns can rule a block out; the group column's values are the answer.
				if !blockMayMatch(blk, predIdxs, preds) {
					base += blk.n
					continue
				}
				col, colOK := newNumCol(blk, groupIdx, groupID, bc)
				if !colOK {
					// The block's schema carries the field but its column will not read (an
					// unreadable cold stream). Skipping the block would drop its records from
					// their groups without saying so, so the query goes back to the scan.
					releaseWindows(wins)
					return nil, false
				}
				if cap(keep) < blk.n {
					keep = make([]bool, blk.n)
					scratch = make([]int64, blk.n)
				}
				keep = keep[:blk.n]
				live := 0
				for k := 0; k < blk.n; k++ {
					gk := base + k
					vis := gk < len(cs.offs)
					if vis {
						o := cs.offs[gk]
						vis = recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0
					}
					keep[k] = vis
					if vis {
						live++
					}
				}
				for i := range preds {
					if live == 0 {
						break
					}
					live = narrowByField(blk, predIdxs[i], preds[i], bc, keep, scratch[:blk.n])
				}
				if live > 0 {
					for k := 0; k < blk.n; k++ {
						if !keep[k] {
							continue
						}
						nv, ok := col.at(k, bc)
						if !ok || !numericKind(nv.kind) {
							// This record matches and forms a group the caller can render but this
							// cannot; see GroupCountQuery. Give the whole query back rather than
							// return a result with groups missing.
							releaseWindows(wins)
							return nil, false
						}
						counts[groupKeyOf(nv)]++
					}
				}
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return counts, true
}

// bruteGroupCount is the row fallback for a segment whose schema lacks a field involved: walk the visible
// records and read only those attributes, never decoding the whole ad.
//
// It returns false if a matching record's group attribute is missing or not a scalar number -- the same
// give-up condition as the column path, for the same reason.
func bruteGroupCount(w segWindow, s0 uint64, groupLookup func(wire.Ad) ([]byte, bool),
	preds []fieldPred, lookups []func(wire.Ad) ([]byte, bool), counts map[groupKey]int) bool {
	var buf []byte
	for off := 0; off < w.used; {
		o := uint32(off)
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(w.data, o) && recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
			if ww, err := w.codec.Decompress(buf[:0], recAd(w.data, o)); err == nil {
				buf = ww
				ad := wire.Ad(ww)
				match := true
				for i := range preds {
					node, found := lookups[i](ad)
					if !found {
						match = false
						break
					}
					nv, isNum := nodeColVal(node)
					if !isNum || !preds[i].eval(nv.f) {
						match = false
						break
					}
				}
				if match {
					node, found := groupLookup(ad)
					if !found {
						return false
					}
					nv, isNum := nodeColVal(node)
					if !isNum || !numericKind(nv.kind) {
						return false
					}
					counts[groupKeyOf(nv)]++
				}
			}
		}
		off += int(total)
	}
	return true
}
