package collections

import (
	"math/bits"
	"sort"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// GROUP BLOCKS, phase 2 storage side.
//
// A group schema's attributes are stored columnar for the records that HAVE them, in a block of
// their own, and not at all for the records that do not -- so an attribute only some ads carry
// costs a column for those ads instead of a slot in every record. Phase 1 established which sets
// of attributes are worth this (see colgroupschema.go); this stores them.
//
// A group block is a SELECTION of one base block, never an independent batching of records. That
// is a load-bearing choice, for three separate reasons:
//
//   - A record's identity stays its index in the base block. colScope.k and the ordered scans
//     address records that way, and an independently batched group block would need a per-record
//     indirection array to get back.
//   - The vectorized evaluator loads a whole column into a vm.Vec and works in masks. A selection
//     can be scattered into a full-length vector with non-members undefined, which is the same
//     operation fixEscapes already performs for records whose value is not in its slot. A
//     reordered group block could not be scattered without a permutation.
//   - Record order within a segment is preserved, so newest-first scans and QueryLimit's early
//     exit are unaffected.
//
// The mapping from a base index to a group index is therefore a RANK over the membership bitmap,
// which is why the bitmap carries a prefix-popcount table.
//
// THE EXCEPTION LIST IS NOT OPTIONAL. Groups are derived from attributes with identical presence,
// which makes the partial state -- an ad holding some but not all of a group -- impossible in the
// sample they came from. It is NOT impossible in data derived later: measured on two snapshots of
// a production AP hours apart, a group whose partial rate was 0.000% when derived read 0.167% on
// the later snapshot, because one member (Notification) merely happened to accompany the others.
// A partial record cannot be stored under the group (values would be missing) and cannot claim the
// group absent either, so it is listed and read the ordinary way.

// colGroup is one group schema and its per-base-block selections.
type colGroup struct {
	schema *adSchema
	ids    []uint32 // member attribute ids, sorted; the membership test
	blocks []*colGroupBlock
}

// colGroupBlock is one group's storage over the in-group records of ONE base block.
type colGroupBlock struct {
	// members has one bit per record of the base block: set when the record holds every member
	// attribute, and its values live in blk.
	members []byte
	// rank[i] is the number of members among the first i*64 records, so a base index maps to a
	// group index in O(1) instead of a popcount over the whole prefix.
	rank []uint32
	// exceptions lists base-block record indices holding SOME but not all members, ascending.
	// Their bits in members are clear -- they are not in blk -- but they must be distinguished
	// from a genuine absence, because a clear bit otherwise PROVES every member undefined.
	exceptions []uint32
	// blk holds the member records' columns, in base-block order. Nil when no record qualified.
	blk *columnarBlock
}

// memberCount is how many of the base block's records are in the group.
func (gb *colGroupBlock) memberCount() int {
	if gb == nil || gb.blk == nil {
		return 0
	}
	return gb.blk.n
}

// index maps a base-block record index to its index within gb.blk. ok is false when the record is
// not a member -- either it holds none of the group (so every member attribute is undefined for
// it, provably, without a decode) or it is an exception (holds some, and needs the row path).
func (gb *colGroupBlock) index(k int) (int, bool) {
	if gb == nil || gb.blk == nil || k < 0 || k>>3 >= len(gb.members) {
		return 0, false
	}
	if gb.members[k>>3]&(1<<uint(k&7)) == 0 {
		return 0, false
	}
	// Members strictly before k: the prefix table to the last 64-boundary, then the bytes and
	// the partial byte after it.
	n := int(gb.rank[k>>6])
	byteStart := (k >> 6) << 3
	for i := byteStart; i < k>>3; i++ {
		n += bits.OnesCount8(gb.members[i])
	}
	n += bits.OnesCount8(gb.members[k>>3] & byte((1<<uint(k&7))-1))
	return n, true
}

// isException reports whether a non-member record holds SOME of the group -- the case where a
// clear membership bit must NOT be read as "every member attribute is undefined".
func (gb *colGroupBlock) isException(k int) bool {
	if gb == nil {
		return false
	}
	// Binary search, not a linear scan. The first version scanned, justified by exceptions being
	// "tiny (0.2% at worst measured)" -- an assumption a temporal holdout then falsified: a group
	// whose partial rate was 0.075% when derived read 45.8% on a snapshot hours later, because one
	// member's presence moved the opposite way from the rest. At that rate a 4096-record block
	// carries ~1876 exceptions and the scan is per record per query.
	//
	// The list is ascending by construction (built in record order), so this costs nothing when the
	// assumption holds and does not collapse when it does not.
	i := sort.Search(len(gb.exceptions), func(i int) bool { return int(gb.exceptions[i]) >= k })
	return i < len(gb.exceptions) && int(gb.exceptions[i]) == k
}

// population is how many membership bits are set. Not rank's last entry: that counts members
// before the last 64-record boundary, which equals the total only when the record count happens to
// be a multiple of 64.
func (gb *colGroupBlock) population() int {
	n := 0
	for _, b := range gb.members {
		n += bits.OnesCount8(b)
	}
	return n
}

// buildRank fills the prefix-popcount table from members.
func (gb *colGroupBlock) buildRank(n int) {
	groups := n>>6 + 1
	gb.rank = make([]uint32, groups)
	var acc uint32
	for i := 0; i < groups; i++ {
		gb.rank[i] = acc
		for j := i << 3; j < (i+1)<<3 && j < len(gb.members); j++ {
			acc += uint32(bits.OnesCount8(gb.members[j]))
		}
	}
}

// groupHave counts how many of ids the record carries.
func groupHave(w []byte, ids map[uint32]struct{}) int {
	have := 0
	wire.Ad(w).ForEach(func(id uint32, _ []byte) bool {
		if _, ok := ids[id]; ok {
			have++
		}
		return true
	})
	return have
}

// buildGroupBlocks builds one colGroupBlock per group over a base block's records, given their
// INTERNED WIRE form in base-block order.
//
// Interned wire rather than the base block's rows: membership is a question about which attributes
// a record carries, which the base schema's row form has already flattened away (an attribute it
// does not carry is indistinguishable from one it stores as escaped). Each member record is then
// re-encoded under the GROUP's schema, which is the row form its own block needs.
// toLocal keys these blocks' cold tails by the SEGMENT's dictionary, for the same reason the base
// block's are: an id written from the global intern table means nothing after a restart. nil keeps
// them globally keyed, which is self-consistent but does not survive one.
func buildGroupBlocks(groups []*colGroup, iws [][]byte, regionCodec Codec, toLocal func(uint32) (uint32, bool)) []*colGroupBlock {
	if len(groups) == 0 || len(iws) == 0 {
		return nil
	}
	out := make([]*colGroupBlock, len(groups))
	for gi, g := range groups {
		idset := make(map[uint32]struct{}, len(g.ids))
		for _, id := range g.ids {
			idset[id] = struct{}{}
		}
		gb := &colGroupBlock{members: make([]byte, (len(iws)+7)/8)}
		var members [][]byte
		for k, iw := range iws {
			switch have := groupHave(iw, idset); have {
			case 0:
				// none: every member attribute is undefined for this record, provably
			case len(g.ids):
				gb.members[k>>3] |= 1 << uint(k&7)
				members = append(members, g.schema.encodeExceptLocal(wire.Ad(iw), nil, toLocal))
			default:
				gb.exceptions = append(gb.exceptions, uint32(k))
			}
		}
		gb.buildRank(len(iws))
		if len(members) > 0 {
			// No hot tier for a group column yet: the groups are derived from presence, so
			// nothing here says which of their columns queries read.
			gb.blk = encodeColumnarBlock(g.schema, members, nil, regionCodec, groupColdToField(g.schema, toLocal))
		}
		out[gi] = gb
	}
	return out
}

// groupSkipSet is the attributes a record's base row can omit: the members of every group the
// record belongs to WHOLLY, since those are stored by that group's column.
//
// Only whole membership qualifies. A record holding part of a group is not in the column, so its
// values must stay in the base cold tail -- which is exactly where the exception path reads them.
func groupSkipSet(groups []*colGroup, iw []byte) map[uint32]struct{} {
	var skip map[uint32]struct{}
	for _, g := range groups {
		idset := make(map[uint32]struct{}, len(g.ids))
		for _, id := range g.ids {
			idset[id] = struct{}{}
		}
		if groupHave(iw, idset) != len(g.ids) {
			continue
		}
		if skip == nil {
			skip = make(map[uint32]struct{}, len(g.ids))
		}
		for _, id := range g.ids {
			skip[id] = struct{}{}
		}
	}
	return skip
}

// groupColdToField maps a group block's cold-tail ids to its schema's field indexes, in whichever id
// space the tail was written in. See classifyEscapes for what goes wrong when the two disagree.
func groupColdToField(s *adSchema, toLocal func(uint32) (uint32, bool)) map[uint32]int {
	if toLocal == nil {
		return nil
	}
	m := make(map[uint32]int, len(s.fields))
	for idx, f := range s.fields {
		if lid, ok := toLocal(f.id); ok {
			m[lid] = idx
		}
	}
	return m
}
