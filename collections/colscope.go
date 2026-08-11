package collections

import (
	"math"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// PROTOTYPE: evaluating a query directly against columnar storage.
//
// The evaluator is already backing-agnostic -- vm.Matcher.EvalResolved takes a resolver and never
// materializes a ClassAd -- and wireScope is a working resolver over an ad's WIRE bytes. What has
// been missing is the columnar equivalent, which is why the columnar path only ever served
// hand-written predicate shapes (one numeric comparison, or one presence probe) and everything else
// fell back to decoding whole ads.
//
// A resolver over a block removes that restriction in principle: any NATIVE expression whose
// references are reachable from the block can be evaluated per record with no decode. This file is
// the narrow version of that, built to answer one question before the full one gets written: does
// per-record interpreter dispatch actually beat the row path by enough to be worth it?
//
// SCOPE OF THE PROTOTYPE -- it gives up (fellBack) rather than handling:
//   - string and bool fields (only int/real slots are read)
//   - attributes with no schema field, which live in the cold tail
//   - non-literal (expression) values
//   - TARGET/PARENT scopes
//
// Each of those is mechanical to add; none of them changes the performance question.

// colScope resolves attribute references for record k of a columnar block.
//
// Reused across a scan (single-threaded): set k and clear fellBack before each evaluation, exactly
// as wireScope is reused across a row scan.
type colScope struct {
	blk      *columnarBlock
	k        int
	bc       *blockCache
	c        *Collection
	fellBack bool

	// bind caches name -> schema field index (-1 = the table does not know this attribute) for the
	// schema in bindFor. A query's references are fixed, so resolving them per RECORD is pure waste:
	// InternTable.LookupID takes a mutex and a defer on every call and may fold the name, and
	// schema.byID is another map hit. Binding once per (schema, name) turns the per-record cost into
	// a small map lookup, and a real implementation would go further and bind the query's references
	// to indices ONCE up front, making the per-record path an array index.
	bind    map[string]int
	bindFor *adSchema
}

// bindField returns name's field index for the current block's schema, caching the answer. -1 means
// the attribute has no schema field (or the table has never seen it, distinguished by known).
func (cs *colScope) bindField(name string) (idx int, known bool) {
	if cs.bindFor != cs.blk.schema {
		cs.bind, cs.bindFor = make(map[string]int, 8), cs.blk.schema
	}
	if v, ok := cs.bind[name]; ok {
		return v, v != -2
	}
	v := -1
	id, ok := cs.c.intern.LookupID(name)
	if !ok {
		v = -2 // unknown to the table: no record has it
	} else if i, ok := cs.blk.schema.byID[id]; ok {
		v = i
	}
	cs.bind[name] = v
	return v, v != -2
}

// resolve is the attribute resolver handed to vm.Matcher.EvalResolved.
func (cs *colScope) resolve(name string, scope ast.AttributeScope) classad.Value {
	if scope != ast.NoScope && scope != ast.MyScope {
		cs.fellBack = true // TARGET/PARENT: out of scope for the prototype
		return classad.NewUndefinedValue()
	}
	idx, known := cs.bindField(name)
	if !known {
		// The table has never seen this attribute, so no record has it: undefined, and that is
		// exact -- no need to fall back.
		return classad.NewUndefinedValue()
	}
	if idx < 0 {
		cs.fellBack = true // lives in the cold tail; the full version reads it from there
		return classad.NewUndefinedValue()
	}
	if testBit(cs.blk.escapeAt(cs.k), idx) {
		// Escaped: missing, or present but not storable in the slot. Read just this field's node.
		node, found, err := cs.blk.escapedNode(cs.k, cs.blk.schema.fields[idx].id, cs.bc)
		if err != nil {
			cs.fellBack = true
			return classad.NewUndefinedValue()
		}
		if !found {
			return classad.NewUndefinedValue() // genuinely absent: exact
		}
		lit, ok := wire.LiteralValue(node)
		if !ok {
			cs.fellBack = true // an expression: only evaluation in the ad's scope can resolve it
			return classad.NewUndefinedValue()
		}
		return litToValue(lit)
	}
	f := cs.blk.schema.fields[idx]
	switch f.kind {
	case akInt:
		v, ok := cs.blk.fieldIntAt(cs.k, idx, cs.bc)
		if !ok {
			cs.fellBack = true
			return classad.NewUndefinedValue()
		}
		return classad.NewIntValue(v)
	case akReal:
		v, ok := cs.blk.fieldIntAt(cs.k, idx, cs.bc)
		if !ok {
			cs.fellBack = true
			return classad.NewUndefinedValue()
		}
		return classad.NewRealValue(math.Float64frombits(uint64(v)))
	}
	cs.fellBack = true // string/bool: not in the prototype
	return classad.NewUndefinedValue()
}

// rowEvalWindow counts matches in a window with no columnar block, by decoding each visible record.
func (c *Collection) rowEvalWindow(w segWindow, s0 uint64, m *vm.Matcher) (matches, records int, ok bool) {
	count := 0
	var buf []byte
	for off := 0; off < w.used; {
		o := uint32(off)
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(w.data, o) && recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
			ww, err := w.codec.Decompress(buf[:0], recAd(w.data, o))
			if err != nil {
				return 0, 0, false
			}
			buf = ww
			a, err := c.decodeWireDict(w.dict(), ww)
			if err != nil {
				return 0, 0, false
			}
			records++
			if m.Matches(classad.FromAST(a)) {
				count++
			}
		}
		off += int(total)
	}
	return count, records, true
}

// lastColEvalSplit records how many records the last colEvalCount served columnar vs by full decode.
// A benchmark that does not check this can be measuring the decode of a blockless active segment and
// calling it a columnar result -- which is exactly what happened the first time this was measured.
var lastColEvalSplit [2]int

// fieldIntAt reads one record's raw value for a numeric field: the hot region strided read for a hot
// column, or the cold numeric column group (decompressed once, cached) for a cold one. A real's slot
// holds math.Float64bits, so the caller converts. Reports false when the field is not numeric.
//
// scanInt already does this for EVERY record; a resolver needs exactly one, since the query decides
// which fields it touches rather than the scan deciding for it.
func (b *columnarBlock) fieldIntAt(k, fieldIdx int, bc *blockCache) (int64, bool) {
	f := b.schema.fields[fieldIdx]
	if off, hot := b.hotFieldOff[fieldIdx]; hot {
		base := k * b.hotStride
		return readIntLE(b.hot[base+off:], f.width, f.unsigned), true
	}
	start, ok := b.coldFieldStart[fieldIdx]
	if !ok {
		return 0, false
	}
	coldNum, err := bc.stream(b, kindColdNum)
	if err != nil {
		return 0, false
	}
	return readIntLE(coldNum[start+k*f.width:], f.width, f.unsigned), true
}

// colEvalCount counts the records matching q by evaluating q itself against each record's columns.
//
// Returns ok=false the moment any record needs something the prototype cannot serve, so a partial
// count is never returned. The full version would fall back per record instead of abandoning the
// query, the way matchWire does.
func (c *Collection) colEvalCount(q *vm.Query) (int, bool) {
	st := c.schemaScan.Load()
	if st == nil || c.intern == nil || q == nil || !q.Native() {
		return 0, false
	}
	colRecs, rowRecs := 0, 0
	defer func() { lastColEvalSplit = [2]int{colRecs, rowRecs} }()
	m := q.Matcher()
	cs := &colScope{bc: st.cache, c: c}
	resolver := cs.resolve // hoisted: a method value expression allocates a closure each time
	count := 0
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			seg := w.seg.colblk.Load()
			if seg == nil || seg.schema() == nil {
				// No block (the active segment, or a segment the accelerator has not reached):
				// evaluate those records the ordinary way. The full version would use matchWire
				// here; for a measurement a decode is fine, and the active segment is a small
				// fraction of a sealed-heavy table.
				n, rec, ok := c.rowEvalWindow(w, s0, m)
				if !ok {
					releaseWindows(wins)
					return 0, false
				}
				count += n
				rowRecs += rec
				continue
			}
			base := 0
			for _, blk := range seg.blocks {
				cs.blk = blk
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
						releaseWindows(wins)
						return 0, false
					}
					colRecs++
					if isTrueValue(v) {
						count++
					}
				}
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return count, true
}
