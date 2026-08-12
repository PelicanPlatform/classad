package collections

import (
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Presence over the columnar accelerator: `attr is undefined` and `attr isnt undefined`.
//
// The escape bitmap already holds the answer for the common case. A schema field's escape bit is
// clear exactly when its value sits in the record's fixed slot, which means the attribute is present
// AND stored as a literal of the field's kind -- so it is defined, with no decode of any sort. On a
// fitted schema the overwhelming majority of records are in that state, so a presence count is the
// same strided bitmap read a MIN/MAX does.
//
// Before this, `count(*) where ProcId is undefined` was declined by the columnar path (numPredOnField
// wants a scalar comparison and a presence probe carries no value), so it fell back to the generic row
// scan: decompress every record and evaluate the whole constraint against a decoded ad. Measured on a
// production jobs table, `max(ProcId)` took 120ms and `count(*) where ProcId is undefined` took
// 1.124s over the same records.
//
// WHY THIS NEEDS CARE. `X is undefined` is `X =?= UNDEFINED`, which is an EVALUATION, not a presence
// test. Present-and-defined are not the same thing:
//
//   - escape bit clear -> a literal of the field's kind is in the slot -> defined.
//   - escape bit set -> the attribute is missing, or present but not storable in the slot. Look it
//     up in the cold tail:
//   - not there at all -> genuinely absent -> UNDEFINED.
//   - a literal -> defined, whatever its kind. A ProcId stored as a string escaped on TYPE, and a
//     string is not undefined.
//   - the UNDEFINED literal itself -> UNDEFINED. Present is not the same as defined.
//   - the ERROR literal -> not undefined (`ERROR =?= UNDEFINED` is false).
//   - an EXPRESSION -> its value depends on the rest of the ad, so it cannot be judged from the
//     node. The scan DECLINES and the caller falls back to the row path.
//
// Declining on an expression is the whole safety argument. A wrong count here would be silent, and
// an expression-valued numeric attribute is exceptional by construction -- it escapes -- so the
// decline costs nothing on data that does not have any.

// presencePred describes a presence predicate the columnar path can serve.
type presencePred struct {
	fieldID    uint32
	wantAbsent bool // true for `is undefined`, false for `isnt undefined`
}

// presencePredOnField analyzes q as a SINGLE presence probe on one field of schema s.
//
// ok=false for anything else: a non-native query, no probes, more than one probe, a probe that is
// not present/absent, or an attribute the schema does not carry. Unlike numPredOnField this accepts
// any schema field kind, since presence does not depend on the value's type.
func (c *Collection) presencePredOnField(q *vm.Query, s *adSchema) (presencePred, bool) {
	probes := q.Probes()
	if !q.Native() || len(probes) != 1 {
		return presencePred{}, false
	}
	p := probes[0]
	if p.Op != "absent" && p.Op != "present" {
		return presencePred{}, false
	}
	id, ok := c.intern.LookupID(p.Attr)
	if !ok {
		return presencePred{}, false
	}
	if _, ok := s.byID[id]; !ok {
		return presencePred{}, false
	}
	return presencePred{fieldID: id, wantAbsent: p.Op == "absent"}, true
}

// nodeIsUndefined reports whether a wire node is UNDEFINED for `=?= UNDEFINED` purposes.
//
// ok=false means the node is an expression: it cannot be judged without evaluating it against the
// rest of the ad, so the caller must decline rather than guess.
func nodeIsUndefined(node []byte) (undefined bool, ok bool) {
	lit, isLit := wire.LiteralValue(node)
	if !isLit {
		return false, false // an expression
	}
	return lit.Kind == wire.LitUndef, true
}

// PresenceCountQuery counts the records matching a single `attr is undefined` / `attr isnt
// undefined` predicate over the columnar accelerator.
//
// Returns ok=false when the columnar path cannot serve it -- the accelerator is off, the predicate is
// not a lone presence probe on a schema field, or some record's value for the attribute is an
// EXPRESSION, which needs evaluating. A caller that gets false scans instead.
func (c *Collection) PresenceCountQuery(q *vm.Query) (int, bool) {
	st := c.schemaScan.Load()
	if st == nil || c.intern == nil || q == nil {
		return 0, false
	}
	pred, ok := c.presencePredOnField(q, st.schema)
	if !ok {
		return 0, false
	}
	// Record the read, as the numeric paths do, so serving a column faster does not make it
	// invisible to the hot-tier demand signal.
	if name, ok := c.schemaFieldName(pred.fieldID); ok {
		c.demand.recordReads([]string{name})
	}
	return c.schemaScanPresenceCount(pred, st.cache)
}

// schemaScanPresenceCount counts live records whose attribute is (or is not) undefined.
//
// Per sealed segment it resolves the field against that segment's OWN schema -- segments sealed under
// different schemas coexist -- and reads the escape bitmap. A segment whose schema lacks the field,
// and the active segment (which has no block), fall back to a row walk that reads only that one
// attribute rather than decoding the whole ad. Returns ok=false if any visible record's value is an
// expression.
func (c *Collection) schemaScanPresenceCount(pred presencePred, bc *blockCache) (int, bool) {
	// The row fallback must honor the collection's wire mode: inline names on a persistent store,
	// interned ids in memory.
	lookup := func(a wire.Ad) ([]byte, bool) { return a.Lookup(pred.fieldID) }
	if c.inline {
		if name, ok := c.intern.Name(pred.fieldID); ok {
			lookup = func(a wire.Ad) ([]byte, bool) { return a.LookupByName(name) }
		}
	}
	count := 0
	tally := func(undefined bool) {
		if undefined == pred.wantAbsent {
			count++
		}
	}
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			idx := -1
			if sch := cs.schema(); sch != nil {
				if i, ok := sch.byID[pred.fieldID]; ok {
					idx = i
				}
			}
			if idx < 0 {
				if !brutePresence(w, s0, lookup, tally) {
					releaseWindows(wins)
					return 0, false
				}
				continue
			}
			base := 0
			for _, blk := range cs.blocks {
				// No record in this block carries the attribute: every visible one of them is
				// undefined, settled without touching a bitmap per record or the cold tail at all.
				if blk.fieldAbsentFromBlock(idx) {
					for k := 0; k < blk.n; k++ {
						gk := base + k
						if gk >= len(cs.offs) {
							break
						}
						o := cs.offs[gk]
						if recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
							tally(true)
						}
					}
					base += blk.n
					continue
				}
				for k := 0; k < blk.n; k++ {
					gk := base + k
					if gk >= len(cs.offs) {
						break
					}
					o := cs.offs[gk]
					if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
						continue // not visible at this snapshot
					}
					if !testBit(blk.escapeAt(k), idx) {
						tally(false) // value is in its fixed slot: present and a literal
						continue
					}
					// Escaped: missing, or present but not storable in the slot. The block's
					// per-field escape class often settles that without reading anything --
					// measured at 95.7% of escapes on real ads, since a field whose values all
					// fit its slot escapes ONLY by being absent.
					if missing, ok := blk.escapeIsMissing(idx, k); ok && missing {
						tally(true)
						continue
					}
					// Either the field genuinely mixes the two, or this block predates the
					// classification. Read just this field from the cold tail -- not the whole
					// record.
					node, found, err := blk.escapedNode(k, pred.fieldID, bc)
					if err != nil {
						releaseWindows(wins)
						return 0, false
					}
					if !found {
						tally(true) // genuinely absent
						continue
					}
					undef, ok := nodeIsUndefined(node)
					if !ok {
						releaseWindows(wins) // an expression: only evaluation can answer
						return 0, false
					}
					tally(undef)
				}
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return count, true
}

// brutePresence is the row fallback: walk a window's visible records and classify the attribute from
// its wire node. It reads ONE attribute per record rather than decoding the ad. Returns false if a
// value is an expression, which the caller must answer by evaluation.
func brutePresence(w segWindow, s0 uint64, lookup func(wire.Ad) ([]byte, bool), tally func(bool)) bool {
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
				return false
			}
			buf = ww
			node, found := lookup(wire.Ad(ww))
			if !found {
				tally(true)
			} else {
				undef, ok := nodeIsUndefined(node)
				if !ok {
					return false
				}
				tally(undef)
			}
		}
		off += int(total)
	}
	return true
}
