package collections

import (
	"encoding/binary"
	"sort"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// Telling MISSING from EXCEPTIONAL, per field.
//
// A schema field's escape bit means "not in the fixed slot", which is two different facts: the
// record does not carry the attribute at all (MISSING -> it is UNDEFINED), or it carries a value
// the slot cannot hold (EXCEPTIONAL -> defined, and living in the cold tail). Everything that wants
// to answer `attr is undefined` has had to resolve that ambiguity by looking in the cold tail --
// the columnar presence path does exactly that, and the absent-probe selectivity estimate could
// only approximate it.
//
// MEASURED on 1500 real OSPool ads (423k record x field pairs) at the production schema options:
//
//	in-slot 98.96%   missing 1.00%   exceptional 0.04%
//	of the escaped pairs: missing 95.7%, exceptional 4.3%
//	of the 142 fields that ever escape: 132 NEVER exceptional, 9 never missing, 1 mixed
//
// That last line is why this is not a second per-record bitmap. A second bitmap costs one bit per
// field per record in the raw, stride-addressed bits region -- ~36 bytes per record at 282 fields --
// to encode something that is a property of the FIELD for 141 of 142 fields. The class is per field
// per block instead (2 bits each, ~71 bytes for the whole block), and only a mixed field needs
// per-record disambiguation, which it gets from a sparse list of its exceptional records.
//
// Same asymmetry as the group blocks: a dense structure for the common state, a sparse list for the
// rare one. The measurement just moved where the dense structure belongs.

// Escape classes for one schema field within one block.
const (
	escNone     uint8 = iota // no record in this block escaped the field
	escMissing               // every escape is a missing attribute: the escape bit PROVES undefined
	escExcept                // every escape is an out-of-slot value: always in the cold tail
	escMixedCls              // both occur: consult escExcRecs
)

// escClassOf returns the field's escape class in this block, or escNone when the block predates the
// classification (an older section that has not been rebuilt).
func (b *columnarBlock) escClassOf(idx int) uint8 {
	if idx < 0 || idx >= len(b.escClass) {
		return escNone
	}
	return b.escClass[idx]
}

// escapeIsMissing reports whether record k's escape of field idx is a MISSING attribute -- so the
// value is UNDEFINED and no cold-tail lookup is needed. ok is false when the block cannot say,
// which is the mixed case with no record list, or a block with no classification at all.
//
// The caller must already know the escape bit is set; this only classifies it.
func (b *columnarBlock) escapeIsMissing(idx, k int) (missing, ok bool) {
	switch b.escClassOf(idx) {
	case escMissing:
		return true, true
	case escExcept:
		return false, true
	case escMixedCls:
		recs, has := b.escExcRecs[idx]
		if !has {
			return false, false
		}
		// Ascending and rare by construction, so a binary search over a short slice beats a map.
		i := sort.Search(len(recs), func(i int) bool { return int(recs[i]) >= k })
		return !(i < len(recs) && int(recs[i]) == k), true
	}
	return false, false
}

// classifyEscapes derives each field's escape class for a block being encoded, plus the per-record
// exception lists for whatever fields turn out mixed.
//
// It reads each record's COLD TAIL, which is where the encoder puts an escaped value: a schema
// field whose id appears there was present but out of slot; a field whose escape bit is set and
// whose id does not appear was absent. That is the same fact the encoder had when it set the bit,
// recovered without re-encoding.
func classifyEscapes(s *adSchema, recs [][]byte) ([]uint8, map[int][]uint32, []byte) {
	if len(s.fields) == 0 {
		return nil, nil, nil
	}
	sawMissing := make([]bool, len(s.fields))
	sawExcept := make([]bool, len(s.fields))
	sawInSlot := make([]bool, len(s.fields))
	excRecs := map[int][]uint32{}
	for k, r := range recs {
		if len(r) < s.escBytes {
			continue
		}
		esc := r[:s.escBytes]
		_, _, cold := s.splitRecord(r)
		// Which schema fields appear in this record's cold tail.
		inCold := map[int]bool{}
		for len(cold) > 0 {
			id, m := binary.Uvarint(cold)
			if m <= 0 {
				break // malformed tail: classify what was read and stop
			}
			cold = cold[m:]
			nl, ok := wire.NodeLen(cold)
			if !ok || nl > len(cold) {
				break
			}
			if idx, ok := s.byID[uint32(id)]; ok {
				inCold[idx] = true
			}
			cold = cold[nl:]
		}
		for i := range s.fields {
			if !testBit(esc, i) {
				sawInSlot[i] = true
				continue
			}
			if inCold[i] {
				sawExcept[i] = true
				excRecs[i] = append(excRecs[i], uint32(k))
			} else {
				sawMissing[i] = true
			}
		}
	}
	class := make([]uint8, len(s.fields))
	out := map[int][]uint32{}
	for i := range s.fields {
		switch {
		case !sawMissing[i] && !sawExcept[i]:
			class[i] = escNone
		case sawExcept[i] && !sawMissing[i]:
			class[i] = escExcept
		case sawMissing[i] && !sawExcept[i]:
			class[i] = escMissing
		default:
			class[i] = escMixedCls
			out[i] = excRecs[i] // ascending: k ascends in the loop above
		}
	}
	if len(out) == 0 {
		out = nil
	}
	// A field NO record of this block carries: every record escaped it and every escape was a
	// missing attribute. That is an exact proof for the whole block -- `attr is undefined` is true
	// of all of it, and any predicate needing the attribute defined can skip it -- and it costs one
	// bit per field, computed here because the loop above already knows.
	var absent []byte
	if len(recs) > 0 {
		absent = make([]byte, (len(s.fields)+7)/8)
		any := false
		for i := range s.fields {
			if !sawInSlot[i] && !sawExcept[i] && sawMissing[i] {
				setBit(absent, i)
				any = true
			}
		}
		if !any {
			absent = nil
		}
	}
	return class, out, absent
}

// fieldAbsentFromBlock reports whether NO record in this block carries the schema field -- an exact
// proof, not a heuristic. A block that predates the classification always answers false, which is
// the safe direction: the caller then does the work it would have done anyway.
func (b *columnarBlock) fieldAbsentFromBlock(idx int) bool {
	if idx < 0 || len(b.escAbsent) == 0 || idx>>3 >= len(b.escAbsent) {
		return false
	}
	return b.escAbsent[idx>>3]&(1<<uint(idx&7)) != 0
}
