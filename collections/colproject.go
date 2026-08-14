package collections

import (
	"math"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// BUILDING A PROJECTED AD FROM COLUMNS, RATHER THAN REBUILDING THE WHOLE RECORD FIRST.
//
// Serving a projected read from a columnarized segment used to mean reassembling the entire ad -- every
// schema field gathered out of its column, every string materialized, the cold tail appended -- and
// then discarding all but the handful of attributes the caller asked for. Reassembly is the single
// largest cost in a columnar read, and a two-attribute projection paid all of it.
//
// A column, though, is exactly the shape a projection wants: the values of ONE attribute, addressable
// by record. So the projected ad is built by reading only the named columns, plus whatever the caller
// asked for that the schema does not carry -- which is in the record, and the record is small precisely
// because the schema carries the rest.
//
// The header comes from the record too, because it holds MyType/TargetType, which a projection keeps.
//
// WHEN THIS IS ALLOWED TO RUN is the subtle part, and it is not "whenever there is a projection". The
// match still has to see whatever it reads, and a narrowed ad cannot be handed to it -- vm.ReadPlan's
// seed set is not closed, since an attribute's value may be an expression whose own references are
// expanded transitively while decoding, so an ad narrowed to the projection can be missing something
// the query needed. Two cases are safe, and the scan checks for exactly those:
//
//   - the query reads no attribute at all, so it is constant and cannot care what the ad contains, and
//   - the match was already decided FROM the columns, so no ad was needed to decide it.
//
// Everything else reassembles as before.

// projPlan is a projection resolved for the scan: which attributes to keep, and where they live.
type projPlan struct {
	// ids are the projected attributes as global intern ids, for testing schema fields.
	ids map[uint32]struct{}
	// names is the projection as given, for resolving into a segment's local id space.
	names []string
	// localDict/local cache names resolved against one segment's dictionary, since the record half
	// of the output is keyed by local ids and resolving is a dictionary probe per name. Keyed by the
	// dict's identity: a different segment is a different id space, and reusing the mapping there
	// would keep the wrong attributes.
	localDict *segDictHandle
	local     map[uint32]struct{}
}

// newProjPlan resolves a projection for the scan, or returns nil when there is nothing to project.
func (c *Collection) newProjPlan(names []string) *projPlan {
	if len(names) == 0 || c.intern == nil {
		return nil
	}
	p := &projPlan{ids: make(map[uint32]struct{}, len(names)), names: names}
	for _, n := range names {
		if id, ok := c.intern.LookupID(n); ok {
			p.ids[id] = struct{}{}
		}
	}
	return p
}

// localWanted returns the projection resolved into dict's id space.
func (p *projPlan) localWanted(dict *segDictHandle) map[uint32]struct{} {
	if p.localDict == dict && p.local != nil {
		return p.local
	}
	m := make(map[uint32]struct{}, len(p.names))
	for _, n := range p.names {
		if id, ok := dict.lookup(n); ok {
			m[id] = struct{}{}
		}
	}
	p.localDict, p.local = dict, m
	return m
}

// projectScratch holds the buffers a projected build reuses across records.
type projectScratch struct {
	remnant []byte
	narrow  narrowScratch
	entries []byte
	out     []byte
}

// projectFromColumns builds record k's ad restricted to the projection, reading the named columns
// directly and never reassembling the record.
//
// Reports false when it cannot be sure of the result -- a value it cannot read from its column, an
// attribute whose translation into the record's id space is missing, a malformed remnant. The caller
// must then fall back to a full reassembly: this is an optimization, and a projected ad that quietly
// omits an attribute the caller asked for is indistinguishable to them from one the record never had.
func (c *Collection) projectFromColumns(cn *colNative, r recRef, k int, p *projPlan, cs *colScope, sc *projectScratch) ([]byte, bool) {
	blk, local := cn.blockFor(k)
	if blk == nil || blk.schema == nil {
		return nil, false
	}
	sd := cn.dict

	// The record half: its header (which carries MyType/TargetType) and whatever the projection asks
	// for that the schema does not carry. Decompressing it is cheap -- it holds only what the columns
	// do not, which is the whole point of the format.
	raw, err := r.codec().Decompress(sc.remnant[:0], r.stored())
	if err != nil {
		return nil, false
	}
	sc.remnant = raw
	var want map[uint32]struct{}
	if sd != nil {
		want = p.localWanted(sd)
	}
	narrowed, ok := narrowAd(&sc.narrow, raw, func(id uint32, name string) bool {
		if name != "" {
			// An inline entry: keep it and let the consumer's own keep test decide. Cheap, and being
			// generous here is safe -- being stingy would drop a requested attribute.
			return true
		}
		if want == nil {
			_, in := p.ids[id]
			return in
		}
		_, in := want[id]
		return in
	})
	if !ok {
		return nil, false
	}
	hdr, entries, n, inline, ok := wire.Ad(narrowed).SplitBody()
	if !ok {
		return nil, false
	}

	// The column half: only the projected attributes, read from their own columns.
	cs.setBlock(blk)
	cs.setGroups(cn.seg, cn.blockIndexOf(blk))
	cs.k, cs.fellBack = local, false
	sc.entries = append(sc.entries[:0], entries...)
	added := 0
	esc := blk.escapeAt(local)
	for idx := range blk.schema.fields {
		f := &blk.schema.fields[idx]
		if _, in := p.ids[f.id]; !in {
			continue
		}
		outID := f.id
		if sd != nil {
			lid, ok := cn.localOf[f.id]
			if !ok {
				return nil, false
			}
			outID = lid
		}
		name := ""
		if inline {
			nm, ok := c.intern.Name(f.id)
			if !ok {
				return nil, false
			}
			name = nm
		}
		if testBit(esc, idx) {
			// Escaped: absent from this record, or present but too wide for its slot. Both live in
			// the cold tail, and both are LITERALS -- an expression is never moved into a column (see
			// storableInColumn) -- so a found node is appended exactly as stored.
			node, found, err := blk.escapedNode(local, f.id, cs.bc)
			if err != nil {
				return nil, false
			}
			if !found {
				continue // genuinely absent from this record: the projection simply omits it
			}
			sc.entries = wire.AppendKey(sc.entries, inline, outID, name)
			sc.entries = append(sc.entries, node...)
			added++
			continue
		}
		sc.entries = wire.AppendKey(sc.entries, inline, outID, name)
		sc.entries, ok = cs.appendSlotNode(sc.entries, idx, f)
		if !ok {
			return nil, false
		}
		if cs.fellBack {
			return nil, false
		}
		added++
	}
	// Attributes the base schema does not carry. They used to be in the record; on an interned
	// segment the rewrite moves them into the block's COLD TAIL, so a projection naming one has to
	// look there or it comes back empty for every record that has it.
	for id := range p.ids {
		if _, isField := blk.schema.byID[id]; isField {
			continue // already emitted from its column above
		}
		node, found, err := blk.escapedNode(local, id, cs.bc)
		if err != nil {
			return nil, false
		}
		if !found {
			continue // not in the tail either: genuinely absent, or still in the record
		}
		outID := id
		name := ""
		if sd != nil {
			// A cold-tail entry is already keyed by the segment dictionary, which is the record's own
			// key space -- so it is emitted as it was stored, with no translation.
			outID = blk.remap.stored(id)
		} else if inline {
			nm, ok := c.intern.Name(id)
			if !ok {
				return nil, false
			}
			name = nm
		}
		sc.entries = wire.AppendKey(sc.entries, inline, outID, name)
		sc.entries = append(sc.entries, node...)
		added++
	}

	// The GROUP columns, restricted to the projection. A group attribute is not a base schema field,
	// so the loop above does not reach it, and for a record belonging to its group wholly it is not in
	// the record either -- the rewrite moved it into the group's column. Without this a projection
	// naming a group attribute would come back empty for exactly the records that have one.
	if len(cn.seg.groups) > 0 {
		var gerr error
		sc.entries, added, gerr = cn.appendGroupValues(c, sc.entries, added, k, inline, sd, p.ids)
		if gerr != nil {
			return nil, false
		}
	}
	sc.out = wire.BuildAd(sc.out[:0], hdr, n+added, sc.entries)
	return sc.out, true
}

// appendSlotNode encodes schema field idx's in-slot value for the current record as a wire node.
//
// It mirrors adSchema.forEach's encoding rather than routing through a classad.Value, so a projected
// read produces the same node bytes a full reassembly would -- the two paths have to be
// interchangeable, and "same value, different encoding" would make them merely similar.
func (cs *colScope) appendSlotNode(dst []byte, idx int, f *adField) ([]byte, bool) {
	switch f.kind {
	case akBool:
		return wire.AppendBoolNode(dst, cs.slotBool(*f)), true
	case akInt:
		v, ok := cs.slotInt(idx, *f)
		if !ok {
			return dst, false
		}
		return wire.AppendIntNode(dst, v), true
	case akReal:
		v, ok := cs.slotInt(idx, *f)
		if !ok {
			return dst, false
		}
		return wire.AppendRealNode(dst, math.Float64frombits(uint64(v))), true
	case akString:
		s, ok := cs.slotString(idx)
		if !ok {
			return dst, false
		}
		return wire.AppendStringNode(dst, s), true
	}
	return dst, false
}
