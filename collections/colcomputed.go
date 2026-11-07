package collections

import (
	"encoding/binary"
	"math"
	"sort"
	"strings"
	"sync"
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
// is computed once, when the segment is columnarized, and written into the field's column (see COMPUTED
// CELLS LIVE IN THE COLUMN below), so the column readers read it like any other value.
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

// computedKind is the kind and value a schema derivation counts an expression-valued cell as: what it
// would be computed to, or akNone when it would not be. The sample is global-interned wire, the form the
// builder computes from, so a field the derivation admits on its computed values is one the builder will
// compute.
func (cc *cellComputer) computedKind(iw []byte, id uint32) (adKind, wire.Literal) {
	if cc == nil {
		return akNone, wire.Literal{}
	}
	if !cc.load(iw) {
		return akNone, wire.Literal{}
	}
	node, ok := cc.value(id)
	if !ok {
		return akNone, wire.Literal{}
	}
	return nodeKind(node)
}

// computedCellsBuilt counts computed cells written into columnarized segments, so a test can see the
// builder took the path it is meant to.
var computedCellsBuilt atomic.Int64

// COMPUTED CELLS LIVE IN THE COLUMN.
//
// A computed value is written into its field's own storage, exactly where a literal of that field would
// be: the typed fixed slot for an int, real or bool (same width and encoding), and the block's string
// dictionary for a string. A per-block, per-field bitmap marks which cells hold one. Column-at-a-time
// readers -- the vector loader, the aggregates, GROUP BY, TopK, presence, zone pruning -- therefore read
// a computed cell as they read any other, and an Arrow-shaped export of the block finds the values in the
// value buffer and the dictionary.
//
// THE ESCAPE BIT STAYS SET. A computed cell is still, for every purpose but its value, the cell of a
// record that keeps an expression: the record holds the expression, the cell is escaped, nothing is in
// the cold tail. The bitmap says only "this escaped cell's slot holds the value the expression evaluates
// to". That is a deliberate choice of which readers have to know about it:
//
//   - A reader that rebuilds an ad or ships an attribute -- reassembly, projection, raw wire, text,
//     re-columnarization, everything recordWire feeds -- tests the escape bit, finds the cell escaped, and
//     takes the attribute from the record: the expression. It reads exactly the bits and bytes it read
//     before the value was in the slot, so it cannot emit the value as if it were the attribute's
//     literal, and no form reader had to change. With the escape bit clear, every one of them would have
//     had to learn the bitmap, and one that did not would silently ship MemoryUsage = 1234.
//   - A reader of VALUES asks for the bitmap (valueEscaped, valueEscapeFree, computedBits) and treats a
//     computed cell as present. One that has not been taught sees an escaped held cell and takes the
//     per-record path, as it did before: slower, never wrong.
//   - A build that predates this reads the same payload correctly. Escaped slots are undefined bytes to it
//     (it never reads one without consulting the escape bit), the persisted zones and dictionary flags
//     are written by the rules it knows, and the bitmap rides where it ignores it (see readComputedTrailer).
//     So no payload version changes, and a rollback costs speed, not records.
//
// WHAT IS STORED. A value goes into the column only when it fits it as a literal would: the same kind as
// the field (an int is not stored in a real column, nor the reverse -- the two differ under =?= and
// integer division), an int within the slot's width, a string only where the block keeps a dictionary for
// the field (forced for a field with computed strings, since the positional string region is walked by
// readers that skip escaped cells). A value that does not fit -- a real result among an int column's, say,
// or one integer far wider than the rest -- is kept beside the column instead, as a literal node in the
// field's short list of exceptions, which a per-record reader finds by record index. The column then has
// no value for that cell (its bit is clear, the block is not exact), so a column reader treats it as any
// escape and asks for it: one lookup, never an evaluation.
//
// Beside each field's bitmap the block keeps whether every escape of the field in the block is a computed
// cell (exact) and, for a numeric field, the range of literal and computed values together. The persisted
// zone and the dictionary's noEscape flag keep their meaning -- literals only -- so an older build prunes
// exactly as it did; this build prunes on the value range when it is exact (see valueZone).

// computedCol is one schema field's computed cells in a block.
type computedCol struct {
	idx      int
	exact    bool    // every escaped cell of the field in this block is a computed cell in the column
	min, max float64 // numeric fields: the range of the literal and computed values, when exact
	bits     []byte  // one bit per record, set where the cell is computed into the column
	// The computed cells that do not fit the column (see WHAT IS STORED): misRecs their record indices,
	// ascending, as little-endian u32s; misOffs (len(misRecs)/4 + 1) u32 offsets into misNodes, which holds
	// each one's literal node. All empty when every computed cell fits.
	misRecs, misOffs, misNodes []byte
}

// misfitNode returns record k's computed value kept beside the column, if it has one. The offsets and the
// node are checked rather than trusted.
func (cc *computedCol) misfitNode(k int) ([]byte, bool) {
	m := len(cc.misRecs) / 4
	if m == 0 {
		return nil, false
	}
	i := sort.Search(m, func(i int) bool { return int(binary.LittleEndian.Uint32(cc.misRecs[4*i:])) >= k })
	if i >= m || int(binary.LittleEndian.Uint32(cc.misRecs[4*i:])) != k {
		return nil, false
	}
	o1 := int(binary.LittleEndian.Uint32(cc.misOffs[4*i:]))
	o2 := int(binary.LittleEndian.Uint32(cc.misOffs[4*i+4:]))
	if o1 >= o2 || o2 > len(cc.misNodes) {
		return nil, false
	}
	node := cc.misNodes[o1:o2]
	if nl, ok := wire.NodeLen(node); !ok || nl != len(node) {
		return nil, false
	}
	if _, ok := wire.LiteralValue(node); !ok {
		return nil, false
	}
	return node, true
}

// colComputedColsMagic opens a block's computed-cell blob (see encodeComputedCols).
const colComputedColsMagic = 0x4c4f4343 // "CCOL" on disk (little-endian)

// computedColOf returns field idx's computed cells in this block, or nil when it has none.
func (b *columnarBlock) computedColOf(idx int) *computedCol {
	for i := range b.ccols {
		if b.ccols[i].idx == idx {
			return &b.ccols[i]
		}
	}
	return nil
}

// computedBits returns field idx's computed bitmap, nil when no cell of it is computed in this block.
func (b *columnarBlock) computedBits(idx int) []byte {
	if cc := b.computedColOf(idx); cc != nil {
		return cc.bits
	}
	return nil
}

// valueEscaped reports whether record k's cell of field idx has no value in the column: escaped and not
// computed. A value reader uses it where it would test the escape bit; a form reader must not.
func (b *columnarBlock) valueEscaped(k, idx int) bool {
	if !testBit(b.escapeAt(k), idx) {
		return false
	}
	cb := b.computedBits(idx)
	return cb == nil || !testBit(cb, k)
}

// valueEscapeFree reports whether every record of the block has field idx's value in the column, as a
// literal or a computed cell -- escapeFree for value readers.
func (b *columnarBlock) valueEscapeFree(idx int) bool {
	if b.escapeFree(idx) {
		return true
	}
	cc := b.computedColOf(idx)
	return cc != nil && cc.exact
}

// valueZone is zone for value readers: where the literal zone is inexact only because of computed cells,
// the range covering them too.
func (b *columnarBlock) valueZone(idx int) (blockZone, bool) {
	z, ok := b.zone(idx)
	if !ok || !z.escaped {
		return z, ok
	}
	if cc := b.computedColOf(idx); cc != nil && cc.exact {
		return blockZone{Min: cc.min, Max: cc.max}, true
	}
	return z, true
}

// anyComputed reports whether the block keeps any computed value, in either form.
func (b *columnarBlock) anyComputed() bool { return len(b.ccols) > 0 || len(b.computed) > 0 }

// slotNode encodes record k's in-column value of field idx as a scalar literal node.
func (b *columnarBlock) slotNode(k, idx int, bc *blockCache) ([]byte, bool) {
	f := &b.schema.fields[idx]
	switch f.kind {
	case akBool:
		base := k*b.bitsStride + b.schema.escBytes
		return wire.AppendBoolNode(nil, testBit(b.bits[base:base+b.schema.boolBytes], f.boolBit)), true
	case akInt, akReal:
		v, ok := b.fieldIntAt(k, idx, bc)
		if !ok {
			return nil, false
		}
		if f.kind == akReal {
			return wire.AppendRealNode(nil, math.Float64frombits(uint64(v))), true
		}
		return wire.AppendIntNode(nil, v), true
	case akString:
		var buf [][]byte
		entries, ok := b.dictEntries(idx, bc, &buf)
		if !ok {
			return nil, false
		}
		codes, w, ok := b.dictCodes(idx, bc)
		if !ok {
			return nil, false
		}
		c := dictCodeAt(codes, w, k)
		if c >= len(entries) {
			return nil, false
		}
		return wire.AppendStringNode(nil, string(entries[c])), true
	}
	return nil, false
}

// computedBuild collects one block's computed cells while it is encoded: which survive into the column,
// and the string values the dictionary builder has to see.
type computedBuild struct {
	s     *adSchema
	n     int
	cells [][]computedCell // per record, the cells that fit their column
	strs  map[int]*computedStrs
	mis   map[int][]misfit // per field, the cells that do not, in record order
}

// misfit is one computed cell kept beside the column.
type misfit struct {
	k    int
	node []byte
}

// computedStrs is one string field's computed values, by record.
type computedStrs struct {
	vals    [][]byte
	present []bool
}

// newComputedBuild sorts each record's computed cells into those that fit their column as a literal would
// and those kept beside it (see WHAT IS STORED). recs are the block's records in row form, in which every
// computed cell is escaped: the builder never sees the expression, only its absence. A cell that is not
// escaped there is a field the record also holds as a literal, which a well-formed record cannot do, and
// it is dropped.
func newComputedBuild(s *adSchema, recs [][]byte, cells [][]computedCell) *computedBuild {
	if len(cells) != len(recs) {
		return nil
	}
	cb := &computedBuild{s: s, n: len(recs), cells: make([][]computedCell, len(recs))}
	kept := false
	for k, row := range cells {
		for _, cell := range row {
			if cell.idx < 0 || cell.idx >= len(s.fields) || !testBit(recs[k][:s.escBytes], cell.idx) {
				continue
			}
			f := &s.fields[cell.idx]
			kind, lit := nodeKind(cell.node)
			if kind != f.kind || (kind == akInt && !intFits(lit.Int, f.width, f.unsigned)) || realEscapes(kind, lit.Real) ||
				(kind == akString && !strDictEnabled) {
				// Not this column's literal -- or, for a string, no dictionary to hold it, and the
				// positional region cannot: beside the column.
				if cb.mis == nil {
					cb.mis = map[int][]misfit{}
				}
				cb.mis[cell.idx] = append(cb.mis[cell.idx], misfit{k: k, node: cell.node})
				kept = true
				continue
			}
			if kind == akString {
				if cb.strs == nil {
					cb.strs = map[int]*computedStrs{}
				}
				cs := cb.strs[cell.idx]
				if cs == nil {
					cs = &computedStrs{vals: make([][]byte, len(recs)), present: make([]bool, len(recs))}
					cb.strs[cell.idx] = cs
				}
				cs.vals[k], cs.present[k] = []byte(lit.Str), true
			}
			cb.cells[k] = append(cb.cells[k], cell)
			kept = true
		}
	}
	if !kept {
		return nil
	}
	return cb
}

// stringsFor returns the computed string values the dictionary builder must include, nil for none.
func (cb *computedBuild) stringsFor() map[int]*computedStrs {
	if cb == nil {
		return nil
	}
	return cb.strs
}

// apply writes the computed values into b's columns -- the hot columns and b.bits in place, the cold
// numerics into coldRaw before it is compressed, the strings as codes in codeRaw against the dictionaries
// that took them -- and records the per-field bitmaps. recs are the block's records in row form.
func (cb *computedBuild) apply(b *columnarBlock, recs [][]byte, coldRaw []byte, dicts map[int]strDictField, dictRaw, codeRaw []byte) {
	if cb == nil {
		return
	}
	s := b.schema
	cols := map[int]*computedCol{}
	var strIndex map[int]map[string]int
	for k, row := range cb.cells {
		for _, cell := range row {
			f := &s.fields[cell.idx]
			lit, _ := wire.LiteralValue(cell.node)
			switch f.kind {
			case akBool:
				if lit.Bool {
					setBit(b.bits[k*b.bitsStride+s.escBytes:], f.boolBit)
				}
			case akInt, akReal:
				raw := lit.Int
				if f.kind == akReal {
					raw = int64(math.Float64bits(lit.Real))
				}
				if start, hot := b.hotColStart(cell.idx); hot {
					putIntLE(b.hotCol[start+k*f.width:], raw, f.width)
				} else if start, ok := b.coldFieldStart(cell.idx); ok {
					putIntLE(coldRaw[start+k*f.width:], raw, f.width)
				} else {
					continue
				}
			case akString:
				info, ok := dicts[cell.idx]
				if !ok {
					// The dictionary was not built after all: beside the column.
					if cb.mis == nil {
						cb.mis = map[int][]misfit{}
					}
					cb.mis[cell.idx] = append(cb.mis[cell.idx], misfit{k: k, node: cell.node})
					continue
				}
				if strIndex == nil {
					strIndex = map[int]map[string]int{}
				}
				index := strIndex[cell.idx]
				if index == nil {
					index = dictIndex(dictRaw, info)
					strIndex[cell.idx] = index
				}
				code, ok := index[lit.Str]
				if !ok {
					continue
				}
				off := info.codeStart + k*info.codeWidth
				codeRaw[off] = byte(code)
				if info.codeWidth == 2 {
					codeRaw[off+1] = byte(code >> 8)
				}
			default:
				continue
			}
			cc := cols[cell.idx]
			if cc == nil {
				cc = &computedCol{idx: cell.idx, bits: make([]byte, (cb.n+7)/8)}
				cols[cell.idx] = cc
			}
			setBit(cc.bits, k)
			computedCellsBuilt.Add(1)
		}
	}
	for idx, ms := range cb.mis {
		cc := cols[idx]
		if cc == nil {
			cc = &computedCol{idx: idx, bits: make([]byte, (cb.n+7)/8)}
			cols[idx] = cc
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i].k < ms[j].k })
		cc.misOffs = binary.LittleEndian.AppendUint32(nil, 0)
		for _, m := range ms {
			cc.misRecs = binary.LittleEndian.AppendUint32(cc.misRecs, uint32(m.k))
			cc.misNodes = append(cc.misNodes, m.node...)
			cc.misOffs = binary.LittleEndian.AppendUint32(cc.misOffs, uint32(len(cc.misNodes)))
		}
		computedCellsBuilt.Add(int64(len(ms)))
	}
	if len(cols) == 0 {
		return
	}
	for idx, cc := range cols {
		f := &s.fields[idx]
		cc.exact = true
		cc.min, cc.max = math.Inf(1), math.Inf(-1)
		for k, r := range recs {
			escaped := testBit(r[:s.escBytes], idx)
			if escaped && !testBit(cc.bits, k) {
				cc.exact = false
				break
			}
			if numericKind(f.kind) {
				var raw int64
				if escaped {
					raw, _ = b.slotIntIn(k, idx, coldRaw)
				} else {
					raw = readIntLE(r[s.escBytes+f.off:], f.width, f.unsigned)
				}
				v := float64(raw)
				if f.kind == akReal {
					v = math.Float64frombits(uint64(raw))
				}
				cc.min, cc.max = math.Min(cc.min, v), math.Max(cc.max, v)
			}
		}
		if !cc.exact || !numericKind(f.kind) {
			cc.min, cc.max = 0, 0
		}
		b.ccols = append(b.ccols, *cc)
	}
	sort.Slice(b.ccols, func(i, j int) bool { return b.ccols[i].idx < b.ccols[j].idx })
	b.ccBlob = encodeComputedCols(b.ccols, b.n)
}

// slotIntIn reads record k's raw numeric slot of field idx from the block's hot columns or from coldRaw,
// the cold numerics as they are before compression.
func (b *columnarBlock) slotIntIn(k, idx int, coldRaw []byte) (int64, bool) {
	f := &b.schema.fields[idx]
	if start, hot := b.hotColStart(idx); hot {
		return readIntLE(b.hotCol[start+k*f.width:], f.width, f.unsigned), true
	}
	if start, ok := b.coldFieldStart(idx); ok {
		return readIntLE(coldRaw[start+k*f.width:], f.width, f.unsigned), true
	}
	return 0, false
}

// dictIndex maps each of a field's dictionary entries to its code, from the raw dictionary region.
func dictIndex(dictRaw []byte, info strDictField) map[string]int {
	out := make(map[string]int, info.count)
	p := dictRaw[info.dictStart:]
	for c := 0; c < info.count; c++ {
		l, m := binary.Uvarint(p)
		if m <= 0 || m+int(l) > len(p) {
			return out
		}
		out[string(p[m:m+int(l)])] = c
		p = p[m+int(l):]
	}
	return out
}

// encodeComputedCols lays out a block's computed cells:
//
//	magic u32 | field count u32 | per field:
//	    index u32 | flags u32 (1: exact) | min f64 | max f64 | bitmap |
//	    misfit count m u32 | m record indices u32 | m+1 offsets u32 | nodes (u32 length, bytes)
//
// with each bitmap (n+7)/8 bytes. Stored uncompressed: it is small, and reading it is the first thing a
// column reader does with the block, so it aliases the mapping like the block's other bitsets.
func encodeComputedCols(cols []computedCol, n int) []byte {
	if len(cols) == 0 {
		return nil
	}
	nb := (n + 7) / 8
	dst := make([]byte, 0, 8+len(cols)*(24+nb))
	dst = appendU32(dst, colComputedColsMagic)
	dst = appendU32(dst, uint32(len(cols)))
	for i := range cols {
		cc := &cols[i]
		dst = appendU32(dst, uint32(cc.idx))
		var flags uint32
		if cc.exact {
			flags |= 1
		}
		dst = appendU32(dst, flags)
		dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(cc.min))
		dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(cc.max))
		dst = append(dst, cc.bits[:nb]...)
		m := len(cc.misRecs) / 4
		dst = appendU32(dst, uint32(m))
		if m > 0 {
			dst = append(dst, cc.misRecs...)
			dst = append(dst, cc.misOffs...)
			dst = appendBytes(dst, cc.misNodes)
		}
	}
	return dst
}

// parseComputedCols decodes b's computed-cell blob. Every field must be one the block can hold a computed
// value for -- a scalar field, a string one only with a dictionary -- in ascending order, with a bitmap of
// the block's length. The bitmaps alias blob.
func parseComputedCols(blob []byte, b *columnarBlock) ([]computedCol, bool) {
	c := &cursor{b: blob}
	if len(blob) < 8 || c.u32() != colComputedColsMagic {
		return nil, false
	}
	nf := int(c.u32())
	if c.err != nil || nf <= 0 || b.schema == nil || nf > len(b.schema.fields) {
		return nil, false
	}
	nb := (b.n + 7) / 8
	out := make([]computedCol, 0, nf)
	prev := -1
	for range nf {
		idx := int(c.u32())
		flags := c.u32()
		lo := c.u64()
		hi := c.u64()
		if c.err != nil || idx <= prev || idx >= len(b.schema.fields) || !c.need(nb) {
			return nil, false
		}
		switch b.schema.fields[idx].kind {
		case akBool, akInt, akReal:
		case akString:
			if !b.dictOwns(idx) {
				return nil, false
			}
		default:
			return nil, false
		}
		prev = idx
		bits := c.b[c.i : c.i+nb : c.i+nb]
		c.i += nb
		cc := computedCol{idx: idx, exact: flags&1 != 0,
			min: math.Float64frombits(lo), max: math.Float64frombits(hi), bits: bits}
		m := int(c.u32())
		if c.err != nil || m < 0 || m > b.n {
			return nil, false
		}
		if m > 0 {
			if !c.need(8*m + 4) {
				return nil, false
			}
			cc.misRecs = c.b[c.i : c.i+4*m : c.i+4*m]
			c.i += 4 * m
			cc.misOffs = c.b[c.i : c.i+4*(m+1) : c.i+4*(m+1)]
			c.i += 4 * (m + 1)
			cc.misNodes = c.bytes()
			last := -1
			for i := 0; i < m; i++ {
				k := int(binary.LittleEndian.Uint32(cc.misRecs[4*i:]))
				if k <= last || k >= b.n {
					return nil, false
				}
				last = k
			}
		}
		out = append(out, cc)
	}
	if c.err != nil || c.i != len(blob) {
		return nil, false
	}
	return out, true
}

// LEGACY COMPUTED REGIONS.
//
// Segments columnarized before computed values moved into the column keep them in a per-block region
// beside it: one section per field, (n+1) offsets over the cells' literal nodes, compressed. Nothing
// writes that form any more. It is still read, so those segments keep their computed values until their
// next re-columnarization writes them into the column.

// computedEntry locates one field's computed cells in a block's decompressed legacy computed region.
type computedEntry struct {
	idx   int    // schema field index
	start uint32 // the field's section offset in the decompressed region
}

// computedEntryOf returns field idx's section in the block's legacy computed region.
func (b *columnarBlock) computedEntryOf(idx int) (computedEntry, bool) {
	i := sort.Search(len(b.computed), func(i int) bool { return b.computed[i].idx >= idx })
	if i < len(b.computed) && b.computed[i].idx == idx {
		return b.computed[i], true
	}
	return computedEntry{}, false
}

// computedNode returns record k's computed value for schema field idx, as a scalar literal node. ok is
// false when the cell is not computed -- or when its value cannot be read, which a caller treats exactly
// the same way, since the record still holds the expression.
func (b *columnarBlock) computedNode(k, idx int, bc *blockCache) ([]byte, bool) {
	if len(b.ccols) > 0 {
		cc := b.computedColOf(idx)
		if cc == nil || k < 0 || k >= b.n {
			return nil, false
		}
		if !testBit(cc.bits, k) {
			return cc.misfitNode(k)
		}
		return b.slotNode(k, idx, bc)
	}
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

// computedNodeIn reads record k's cell from a legacy field section starting at start in the decompressed
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
// count, then per block a byte string and that block's legacy field sections as (field index, start)
// pairs. A block written now has no legacy sections, and its byte string is its computed-cell blob
// (encodeComputedCols); a legacy block's is its compressed legacy region.
//
// A TRAILER, after the remnant-name list, rather than a payload version: the records are unchanged and a
// computed cell is still an escaped one, so a build that predates computed cells reads such a payload
// correctly by ignoring what follows the group section, and one that reads legacy regions parses this
// trailer as blocks with no sections and never looks at their bytes. A store written here still opens
// under either.
const colComputedTrailer = 0x504d4f43 // "COMP" on disk (little-endian)

// hasComputed reports whether any base block of the segment carries computed values.
func (cs *colSegment) hasComputed() bool {
	for _, b := range cs.blocks {
		if b.anyComputed() {
			return true
		}
	}
	return false
}

func appendComputedTrailer(dst []byte, blocks []*columnarBlock) []byte {
	dst = appendU32(dst, colComputedTrailer)
	dst = appendU32(dst, uint32(len(blocks)))
	for _, b := range blocks {
		if len(b.computed) > 0 {
			dst = appendBytes(dst, b.computedComp)
			dst = appendU32(dst, uint32(len(b.computed)))
			for _, e := range b.computed {
				dst = appendU32(dst, uint32(e.idx))
				dst = appendU32(dst, e.start)
			}
			continue
		}
		dst = appendBytes(dst, b.ccBlob)
		dst = appendU32(dst, 0)
	}
	return dst
}

// readComputedTrailer decodes a computed trailer occupying exactly rest onto cs's base blocks. A trailer
// that does not parse, or describes a different block list, is dropped whole: the cells read as held
// cells with no computed value, which is correct, only slower.
func readComputedTrailer(rest []byte, cs *colSegment) bool {
	n, ok := readComputedTrailerPrefix(rest, cs, true)
	return ok && n == len(rest)
}

// readComputedTrailerPrefix decodes a computed trailer at the start of rest onto cs's base blocks and
// returns its length; what follows it is an extension trailer (colExtTrailer). With exact set it attaches
// only a trailer that occupies all of rest.
func readComputedTrailerPrefix(rest []byte, cs *colSegment, exact bool) (int, bool) {
	c := &cursor{b: rest}
	if len(rest) < 8 || c.u32() != colComputedTrailer {
		return 0, false
	}
	nb := int(c.u32())
	if c.err != nil || nb != len(cs.blocks) {
		return 0, false
	}
	comps := make([][]byte, nb)
	entries := make([][]computedEntry, nb)
	cols := make([][]computedCol, nb)
	for i := 0; i < nb; i++ {
		comps[i] = c.bytes()
		ne := int(c.u32())
		if c.err != nil || ne < 0 || ne > len(cs.blocks[i].schema.fields) {
			return 0, false
		}
		prev := -1
		for j := 0; j < ne; j++ {
			idx := int(c.u32())
			start := c.u32()
			if c.err != nil || idx <= prev || idx >= len(cs.blocks[i].schema.fields) {
				return 0, false
			}
			prev = idx
			entries[i] = append(entries[i], computedEntry{idx: idx, start: start})
		}
		if ne > 0 && len(comps[i]) == 0 {
			return 0, false
		}
		if ne == 0 && len(comps[i]) > 0 {
			cc, ok := parseComputedCols(comps[i], cs.blocks[i])
			if !ok {
				return 0, false
			}
			cols[i] = cc
		}
	}
	if c.err != nil || (exact && c.i != len(rest)) {
		return 0, false
	}
	for i, b := range cs.blocks {
		if cols[i] != nil {
			b.ccols, b.ccBlob = cols[i], comps[i]
			continue
		}
		b.computedComp, b.computed = comps[i], entries[i]
	}
	return c.i, true
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
func (b *columnarBlock) heldUncomputed(idx int, id uint32, bc *blockCache) int {
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
	cb := b.computedBits(idx)
	i := sort.Search(len(list), func(i int) bool { return list[i] >= lo })
	for ; i < len(list) && list[i] < lo+uint32(b.n); i++ {
		k := int(list[i] - lo)
		if cb != nil && testBit(cb, k) {
			continue // computed into the column: no value to decode to know that
		}
		if _, ok := b.computedNode(k, idx, bc); !ok {
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
func (b *columnarBlock) heldValueNode(k int, fieldID uint32, bc *blockCache) ([]byte, bool) {
	cs := b.native
	if cs == nil || cs.owner == nil || cs.arena == nil || cs.owner.intern == nil {
		return nil, false
	}
	c, seg := cs.owner, cs.arena
	idx, ok := b.schema.byID[fieldID]
	if !ok {
		return nil, false
	}
	if n := b.heldUncomputed(idx, fieldID, bc); n < 0 || (n > heldExceptionFloor && n*heldExceptionShare > b.n) {
		return nil, false
	}
	name, ok := c.intern.Name(fieldID)
	if !ok || c.shouldEncrypt(name) {
		return nil, false
	}
	gk := b.segBase + k
	if gk >= cs.offsLen() || seg.dictUnusable.Load() {
		return nil, false
	}
	ww, err := c.recordWire(seg, cs.offAt(gk))
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
	s := getRecordScratch()
	defer func() {
		s.reset()
		recordScratchPool.Put(s)
	}()

	// names is the closure walk's queue AND its visited set: every name in it has been queued, so a
	// reference already there is a duplicate. It replaces a map keyed by strings.ToLower(name), whose
	// fold allocated a string per reference of every record read -- a closure is at most
	// computedClosureMax names and in practice two or three, so a folding scan of it is cheaper than
	// one map key.
	s.names = append(s.names, name)
	for i := 0; i < len(s.names); i++ {
		nm := s.names[i]
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
		refs, ok := deterministicRefs(expr, s.refs[:0])
		if !ok {
			return nil, false
		}
		s.refs = refs
		s.assigns = append(s.assigns, ast.AttributeAssignment{Name: nm, Value: expr})
		for _, r := range refs {
			if s.queued(r) {
				continue
			}
			if len(s.names) >= computedClosureMax {
				return nil, false
			}
			s.names = append(s.names, r)
		}
	}
	// The closure as a READ-ONLY ad. An owned ad (classad.New plus an Insert per attribute) builds a
	// name index: a map, and a lowercased key per attribute, both thrown away with the record. The
	// closure is read a handful of times by one evaluation, so a view -- which resolves a name by a
	// folding scan of the attributes -- answers identically and allocates nothing per attribute.
	//
	// The walk is over, so assigns is not appended to again and these pointers stay valid for the rest
	// of the call; the next call's reset drops them before it refills assigns.
	for i := range s.assigns {
		s.ptrs = append(s.ptrs, &s.assigns[i])
	}
	s.ad.Attributes = s.ptrs
	v := classad.NewView(&s.ad).EvaluateAttr(name)
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

// recordScratch is the working state of one recordValueNode call: the closure's names, the reference
// list walkRefs fills, and the attribute list the closure ad reads. All of it is per record and thrown
// away, which on a row walk over an unsealed segment is per row of the scan, so it is pooled and reused
// instead. Nothing in it outlives the call: reset drops every reference before the scratch goes back,
// so one record's attributes cannot be read as the next record's and a pooled scratch does not pin a
// record's decoded expressions.
type recordScratch struct {
	ad      ast.ClassAd                // the closure, as the view's AST
	assigns []ast.AttributeAssignment  // its attributes, stored by value
	ptrs    []*ast.AttributeAssignment // ad.Attributes, pointing into assigns
	names   []string                   // the closure walk's queue and visited set
	refs    []string                   // walkRefs' output for the node being walked
}

var recordScratchPool = sync.Pool{New: func() any { return new(recordScratch) }}

// getRecordScratch takes a scratch from the pool, ready to fill.
func getRecordScratch() *recordScratch {
	if s, ok := recordScratchPool.Get().(*recordScratch); ok {
		return s
	}
	return new(recordScratch)
}

// queued reports whether name is already in the closure walk's queue, case-insensitively.
func (s *recordScratch) queued(name string) bool {
	for _, n := range s.names {
		if foldEqualStrings(n, name) {
			return true
		}
	}
	return false
}

// reset empties the scratch, dropping every reference it holds.
func (s *recordScratch) reset() {
	s.ad.Attributes = nil
	clear(s.assigns)
	clear(s.ptrs)
	clear(s.names)
	clear(s.refs)
	s.assigns, s.ptrs, s.names, s.refs = s.assigns[:0], s.ptrs[:0], s.names[:0], s.refs[:0]
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
