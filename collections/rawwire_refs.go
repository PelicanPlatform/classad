package collections

import (
	"iter"
	"slices"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// A WIRE ROW THAT EVALUATES.
//
// QueryRawWire ships exactly the projected attributes, which is right for a relay and wrong
// for a consumer that evaluates what it receives: Requirements arrives without the
// RequestMemory it compares against and evaluates to undefined. The text projection solved
// this with chaseRefs (see ScanRawProjected); these are the wire-row twins, and they produce
// the SAME attribute set -- the projection closed under the projected expressions'
// unscoped/MY. references, transitively, per record.
//
// Per record, not per query: whether an attribute holds an expression is a property of the
// stored ad, so two records matching one query can need different closures. The closure is
// therefore computed where the record's own bytes are, and twice over, in whichever key space
// the record is in at that point: once in id space while an interned record is narrowed (so
// the narrowing stays a superset of what will be kept) and once, authoritatively, by name as
// the row is assembled.

// ScanRawWireRefs is ScanRawWire whose projection is closed under the projected expressions'
// own attribute references, so each row evaluates self-contained at the far end. An empty
// projection already ships every attribute and behaves exactly like ScanRawWire.
func (c *Collection) ScanRawWireRefs(projection []string, redact bool) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		sel := c.newWireSubsetSelectorRefs(projection, redact, true)
		if c.runParallelWireScan(sel, sel.neededCount(), yield) {
			return
		}
		emit := c.yieldWireRow(yield, sel)
		q := queryPlan{}
		for _, sh := range c.shards {
			if !c.scanShard(sh, q, emit) {
				return
			}
		}
	}
}

// QueryRawWireRefs is ScanRawWireRefs restricted to ads matching q.
func (c *Collection) QueryRawWireRefs(q *vm.Query, projection []string, redact bool) iter.Seq[[]byte] {
	return c.QueryRawWireRefsStats(q, projection, redact, nil)
}

// QueryRawWireRefsStats is QueryRawWireRefs that also fills stats (may be nil) with the
// per-scan work breakdown.
func (c *Collection) QueryRawWireRefsStats(q *vm.Query, projection []string, redact bool, stats *ScanStats) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		sel := c.newWireSubsetSelectorRefs(projection, redact, true)
		// A projection plan, but one MARKED as feeding a consumer that evaluates. Passing no
		// plan at all costs the columnar fast path on every record: measured on 20k real job
		// ads it was 83ms with the plan against 255ms without, dwarfing the closure itself at
		// 17ms.
		//
		// The plan is safe here because the scan checks each projected record for being
		// closed under its references and reassembles the ones that are not (see adClosed). A
		// projection of literals refers to nothing and never triggers that; a projection
		// naming an expression that reaches outside itself falls back per record, which is
		// exactly the behaviour passing no plan gave unconditionally.
		var proj *projPlan
		if len(projection) > 0 {
			proj = c.newProjPlan(append(slices.Clip(projection), rawTypeFieldNames...))
			proj.closure = true
		}
		qp := c.newQueryPlan(q, proj, stats)
		rp := c.planReadQuery(qp)
		if c.queryPar > 1 && !rp.disjunctive() && len(rp.usable) == 0 && !c.reverseScan {
			if c.runParallelWireQuery(qp, sel, sel.neededCount(), yield) {
				return
			}
		}
		c.runQuery(qp, rp, c.yieldWireRow(yield, sel))
	}
}

// wireRefSet is ONE row assembly's reference closure: the attribute names an emitted
// expression referred to and the projection did not name. The names are sub-slices of the
// record being assembled, so the set is valid only for that record and is reset at the start
// of each one.
type wireRefSet struct {
	names  [][]byte
	hashes []uint32
	// found marks a surfaced reference the assembly has since emitted. It is what lets the
	// hot phase know it is DONE: a reference the hot header satisfied leaves nothing to look
	// for in the cold tail, and without this the assembler could only tell that a reference
	// had surfaced, never that it had been met.
	found []bool
}

func (rs *wireRefSet) reset() {
	rs.names, rs.hashes, rs.found = rs.names[:0], rs.hashes[:0], rs.found[:0]
}

func (rs *wireRefSet) has(name []byte, h uint32) bool {
	_, ok := rs.index(name, h)
	return ok
}

func (rs *wireRefSet) index(name []byte, h uint32) (int, bool) {
	for i, rh := range rs.hashes {
		if rh == h && wire.FoldEqualNames(name, rs.names[i]) {
			return i, true
		}
	}
	return 0, false
}

// wireRowAssembler is ONE GOROUTINE's row-assembly state: the subset assembler's scratch and,
// under chaseRefs, the per-record closure and the per-node reference scratch.
//
// It exists because the parallel wire scans run W workers over one shared, immutable
// selector: anything the assembly WRITES has to be per worker, and holding it in one object
// is what keeps a new piece of per-record state from being accidentally shared the way the
// narrower's resolved-projection cache once was.
type wireRowAssembler struct {
	c       *Collection
	sel     *wireSubsetSelector
	sc      wire.SubsetScratch
	refs    wireRefSet
	scratch [][]byte // per-node reference-name scratch, recycled across nodes
	// flat holds an in-memory record rewritten to inline names, when one had to be; see
	// appendRow. Reused across records like everything else here.
	flat []byte
	// Counts for pending, reset per record: entries taken because the SELECTOR wanted them
	// (the projection plus the two type fields) and references taken because the closure
	// surfaced them.
	selBase   int
	refsFound int
	// sealed names the entries of the record being assembled whose STORED value is sealed, for
	// a record that reaches the assembly already converted to inline names: the conversion opens
	// sealed values, and the converted bytes no longer say which ones were. The closure does not
	// follow a reference inside a sealed value (see adClosed), and this is how it still knows
	// where one was. Empty for a record that was not converted -- an inline record's sealed
	// nodes are recognised as they are emitted.
	sealed wireRefSet
	// noteOff is whether the entry keep last accepted is in sealed. The subset assembler notes
	// an accepted entry's node right after accepting it, and note then records nothing.
	noteOff bool
}

// pending reports how many wanted attributes this record has not yielded yet: the projected
// ones still missing plus the surfaced references still missing. Zero means the assembly is
// complete and whatever part of the record has not been looked at cannot hold anything else
// the row needs -- which is what allows the hot phase to stop without touching the cold tail.
func (a *wireRowAssembler) pending(needed int) int {
	return (needed - a.selBase) + (len(a.refs.names) - a.refsFound)
}

func (c *Collection) newWireRowAssembler(sel *wireSubsetSelector) *wireRowAssembler {
	return &wireRowAssembler{c: c, sel: sel}
}

// appendRow assembles one wire-form row for src into dst, returning the extended buffer.
// Reports false for a record the assembler cannot read (malformed, or an id it cannot
// resolve), in which case the caller discards the partial append and keeps scanning.
//
// It is the one place that knows how the two stored encodings reach the same self-contained
// inline-names row, so every wire scan -- serial and parallel -- agrees on it. A persistent
// collection's records already carry inline names (an interned SEALED segment's are converted
// upstream by wireToInlineRead). An IN-MEMORY collection's are keyed by ids against the shared
// intern table, which the far side of a relay does not have, so they are rewritten here.
//
// open is the caller's renderKey(redact) and governs sealed values identically on every path:
// no key means an at-rest-encrypted entry is dropped rather than shipped.
func (a *wireRowAssembler) appendRow(dst []byte, src wire.Ad, needed int, open wire.Sealer) ([]byte, bool) {
	if !a.c.inline {
		if !a.sel.chaseRefs {
			// No closure to compute, so the rewrite is FUSED with the selection and only
			// the entries actually shipped are converted.
			return wire.AppendAdSubsetInlineFromInterned(dst, src, a.c.intern.Name, a.sel.keep, open, &a.sc)
		}
		// The closure resolves references BY NAME against the record it is assembling, so
		// the record has to be in name space before it runs -- and it must be the WHOLE
		// record, because a referenced attribute the selection would have dropped is
		// exactly what the closure exists to recover. Rewriting once and reusing the one
		// tested closure beats a second implementation of it over interned ids.
		a.markSealed(src, nil)
		flat, ok := wire.AppendAdSubsetInlineFromInterned(a.flat[:0], src, a.c.intern.Name, nil, open, &a.sc)
		a.flat = flat
		if !ok {
			return dst, false
		}
		// Sealed values were opened by the rewrite, so there is no key left to apply.
		src, open = wire.Ad(flat), nil
	}
	if !a.sel.chaseRefs {
		return wire.AppendAdSubsetInlineHotFirst(dst, src, a.sel.keep, needed, open, &a.sc)
	}
	a.refs.reset()
	a.selBase, a.refsFound = 0, 0
	return wire.AppendAdSubsetInlineRefs(dst, src, a.keep, a.note,
		func() int { return a.pending(needed) }, needed, open, &a.sc)
}

// appendRowConverted is appendRow for a record of an INTERNED segment (dict non-nil; nil passes
// w through): it converts w to inline names with wireToInlineRead, which opens the sealed values
// an entitled reader may see, and assembles the row -- noting first which entries were sealed,
// so the closure does not follow references inside their opened plaintext.
func (a *wireRowAssembler) appendRowConverted(dst []byte, dict *segDictHandle, w []byte, needed int, open wire.Sealer) ([]byte, bool) {
	if dict != nil {
		a.markSealed(w, dict)
	}
	iw, ok := a.c.wireToInlineRead(dict, w, a.sel.redact)
	if !ok {
		a.sealed.reset()
		return dst, false
	}
	out, ok := a.appendRow(dst, wire.Ad(iw), needed, open)
	a.sealed.reset()
	return out, ok
}

// markSealed records in a.sealed the entries of w, a record about to be converted to inline
// names, whose stored value is sealed. Ids resolve through dict, or the collection's intern
// table when dict is nil. Only a closure-computing, entitled read needs it: a redacted
// conversion turns a sealed value into undefined, which refers to nothing anyway, and an
// unencrypted collection has none to find.
func (a *wireRowAssembler) markSealed(w []byte, dict *segDictHandle) {
	a.sealed.reset()
	if !a.sel.chaseRefs || a.sel.redact || a.c.sealer == nil {
		return
	}
	wire.Ad(w).ForEachRaw(func(id uint32, inlineName string, node []byte) bool {
		if !wire.IsEncryptedNode(node) {
			return true
		}
		var name []byte
		switch {
		case inlineName != "":
			name = []byte(inlineName)
		case dict != nil:
			name = dict.name(id)
		default:
			if n, ok := a.c.intern.Name(id); ok {
				name = []byte(n)
			}
		}
		if len(name) > 0 {
			a.sealed.names = append(a.sealed.names, name)
			a.sealed.hashes = append(a.sealed.hashes, wire.NameHash32Bytes(name))
		}
		return true
	})
}

// keep is the selector's membership test widened by whatever the closure has surfaced for
// THIS record so far.
func (a *wireRowAssembler) keep(name []byte, h uint32, node []byte) bool {
	if a.sel.keepHashed(name, h, node) {
		a.selBase++
		a.noteOff = len(a.sealed.names) > 0 && a.sealed.has(name, h)
		return true
	}
	i, ok := a.refs.index(name, h)
	if ok {
		if !a.refs.found[i] {
			a.refs.found[i], a.refsFound = true, a.refsFound+1
		}
		a.noteOff = len(a.sealed.names) > 0 && a.sealed.has(name, h)
	}
	return ok
}

// note records the references an emitted node made, reporting whether any were new. A node
// whose stored value was sealed records nothing: a sealed value refers to no other attribute.
func (a *wireRowAssembler) note(node []byte) bool {
	if a.noteOff {
		return false
	}
	a.scratch = wire.AppendNodeRefNames(node, a.scratch[:0])
	added := false
	for _, rn := range a.scratch {
		if len(rn) == 0 {
			continue
		}
		// A private attribute never leaves a redacted row, reference or not -- the same rule
		// the text projection's closure applies.
		if a.sel.redact && isPrivateNameBytes(rn) {
			continue
		}
		h := wire.NameHash32Bytes(rn)
		if a.sel.inProjection(rn, h) || a.refs.has(rn, h) {
			continue
		}
		a.refs.names = append(a.refs.names, rn)
		a.refs.hashes = append(a.refs.hashes, h)
		a.refs.found = append(a.refs.found, false)
		added = true
	}
	return added
}

// narrowAdRefs is narrowAd for a reference-chasing read: it keeps the wanted attributes AND
// the ones the kept nodes refer to, transitively, resolved in the RECORD'S OWN id space.
//
// Narrowing before the inline conversion is worth 1.4x on an interned store (see
// narrowProjected), and plain narrowAd cannot be used under chasing because it would drop
// exactly the attributes the closure is there to recover. References inside an interned
// record are stored as that segment's own dictionary ids -- the same ids its entries are
// keyed by -- so the closure needs no dictionary probe and no conversion here; the
// authoritative name-space closure still runs afterwards, over this superset.
//
// An inline-keyed record (an interned segment may hold one) has no id space to chase in, and is
// handed on whole for the assembler's name-space closure to cover. A kept node that is SEALED is
// kept and not chased: a sealed value refers to no other attribute (see adClosed), and the
// assembler does not follow references inside one either.
//
// Reports false if w is not a well-formed ad, in which case the caller must fall back rather
// than treat the result as an empty ad.
func narrowAdRefs(sc *narrowScratch, w []byte, want map[uint32]struct{}) ([]byte, bool) {
	hdr, _, _, inline, ok := wire.Ad(w).SplitBody()
	if !ok {
		return nil, false
	}
	if inline {
		return w, true
	}
	sc.ids, sc.nodes, sc.keep = sc.ids[:0], sc.nodes[:0], sc.keep[:0]
	if !wire.Ad(w).ForEachRaw(func(id uint32, _ string, node []byte) bool {
		sc.ids = append(sc.ids, id)
		sc.nodes = append(sc.nodes, node)
		sc.keep = append(sc.keep, false)
		return true
	}) {
		return nil, false
	}
	sc.refIDs = sc.refIDs[:0]
	for {
		again := false
		for i, id := range sc.ids {
			if sc.keep[i] {
				continue
			}
			if _, in := want[id]; !in && !slices.Contains(sc.refIDs, id) {
				continue
			}
			sc.keep[i] = true
			sc.refs = wire.AppendNodeRefIDs(sc.nodes[i], sc.refs[:0]) // nothing for a sealed node
			for _, r := range sc.refs {
				if _, isWanted := want[r]; isWanted || slices.Contains(sc.refIDs, r) {
					continue
				}
				sc.refIDs = append(sc.refIDs, r)
				again = true
			}
		}
		if !again {
			break
		}
	}
	sc.entries = sc.entries[:0]
	n := 0
	for i := range sc.ids {
		if !sc.keep[i] {
			continue
		}
		sc.entries = wire.AppendKey(sc.entries, false, sc.ids[i], "")
		sc.entries = append(sc.entries, sc.nodes[i]...)
		n++
	}
	sc.out = wire.BuildAd(sc.out[:0], hdr, n, sc.entries)
	return sc.out, true
}
