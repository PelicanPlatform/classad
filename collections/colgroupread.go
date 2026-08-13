package collections

import (
	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Reading a group schema's attributes.
//
// The base schema's attributes resolve out of the base block; a group's resolve out of that group's
// block, at the record's RANK in the membership bitmap. Three outcomes, and the middle one is the
// point of the whole design:
//
//	member    -> read the column at rank(k), as cheap as a base column
//	none      -> every attribute of the group is UNDEFINED for this record, with no decode of any
//	             kind. This is the case a query like `RemoteHost is undefined` is made of, and on
//	             the measured queue it is 58% of records for the largest group.
//	exception -> the record holds SOME of the group (see colgroupblock.go: measured at 0.167% a few
//	             hours after derivation). Its values are not in the group block, but they ARE in the
//	             BASE block's cold tail: a group attribute is by definition not a base schema field,
//	             so the base encoder already put it there. So an exception reads the cold tail, the
//	             same way any non-schema attribute does, and needs no row fallback.
//
// That last point makes a group block a pure ACCELERATOR: every value it holds is also reachable
// the ordinary way, so no correctness depends on the selection being right -- only speed does.
//
// Delegating to a sub-scope bound to the group block, rather than teaching every slot reader which
// block it is addressing, is what keeps a group column exactly as fast to read as a base column:
// the readers are unchanged, and the only extra work per access is the rank.

// groupBind locates an attribute in a group: which group, and which field of its schema.
type groupBind struct{ gi, fi int }

// setGroups binds the selections for the base block colScope is currently on. Called with the
// block's own index because a colGroupBlock is per (group, base block).
func (cs *colScope) setGroups(seg *colSegment, bi int) {
	cs.gblocks = cs.gblocks[:0]
	cs.groups = nil
	cs.schemaOnly = seg != nil && seg.schemaOnly
	if seg == nil || len(seg.groups) == 0 {
		return
	}
	cs.groups = seg.groups
	for _, g := range seg.groups {
		if bi < len(g.blocks) {
			cs.gblocks = append(cs.gblocks, g.blocks[bi])
		} else {
			cs.gblocks = append(cs.gblocks, nil)
		}
	}
}

// bindGroupField finds name in a group schema, caching the answer for the query's lifetime as
// bindField does for the base schema. ok is false when no group carries it.
func (cs *colScope) bindGroupField(name string) (groupBind, bool) {
	if len(cs.groups) == 0 {
		return groupBind{}, false
	}
	if cs.bindG == nil {
		cs.bindG = make(map[string]groupBind, 4)
	} else if b, ok := cs.bindG[name]; ok {
		return b, b.gi >= 0
	}
	miss := groupBind{gi: -1}
	id, ok := cs.c.intern.LookupID(name)
	if !ok {
		cs.bindG[name] = miss
		return miss, false
	}
	for gi, g := range cs.groups {
		if fi, ok := g.schema.byID[id]; ok {
			b := groupBind{gi: gi, fi: fi}
			cs.bindG[name] = b
			return b, true
		}
	}
	cs.bindG[name] = miss
	return miss, false
}

// resolveGroup reads a group attribute for the current record. handled is false when no group
// carries the name, so the caller continues to the cold tail.
func (cs *colScope) resolveGroup(name string) (classad.Value, bool) {
	b, ok := cs.bindGroupField(name)
	if !ok {
		return classad.Value{}, false
	}
	gb := cs.gblocks[b.gi]
	idx, member := gb.index(cs.k)
	if !member {
		if gb.isException(cs.k) {
			// Holds part of the group. Not in the group block, but a group attribute is not a
			// base schema field either, so the base encoder left it in the cold tail.
			return cs.fromColdTail(cs.groups[b.gi].schema.fields[b.fi].id), true
		}
		// Holds none of the group: every member attribute is undefined here, proven by the
		// membership bit alone -- no decode of any kind.
		return classad.NewUndefinedValue(), true
	}
	sub := cs.groupScope(b.gi, gb)
	sub.k = idx
	sub.fellBack = false
	v := sub.resolveField(b.fi)
	if sub.fellBack {
		cs.fellBack = true
	}
	return v, true
}

// groupScope returns the reusable sub-scope for group gi, bound to gb's block.
func (cs *colScope) groupScope(gi int, gb *colGroupBlock) *colScope {
	for len(cs.gscope) <= gi {
		cs.gscope = append(cs.gscope, nil)
	}
	sub := cs.gscope[gi]
	if sub == nil {
		sub = &colScope{bc: cs.bc, c: cs.c}
		cs.gscope[gi] = sub
	}
	if sub.blk != gb.blk {
		sub.setBlock(gb.blk)
	}
	return sub
}

// resolveField reads schema field fi for the current record, which is what resolve does once it has
// bound a name to a field. Split out so the group path can reuse it without re-resolving a name it
// has already bound.
func (cs *colScope) resolveField(fi int) classad.Value {
	if testBit(cs.blk.escapeAt(cs.k), fi) {
		return cs.fromColdTail(cs.blk.schema.fields[fi].id)
	}
	return cs.slotValue(fi)
}

// groupAbsentAt reports whether every attribute of the group carrying name is provably undefined
// for record k -- membership clear and not an exception. Used by the presence fast path, which can
// then answer `attr is undefined` for the whole group without touching a value.
func (cs *colScope) groupAbsentAt(name string, k int) (bool, bool) {
	b, ok := cs.bindGroupField(name)
	if !ok {
		return false, false
	}
	gb := cs.gblocks[b.gi]
	if _, member := gb.index(k); member {
		return false, true
	}
	return !gb.isException(k), true
}

// setGroups binds the selections for the base block the vector source is on, as colScope.setGroups
// does for the per-record path.
func (s *blockVecSource) setGroups(seg *colSegment, bi int) {
	s.gblocks = s.gblocks[:0]
	s.groups = nil
	if seg == nil || len(seg.groups) == 0 {
		return
	}
	s.groups = seg.groups
	for _, g := range seg.groups {
		if bi < len(g.blocks) {
			s.gblocks = append(s.gblocks, g.blocks[bi])
		} else {
			s.gblocks = append(s.gblocks, nil)
		}
	}
}

// loadGroupColumn loads a group attribute as a full-length column over the BASE block's records:
// members scattered from the group block at their rank, non-members undefined, and exceptions read
// from the base block's cold tail.
//
// This is what makes a group column vectorizable. The executor works over one element per base
// record and in masks, so a column that exists for only some records has to be presented at full
// length with the rest undefined -- which is exactly what fixEscapes already does for records whose
// value is not in its slot. ok=false declines the column (the block falls back to the per-record
// scope) rather than returning a partly-filled vector.
func (s *blockVecSource) loadGroupColumn(name string, dst *vm.Vec) (bool, bool) {
	b, ok := s.bindGroupField(name)
	if !ok {
		return false, false
	}
	gb := s.gblocks[b.gi]
	g := s.groups[b.gi]
	id := g.schema.fields[b.fi].id
	dst.Mask = false
	dst.Raw = nil

	// Non-members first, so every element has a state even if nothing else writes it: an element
	// left stale from a previous block's column would be read as a value.
	for k := 0; k < s.blk.n; k++ {
		dst.St[k] = vm.VsUndef
	}
	if gb == nil || gb.blk == nil {
		// No member in this block; only the exceptions carry a value.
		return s.fillGroupExceptions(gb, id, dst), true
	}
	// Members: load the group's own column DENSELY through the same loaders a base column uses,
	// then scatter it to the members' base indices.
	//
	// Dense-then-scatter rather than a read per record: the dense load is the vectorized one (raw
	// widths, dictionary codes, the batch int path), and vm.CopyElem moves an element without
	// boxing it through classad.Value -- which for a dictionary-encoded string is a code copy
	// rather than materializing the string per record.
	sub := s.groupSource(b.gi, gb)
	tmp := vm.NewVec(gb.blk.n)
	if !sub.LoadColumn(name, ast.NoScope, tmp) {
		return false, true // the group's own loaders declined: so does this column
	}
	dst.Dict = tmp.Dict
	for k := 0; k < s.blk.n; k++ {
		gi, member := gb.index(k)
		if !member {
			continue
		}
		if !dst.CopyElem(k, tmp, gi) {
			return false, true
		}
	}
	return s.fillGroupExceptions(gb, id, dst), true
}

// fillGroupExceptions fills the partial records' elements from the base block's cold tail. They are
// rare by construction, so this is a handful of lookups per block rather than a per-record cost.
func (s *blockVecSource) fillGroupExceptions(gb *colGroupBlock, id uint32, dst *vm.Vec) bool {
	if gb == nil {
		return true
	}
	for _, e := range gb.exceptions {
		k := int(e)
		if k >= s.blk.n {
			return false
		}
		node, found, err := s.blk.escapedNode(k, id, s.bc)
		if err != nil {
			return false
		}
		if !found {
			dst.St[k] = vm.VsUndef
			continue
		}
		lit, ok := wire.LiteralValue(node)
		if !ok {
			return false // a computed expression: decline rather than guess
		}
		switch lit.Kind {
		case wire.LitInt:
			dst.SetInt(k, lit.Int)
		case wire.LitReal:
			dst.SetReal(k, lit.Real)
		case wire.LitBool:
			dst.SetBool(k, lit.Bool)
		case wire.LitString:
			dst.SetString(k, lit.Str)
		default:
			dst.St[k] = vm.VsUndef
		}
	}
	return true
}

// bindGroupField is blockVecSource's copy of the name -> (group, field) resolution.
func (s *blockVecSource) bindGroupField(name string) (groupBind, bool) {
	if len(s.groups) == 0 {
		return groupBind{}, false
	}
	id, ok := s.c.intern.LookupID(name)
	if !ok {
		return groupBind{}, false
	}
	for gi, g := range s.groups {
		if fi, ok := g.schema.byID[id]; ok {
			return groupBind{gi: gi, fi: fi}, true
		}
	}
	return groupBind{}, false
}

// groupSource returns the reusable sub-source for group gi, bound to gb's block.
func (s *blockVecSource) groupSource(gi int, gb *colGroupBlock) *blockVecSource {
	for len(s.gsrc) <= gi {
		s.gsrc = append(s.gsrc, nil)
	}
	sub := s.gsrc[gi]
	if sub == nil {
		sub = &blockVecSource{c: s.c, bc: s.bc, dicts: map[int]*blockDict{}}
		s.gsrc[gi] = sub
	}
	sub.blk = gb.blk
	return sub
}
