package collections

import (
	"encoding/binary"
	"math"
	"sync/atomic"
	"unsafe"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Evaluating a query directly against columnar storage.
//
// The hand-written column scans serve `Attr OP literal` conjunctions on numeric fields, which is the
// common case and is 10-500x. Everything else -- a string comparison, an attribute-to-attribute
// comparison, arithmetic between attributes, a disjunction -- reached no columnar path at all and
// decoded whole ads, even though all of it compiles to NATIVE instructions:
//
//	Owner == "user3"                        native, not served
//	Owner == "user3" && RequestMemory > 4096 native, not served
//	RequestMemory > RequestCpus * 512        native, not served
//	ProcId >= 5 && ClusterId != ProcId       native, not served
//	RequestMemory > 4096 || RequestCpus >= 4 native, not served
//
// Those are four different shapes, and extending the narrowing scan to each would be four special
// cases -- disjunction does not fit a narrowing design at all. One resolver covers them, because the
// evaluator is already backing-agnostic: vm.Matcher.EvalResolved takes a resolver and materializes no
// ClassAd, and wireScope is the same idea over an ad's WIRE bytes. colScope is that resolver over a
// block.
//
// It is the LAST tier, not the first. Per-record interpreter dispatch costs roughly an order of
// magnitude more than a strided column read, so the hand-written paths run ahead of it and this
// catches what they decline.

// colScope resolves attribute references for record k of a columnar block.
//
// Reused across a scan (single-threaded): set k and clear fellBack before each evaluation, exactly as
// wireScope is reused across a row scan.
type colScope struct {
	blk *columnarBlock
	k   int
	bc  *blockCache
	c   *Collection
	// fellBack means this record needs the ordinary evaluator: an attribute's stored value is an
	// EXPRESSION, whose value depends on the rest of the ad. The caller re-evaluates that one record
	// rather than abandoning the query.
	fellBack bool

	// bind caches name -> schema field index for the schema in bindFor: -1 means "not a schema field"
	// (it lives in the cold tail), -2 means the table has never seen the name. A query's references
	// are fixed, so resolving them per RECORD would pay InternTable.LookupID's mutex and possible
	// case-fold on every value.
	bind    map[string]int
	bindFor *adSchema

	strScratch []byte // decompressed string region, cached per block by the block cache
}

const (
	bindColdTail = -1 // not a schema field: look in the cold tail
	bindUnknown  = -2 // the table has never interned this name: no record can have it
)

// bindField resolves name against the current block's schema, caching the answer.
func (cs *colScope) bindField(name string) int {
	if cs.bindFor != cs.blk.schema {
		cs.bind, cs.bindFor = make(map[string]int, 8), cs.blk.schema
	}
	if v, ok := cs.bind[name]; ok {
		return v
	}
	v := bindColdTail
	id, ok := cs.c.intern.LookupID(name)
	if !ok {
		v = bindUnknown
	} else if i, ok := cs.blk.schema.byID[id]; ok {
		v = i
	}
	cs.bind[name] = v
	return v
}

// resolve is the attribute resolver handed to vm.Matcher.EvalResolved.
func (cs *colScope) resolve(name string, scope ast.AttributeScope) classad.Value {
	if scope != ast.NoScope && scope != ast.MyScope {
		// TARGET/PARENT: a collection scan has no match target, and a chained parent lives in another
		// record. wireScope returns undefined for TARGET; do the same rather than fall back, since
		// falling back would not find one either.
		return classad.NewUndefinedValue()
	}
	idx := cs.bindField(name)
	if idx == bindUnknown {
		// The table never interned this name, so no record defines it. Exact, no fallback.
		return classad.NewUndefinedValue()
	}
	if idx == bindColdTail {
		id, ok := cs.c.intern.LookupID(name)
		if !ok {
			return classad.NewUndefinedValue()
		}
		return cs.fromColdTail(id)
	}
	if testBit(cs.blk.escapeAt(cs.k), idx) {
		// Escaped: missing, or present but not storable in its slot. Both live in the cold tail.
		return cs.fromColdTail(cs.blk.schema.fields[idx].id)
	}
	f := cs.blk.schema.fields[idx]
	switch f.kind {
	case akInt:
		v, ok := cs.slotInt(idx, f)
		if !ok {
			cs.fellBack = true
			return classad.NewUndefinedValue()
		}
		return classad.NewIntValue(v)
	case akReal:
		v, ok := cs.slotInt(idx, f)
		if !ok {
			cs.fellBack = true
			return classad.NewUndefinedValue()
		}
		return classad.NewRealValue(math.Float64frombits(uint64(v)))
	case akBool:
		return classad.NewBoolValue(cs.slotBool(f))
	case akString:
		s, ok := cs.slotString(idx)
		if !ok {
			cs.fellBack = true
			return classad.NewUndefinedValue()
		}
		return classad.NewStringValue(s)
	}
	cs.fellBack = true
	return classad.NewUndefinedValue()
}

// fromColdTail reads an attribute the block stores in its cold tail: a non-schema attribute, or a
// schema field whose value escaped. An expression there sets fellBack, since only the ordinary
// evaluator can resolve it against the rest of the ad.
func (cs *colScope) fromColdTail(id uint32) classad.Value {
	node, found, err := cs.blk.escapedNode(cs.k, id, cs.bc)
	if err != nil {
		cs.fellBack = true
		return classad.NewUndefinedValue()
	}
	if !found {
		return classad.NewUndefinedValue() // genuinely absent from this record
	}
	lit, ok := wire.LiteralValue(node)
	if !ok {
		cs.fellBack = true
		return classad.NewUndefinedValue()
	}
	return litToValue(lit)
}

// slotInt reads a numeric field's raw slot value (a real's slot holds math.Float64bits).
func (cs *colScope) slotInt(idx int, f adField) (int64, bool) {
	b := cs.blk
	base := cs.k * b.hotStride
	if off, hot := b.hotFieldOff[idx]; hot {
		return readIntLE(b.hot[base+off:], f.width, f.unsigned), true
	}
	start, ok := b.coldFieldStart[idx]
	if !ok {
		return 0, false
	}
	coldNum, err := cs.bc.stream(b, kindColdNum)
	if err != nil {
		return 0, false
	}
	return readIntLE(coldNum[start+cs.k*f.width:], f.width, f.unsigned), true
}

// slotBool reads a bit-packed boolean. The bool bitset sits immediately after the escape bitmap in
// each record's hot region, so it is an uncompressed strided read like a hot numeric.
func (cs *colScope) slotBool(f adField) bool {
	b := cs.blk
	base := cs.k*b.hotStride + b.schema.escBytes
	return testBit(b.hot[base:base+b.schema.boolBytes], f.boolBit)
}

// slotString reads a string field from the record's string region.
//
// The region is POSITIONAL -- uvarint(len)+bytes for each non-escaped string field in schema order --
// so reaching field idx means walking the string fields before it; there is no fixed offset to jump
// to as there is for a numeric slot. The region itself decompresses once per block (cached), not once
// per record.
//
// The walk is not what costs. Copying was: returning string(region[...]) allocated once per record and
// made a string predicate SLOWER here (6.95ms) than in the row path (6.68ms) over 60k records.
// Aliasing the region instead brings it to 6.09ms with 111KB allocated against the row path's 3.5MB.
// A comparison never needed the copy.
func (cs *colScope) slotString(idx int) (string, bool) {
	b := cs.blk
	if cs.strScratch == nil {
		raw, err := cs.bc.stream(b, kindStr)
		if err != nil {
			return "", false
		}
		cs.strScratch = raw
	}
	region := cs.strScratch[b.strOff[cs.k]:b.strOff[cs.k+1]]
	esc := b.escapeAt(cs.k)
	for i := range b.schema.fields {
		if b.schema.fields[i].kind != akString || testBit(esc, i) {
			continue
		}
		l, n := binary.Uvarint(region)
		if n <= 0 || int(l)+n > len(region) {
			return "", false
		}
		if i == idx {
			// ZERO COPY: alias the decompressed region instead of copying it. A comparison needs no
			// copy, and the copy was the cost -- one Go string allocation per record, which is what
			// made a string predicate slower here than in the row path.
			//
			// Safe unconditionally, not just for the scan's duration: the region is an immutable
			// decompressed buffer that nothing mutates, and the returned string's header keeps the
			// underlying array alive through the GC even if a caller retains the value.
			if l == 0 {
				return "", true
			}
			return unsafe.String(&region[n], int(l)), true
		}
		region = region[n+int(l):]
	}
	return "", false
}

// setBlock rebinds the scope to a new block, dropping per-block caches.
func (cs *colScope) setBlock(blk *columnarBlock) {
	cs.blk = blk
	cs.strScratch = nil
}

// prunePlan caches the zone-prunable probe analysis, which depends only on the query and the SCHEMA.
//
// A scan visits one window per segment -- 162 of them on a 60k-record table -- and they almost always
// share a single schema OBJECT, so recomputing the analysis per window repeated identical work 161 times:
// measured at 11% of a scan's time and 44% of its allocations, for an answer that never changed. Keyed on
// pointer identity, so two equal-but-distinct schemas merely recompute rather than answer wrongly.
// zonePruneAnalyses counts how many times the probe analysis actually ran, so a test can hold the line
// that it runs once per query rather than once per segment. One atomic add per query is free; the
// alternative is a regression that only shows up as a query being mysteriously slower on a table with
// many segments, which is how this cost went unnoticed in the first place.
var zonePruneAnalyses atomic.Int64

type prunePlan struct {
	schema *adSchema
	idxs   []int
	preds  []fieldPred
	valid  bool
}

func (p *prunePlan) tests(c *Collection, q *vm.Query, s *adSchema) ([]int, []fieldPred) {
	if !p.valid || s != p.schema {
		p.idxs, p.preds = c.zonePrunableTests(q, s)
		p.schema, p.valid = s, true
	}
	return p.idxs, p.preds
}

// zonePrunableTests extracts the query's probes that a block zone can reason about: scalar numeric
// comparisons on numeric fields of THIS schema, as (field index, tests).
//
// A SUBSET is sound, and Probes (not ExactProbes) is the right input here: pruning only skips a block
// when some NECESSARY condition cannot hold in it, so omitting conjuncts costs selectivity and never
// correctness. That is the opposite of answering from probes, which needs them to cover the query --
// colScope answers by evaluating the query itself, so it is exact regardless.
func (c *Collection) zonePrunableTests(q *vm.Query, s *adSchema) ([]int, []fieldPred) {
	zonePruneAnalyses.Add(1)
	var idxs []int
	var preds []fieldPred
	for _, p := range q.Probes() {
		id, ok := c.intern.LookupID(p.Attr)
		if !ok {
			continue
		}
		idx, ok := s.byID[id]
		if !ok || !numericKind(s.fields[idx].kind) {
			continue
		}
		up, ok := valUsable(id, p)
		if !ok || len(up.fvals) != 1 {
			continue
		}
		if _, ok := numCmp(up.op, up.fvals[0]); !ok {
			continue
		}
		idxs = append(idxs, idx)
		preds = append(preds, fieldPred{fieldID: id, tests: []zoneTest{{op: up.op, vals: up.fvals}}})
	}
	return idxs, preds
}

// indexCanPrune reports whether any of q's probes names an INDEXED attribute, i.e. whether the
// ordinary row path could narrow to a candidate set instead of scanning everything.
//
// This gates the routing, and the gate is not theoretical: an archive's constrained COUNT(*) was
// measured 2.9x SLOWER than the scan when the scan had an index to prune with and the columnar path
// read every value. Block zones fixed that for numeric ranges, but they cannot help a string equality
// -- there are no numeric zones for a string column -- so an indexed `Owner == "x"` is still better
// served by the posting list than by evaluating 60k records.
func (c *Collection) indexCanPrune(q *vm.Query) bool {
	spec := c.spec.Load()
	if spec == nil || !spec.any() {
		return false
	}
	for _, p := range q.Probes() {
		id, ok := c.intern.LookupID(p.Attr)
		if !ok {
			continue
		}
		if spec.catHas(id) || spec.valHas(id) {
			return true
		}
	}
	return false
}

// ColumnarEvalCount counts the records matching q by evaluating q itself against each record's
// columns, for any NATIVE query.
//
// ok=false when it cannot serve the query at all: the accelerator is off, or the query is not native
// (a delegated subtree needs a real ClassAd scope). Individual records whose stored value is an
// expression are re-evaluated the ordinary way; that never fails the query.
//
// Because it evaluates the query rather than a summary of it, it is exact by construction -- the class
// of bug that required ExactProbes cannot arise here.
func (c *Collection) ColumnarEvalCount(q *vm.Query) (int, bool) {
	st := c.schemaScan.Load()
	if st == nil || c.intern == nil || q == nil || !q.Native() {
		return 0, false
	}
	m := q.Matcher()
	cs := &colScope{bc: st.cache, c: c}
	resolver := cs.resolve // hoisted: a method value expression allocates
	fallbackM := q.Matcher()
	count := 0
	var plan prunePlan
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			seg := w.seg.colblk.Load()
			if seg == nil || seg.schema() == nil {
				count += c.rowEvalWindow(w, s0, fallbackM)
				continue
			}
			pruneIdxs, prunePreds := plan.tests(c, q, seg.schema())
			base := 0
			for _, blk := range seg.blocks {
				if len(prunePreds) > 0 && !blockMayMatch(blk, pruneIdxs, prunePreds) {
					base += blk.n
					continue
				}
				cs.setBlock(blk)
				count += c.countBlockScoped(cs, resolver, m, fallbackM, w, seg, blk, base, s0)
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return count, true
}

// countBlockScoped counts one block's visible matching records with the per-record resolver. Extracted
// so the vectorized scan can fall back to it for a single block it cannot serve, rather than abandoning
// the whole query.
func (c *Collection) countBlockScoped(cs *colScope, resolver func(name string, scope ast.AttributeScope) classad.Value,
	m, fallbackM *vm.Matcher, w segWindow, seg *colSegment, blk *columnarBlock, base int, s0 uint64) int {
	count := 0
	for k := 0; k < blk.n; k++ {
		gk := base + k
		if gk >= len(seg.offs) {
			break
		}
		o := seg.offs[gk]
		if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
			continue
		}
		cs.k, cs.fellBack = k, false
		v := m.EvalResolved(resolver)
		if cs.fellBack {
			// One record needs the ordinary evaluator; the rest of the scan is unaffected.
			if c.evalOneRecord(w, o, fallbackM) {
				count++
			}
			continue
		}
		if isTrueValue(v) {
			count++
		}
	}
	return count
}

// evalOneRecord decodes a single arena record and evaluates the query against it the ordinary way.
func (c *Collection) evalOneRecord(w segWindow, off uint32, m *vm.Matcher) bool {
	ww, err := w.codec.Decompress(nil, recAd(w.data, off))
	if err != nil {
		return false
	}
	node, err := c.decodeWireDict(w.dict(), ww)
	if err != nil {
		return false
	}
	return m.Matches(classad.FromAST(node))
}

// rowEvalWindow counts matches in a window with no columnar block (the active segment, or one the
// accelerator has not reached) by evaluating each visible record the ordinary way.
func (c *Collection) rowEvalWindow(w segWindow, s0 uint64, m *vm.Matcher) int {
	count := 0
	for off := 0; off < w.used; {
		o := uint32(off)
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(w.data, o) && recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
			if c.evalOneRecord(w, o, m) {
				count++
			}
		}
		off += int(total)
	}
	return count
}
