package collections

import (
	"encoding/binary"
	"math"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// COMPUTED VALUES FOR EXPRESSION-VALUED COLUMN CELLS.
//
// A columnarized segment moves every literal into its columns and leaves expressions in the record
// (storableInColumn): an expression's attribute references are ids in the record's key space, and only
// evaluating it against the rest of the ad gives its value. So a schema field whose cell holds an
// expression is escaped, the column readers find it held in the record (colheld.go), and the query
// goes back to the row path for that record. On a field that is an expression in nearly every record --
// a usage figure derived from another usage figure, say -- every columnar path is then a row scan.
//
// Sealed records are immutable, so a value computed from one at columnarize time never goes stale. For
// a schema field whose cell is an expression that depends on nothing but the record itself, the value
// is computed once, when the segment is columnarized, and kept beside the block. A reader that meets the
// escaped cell asks for the computed value before deciding the record must be read.
//
// WHAT DOES NOT CHANGE. The expression stays in the record, exactly as it is today, and the cell stays
// escaped. Reassembly, projection, re-columnarization and every reader that does not ask for a computed
// value see precisely the bytes and bits they saw before, so the computed value cannot leak into an ad as
// if it were the attribute's literal, and a reader nobody taught about it declines as it always did. It
// is an accelerator in the strict sense: dropping it costs speed, never an answer.
//
// WHEN A CELL QUALIFIES. Every value here is one plain evaluation of the record, alone, would give; any
// doubt leaves the cell uncomputed:
//
//   - Every attribute reference in the transitive closure is unscoped or MY., names something other than
//     CurrentTime, and names an attribute the record HOLDS. An unscoped reference to an absent attribute is
//     undefined only when the ad is evaluated alone: under matchmaking it falls through to the target ad
//     (see the evaluator's resolveAttributeReference). Requiring presence makes the value the same in
//     every context, rather than correct only for the readers that happen to evaluate alone.
//   - The closure is built only from literals, operators, conditionals and a short list of deterministic
//     built-in functions (pureFuncs). Anything else -- time(), random(), eval(), a nested ad, a list, a
//     select -- leaves the cell alone.
//   - Nothing in the closure is sealed, or would be sealed if it were written now: a value derived from a
//     sealed attribute would put a function of its plaintext into the column.
//   - The value is a number, a string or a boolean. UNDEFINED (only reachable through an explicit
//     `undefined`), ERROR (a cycle, a division by zero) and NaN are left to the row path, where nothing
//     about their handling is new.
//   - Only plain full records reach the builder; a delta is collapsed into its full ad before
//     columnarizing (see columnarizeSegment).

// pureFuncs are the built-in functions a computed value may call: each is a function of its argument
// values alone, with no clock, no randomness and no access to the evaluation scope. Keyed lower-case, as
// the evaluator matches function names.
var pureFuncs = map[string]struct{}{
	"ifthenelse": {}, "isundefined": {}, "iserror": {}, "isstring": {}, "isinteger": {}, "isreal": {},
	"isboolean": {}, "int": {}, "real": {}, "floor": {}, "ceiling": {}, "ceil": {}, "round": {},
	"strcat": {}, "tolower": {}, "toupper": {}, "substr": {},
}

// pureBinOps are the binary operators a computed value may use: the arithmetic, comparison, bitwise,
// logical and identity operators, all of which read only their operands.
var pureBinOps = map[string]struct{}{
	"+": {}, "-": {}, "*": {}, "/": {}, "%": {},
	"<": {}, ">": {}, "<=": {}, ">=": {}, "==": {}, "!=": {},
	"&": {}, "|": {}, "^": {}, "<<": {}, ">>": {}, ">>>": {},
	"&&": {}, "||": {}, "is": {}, "isnt": {}, "=?=": {}, "=!=": {},
}

// computedClosureMax bounds the attributes one computed value may depend on. A closure that large is
// unusual enough that evaluating it per record is not the case worth accelerating.
const computedClosureMax = 64

// pureRefs appends the attribute names e references to refs and reports whether e qualifies (see the
// rules above). The names are as written; the caller resolves them case-insensitively.
func pureRefs(e ast.Expr, refs []string) ([]string, bool) { return walkRefs(e, refs, false) }

// deterministicRefs is pureRefs accepting TARGET., PARENT. and absolute references, which it does not
// report: evaluated alone, as every reader that uses it evaluates, each of them is UNDEFINED. The
// expression's value is then a function of the record and nothing else, but only in that context --
// which is why it decides a per-record evaluation (heldValueNode) and never a stored value.
func deterministicRefs(e ast.Expr, refs []string) ([]string, bool) { return walkRefs(e, refs, true) }

func walkRefs(e ast.Expr, refs []string, scoped bool) ([]string, bool) {
	rec := func(e ast.Expr, refs []string) ([]string, bool) { return walkRefs(e, refs, scoped) }
	switch v := e.(type) {
	case *ast.IntegerLiteral, *ast.RealLiteral, *ast.StringLiteral, *ast.BooleanLiteral,
		*ast.UndefinedLiteral, *ast.ErrorLiteral:
		return refs, true
	case *ast.AttributeReference:
		if v.Scope != ast.NoScope && v.Scope != ast.MyScope {
			if scoped {
				return refs, true
			}
			return nil, false // TARGET., PARENT. and absolute references name another ad
		}
		if classad.IsCurrentTimeName(v.Name) {
			return nil, false // the clock, unless the ad defines it; either way not worth the doubt
		}
		return append(refs, v.Name), true
	case *ast.BinaryOp:
		if _, ok := pureBinOps[v.Op]; !ok {
			return nil, false
		}
		left, ok := rec(v.Left, refs)
		if !ok {
			return nil, false
		}
		return rec(v.Right, left)
	case *ast.UnaryOp:
		switch v.Op {
		case "-", "+", "!", "~":
		default:
			return nil, false
		}
		return rec(v.Expr, refs)
	case *ast.ParenExpr:
		return rec(v.Inner, refs)
	case *ast.ConditionalExpr:
		for _, x := range []ast.Expr{v.Condition, v.TrueExpr, v.FalseExpr} {
			var ok bool
			if refs, ok = rec(x, refs); !ok {
				return nil, false
			}
		}
		return refs, true
	case *ast.ElvisExpr:
		left, ok := rec(v.Left, refs)
		if !ok {
			return nil, false
		}
		return rec(v.Right, left)
	case *ast.FunctionCall:
		if _, ok := pureFuncs[strings.ToLower(v.Name)]; !ok {
			return nil, false
		}
		for _, a := range v.Args {
			var ok bool
			if refs, ok = rec(a, refs); !ok {
				return nil, false
			}
		}
		return refs, true
	}
	return nil, false
}

// computedCell is one record's computed value for one schema field.
type computedCell struct {
	idx  int    // the field's index in the block schema
	node []byte // the value as a scalar literal wire node
}

// exprInfo is the purity analysis of one expression node, cached by its bytes: a segment's records
// overwhelmingly repeat the same few expression texts.
type exprInfo struct {
	pure bool
	refs []uint32 // the global ids of the attributes it references
}

// cellComputer computes the values of expression-valued schema cells from records in GLOBAL-interned
// wire form (the form the columnar builder and the schema derivation both read). Not safe for concurrent
// use; one per build.
type cellComputer struct {
	c     *Collection
	attrs map[uint32][]byte // the current record's attributes by global id
	info  map[string]exprInfo
	seen  map[uint32]struct{}
	queue []uint32
}

func newCellComputer(c *Collection) *cellComputer {
	if c == nil || c.intern == nil {
		return nil
	}
	return &cellComputer{
		c:     c,
		attrs: make(map[uint32][]byte, 64),
		info:  make(map[string]exprInfo, 8),
		seen:  make(map[uint32]struct{}, 8),
	}
}

// load indexes a record's attributes for the lookups that follow.
func (cc *cellComputer) load(iw []byte) bool {
	clear(cc.attrs)
	return wire.Ad(iw).ForEach(func(id uint32, node []byte) bool {
		if _, dup := cc.attrs[id]; !dup {
			cc.attrs[id] = node
		}
		return true
	})
}

// cells computes every computable expression-valued cell of schema s in the record iw.
func (cc *cellComputer) cells(iw []byte, s *adSchema) []computedCell {
	if cc == nil || s == nil {
		return nil
	}
	var out []computedCell
	loaded := false
	wire.Ad(iw).ForEach(func(id uint32, node []byte) bool {
		idx, ok := s.byID[id]
		if !ok || storableInColumn(node) || wire.IsEncryptedNode(node) {
			return true // not a field, or a literal the column holds itself, or sealed
		}
		if !loaded {
			if !cc.load(iw) {
				return false
			}
			loaded = true
		}
		if v, ok := cc.value(id); ok {
			out = append(out, computedCell{idx: idx, node: v})
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].idx < out[j].idx })
	return out
}

// value evaluates attribute id of the loaded record, returning its value as a literal node when the
// cell qualifies.
func (cc *cellComputer) value(id uint32) ([]byte, bool) {
	c := cc.c
	clear(cc.seen)
	cc.queue = append(cc.queue[:0], id)
	cc.seen[id] = struct{}{}
	for i := 0; i < len(cc.queue); i++ {
		aid := cc.queue[i]
		node, ok := cc.attrs[aid]
		if !ok {
			return nil, false // a reference to an attribute the record lacks
		}
		name, ok := c.intern.Name(aid)
		if !ok || wire.IsEncryptedNode(node) || c.shouldEncrypt(name) {
			return nil, false
		}
		if _, lit := scalarValue(node); lit {
			continue
		}
		inf := cc.analyze(node)
		if !inf.pure {
			return nil, false
		}
		for _, r := range inf.refs {
			if _, dup := cc.seen[r]; dup {
				continue
			}
			if len(cc.seen) >= computedClosureMax {
				return nil, false
			}
			cc.seen[r] = struct{}{}
			cc.queue = append(cc.queue, r)
		}
	}
	// Every attribute the value can reach is in the queue, and every one is in the record, so an ad
	// holding exactly these evaluates the attribute as the whole record would.
	ad := classad.New()
	for _, aid := range cc.queue {
		name, _ := c.intern.Name(aid)
		expr, err := wire.DecodeNodeResolve(cc.attrs[aid], c.intern.Name)
		if err != nil {
			return nil, false
		}
		ad.Insert(name, expr)
	}
	name, _ := c.intern.Name(id)
	return valueNode(ad.EvaluateAttr(name))
}

// analyze decodes an expression node and checks it, caching the answer by the node's bytes.
func (cc *cellComputer) analyze(node []byte) exprInfo {
	if inf, ok := cc.info[string(node)]; ok {
		return inf
	}
	inf := exprInfo{}
	expr, err := wire.DecodeNodeResolve(node, cc.c.intern.Name)
	if err == nil {
		if names, ok := pureRefs(expr, nil); ok {
			inf.pure = true
			for _, n := range names {
				rid, known := cc.c.intern.LookupID(n)
				if !known {
					// Never interned, so no record holds it: an absent reference.
					inf.pure = false
					break
				}
				inf.refs = append(inf.refs, rid)
			}
		}
	}
	if len(cc.info) < 1024 {
		cc.info[string(node)] = inf
	}
	return inf
}

// valueNode encodes an evaluated value as a scalar literal node, for the kinds a computed cell holds.
func valueNode(v classad.Value) ([]byte, bool) {
	switch {
	case v.IsInteger():
		if i, err := v.IntValue(); err == nil {
			return wire.AppendIntNode(nil, i), true
		}
	case v.IsReal():
		if f, err := v.RealValue(); err == nil && !math.IsNaN(f) {
			return wire.AppendRealNode(nil, f), true
		}
	case v.IsString():
		if s, err := v.StringValue(); err == nil {
			return wire.AppendStringNode(nil, s), true
		}
	case v.IsBool():
		if b, err := v.BoolValue(); err == nil {
			return wire.AppendBoolNode(nil, b), true
		}
	}
	return nil, false
}

// computedKind is the kind a schema derivation counts an expression-valued cell as: the kind of the
// value it would be computed to, or akNone when it would not be computed. The sample is global-interned
// wire, the form the builder computes from, so a field the derivation admits on its computed values is
// one the builder will compute.
func (cc *cellComputer) computedKind(iw []byte, id uint32) adKind {
	if cc == nil {
		return akNone
	}
	if !cc.load(iw) {
		return akNone
	}
	node, ok := cc.value(id)
	if !ok {
		return akNone
	}
	k, _ := nodeKind(node)
	return k
}

// computedCellsBuilt counts computed cells written into columnarized segments, so a test can see the
// builder took the path it is meant to.
var computedCellsBuilt atomic.Int64

// computedEntry locates one field's computed cells in a block's decompressed computed region.
type computedEntry struct {
	idx   int    // schema field index
	start uint32 // the field's section offset in the decompressed region
}

// buildComputedRegion lays out a block's computed cells. rows[k] are record k's cells, sorted by field.
//
// The region is one section per field that has any computed cell, each
//
//	(n+1) little-endian u32 offsets | the cells' literal nodes, concatenated
//
// so record k's value is nodes[off[k]:off[k+1]], and an empty span means the cell is not computed. A
// reader finds a value with two loads and no walk, which is what lets the per-record readers ask for one
// at the cost of an escaped literal rather than a record read.
func buildComputedRegion(rows [][]computedCell) ([]computedEntry, []byte) {
	n := len(rows)
	var idxs []int
	seen := map[int]struct{}{}
	for _, r := range rows {
		for _, cell := range r {
			if _, ok := seen[cell.idx]; !ok {
				seen[cell.idx] = struct{}{}
				idxs = append(idxs, cell.idx)
			}
		}
	}
	if len(idxs) == 0 {
		return nil, nil
	}
	sort.Ints(idxs)
	var raw []byte
	entries := make([]computedEntry, 0, len(idxs))
	cursor := make([]int, n) // per record, the next of its cells to consider
	for _, idx := range idxs {
		entries = append(entries, computedEntry{idx: idx, start: uint32(len(raw))})
		var nodes []byte
		offs := make([]byte, 0, 4*(n+1))
		offs = binary.LittleEndian.AppendUint32(offs, 0)
		for k, r := range rows {
			for cursor[k] < len(r) && r[cursor[k]].idx < idx {
				cursor[k]++
			}
			if cursor[k] < len(r) && r[cursor[k]].idx == idx {
				nodes = append(nodes, r[cursor[k]].node...)
				cursor[k]++
			}
			offs = binary.LittleEndian.AppendUint32(offs, uint32(len(nodes)))
		}
		raw = append(raw, offs...)
		raw = append(raw, nodes...)
	}
	return entries, raw
}

// computedEntryOf returns field idx's section in the block's computed region.
func (b *columnarBlock) computedEntryOf(idx int) (computedEntry, bool) {
	i := sort.Search(len(b.computed), func(i int) bool { return b.computed[i].idx >= idx })
	if i < len(b.computed) && b.computed[i].idx == idx {
		return b.computed[i], true
	}
	return computedEntry{}, false
}

// computedNode returns record k's computed value for schema field idx, as a scalar literal node. ok is
// false when the cell is not computed -- or when the region cannot be read, which a caller treats
// exactly the same way, since the record still holds the expression.
func (b *columnarBlock) computedNode(k, idx int, bc *blockCache) ([]byte, bool) {
	if len(b.computed) == 0 {
		return nil, false
	}
	e, ok := b.computedEntryOf(idx)
	if !ok {
		return nil, false
	}
	raw, err := bc.stream(b, kindComputed)
	if err != nil {
		return nil, false
	}
	return computedNodeIn(raw, e.start, b.n, k)
}

// computedNodeIn reads record k's cell from a field section starting at start in the decompressed
// region raw, for a block of n records. Bounds and the literal are checked rather than trusted.
func computedNodeIn(raw []byte, start uint32, n, k int) ([]byte, bool) {
	hdr := int(start)
	base := hdr + 4*(n+1)
	if k < 0 || k >= n || hdr < 0 || base > len(raw) {
		return nil, false
	}
	o1 := int(binary.LittleEndian.Uint32(raw[hdr+4*k:]))
	o2 := int(binary.LittleEndian.Uint32(raw[hdr+4*k+4:]))
	if o1 >= o2 || base+o2 > len(raw) {
		return nil, false
	}
	node := raw[base+o1 : base+o2]
	if nl, ok := wire.NodeLen(node); !ok || nl != len(node) {
		return nil, false
	}
	if _, ok := wire.LiteralValue(node); !ok {
		return nil, false
	}
	return node, true
}

// computedValue is computedNode as a ClassAd value, for the per-record resolver.
func (b *columnarBlock) computedValue(k, idx int, bc *blockCache) (classad.Value, bool) {
	node, ok := b.computedNode(k, idx, bc)
	if !ok {
		return classad.Value{}, false
	}
	lit, ok := wire.LiteralValue(node)
	if !ok {
		return classad.Value{}, false
	}
	return litToValue(lit), true
}

// colComputedTrailer marks a columnarized segment's computed-value trailer: the magic, the base block
// count, then per block its compressed region and its field sections as (field index, start) pairs.
//
// A TRAILER, after the remnant-name list, rather than a payload version: the records and the columns are
// unchanged, so a build that does not know about computed values reads such a payload correctly by
// ignoring what follows the group section, and a store written here still opens under it. It cannot
// parse the remnant-name list once something follows it, so such a build answers some reads by the
// record that the list would have settled -- slower, never wrong.
const colComputedTrailer = 0x504d4f43 // "COMP" on disk (little-endian)

// hasComputed reports whether any base block of the segment carries computed values.
func (cs *colSegment) hasComputed() bool {
	for _, b := range cs.blocks {
		if len(b.computed) > 0 {
			return true
		}
	}
	return false
}

func appendComputedTrailer(dst []byte, blocks []*columnarBlock) []byte {
	dst = appendU32(dst, colComputedTrailer)
	dst = appendU32(dst, uint32(len(blocks)))
	for _, b := range blocks {
		dst = appendBytes(dst, b.computedComp)
		dst = appendU32(dst, uint32(len(b.computed)))
		for _, e := range b.computed {
			dst = appendU32(dst, uint32(e.idx))
			dst = appendU32(dst, e.start)
		}
	}
	return dst
}

// readComputedTrailer decodes a computed trailer occupying exactly rest onto cs's base blocks. A trailer
// that does not parse, or describes a different block list, is dropped whole: the cells read as they did
// before computed values existed.
func readComputedTrailer(rest []byte, cs *colSegment) bool {
	c := &cursor{b: rest}
	if len(rest) < 8 || c.u32() != colComputedTrailer {
		return false
	}
	nb := int(c.u32())
	if c.err != nil || nb != len(cs.blocks) {
		return false
	}
	comps := make([][]byte, nb)
	entries := make([][]computedEntry, nb)
	for i := 0; i < nb; i++ {
		comps[i] = c.bytes()
		ne := int(c.u32())
		if c.err != nil || ne < 0 || ne > len(cs.blocks[i].schema.fields) {
			return false
		}
		prev := -1
		for j := 0; j < ne; j++ {
			idx := int(c.u32())
			start := c.u32()
			if c.err != nil || idx <= prev || idx >= len(cs.blocks[i].schema.fields) {
				return false
			}
			prev = idx
			entries[i] = append(entries[i], computedEntry{idx: idx, start: start})
		}
		if ne > 0 && len(comps[i]) == 0 {
			return false
		}
	}
	if c.err != nil || c.i != len(rest) {
		return false
	}
	for i, b := range cs.blocks {
		b.computedComp, b.computed = comps[i], entries[i]
	}
	return true
}

// attachComputed gives each block its computed region, from rows indexed by segment record position.
// The blocks are in record order and partition the rows.
func attachComputed(blocks []*columnarBlock, rows [][]computedCell, codec Codec) {
	base := 0
	for _, b := range blocks {
		if base+b.n > len(rows) {
			return
		}
		entries, raw := buildComputedRegion(rows[base : base+b.n])
		if len(entries) > 0 {
			b.computed, b.computedComp = entries, codec.Compress(nil, raw)
			for _, r := range rows[base : base+b.n] {
				computedCellsBuilt.Add(int64(len(r)))
			}
		}
		base += b.n
	}
}

// PER-RECORD EXCEPTIONS.
//
// A held cell that was not computed -- its expression names another ad, an attribute the record lacks,
// or anything else the rules above refuse -- used to give the whole query back to the row path the
// moment a numeric reader met it. With computed values the remaining held cells are the exceptions, and
// one user-written `TARGET.` expression in an archive would cost every query on that attribute its
// columns. So a reader that meets one evaluates THAT record instead: the record is reassembled and the
// attribute evaluated in it alone, exactly as the scan the reader stands in for evaluates it, and the
// value is then handled as the literal it evaluates to.
//
// Only where that is exactly the scan's answer. The value must not depend on the clock or on chance
// (deterministicRefs), and it must be a scalar literal other than ERROR -- the readers' handling of an
// ERROR cell is not the scan's. Anything else keeps the whole-query fallback.
//
// And only where it is cheap. Reassembling a record costs far more than reading a cell, so a block
// whose held cells are mostly uncomputed -- every block of a segment columnarized before computed values
// existed, for one -- keeps the whole-query fallback, which is the faster way to read that many records.

// heldExceptionShare bounds the share of a block's records that may be evaluated one at a time: past
// 1 in heldExceptionShare, the block's reader gives the query back instead. A block may always evaluate
// heldExceptionFloor of them, so a small block is not refused over a handful of records.
const (
	heldExceptionShare = 8
	heldExceptionFloor = 8
)

// heldExceptionEvals counts held cells evaluated one record at a time, so a test can see the path ran.
var heldExceptionEvals atomic.Int64

// heldUncomputed returns how many records of this block hold field idx (id) in the record without a
// computed value, or -1 when the held index cannot say. Cached on the segment's held index, since both
// the index and the computed values are fixed for the segment's life.
func (b *columnarBlock) heldUncomputed(idx int, id globalID, bc *blockCache) int {
	h := b.native.held()
	if h == nil || !h.ok {
		return -1
	}
	key := [2]int{b.segBase, idx}
	h.excMu.Lock()
	v, ok := h.uncomputed[key]
	h.excMu.Unlock()
	if ok {
		return v
	}
	list := h.recs[id]
	lo := uint32(b.segBase)
	i := sort.Search(len(list), func(i int) bool { return list[i] >= lo })
	for ; i < len(list) && list[i] < lo+uint32(b.n); i++ {
		if _, ok := b.computedNode(int(list[i]-lo), idx, bc); !ok {
			v++
		}
	}
	h.excMu.Lock()
	if h.uncomputed == nil {
		h.uncomputed = map[[2]int]int{}
	}
	h.uncomputed[key] = v
	h.excMu.Unlock()
	return v
}

// heldValueNode evaluates schema field fieldID of record k, which holds it in the record uncomputed,
// returning the value as a literal node when that is exactly the scan's value (see above). The caller
// has the segment pinned, as for heldAt, and has already decided the record is visible.
func (b *columnarBlock) heldValueNode(k int, fieldID globalID, bc *blockCache) ([]byte, bool) {
	cs := b.native
	if cs == nil || cs.owner == nil || cs.arena == nil || cs.owner.intern == nil {
		return nil, false
	}
	c, seg := cs.owner, cs.arena
	idx, ok := b.schema.byID[uint32(fieldID)]
	if !ok {
		return nil, false
	}
	if n := b.heldUncomputed(idx, fieldID, bc); n < 0 || (n > heldExceptionFloor && n*heldExceptionShare > b.n) {
		return nil, false
	}
	name, ok := c.intern.Name(uint32(fieldID))
	if !ok || c.shouldEncrypt(name) {
		return nil, false
	}
	gk := b.segBase + k
	if gk >= cs.offsLen() || seg.dictUnusable.Load() {
		return nil, false
	}
	ww, err := c.recordWire(seg, cs.offAt(gk), nil)
	if err != nil {
		return nil, false
	}
	return c.recordValueNode(seg.dict.Load(), wire.Ad(ww), name)
}

// recordValueNode evaluates attribute name of one record -- its wire form a, in its own key space (d
// for an interned segment) -- alone, as the scan evaluates it, and returns the value as a literal node
// when that is exactly the scan's value (see PER-RECORD EXCEPTIONS). Only the attribute's closure is
// decoded: an ad holding exactly the attributes the value can reach evaluates it as the whole record
// does, a reference to an attribute the record lacks being UNDEFINED in both.
func (c *Collection) recordValueNode(d *segDictHandle, a wire.Ad, name string) ([]byte, bool) {
	if c.intern == nil {
		return nil, false
	}
	ad := classad.New()
	seen := map[string]struct{}{strings.ToLower(name): {}}
	queue := []string{name}
	for i := 0; i < len(queue); i++ {
		nm := queue[i]
		node, ok := c.wireLookupDict(d, a, nm)
		if !ok {
			continue // absent: UNDEFINED, alone as in the whole record
		}
		if wire.IsEncryptedNode(node) || c.shouldEncrypt(nm) {
			return nil, false
		}
		expr, err := c.decodeNodeDict(d, node)
		if err != nil {
			return nil, false
		}
		refs, ok := deterministicRefs(expr, nil)
		if !ok {
			return nil, false
		}
		ad.Insert(nm, expr)
		for _, r := range refs {
			f := strings.ToLower(r)
			if _, dup := seen[f]; dup {
				continue
			}
			if len(seen) >= computedClosureMax {
				return nil, false
			}
			seen[f] = struct{}{}
			queue = append(queue, r)
		}
	}
	v := ad.EvaluateAttr(name)
	var node []byte
	switch {
	case v.IsUndefined():
		node = wire.AppendUndefinedNode(nil)
	case v.IsReal():
		f, err := v.RealValue()
		if err != nil {
			return nil, false
		}
		node = wire.AppendRealNode(nil, f) // NaN included: a NaN literal is read the same way
	default:
		var ok bool
		if node, ok = valueNode(v); !ok {
			return nil, false // ERROR, a list, an ad
		}
	}
	heldExceptionEvals.Add(1)
	return node, true
}

// evaluatingLookup wraps a row fallback's attribute lookup so an expression-valued attribute comes back
// as the literal it evaluates to in that record, where recordValueNode can give it exactly; anything else
// comes back unchanged, and the caller declines on an expression as it always has. This is the row-walk
// half of the per-record exceptions: a segment with no columns -- the one still being written, above
// all -- would otherwise give back every query on an attribute any of its records computes.
func (c *Collection) evaluatingLookup(id uint32, base fieldLookup) fieldLookup {
	name, ok := c.intern.Name(id)
	if !ok {
		return base
	}
	return func(d *segDictHandle, a wire.Ad) ([]byte, bool) {
		node, found := base(d, a)
		if !found || !isExprNode(node) {
			return node, found
		}
		if v, ok := c.recordValueNode(d, a, name); ok {
			return v, true
		}
		return node, true
	}
}
