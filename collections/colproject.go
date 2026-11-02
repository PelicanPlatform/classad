package collections

import (
	"encoding/binary"
	"math"
	"slices"

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
// match still has to see whatever it reads, and an ad narrowed to the PROJECTION cannot be handed to
// it -- an attribute's value may be an expression whose own references are expanded transitively while
// decoding, so the projection can be missing something the query needed. Two cases are safe, and the
// scan checks for exactly those:
//
//   - the query reads no attribute at all, so it is constant and cannot care what the ad contains, and
//   - the match was already decided without the whole ad: from the columns, or on the record narrowed
//     to what the QUERY reads, built here too and checked closed under its references (adClosed).
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
	// closure marks a plan whose consumer FOLLOWS REFERENCES out of the entries it is given --
	// the refs-chasing wire-row and text projections, and the evaluating projection. Such a plan
	// is still used, but each projected record is checked for being closed (adClosed) and
	// reassembled when it is not.
	closure bool
	// evaluates marks the narrower set of those whose consumer only ever sees the EVALUATED
	// result, never the entries themselves. It is a strictly stronger claim than closure, and
	// the two must not be conflated: QueryRawWireRefs follows references and then SHIPS what it
	// was given, so it sets closure and not this.
	//
	// It licenses exactly one thing -- substituting a precomputed literal for an escaped
	// expression cell (see projectFromColumns) -- and it is the only licence that would be
	// sound, because such a substitution changes what the ad CONTAINS while leaving what it
	// MEANS alone. A relaying consumer would ship the difference.
	evaluates bool
	// readsSealed marks a narrowed-match plan naming an attribute the collection seals. Matching
	// on such a record opens the sealed value (a decrypt and a partial decode), which makes the
	// narrowed match dearer than the column conjuncts the scan otherwise asks after it; see
	// refVisitorState.
	readsSealed bool
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

// wantsNameBytes is wantsName for a name held as bytes (a reference read out of a node).
func (p *projPlan) wantsNameBytes(name []byte) bool {
	h := wire.NameHash32Bytes(name)
	for i, ph := range p.nameHashes {
		if ph == h && wire.FoldEqualBytes(name, p.names[i]) {
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
	remnant  []byte
	expanded []byte // the remnant with its dictionary references expanded, when it held any
	narrow   narrowScratch
	entries  []byte
	out      []byte
	// coldBlk/coldPlan key coldWant: the plan's attributes as the ids the block's cold tail stores
	// them under (one translation per block, not per record), and how many of them are not schema
	// fields -- the ones the tail is the only home of.
	coldBlk       *columnarBlock
	coldPlan      *projPlan
	coldWant      []uint32
	coldNonSchema int
	// substIdx/substNode are the escaped schema cells of the CURRENT record whose precomputed
	// literal the column half substitutes for the stored expression, and substID/substName the
	// same cells in the record's own key space so the record half can leave them out. Per
	// record, reused across them.
	substIdx  []int
	substNode [][]byte
	substID   []uint32
	substName []string
	// substCand is which PROJECTED schema fields could have a computed cell in this block at
	// all, resolved once per block rather than per record. Whether a given record's cell is
	// computed still has to be asked per record; this only stops the per-record pass walking
	// every schema field of a wide schema to discover that none of them can be.
	substBlk  *columnarBlock
	substPlan *projPlan
	substCand []int
}

// substCandidates returns the projected schema fields of blk that have a computed section, caching
// per block and plan. A field with no section cannot have a computed cell in any record of the
// block, so the per-record pass need not consider it.
func (sc *projectScratch) substCandidates(blk *columnarBlock, p *projPlan) []int {
	if sc.substBlk == blk && sc.substPlan == p {
		return sc.substCand
	}
	sc.substBlk, sc.substPlan = blk, p
	sc.substCand = sc.substCand[:0]
	if len(blk.computed) == 0 {
		return sc.substCand
	}
	for idx := range blk.schema.fields {
		if _, in := p.ids[blk.schema.fields[idx].id]; !in {
			continue
		}
		if _, ok := blk.computedEntryOf(idx); ok {
			sc.substCand = append(sc.substCand, idx)
		}
	}
	return sc.substCand
}

// substNodeFor returns the precomputed literal collected for schema field idx, if any. Linear over
// a list that holds one entry per PROJECTED expression-valued field -- a handful at most, and
// usually none -- which is cheaper here than a map allocated per record.
func (sc *projectScratch) substNodeFor(idx int) ([]byte, bool) {
	for i, x := range sc.substIdx {
		if x == idx {
			return sc.substNode[i], true
		}
	}
	return nil, false
}

// coldWanted returns p's attributes in blk's cold-tail id space, and how many are not schema fields.
func (sc *projectScratch) coldWanted(blk *columnarBlock, p *projPlan) ([]uint32, int) {
	if sc.coldBlk == blk && sc.coldPlan == p {
		return sc.coldWant, sc.coldNonSchema
	}
	sc.coldWant, sc.coldNonSchema = sc.coldWant[:0], 0
	for id := range p.ids {
		sid, ok := blk.remap.storedID(id)
		if !ok {
			continue // no record of this payload has it in its tail
		}
		sc.coldWant = append(sc.coldWant, sid)
		if _, isField := blk.schema.byID[id]; !isField {
			sc.coldNonSchema++
		}
	}
	sc.coldBlk, sc.coldPlan = blk, p
	return sc.coldWant, sc.coldNonSchema
}

// projectFromColumns builds record k's ad restricted to p's attributes, reading the named columns
// directly and never reassembling the record.
//
// The result is the reassembled record (spliceInto) with every other attribute removed, in the same
// order: the remnant's entries p names, then p's schema fields held in their slots in schema order,
// then p's attributes in the block's cold tail in the order the tail stores them (escaped schema
// fields and non-schema attributes alike), then p's group-column values. Keeping the order is what
// lets a consumer that walks the ad -- a text projection renders in ad order -- produce the same
// bytes from either.
//
// Two callers. The projection fast path builds the projected ad it will emit. The narrowed match
// builds the record restricted to what the query reads, to evaluate the query on it (see adClosed
// for when that is exact).
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
	// Expanded before anything looks inside it: a dictionary reference is not a value (colremdict.go).
	// One that does not resolve sends the record to the full read, which refuses it.
	if exp, expanded, eerr := blk.expandRemnant(raw, cs.bc, sc.expanded[:0]); eerr != nil {
		return nil, false
	} else if expanded {
		sc.expanded = exp
		raw = exp
	}
	if c.deltaRead && isDeltaRecord(raw) {
		return nil, false // a delta's bytes are one write's changes, not the record: only the full read merges them
	}
	var want map[uint32]struct{}
	if sd != nil {
		want = p.localWanted(sd)
	}

	// SUBSTITUTING A PRECOMPUTED VALUE FOR AN ESCAPED EXPRESSION CELL.
	//
	// A schema field whose value is an expression is escaped out of its column and stays in the
	// record, so a projection naming one cannot be served from the columns: the expression comes
	// back in the narrowed ad, the closure check rejects it (its references are the attributes the
	// projection did not name), and after a few such records the scan stops trying the fast path
	// at all. One ordinary attribute -- a usage figure written over another -- therefore costs the
	// whole projection its columnar path, for every row, because every row has one.
	//
	// The value is already there. Columnarizing computes such a cell once and keeps the literal
	// beside the block (colcomputed.go), which is how the aggregates read these attributes from
	// columns today. This substitutes it here as well.
	//
	// ONLY FOR AN EVALUATING PLAN, and that is the whole of the safety argument. The storage
	// layer's rule is that a computed value never appears in an ad as if it were the attribute's
	// stored literal -- a consumer that RELAYS what it is given would then ship an ad different
	// from the record. projPlan.evaluates marks the plans whose consumer sees only the evaluated
	// result, which is by construction the value the expression has.
	//
	// NOT projPlan.closure, which is the near miss: closure means the consumer follows
	// references, and QueryRawWireRefs does that and then ships the entries. Gating on it would
	// put computed literals into relayed rows.
	//
	// It also makes the closure check pass rather than circumventing it: the substituted entry is
	// a literal, so the walk from it reaches nothing, and the narrowed ad is self-contained for
	// real.
	sc.substIdx, sc.substNode = sc.substIdx[:0], sc.substNode[:0]
	sc.substID, sc.substName = sc.substID[:0], sc.substName[:0]
	if p.evaluates {
		esc0 := blk.escapeAt(local)
		for _, idx := range sc.substCandidates(blk, p) {
			f := &blk.schema.fields[idx]
			if !testBit(esc0, idx) {
				continue
			}
			node, ok := blk.computedNode(local, idx, cs.bc)
			if !ok {
				continue
			}
			if sd != nil {
				lid, ok := cn.localOf[f.id]
				if !ok {
					continue
				}
				sc.substID = append(sc.substID, lid)
			} else {
				nm, ok := c.intern.Name(f.id)
				if !ok {
					continue
				}
				sc.substName = append(sc.substName, nm)
			}
			sc.substIdx = append(sc.substIdx, idx)
			sc.substNode = append(sc.substNode, node)
		}
	}

	narrowed, ok := narrowAd(&sc.narrow, raw, func(id uint32, name string) bool {
		if name != "" {
			// An inline entry (an inline-keyed segment's remnant holds nothing else): keep it only if
			// it is one of p's names. Every consumer's own keep test admits a subset of them, so
			// nothing it wants is dropped, and a narrowed match needs nothing else. Keeping the rest
			// instead would hand the closure check (adClosed) every expression in the remnant, and
			// one of them reaching outside the plan sends a record that never asked for it back to
			// reassembly.
			if !p.wantsName(name) {
				return false
			}
			// Substituted below, from the computed region: keeping the stored expression too
			// would put the attribute in the ad twice.
			for _, nm := range sc.substName {
				if foldEqualStrings(name, nm) {
					return false
				}
			}
			return true
		}
		if slices.Contains(sc.substID, id) {
			return false
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
	if inline && sd != nil {
		// An inline-keyed record in an interned segment: its cold-tail entries are keyed by the
		// segment dictionary, not by this process's ids, and the cold-tail walk below names an inline
		// record's entries through the latter. The reassembly names them through the dictionary; leave
		// the record to it.
		return nil, false
	}

	// The column half: only p's schema fields held in their slots. An escaped field is in the cold
	// tail if anywhere, and is taken from there below, in the tail's order.
	if cs.blk != blk {
		cs.setBlock(blk)
		cs.setGroups(cn.seg, cn.blockIndexOf(blk))
	}
	cs.k, cs.fellBack = local, false
	sc.entries = append(sc.entries[:0], entries...)
	added, escWanted := 0, 0
	esc := blk.escapeAt(local)
	for idx := range blk.schema.fields {
		f := &blk.schema.fields[idx]
		if _, in := p.ids[f.id]; !in {
			continue
		}
		if testBit(esc, idx) {
			// An escaped cell with a precomputed literal: emit that, in place of the expression
			// the record half was told to leave out.
			if node, ok := sc.substNodeFor(idx); ok {
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
				sc.entries = wire.AppendKey(sc.entries, inline, outID, name)
				sc.entries = append(sc.entries, node...)
				added++
				continue
			}
			// A cell the record itself holds -- an expression, already in the remnant half above --
			// has no cold-tail entry, so counting it would walk the whole tail looking for it.
			if !blk.heldKnown(local, f.id) {
				escWanted++
			}
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

	// The cold tail: escaped schema fields (absent, or present but too wide for the slot) and the
	// attributes the base schema does not carry, which the rewrite moves here. Both are LITERALS --
	// an expression is never moved out of the record (see storableInColumn) -- so a found node is
	// appended exactly as stored. One walk, in stored order, stopping once every attribute that can
	// be here has been found.
	cw, nonSchema := sc.coldWanted(blk, p)
	if need := nonSchema + escWanted; need > 0 {
		tail, err := cs.bc.stream(blk, kindCold)
		if err != nil {
			return nil, false
		}
		cold := tail[blk.coldOffAt(local):blk.coldOffAt(local+1)]
		for found := 0; len(cold) > 0 && found < need; {
			sid, m := binary.Uvarint(cold)
			if m <= 0 {
				return nil, false
			}
			cold = cold[m:]
			nl, ok := wire.NodeLen(cold)
			if !ok || nl > len(cold) {
				return nil, false
			}
			node := cold[:nl]
			cold = cold[nl:]
			if !slices.Contains(cw, uint32(sid)) {
				continue
			}
			found++
			// Keyed as the reassembly keys a cold-tail entry: an interned segment's tail is already in
			// the record's own key space, and an inline record names it through this process's ids.
			name := ""
			if inline {
				nm, ok := c.intern.Name(blk.remap.current(uint32(sid)))
				if !ok {
					return nil, false
				}
				name = nm
			}
			sc.entries = wire.AppendKey(sc.entries, inline, uint32(sid), name)
			sc.entries = append(sc.entries, node...)
			added++
		}
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

// selfClosedScratch is adClosed's reusable working set, held by the scan so the check
// allocates nothing per record.
type selfClosedScratch struct {
	ids   []uint32
	names [][]byte
	nodes [][]byte
	reach []bool
	done  []bool
	refs  wire.NodeRefs
}

// adClosed reports whether w, a record narrowed to p's attributes, reads exactly as the whole record
// would wherever p's attributes lead: every reference reachable from them names an attribute w
// carries, or one p names.
//
// That is the condition under which a narrowed ad can stand in for the whole one, and it is the same
// question for both uses. A consumer that chases the references of the attributes it emits -- the
// wire-row and text projections under chaseRefs -- reaches only what the walk here reaches. And a
// query evaluated on the narrowed record reads its seeds and whatever their expressions refer to,
// transitively, which is again the walk from p (the plan built from the seeds).
//
// Both halves of the condition are exact. An attribute w carries is a copy of the record's entry. An
// attribute p names but w lacks is one the record lacks too, because the narrowing kept every one of
// p's attributes the record has -- so it is undefined either way. Anything else might be in the record
// and not in w, and w is then not a stand-in.
//
// The reference walk must be COMPLETE, so it is wire.AppendNodeRefsExact: a node calling eval(), whose
// argument names an attribute only at run time, makes w not closed. So does a reference in the other
// key encoding than the record's, or an entry keyed that way, which the walk cannot match against the
// record's keys. Each of those is the safe answer: the caller reassembles.
//
// A sealed value refers to nothing. Its plaintext cannot be read here, and an attribute is sealed
// (EncryptedAttrs, or private) on the contract that its value does not depend on other attributes --
// a token or a secret string. Every consumer holds to the same rule: none follows a reference inside
// a sealed value (the text projection, the wire-row assembler and the partial match all stop at one),
// so treating it as reference-free here keeps the closure computed and the closure consumed the same.
//
// p nil makes every entry a root and accepts no absent reference: whether the ad refers to nothing
// outside itself at all.
func (c *Collection) adClosed(w []byte, p *projPlan, sd *segDictHandle, sc *selfClosedScratch) bool {
	_, _, _, inline, ok := wire.Ad(w).SplitBody()
	if !ok {
		return false
	}
	sc.ids, sc.names, sc.nodes = sc.ids[:0], sc.names[:0], sc.nodes[:0]
	if inline {
		if !wire.Ad(w).ForEachNameNode(func(name, node []byte) bool {
			sc.names = append(sc.names, name)
			sc.nodes = append(sc.nodes, node)
			return true
		}) {
			return false
		}
	} else {
		mixed := false
		if !wire.Ad(w).ForEachRaw(func(id uint32, name string, node []byte) bool {
			if name != "" {
				mixed = true
				return false
			}
			sc.ids = append(sc.ids, id)
			sc.nodes = append(sc.nodes, node)
			return true
		}) || mixed {
			return false
		}
	}
	var want map[uint32]struct{} // p's attributes in the record's id space (interned records)
	if p != nil && !inline {
		want = p.ids
		if sd != nil {
			want = p.localWanted(sd)
		}
	}
	n := len(sc.nodes)
	sc.reach = slices.Grow(sc.reach[:0], n)[:n]
	sc.done = slices.Grow(sc.done[:0], n)[:n]
	clear(sc.done)
	for i := 0; i < n; i++ {
		switch {
		case p == nil:
			sc.reach[i] = true
		case inline:
			sc.reach[i] = p.wantsNameBytes(sc.names[i])
		default:
			_, sc.reach[i] = want[sc.ids[i]]
		}
	}
	var isEval func(uint32) bool
	if !inline {
		isEval = func(id uint32) bool {
			var nm []byte
			if sd != nil {
				nm = sd.name(id)
			} else if s, ok := c.intern.Name(id); ok {
				nm = []byte(s)
			} else {
				return true // an id this process cannot name: assume the worst
			}
			return wire.FoldEqualBytes(nm, "eval")
		}
	}
	for changed := true; changed; {
		changed = false
		for i := 0; i < n; i++ {
			if !sc.reach[i] || sc.done[i] {
				continue
			}
			sc.done[i] = true
			sc.refs.IDs, sc.refs.Names = sc.refs.IDs[:0], sc.refs.Names[:0]
			if !wire.AppendNodeRefsExact(sc.nodes[i], inline, &sc.refs, isEval) {
				return false
			}
			if inline {
				if len(sc.refs.IDs) > 0 {
					return false
				}
				for _, r := range sc.refs.Names {
					j := slices.IndexFunc(sc.names, func(have []byte) bool { return wire.FoldEqualNames(r, have) })
					switch {
					case j >= 0:
						if !sc.reach[j] {
							sc.reach[j], changed = true, true
						}
					case p == nil || !p.wantsNameBytes(r):
						return false
					}
				}
				continue
			}
			if len(sc.refs.Names) > 0 {
				return false
			}
			for _, r := range sc.refs.IDs {
				j := slices.Index(sc.ids, r)
				switch {
				case j >= 0:
					if !sc.reach[j] {
						sc.reach[j], changed = true, true
					}
				case p == nil:
					return false
				default:
					if _, in := want[r]; !in {
						return false
					}
				}
			}
		}
	}
	return true
}

// closureMissGiveUp is how many CONSECUTIVE records may fail the self-contained check before
// a scan stops attempting the columnar projection at all. Whether a projection's expressions
// reach outside it is a property of the data, so it is discovered rather than declared --
// and once discovered, every further attempt is pure waste ahead of the reassembly that
// follows it. Small, because the attempt is not free and the signal is consistent: a
// projection either names expressions that escape or it does not.
const closureMissGiveUp = 8
