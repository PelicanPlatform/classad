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
//
// WHICH CONSUMERS MAY ASK FOR IT is the other half, and it is a property of the consumer rather
// than of the scan. A narrowed ad is enough to SHIP: the entries are emitted as stored, which is
// what QueryRawWire does. It is not enough to EVALUATE, because a kept attribute may be an
// expression whose references were narrowed away, and evaluating it then yields UNDEFINED rather
// than an error. A consumer that follows references -- QueryRawWireRefs, and QueryRawProjected
// under chaseRefs -- passes a closure plan (projPlan.closure): the scan keeps a narrowed ad only
// when it is self-contained and reassembles the rest. QueryProject, which evaluates, passes no
// plan at all; see the note there.

// projPlan is a projection resolved for the scan: which attributes to keep, and where they live.
type projPlan struct {
	// ids are the projected attributes as global intern ids, for testing schema fields.
	ids map[uint32]struct{}
	// names is the projection as given, for resolving into a segment's local id space.
	names []string
	// nameHashes are names' case-folded hashes (wire.NameHash32), for testing an inline-keyed
	// entry of the record by name.
	nameHashes []uint32
	// localDict/local cache names resolved against one segment's dictionary, since the record half
	// of the output is keyed by local ids and resolving is a dictionary probe per name. Keyed by the
	// dict's identity: a different segment is a different id space, and reusing the mapping there
	// would keep the wrong attributes.
	localDict *segDictHandle
	local     map[uint32]struct{}
	// closure marks a plan whose consumer will EVALUATE the projected ad rather than just
	// relay it. Such a plan is still used, but each projected record is checked for being
	// self-contained (adSelfClosed) and reassembled when it is not.
	closure bool
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
		p.nameHashes = append(p.nameHashes, wire.NameHash32(n))
	}
	return p
}

// wantsName reports whether name, an inline-keyed entry's name, is one of the projected names,
// compared case-insensitively as attribute names are.
func (p *projPlan) wantsName(name string) bool {
	h := wire.NameHash32(name)
	for i, ph := range p.nameHashes {
		if ph == h && foldEqualStrings(name, p.names[i]) {
			return true
		}
	}
	return false
}

// foldEqualStrings is wire.FoldEqualBytes for two strings: equal under ASCII case folding, the
// folding wire.NameHash32 applies.
func foldEqualStrings(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
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
			// An inline entry (an inline-keyed segment's remnant holds nothing else): keep it only if
			// it is projected. Every consumer's own keep test admits a subset of the plan's names, so
			// nothing it wants is dropped. Keeping the rest instead would hand a closure plan's check
			// (adSelfClosed) every expression in the remnant, and one of them reaching outside the
			// projection sends a record that never asked for it back to reassembly.
			return p.wantsName(name)
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

// selfClosedScratch is adSelfClosed's reusable working set, held by the scan so the check
// allocates nothing per record.
type selfClosedScratch struct {
	ids   []uint32
	refs  []uint32
	names [][]byte
	nrefs [][]byte
}

// adSelfClosed reports whether every attribute reference the ad's own nodes make resolves to
// an attribute the ad itself carries.
//
// This is the question the blanket "evaluating consumers get no plan" rule was standing in
// for, and it is a weaker one. A projected ad is unsafe to evaluate only when it kept an
// EXPRESSION whose references were narrowed away; an ad of literals refers to nothing and is
// perfectly self-contained however hard it was narrowed. Asking per record lets the common
// projection -- all literals -- keep the columnar fast path, and makes only the records that
// really need the rest of the ad pay for it.
//
// The walk is in whichever key space the record is in, for the reason narrowAdRefs gives:
// an interned record's references are its segment's own dictionary ids, and an inline one's
// are names. A record it cannot read is reported NOT closed, so the caller reassembles --
// the safe direction, since a wrongly-kept fast path ships a row that evaluates to undefined.
func adSelfClosed(w []byte, sc *selfClosedScratch) bool {
	_, _, _, inline, ok := wire.Ad(w).SplitBody()
	if !ok {
		return false
	}
	if inline {
		sc.names = sc.names[:0]
		if !wire.Ad(w).ForEachNameNode(func(name, _ []byte) bool {
			sc.names = append(sc.names, name)
			return true
		}) {
			return false
		}
		closed := true
		if !wire.Ad(w).ForEachNameNode(func(_, node []byte) bool {
			sc.nrefs = wire.AppendNodeRefNames(node, sc.nrefs[:0])
			for _, r := range sc.nrefs {
				if len(r) == 0 {
					continue
				}
				found := false
				for _, have := range sc.names {
					if wire.FoldEqualNames(r, have) {
						found = true
						break
					}
				}
				if !found {
					closed = false
					return false
				}
			}
			return true
		}) {
			return false
		}
		return closed
	}
	sc.ids = sc.ids[:0]
	if !wire.Ad(w).ForEachRaw(func(id uint32, name string, _ []byte) bool {
		if name != "" {
			// An inline entry inside an interned record: it has no id to match a reference
			// against, so treat the record as unreadable for this purpose rather than
			// guessing. Rare, and the cost of being wrong is a row that cannot evaluate.
			id = 0
		}
		sc.ids = append(sc.ids, id)
		return true
	}) {
		return false
	}
	closed := true
	if !wire.Ad(w).ForEachRaw(func(_ uint32, _ string, node []byte) bool {
		sc.refs = wire.AppendNodeRefIDs(node, sc.refs[:0])
		for _, r := range sc.refs {
			found := false
			for _, have := range sc.ids {
				if have == r {
					found = true
					break
				}
			}
			if !found {
				closed = false
				return false
			}
		}
		return true
	}) {
		return false
	}
	return closed
}

// closureMissGiveUp is how many CONSECUTIVE records may fail the self-contained check before
// a scan stops attempting the columnar projection at all. Whether a projection's expressions
// reach outside it is a property of the data, so it is discovered rather than declared --
// and once discovered, every further attempt is pure waste ahead of the reassembly that
// follows it. Small, because the attempt is not free and the signal is consistent: a
// projection either names expressions that escape or it does not.
const closureMissGiveUp = 8
