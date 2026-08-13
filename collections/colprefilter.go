package collections

import (
	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// DECIDING FROM COLUMNS BEFORE PAYING TO REBUILD A RECORD.
//
// A columnarized segment stores each attribute the schema carries in a column and removes it from the
// record, so serving a whole ad means reassembling the two halves. That is real work -- measured, a
// full-ad scan over columnarized segments is ~1.2x the cost of reading whole records -- and a scan
// that rejects a record has done all of it for nothing.
//
// The columns can answer the match on their own. The columnar evaluator already does exactly that for
// CountQuery, where it is ~2.6x faster than the row path, because it reads the handful of values the
// query names instead of decoding hundreds it does not. So the prefilter asks the columns first, and
// only records that survive get reassembled.
//
// WHY A FILTER RATHER THAN A SEPARATE SCAN. A second scan would have to re-derive visibility,
// ordering, newest-first, limit early-exit, the indexed candidate paths and the row fallback for
// segments without a block -- every one of them a chance to disagree with the scan it replaces. As a
// filter it inherits all of that, cannot reorder or drop anything the scan would have emitted, and
// the bytes handed to the consumer are the same bytes as before. Its only effect is not doing work.
//
// It is deliberately allowed to be UNSURE. A stored value that is an expression, an attribute the
// schema does not carry, a record in a segment with no columnar payload: each returns undecided, and
// the scan proceeds exactly as it would have. Being wrong is not among the options -- a false
// "matches" costs a reassembly the ordinary match then rejects, but a false "does not match" would
// silently drop a row from a query result, so the undecided answer is the only safe default.

// colPrefilter answers "does this record match?" from a columnarized segment's columns.
//
// Not safe for concurrent use: it carries a colScope, which caches per-block state. The serial scan
// owns one; the parallel workers do not use it.
type colPrefilter struct {
	c        *Collection
	cs       *colScope
	m        *vm.Matcher
	resolver func(name string, scope ast.AttributeScope) classad.Value
	// blk is the block cs is currently bound to, so a scan walking records in order rebinds once per
	// block rather than once per record.
	blk *columnarBlock
}

// newColPrefilter returns a prefilter for q, or nil when the columns cannot usefully decide it.
//
// The gate matters as much as the mechanism. Evaluating from columns is only a saving if it can
// actually reject a record, so a query that reads NO attribute (`true`, the whole-table scan) is
// refused: it matches everything, and the columnar evaluation would be pure overhead on top of the
// reassembly it cannot avoid. A query reading an attribute the schema does not carry is refused for
// the same reason from the other direction -- every record would come back undecided, and the scan
// would pay for the attempt on all of them.
func (c *Collection) newColPrefilter(q *vm.Query) *colPrefilter {
	if q == nil || !q.Native() || c.intern == nil {
		return nil
	}
	st := c.schemaScan.Load()
	if st == nil || st.schema == nil {
		return nil
	}
	plan := q.ReadPlan()
	if !plan.PartialSafe || len(plan.Seeds) == 0 {
		return nil // reads nothing statically visible, or nothing at all: no rejection to win
	}
	for _, name := range plan.Seeds {
		id, ok := c.intern.LookupID(name)
		if !ok {
			return nil
		}
		if _, ok := st.schema.byID[id]; !ok {
			// Not a schema field. It may still be reachable (a group column, the cold tail, the
			// record), but not by a test that has to be cheap to be worth running.
			return nil
		}
	}
	cs := &colScope{bc: st.cache, c: c}
	return &colPrefilter{c: c, cs: cs, m: q.Matcher(), resolver: cs.resolve}
}

// test reports whether record k of the segment matches, and whether it could tell.
//
// decided is false whenever anything about the record needs the ordinary evaluator, which the caller
// must then run -- see the note above on why unsure is the safe answer and wrong is not.
func (p *colPrefilter) test(cn *colNative, k int) (matches, decided bool) {
	blk, local := cn.blockFor(k)
	if blk == nil || blk.schema == nil {
		return false, false
	}
	if blk != p.blk {
		p.cs.setBlock(blk)
		p.cs.setGroups(cn.seg, cn.blockIndexOf(blk))
		p.blk = blk
	}
	p.cs.k, p.cs.fellBack = local, false
	v := p.m.EvalResolved(p.resolver)
	if p.cs.fellBack {
		return false, false
	}
	// isTrueValue, the same truth test the columnar count path uses -- which treats undefined and
	// error as non-matching exactly as the row path does. Reusing it is what makes this filter agree
	// with the scan it sits in front of, rather than approximating it.
	return isTrueValue(v), true
}

// blockIndexOf returns a block's position in the segment, which the group selections are keyed by.
func (cn *colNative) blockIndexOf(blk *columnarBlock) int {
	for i, b := range cn.seg.blocks {
		if b == blk {
			return i
		}
	}
	return 0
}
