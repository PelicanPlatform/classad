package collections

import (
	"encoding/binary"
	"math"
	"unsafe"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// GROUP BY a STRING column, answered from the columns rather than by scanning records.
//
// Against a production history archive, grouping one numeric column took 90 ms and grouping Owner --
// the same query shape, the same records -- took 21.1 s, because a non-numeric group column declined
// every columnar tier and fell to projecting each matching record. Two hundred times apart for a
// difference in the column's TYPE, not in the work.
//
// The lever is the per-block string dictionary (see strdict.go): a dictionary-encoded field stores its
// distinct values once per block and gives each record a fixed-width CODE into them. So the per-record
// read is an indexed one rather than a walk down the positional string region -- but more importantly,
// the CODE is enough to identify the group. Resolving a code to a group is work per DISTINCT VALUE PER
// BLOCK (a few hundred), not per record (millions): groupCol holds a code -> *groupAcc table filled
// lazily as codes are met, and the per-record step becomes an array index and a counter bump.
//
// Codes are per BLOCK -- the same owner has a different code in the next block -- so the table is
// rebuilt at every block boundary. That is the whole cost of the string case, and it is why the
// dictionary makes this cheap instead of merely possible.
//
// Three things bypass the dictionary and take a per-record path: a block where the field has no
// dictionary at all (positional read, the pre-dictionary cost), an ESCAPED record whose value did not
// fit the column and lives in the cold tail, and a segment whose schema does not carry the field
// (the row fallback, which reads the attribute from the record itself).

// groupableKind reports whether a schema field of this kind can be a GROUP column. Numeric and string
// fields can: both are columnar, and both render to the text the scan groups by.
//
// A BOOL field cannot, even though a boolean VALUE can be a group: bools are bit-packed rather than
// slotted, so there is no column reader for one here. Such a group column declines and scans, which is
// what it did before.
func groupableKind(k adKind) bool { return numericKind(k) || k == akString }

// groupCol reads the GROUP column of one block, with everything resolvable hoisted out of the
// per-record loop. Numeric fields keep the numCol reader unchanged -- the fast path that was already
// there costs exactly what it did. String fields get the dictionary machinery above.
type groupCol struct {
	blk      *columnarBlock
	fieldIdx int
	fieldID  uint32
	isStr    bool

	num numCol // numeric fields

	// String fields, dictionary path.
	//
	// Two per-block memo tables over the same codes, because the two shapes want different things
	// from a code. codeAcc[c] is the GROUP code c resolved to, which is only meaningful when this
	// column alone decides the group -- one unbucketed group column -- and is what makes that case
	// cost an array index per record and no hashing at all. codeID[c] is the code's query-local
	// string id PLUS ONE (0 = not yet resolved), which is what a component of a multi-column key
	// needs: the group then depends on the other columns too, so the code cannot name it, but it
	// can still name the component without hashing the string again.
	//
	// Codes are per BLOCK, so both are rebuilt at every block boundary; only the one in use is
	// sized and cleared.
	hasDict bool
	entries [][]byte
	codes   []byte
	codeW   int
	codeAcc []*groupAcc
	codeID  []int64

	// String fields with no dictionary in this block: the decompressed positional string region,
	// walked per record.
	strRegion []byte
}

// groupColScratch is the per-query scratch ONE group column reuses from block to block: the
// dictionary parse buffer and the code tables. All are sized by a block's cardinality, so allocating
// them per block would make a segment's worth of garbage out of buffers that are overwritten anyway
// -- the same reason dictEntries takes the caller's buffer rather than allocating one.
//
// One per group column, since several columns of the same block are live at once.
type groupColScratch struct {
	dict [][]byte
	code []*groupAcc
	ids  []int64
}

// groupCols is the GROUP BY columns of one block, in request order, plus the per-record component
// scratch they fill. One value per query, re-pointed at each block.
//
// The scratch lives here rather than in a local so that packing a record's components is a write
// into an array the compiler can prove does not escape; a slice built per record would allocate once
// per record on the hottest loop in the package.
type groupCols struct {
	n     int
	cols  [maxGroupCols]groupCol
	sc    [maxGroupCols]groupColScratch
	comps [maxGroupCols]groupComp
}

// setBlock re-resolves every group column against blk. ok=false when any of them cannot be read
// here, which gives the whole query back to the scan rather than silently dropping the block's
// records -- one unreadable component is one unreadable group.
//
// The scratch each column reuses is gc's own, so a groupCols value held for the whole query keeps
// reusing one set of buffers per column from block to block.
func (gc *groupCols) setBlock(blk *columnarBlock, fieldIdxs []int, fieldIDs []uint32, bc *blockCache) bool {
	gc.n = len(fieldIdxs)
	for i := range fieldIdxs {
		col, ok := newGroupCol(blk, fieldIdxs[i], fieldIDs[i], bc, &gc.sc[i], gc.n == 1)
		if !ok {
			return false
		}
		gc.cols[i] = col
	}
	return true
}

// newGroupCol resolves one group column for one block. wholeKey says this column alone decides the
// group, which is what lets its dictionary memo cache an accumulator rather than a string id.
// ok=false when the field cannot be read here.
func newGroupCol(blk *columnarBlock, fieldIdx int, fieldID uint32, bc *blockCache,
	sc *groupColScratch, wholeKey bool) (groupCol, bool) {
	if !blk.hasFieldIdx(fieldIdx) {
		return groupCol{}, false
	}
	c := groupCol{blk: blk, fieldIdx: fieldIdx, fieldID: fieldID}
	if blk.schema.fields[fieldIdx].kind != akString {
		num, ok := newNumCol(blk, fieldIdx, fieldID, bc)
		if !ok {
			return groupCol{}, false
		}
		c.num = num
		return c, true
	}
	c.isStr = true
	if blk.dictOwns(fieldIdx) {
		// The dictionary is AUTHORITATIVE for this field: its values are not in the positional
		// region at all, so if either half of it fails to read there is no other place to look.
		entries, ok := blk.dictEntries(fieldIdx, bc, &sc.dict)
		if !ok {
			return groupCol{}, false
		}
		codes, w, ok := blk.dictCodes(fieldIdx, bc)
		if !ok {
			return groupCol{}, false
		}
		c.hasDict, c.entries, c.codes, c.codeW = true, entries, codes, w
		if wholeKey {
			if cap(sc.code) < len(entries) {
				sc.code = make([]*groupAcc, len(entries))
			}
			sc.code = sc.code[:len(entries)]
			clear(sc.code)
			c.codeAcc = sc.code
			return c, true
		}
		if cap(sc.ids) < len(entries) {
			sc.ids = make([]int64, len(entries))
		}
		sc.ids = sc.ids[:len(entries)]
		clear(sc.ids)
		c.codeID = sc.ids
		return c, true
	}
	region, err := bc.stream(blk, kindStr)
	if err != nil {
		return groupCol{}, false
	}
	c.strRegion = region
	return c, true
}

// accFor returns record k's group accumulator, creating the group when it is new.
//
// ok=false means a component's value is one only evaluation can produce -- an expression, a list, a
// nested ad. Such a record still forms a group in the scan, so the caller gives the query back
// rather than returning a result with a group missing.
func (gc *groupCols) accFor(k int, bc *blockCache, acc *groupAccum, nAgg int) (*groupAcc, bool) {
	if gc.n == 1 {
		c := &gc.cols[0]
		// The dictionary fast path: one indexed read of the code column, then an array index. The
		// group is resolved once per distinct value per block, so this loop does no hashing at all
		// after the block's first few records. Sound only because this column alone decides the
		// group, which is what newGroupCol's wholeKey established.
		if c.hasDict && !testBit(c.blk.escapeAt(k), c.fieldIdx) {
			// Under a bucket this record's value comes from the string dictionary, and a string
			// has no bucket to be floored into, so it leaves the series exactly as the scan drops
			// it.
			//
			// Only this RECORD, not the column. A column is typed by what dominates it, and a
			// string column still carries the records whose value escaped that type -- a number
			// among the strings buckets normally, and those records reach compAt below. Skipping
			// the whole column here instead lost them silently: every count stayed plausible and
			// most of the groups simply were not there.
			if acc.bucketed(0) {
				return nil, true
			}
			code := dictCodeAt(c.codes, c.codeW, k)
			if code >= len(c.entries) {
				return nil, false
			}
			if g := c.codeAcc[code]; g != nil {
				g.n++
				return g, true
			}
			// Unreachable with a bucket (the guard above returns first), so accFor cannot decline.
			gc.comps[0] = acc.strComp(bytesToStr(c.entries[code]))
			g, _ := acc.accFor(&gc.comps, nAgg)
			c.codeAcc[code] = g
			return g, true
		}
		comp, ok := c.compAt(k, bc, acc)
		if !ok {
			return nil, false
		}
		gc.comps[0] = comp
		return acc.accFor(&gc.comps, nAgg)
	}
	for i := 0; i < gc.n; i++ {
		c := &gc.cols[i]
		// The same dictionary fast path, one level down: the code names the COMPONENT rather than
		// the group, so it resolves to a query-local string id by array index and the key is
		// assembled from that. A dictionary-coded component still costs no string hash per record.
		if c.hasDict && !testBit(c.blk.escapeAt(k), c.fieldIdx) {
			// A string has no bucket to be floored into, and the reference drops a RECORD as soon
			// as any one of its bucket keys fails to render -- so this record is in no group at
			// all, whatever the other columns say. Only this record, not the column: a number that
			// escaped a string column reaches compAt below and buckets normally.
			if acc.bucketed(i) {
				return nil, true
			}
			code := dictCodeAt(c.codes, c.codeW, k)
			if code >= len(c.entries) {
				return nil, false
			}
			// The table holds the id PLUS ONE, so its zero value is "not yet resolved" and the
			// whole table is armed by clearing it rather than by filling it with a sentinel.
			id := c.codeID[code]
			if id == 0 {
				id = acc.internStr(bytesToStr(c.entries[code])) + 1
				c.codeID[code] = id
			}
			gc.comps[i] = groupComp{raw: id - 1, kind: gkString}
			continue
		}
		comp, ok := c.compAt(k, bc, acc)
		if !ok {
			return nil, false
		}
		gc.comps[i] = comp
	}
	return acc.accFor(&gc.comps, nAgg)
}

// compAt derives record k's component the slow way: for a numeric column, a block where the string
// field has no dictionary, or an escaped record whose value lives in the cold tail.
func (c *groupCol) compAt(k int, bc *blockCache, acc *groupAccum) (groupComp, bool) {
	if !c.isStr {
		nv, cell := c.num.cell(k, bc)
		switch cell {
		case cellNumber:
			return compOf(nv), true
		case cellExpr:
			return groupComp{}, false
		}
		// cellAbsent or cellOther: the value escaped the numeric slot and is not a number. It is
		// still a group -- an absent attribute groups as `undefined`, a string as its text -- so
		// read the literal itself rather than declining the way the numbers-only path had to.
		return escapedGroupComp(c.blk, k, c.fieldID, bc, acc)
	}
	if c.hasDict || testBit(c.blk.escapeAt(k), c.fieldIdx) {
		// With a dictionary this is reached only for an ESCAPED record -- accFor serves every other
		// one -- and an escaped value is never in the dictionary.
		return escapedGroupComp(c.blk, k, c.fieldID, bc, acc)
	}
	s, ok := c.positionalStrAt(k)
	if !ok {
		return groupComp{}, false
	}
	return acc.strComp(s), true
}

// positionalStrAt reads record k's string out of the positional region, for a block where this field
// has no dictionary: uvarint(len)+bytes per non-escaped, non-dictionary string field in schema order, so
// reaching this field means walking the ones before it. That walk is the cost the dictionary exists to
// remove, and a block without one still pays it.
//
// It ALIASES the decompressed region rather than copying. One Go string allocation per record is what
// made string column reads slower than the row path they replaced, and nothing here retains the alias:
// groupAccum.internStr copies on the one path that does.
func (c *groupCol) positionalStrAt(k int) (string, bool) {
	b := c.blk
	region := c.strRegion[b.strOffAt(k):b.strOffAt(k+1)]
	esc := b.escapeAt(k)
	for i := range b.schema.fields {
		// A field the dictionary owns is NOT written here, so walking past it would misalign every
		// field after it.
		if b.schema.fields[i].kind != akString || testBit(esc, i) || b.dictOwns(i) {
			continue
		}
		l, n := binary.Uvarint(region)
		if n <= 0 || int(l)+n > len(region) {
			return "", false
		}
		if i == c.fieldIdx {
			if l == 0 {
				return "", true
			}
			return unsafe.String(&region[n], int(l)), true
		}
		region = region[n+int(l):]
	}
	return "", false
}

// escapedGroupComp reads the group component of a record whose column slot does not hold it: the
// value is in the block's cold tail, or the record has no such attribute at all.
func escapedGroupComp(b *columnarBlock, k int, fieldID uint32, bc *blockCache, acc *groupAccum) (groupComp, bool) {
	if len(b.computed) > 0 {
		if idx, ok := b.schema.byID[fieldID]; ok {
			if node, ok := b.computedNode(k, idx, bc); ok {
				return litGroupComp(node, acc)
			}
		}
	}
	node, found, err := b.escapedNode(k, fieldID, bc)
	if err != nil {
		return groupComp{}, false
	}
	if !found {
		if b.heldAt(k, fieldID) {
			// The value stayed in the record: only evaluating it answers, which this record alone may
			// (see heldValueNode).
			if held, ok := b.heldValueNode(k, fieldID, bc); ok {
				return litGroupComp(held, acc)
			}
			return groupComp{}, false
		}
		return groupComp{kind: gkUndef}, true
	}
	return litGroupComp(node, acc)
}

// litGroupComp turns a wire node into a group component. ok=false for anything that is not a scalar
// literal -- an expression, a list, a nested ad -- because rendering one needs the evaluator the scan
// has and this does not.
func litGroupComp(node []byte, acc *groupAccum) (groupComp, bool) {
	lit, ok := scalarValue(node)
	if !ok {
		return groupComp{}, false
	}
	switch lit.Kind {
	case wire.LitInt:
		return groupComp{kind: gkInt, raw: lit.Int}, true
	case wire.LitReal:
		return groupComp{kind: gkReal, raw: int64(math.Float64bits(lit.Real))}, true
	case wire.LitBool:
		var i int64
		if lit.Bool {
			i = 1
		}
		return groupComp{kind: gkBool, raw: i}, true
	case wire.LitString:
		return acc.strComp(lit.Str), true
	case wire.LitUndef:
		return groupComp{kind: gkUndef}, true
	case wire.LitError:
		return groupComp{kind: gkError}, true
	}
	return groupComp{}, false
}

// adGroupComp derives a group component from a record's own wire node, for the row fallbacks. An
// attribute the record does not have groups as `undefined`, which is what the scan's projection
// yields for it.
func adGroupComp(node []byte, found bool, acc *groupAccum) (groupComp, bool) {
	if !found {
		return groupComp{kind: gkUndef}, true
	}
	return litGroupComp(node, acc)
}
