package collections

import (
	"math"
	"sort"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// GROUP BY one numeric column, answered from the columns.
//
// The ungrouped count already reads a column and never decodes a record:
//
//	count(*) where NumJobStarts < 10                                        38ms
//	NumJobStarts, count(*) where NumJobStarts < 10 GROUP BY NumJobStarts   8112ms
//
// 210x apart on the same predicate over the same 631,983 matching records, with the group column HOT. The
// grouped form declined every columnar path -- an index answers only the unconstrained case and only for a
// categorically indexed column, the count fast path is gated on there being no grouping, and the general
// columnar aggregate has no attribute to aggregate for COUNT(*) -- and fell through to projecting every
// matching record one at a time.
//
// But a GROUP BY on one numeric column IS a histogram of that column, and the count scan is already walking
// it. This does the same narrowing passes and then reads the group column for the survivors, bucketing them
// instead of only counting them. MIN/MAX/SUM/AVG/COUNT(attr) per group come from reading their columns in
// the same pass, since the records have already been narrowed.
//
// Memory is one entry per distinct group value, which is what the scanning aggregator this replaces also
// builds -- so a high-cardinality group column is no worse here, just faster.

// GroupCount is one group of a GROUP BY: the group column's value and how many records held it.
type GroupCount struct {
	Value classad.Value
	Count int
}

// GroupStats is one group of a GROUP BY with value aggregates: the group column's value, how many records
// fell in the group, and one NumStats per requested aggregate attribute, in the order requested.
type GroupStats struct {
	Value classad.Value
	Count int
	Stats []NumStats
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
// blocks, returning the groups sorted by value. See GroupStatsQuery, of which this is the no-value-
// aggregate case.
func (c *Collection) GroupCountQuery(q *vm.Query, groupAttr string) ([]GroupCount, bool) {
	return countsOf(c.GroupStatsQuery(q, groupAttr, nil))
}

// GroupCountAll is GroupCountQuery with no constraint: the histogram of the whole column.
func (c *Collection) GroupCountAll(groupAttr string) ([]GroupCount, bool) {
	return countsOf(c.GroupStatsAll(groupAttr, nil))
}

// countsOf drops the (absent) aggregate columns from a grouped-stats result, so the count API is the
// no-aggregate case of the one scan rather than a second scan that could drift from it.
func countsOf(gs []GroupStats, ok bool) ([]GroupCount, bool) {
	if !ok {
		return nil, false
	}
	out := make([]GroupCount, len(gs))
	for i, g := range gs {
		out[i] = GroupCount{Value: g.Value, Count: g.Count}
	}
	return out, true
}

// GroupStatsConstraint parses a constraint string and, if the shape is columnar-eligible, answers the
// per-group record count plus a NumStats per aggAttr over the matching records (see GroupStatsQuery).
// ok=false ⇒ use the normal grouping path.
func (c *Collection) GroupStatsConstraint(constraint, groupAttr string, aggAttrs []string) ([]GroupStats, bool) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, false
	}
	return c.GroupStatsQuery(q, groupAttr, aggAttrs)
}

// GroupStatsQuery answers `SELECT groupAttr, COUNT(*), <aggregates over aggAttrs> ... WHERE q GROUP BY
// groupAttr` from the columnar blocks, returning the groups sorted by value. Each group carries a
// NumStats per aggAttr, from which the caller renders MIN/MAX/SUM/AVG/COUNT(attr) -- the same inputs
// NumStatsQuery returns for the ungrouped case, so the rendering is shared and cannot diverge.
//
// ok=false when the shape is not columnar-servable -- no accelerator, a non-native constraint, a predicate
// that is not a conjunction of scalar numeric comparisons, or a group/aggregate attribute the schema does
// not carry as a numeric field -- and the caller then scans as before. It also declines mid-flight if a
// matching record's GROUP value is not a scalar number, because such a record still forms a group: the row
// path emits a group per distinct rendered value (`undefined` for an absent attribute, the text for a
// string), and a column read cannot tell absent from present-but-non-numeric, let alone render one.
// Dropping those records instead would return a result silently missing groups, so the query goes back to
// the scan that can render them.
//
// An absent AGGREGATE value is different and is simply skipped: MIN/SUM/COUNT(attr) over a set that does
// not include the record is what the reference computes for a record whose attribute is undefined. Only
// the group column decides which rows exist.
//
// Segments whose own schema lacks a field fall back to a row walk that reads only the attributes involved,
// so a schema change costs those segments and not the query.
func (c *Collection) GroupStatsQuery(q *vm.Query, groupAttr string, aggAttrs []string) ([]GroupStats, bool) {
	st := c.schemaScan.Load()
	if st == nil || c.intern == nil || q == nil {
		return nil, false
	}
	groupID, aggIDs, ok := c.resolveGroupAttrs(st, groupAttr, aggAttrs)
	if !ok {
		return nil, false
	}
	// First tier: a conjunction of numeric comparisons against literals, narrowed column by column with
	// zone-map pruning. Fastest, and the shape a history dashboard asks most.
	if preds, ok := c.numPredsOnFields(q, st.schema); ok {
		return c.groupStats(groupID, aggIDs, preds, st)
	}
	// Second tier: evaluate the query itself against the columns -- a column at a time where the
	// expression allows it, per record where it does not. This serves any NATIVE query, so a string
	// comparison, an attribute-to-attribute comparison, arithmetic or a disjunction groups from the
	// columns instead of falling to a record scan, matching what the ungrouped count already served.
	//
	// Skipped when an index could prune the row path instead, for the reason CountQuery skips it: this
	// evaluates EVERY visible record, so against a selective indexed constraint the scan wins.
	if c.indexCanPrune(q) {
		return nil, false
	}
	acc, ok := c.vecGroupStats(q, groupID, aggIDs, st)
	if !ok {
		return nil, false
	}
	return c.shapeGroupStats(acc, groupID, aggIDs)
}

// GroupStatsAll is GroupStatsQuery with no constraint: the whole column's histogram plus aggregates. The
// caller is asserting the query matches every record, which only it can know (see db.IsMatchAll) -- there
// is no predicate here to check that against.
//
// This is not reachable through GroupStatsQuery: an unconstrained query has no probes to analyze, so the
// predicate analysis declines it. And the index path that answers other unconstrained grouped counts only
// covers CATEGORICALLY indexed attributes, so `GROUP BY <numeric>` with no WHERE fell to a record scan the
// same way the constrained form did.
func (c *Collection) GroupStatsAll(groupAttr string, aggAttrs []string) ([]GroupStats, bool) {
	st := c.schemaScan.Load()
	if st == nil || c.intern == nil {
		return nil, false
	}
	groupID, aggIDs, ok := c.resolveGroupAttrs(st, groupAttr, aggAttrs)
	if !ok {
		return nil, false
	}
	return c.groupStats(groupID, aggIDs, nil, st)
}

// resolveGroupAttrs interns the group and aggregate attribute names and checks each is carried by the
// collection-wide schema as a numeric field. A name repeated in aggAttrs resolves to the same id, which
// costs one extra read per record and keeps the result positional -- callers ask for MIN(x) and MAX(x).
func (c *Collection) resolveGroupAttrs(st *schemaScanState, groupAttr string, aggAttrs []string) (uint32, []uint32, bool) {
	numericField := func(name string) (uint32, bool) {
		id, ok := c.intern.LookupID(name)
		if !ok {
			return 0, false
		}
		idx, ok := st.schema.byID[id]
		if !ok || !numericKind(st.schema.fields[idx].kind) {
			return 0, false
		}
		return id, true
	}
	groupID, ok := numericField(groupAttr)
	if !ok {
		return 0, nil, false
	}
	var aggIDs []uint32
	for _, a := range aggAttrs {
		id, ok := numericField(a)
		if !ok {
			return 0, nil, false
		}
		aggIDs = append(aggIDs, id)
	}
	return groupID, aggIDs, true
}

// groupStats runs the scan and shapes its result: the part GroupStatsQuery and GroupStatsAll share once
// the columns and the predicate set are resolved. A nil preds means every visible record survives.
func (c *Collection) groupStats(groupID uint32, aggIDs []uint32, preds []fieldPred, st *schemaScanState) ([]GroupStats, bool) {
	acc, ok := c.schemaScanGroupStats(groupID, aggIDs, preds, st.cache)
	if !ok {
		return nil, false
	}
	return c.shapeGroupStats(acc, groupID, aggIDs)
}

// shapeGroupStats turns an accumulator map into the sorted result and records the read demand. Shared by
// every tier, so the shape and order of the answer cannot depend on which one produced it.
func (c *Collection) shapeGroupStats(acc map[groupKey]*groupAcc, groupID uint32, aggIDs []uint32) ([]GroupStats, bool) {
	out := make([]GroupStats, 0, len(acc))
	for raw, a := range acc {
		g := GroupStats{Value: raw.value(), Count: a.n}
		if len(aggIDs) > 0 {
			g.Stats = make([]NumStats, len(aggIDs))
			for i := range a.stats {
				g.Stats[i] = a.stats[i].result()
			}
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return groupLess(out[i].Value, out[j].Value) })
	// Reading a column is demand for it, the same as any other query that touches one.
	read := make([]string, 0, 1+len(aggIDs))
	for _, id := range append([]uint32{groupID}, aggIDs...) {
		if name, ok := c.schemaFieldName(id); ok {
			read = append(read, name)
		}
	}
	c.demand.recordReads(read)
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

// groupAcc is one group's accumulation: the record count plus one aggregate accumulator per requested
// aggregate column.
type groupAcc struct {
	n     int
	stats []statsAccum
}

func newGroupAcc(nAgg int) *groupAcc {
	g := &groupAcc{}
	if nAgg > 0 {
		g.stats = make([]statsAccum, nAgg)
		for i := range g.stats {
			g.stats[i] = newStatsAccum()
		}
	}
	return g
}

// schemaScanGroupStats buckets the group column for every record satisfying preds -- one narrowing pass
// per predicate column, then one read of the group column and of each aggregate column for the survivors.
//
// ok=false means a surviving record's GROUP value was not a scalar number, which this cannot turn into a
// group (see GroupStatsQuery); the partial result is then discarded and the caller scans.
func (c *Collection) schemaScanGroupStats(groupID uint32, aggIDs []uint32, preds []fieldPred,
	bc *blockCache) (map[groupKey]*groupAcc, bool) {
	acc := map[groupKey]*groupAcc{}
	lookups := make([]func(wire.Ad) ([]byte, bool), len(preds))
	for i, p := range preds {
		lookups[i] = c.attrLookup(p.fieldID)
	}
	groupLookup := c.attrLookup(groupID)
	aggLookups := make([]func(wire.Ad) ([]byte, bool), len(aggIDs))
	for i, id := range aggIDs {
		aggLookups[i] = c.attrLookup(id)
	}

	var keep []bool
	var scratch []int64
	aggCols := make([]numCol, len(aggIDs))
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			groupIdx, aggIdxs, predIdxs, ok := resolveGroupFields(cs, groupID, aggIDs, preds)
			if !ok {
				if !bruteGroupStats(w, s0, groupLookup, aggLookups, preds, lookups, acc) {
					releaseWindows(wins)
					return nil, false
				}
				continue
			}
			base := 0
			for _, blk := range cs.blocks {
				// Only the PREDICATE columns can rule a block out; the group and aggregate columns'
				// values are the answer.
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
				for i := range aggIDs {
					aggCols[i], colOK = newNumCol(blk, aggIdxs[i], aggIDs[i], bc)
					if !colOK {
						releaseWindows(wins)
						return nil, false
					}
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
							// cannot; see GroupStatsQuery. Give the whole query back rather than
							// return a result with groups missing.
							releaseWindows(wins)
							return nil, false
						}
						key := groupKeyOf(nv)
						g := acc[key]
						if g == nil {
							g = newGroupAcc(len(aggIDs))
							acc[key] = g
						}
						g.n++
						for i := range aggCols {
							// An absent or non-numeric aggregate value contributes nothing, which is
							// what the reference computes for it -- unlike the group value above,
							// which decides whether the row exists at all.
							if av, ok := aggCols[i].at(k, bc); ok {
								g.stats[i].add(av)
							}
						}
					}
				}
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return acc, true
}

// resolveGroupFields maps the group column, every aggregate column and every predicated field into this
// SEGMENT's schema. ok=false when any is absent, since segments sealed under different schemas coexist.
func resolveGroupFields(cs *colSegment, groupID uint32, aggIDs []uint32,
	preds []fieldPred) (int, []int, []int, bool) {
	groupIdx, predIdxs, ok := resolveStatsFields(cs, groupID, preds)
	if !ok {
		return 0, nil, nil, false
	}
	sch := cs.schema()
	aggIdxs := make([]int, len(aggIDs))
	for i, id := range aggIDs {
		idx, ok := sch.byID[id]
		if !ok || !numericKind(sch.fields[idx].kind) {
			return 0, nil, nil, false
		}
		aggIdxs[i] = idx
	}
	return groupIdx, aggIdxs, predIdxs, true
}

// bruteGroupStats is the row fallback for a segment whose schema lacks a field involved: walk the visible
// records and read only those attributes, never decoding the whole ad.
//
// It returns false if a matching record's group attribute is missing or not a scalar number -- the same
// give-up condition as the column path, for the same reason.
func bruteGroupStats(w segWindow, s0 uint64, groupLookup func(wire.Ad) ([]byte, bool),
	aggLookups []func(wire.Ad) ([]byte, bool), preds []fieldPred,
	lookups []func(wire.Ad) ([]byte, bool), acc map[groupKey]*groupAcc) bool {
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
					key := groupKeyOf(nv)
					g := acc[key]
					if g == nil {
						g = newGroupAcc(len(aggLookups))
						acc[key] = g
					}
					g.n++
					for i := range aggLookups {
						if an, found := aggLookups[i](ad); found {
							if av, isNum := nodeColVal(an); isNum {
								g.stats[i].add(av)
							}
						}
					}
				}
			}
		}
		off += int(total)
	}
	return true
}
