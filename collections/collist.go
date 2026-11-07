package collections

import (
	"encoding/binary"
	"math"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// LIST-VALUED ATTRIBUTES IN COLUMNAR FORM.
//
// A list is never a schema field: storableInColumn admits scalars only, so a list-valued attribute stays
// in the record and every query reading it takes the per-record path, which on a columnarized segment
// means decompressing the block's remnants. A partitionable slot's Child* attributes are the common case
// (`member("alice@ap", ChildRemoteUser)`, `size(ChildCpus)`).
//
// A columnarized segment's base block may therefore keep a TYPED COPY of its list-valued attributes,
// laid out the way Apache Arrow lays out a list column: per field a presence bitmap, n+1 offsets, and one
// child array holding every element of every record back to back -- fixed-width values for an int or a
// real, a bitmap for a boolean, a per-block dictionary plus codes for a string -- with a child validity
// bitmap when some element is UNDEFINED.
//
// A COPY, AND THEN A MOVE. The column is built as a copy: the list stays in the record, byte for byte as
// it was written, and nothing about the record, the escape bits or the scalar columns changes. Unless
// Options.MoveListColumns is negative, each cell whose stored bytes the column reproduces exactly is then
// moved out of its record, which keeps a placeholder in its place (collistmove.go); the readers that
// decompress a remnant put the list back before anything looks at the record, so every reader that
// rebuilds an ad or ships an attribute -- reassembly, projection, raw wire, text and redacted renders,
// re-columnarization, compaction, fsck -- sees exactly the bytes it saw before. Only the list built-ins'
// vector path (vm/veclist.go) reads the column as a column.
//
// WHEN A CELL QUALIFIES. A record's value goes into the column only when it is a list literal whose every
// element is UNDEFINED or a literal of the field's element kind -- int, real, string or boolean, chosen
// per block and field as the kind most of the block's lists hold. An int is never promoted to a real:
// the two differ under =?= and integer division. Anything else -- an element of another kind, an ERROR,
// a nested list or ad, an expression, a sealed value, a scalar where other records hold a list -- marks
// the cell OTHER: the record holds the attribute, but not as a list the column can carry. A batch with an
// OTHER cell is not served from the column at all; it goes to the per-record path as it always did.
// Absent and empty are distinct: an empty list is present with equal offsets.
//
// SEALED VALUES. A sealed value is not a list literal, so it is never copied. An attribute the sealing
// policy names is not copied either, whatever its stored form, so a widen after columnarizing leaves
// copies made before it only in segments whose records also still hold the value in the clear; a read
// not entitled to sealed values replaces every reference to such an attribute with UNDEFINED before
// planning (keylessQuery), so it never loads the column.
//
// ON DISK. While every list is still in its record, the columns ride in a trailer after the computed-value
// trailer (colExtTrailer), which a build that reads extension trailers skips section by section. A build
// that predates it reads every record identically -- the records are unchanged -- but stops trusting the
// trailers that precede it on that payload (its trailer parser wants them to end the payload), so such a
// segment reads there as one columnarized before remnant-name lists and computed values: correct, and
// slower until it is re-columnarized or the build rolls forward. A segment with a moved cell keeps its
// columns in the payload body instead (version 3; see collistmove.go).

// colListColumns gates writing list columns at columnarize time. A test seam; production leaves it true.
var colListColumns = true

// listColsBuilt counts list-column cells written, so a test can see the builder took the path.
var listColsBuilt atomic.Int64

// listCell is one record's qualifying list for one attribute.
type listCell struct {
	id   uint32
	kind adKind // the defined elements' kind, akNone for an empty or all-UNDEFINED list
	node []byte
}

// listRow is what the builder captured of one record: its qualifying lists, and every attribute id it
// holds (to tell an absent cell from an OTHER one).
type listRow struct {
	lists []listCell
	ids   []uint32
}

// captureListRow records iw's qualifying lists and attribute ids. iw is the record in global-interned
// wire form, as the columnar builder reads it; sizeHint is how many attributes to expect.
func captureListRow(iw []byte, sizeHint int) listRow {
	r := listRow{ids: make([]uint32, 0, sizeHint)}
	wire.Ad(iw).ForEach(func(id uint32, node []byte) bool {
		r.ids = append(r.ids, id)
		if kind, ok := listKind(node); ok {
			r.lists = append(r.lists, listCell{id: id, kind: kind, node: append([]byte(nil), node...)})
		}
		return true
	})
	return r
}

// listKind reports whether node is a qualifying list (see WHEN A CELL QUALIFIES) and its elements'
// kind: akNone when it has no defined element, ok=false when it mixes kinds or holds anything but
// scalar literals and UNDEFINED.
func listKind(node []byte) (adKind, bool) {
	if len(node) == 0 {
		return akNone, false
	}
	kind := akNone
	_, ok := wire.ScalarListElems(node, func(elem []byte) bool {
		var k adKind
		switch wire.ElemKind(elem) {
		case wire.LitUndef:
			return true
		case wire.LitBool:
			k = akBool
		case wire.LitInt:
			k = akInt
		case wire.LitReal:
			k = akReal
		case wire.LitString:
			k = akString
		default:
			return false
		}
		if kind == akNone {
			kind = k
		}
		return k == kind
	})
	return kind, ok
}

// listColBuild is one field of one block being laid out.
type listColBuild struct {
	id    uint32
	name  string
	kind  adKind
	cells []int // per record: index into that record's lists, -1 absent, -2 OTHER
}

// buildBlockLists lays out the list columns of one block from its records' captured rows. name resolves
// a field's global id, and declines one the sealing policy names. Returns the fields' ids and names and
// the uncompressed body (see encodeListBody), or nil ids when the block has none.
func buildBlockLists(rows []listRow, name func(uint32) (string, bool), known map[uint32]struct{}) ([]uint32, []string, []byte) {
	type tally struct{ kinds [5]int }
	counts := map[uint32]*tally{}
	// An attribute other blocks keep a list column of gets one here too, even where no record of this
	// block holds a list of it: a column of absent (or exception) cells is what lets a list built-in serve
	// this block rather than hand it back for not knowing that no record holds the attribute.
	for id := range known {
		counts[id] = &tally{}
	}
	for _, r := range rows {
		for _, lc := range r.lists {
			t := counts[lc.id]
			if t == nil {
				t = &tally{}
				counts[lc.id] = t
			}
			t.kinds[lc.kind]++
		}
	}
	if len(counts) == 0 {
		return nil, nil, nil
	}
	ids := make([]uint32, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var fields []listColBuild
	fieldAt := make(map[uint32]int, len(ids))
	for _, id := range ids {
		nm, ok := name(id)
		if !ok {
			continue
		}
		t := counts[id]
		kind := akInt // a field whose lists hold no defined element takes any kind; int is as good as any
		best := 0
		for _, k := range []adKind{akInt, akReal, akString, akBool} {
			if t.kinds[k] > best {
				kind, best = k, t.kinds[k]
			}
		}
		f := listColBuild{id: id, name: nm, kind: kind, cells: make([]int, len(rows))}
		for k := range f.cells {
			f.cells[k] = -1
		}
		fieldAt[id] = len(fields)
		fields = append(fields, f)
	}
	for k, r := range rows {
		for j, lc := range r.lists {
			if fi, ok := fieldAt[lc.id]; ok {
				if f := &fields[fi]; lc.kind == f.kind || lc.kind == akNone {
					f.cells[k] = j
				} else {
					f.cells[k] = -2 // a list of another kind: the record holds it, the column cannot
				}
			}
		}
		// A field the record holds as anything but a qualifying list -- a scalar, an expression, a list
		// mixing kinds, a sealed value -- is OTHER.
		for _, id := range r.ids {
			if fi, ok := fieldAt[id]; ok && fields[fi].cells[k] == -1 {
				fields[fi].cells[k] = -2
			}
		}
	}
	if len(fields) == 0 {
		return nil, nil, nil
	}
	outIDs := make([]uint32, len(fields))
	names := make([]string, len(fields))
	var body []byte
	for i := range fields {
		outIDs[i], names[i] = fields[i].id, fields[i].name
		body = encodeListField(body, &fields[i], rows)
	}
	return outIDs, names, body
}

// List field flags.
const (
	listFlagValid = 1 << 0 // a child validity bitmap follows the child values
	listFlagOther = 1 << 1 // an OTHER bitmap follows the presence bitmap
	listFlagNone  = 1 << 2 // no record of the block holds a list: no presence bitmap, offsets or child
)

// encodeListField appends one field's column:
//
//	kind u32 | flags u32 | presence bitmap | [OTHER bitmap] | (n+1) u32 offsets | child | [validity]
//
// where each per-record bitmap is (n+7)/8 bytes and the child is, for m elements: m 8-byte values for an
// int or real (a real as its IEEE-754 bits); a (m+7)/8-byte bitmap for a boolean; for a string a
// dictionary -- count u32, then each entry as (u32 length, bytes) -- and the code width u32 (1, 2 or 4)
// and m codes. The validity bitmap is (m+7)/8 bytes, a set bit marking a defined element.
func encodeListField(dst []byte, f *listColBuild, rows []listRow) []byte {
	n := len(rows)
	nb := (n + 7) / 8
	has := make([]byte, nb)
	other := make([]byte, nb)
	anyOther := false
	offs := binary.LittleEndian.AppendUint32(nil, 0)
	var elems [][]byte
	for k, r := range rows {
		switch c := f.cells[k]; {
		case c >= 0:
			setBit(has, k)
			wire.ScalarListElems(r.lists[c].node, func(e []byte) bool {
				elems = append(elems, e)
				return true
			})
			listColsBuilt.Add(1)
		case c == -2:
			setBit(other, k)
			anyOther = true
		}
		offs = binary.LittleEndian.AppendUint32(offs, uint32(len(elems)))
	}
	m := len(elems)
	valid := make([]byte, (m+7)/8)
	anyUndef := false
	for i, e := range elems {
		if wire.ElemKind(e) == wire.LitUndef {
			anyUndef = true
		} else {
			setBit(valid, i)
		}
	}
	var flags uint32
	if anyUndef {
		flags |= listFlagValid
	}
	if anyOther {
		flags |= listFlagOther
	}
	if m == 0 && !anyHas(has) {
		// Every cell absent or an exception: the presence bitmap and offsets would be all zeros.
		flags |= listFlagNone
	}
	dst = appendU32(dst, uint32(f.kind))
	dst = appendU32(dst, flags)
	if flags&listFlagNone != 0 {
		if anyOther {
			dst = append(dst, other...)
		}
		return dst
	}
	dst = append(dst, has...)
	if anyOther {
		dst = append(dst, other...)
	}
	dst = append(dst, offs...)
	switch f.kind {
	case akInt, akReal:
		for _, e := range elems {
			var raw uint64
			if lit, ok := wire.LiteralValue(e); ok {
				switch lit.Kind {
				case wire.LitInt:
					raw = uint64(lit.Int)
				case wire.LitReal:
					raw = math.Float64bits(lit.Real)
				}
			}
			dst = binary.LittleEndian.AppendUint64(dst, raw)
		}
	case akBool:
		bits := make([]byte, (m+7)/8)
		for i, e := range elems {
			if lit, ok := wire.LiteralValue(e); ok && lit.Kind == wire.LitBool && lit.Bool {
				setBit(bits, i)
			}
		}
		dst = append(dst, bits...)
	case akString:
		index := map[string]int{}
		var dict [][]byte
		codes := make([]int, m)
		for i, e := range elems {
			s, ok := wire.StringLiteralValue(e)
			if !ok {
				continue // UNDEFINED: code 0, masked by validity
			}
			c, seen := index[string(s)]
			if !seen {
				c = len(dict)
				index[string(s)] = c
				dict = append(dict, s)
			}
			codes[i] = c
		}
		dst = appendU32(dst, uint32(len(dict)))
		for _, s := range dict {
			dst = appendBytes(dst, s)
		}
		w := listCodeWidth(len(dict))
		dst = appendU32(dst, uint32(w))
		for _, c := range codes {
			switch w {
			case 1:
				dst = append(dst, byte(c))
			case 2:
				dst = binary.LittleEndian.AppendUint16(dst, uint16(c))
			default:
				dst = binary.LittleEndian.AppendUint32(dst, uint32(c))
			}
		}
	}
	if anyUndef {
		dst = append(dst, valid...)
	}
	return dst
}

func listCodeWidth(entries int) int {
	switch {
	case entries <= 1<<8:
		return 1
	case entries <= 1<<16:
		return 2
	}
	return 4
}

// attachBlockLists builds block b's list columns from its records' rows and attaches them, compressed.
func attachBlockLists(b *columnarBlock, rows []listRow, codec Codec, name func(uint32) (string, bool), known map[uint32]struct{}) {
	if b == nil || len(rows) != b.n {
		return
	}
	ids, names, body := buildBlockLists(rows, name, known)
	if len(ids) == 0 {
		return
	}
	b.listIDs, b.listNames = ids, names
	b.listComp = codec.Compress(nil, body)
}

// listFieldOf returns the position of attribute id among b's list fields.
func (b *columnarBlock) listFieldOf(id uint32) (int, bool) {
	for i, x := range b.listIDs {
		if x == id {
			return i, true
		}
	}
	return 0, false
}

// listColumn parses field j of b's list body raw into a ListColumn, aliasing raw. ok=false when the body
// does not parse or does not describe a column of b's length: bounds, offsets and codes are checked
// rather than trusted. other reports whether some record holds the attribute as something the column
// cannot carry.
func (b *columnarBlock) listColumn(raw []byte, j int, buf *[]string) (lc *vm.ListColumn, other, ok bool) {
	c := &cursor{b: raw}
	n := b.n
	nb := (n + 7) / 8
	for f := 0; f <= j; f++ {
		kind := adKind(c.u32())
		flags := c.u32()
		if c.err != nil || flags&^(listFlagValid|listFlagOther|listFlagNone) != 0 {
			return nil, false, false
		}
		if flags&listFlagNone != 0 {
			// No list in the block: an all-absent column, with the exceptions' bitmap if there are any.
			if flags&listFlagValid != 0 {
				return nil, false, false
			}
			var oth []byte
			if flags&listFlagOther != 0 {
				if !c.need(nb) {
					return nil, false, false
				}
				oth = c.b[c.i : c.i+nb]
				c.i += nb
			}
			if f != j {
				continue
			}
			col := &vm.ListColumn{N: n, Present: make([]byte, nb), Offs: make([]byte, 4*(n+1)), Kind: vm.VsInt}
			if anyHas(oth) {
				col.Other = oth
				return col, true, true
			}
			return col, false, true
		}
		if !c.need(nb) {
			return nil, false, false
		}
		col := &vm.ListColumn{N: n, Present: c.b[c.i : c.i+nb]}
		c.i += nb
		var oth []byte
		if flags&listFlagOther != 0 {
			if !c.need(nb) {
				return nil, false, false
			}
			oth = c.b[c.i : c.i+nb]
			c.i += nb
		}
		if !c.need(4 * (n + 1)) {
			return nil, false, false
		}
		col.Offs = c.b[c.i : c.i+4*(n+1)]
		c.i += 4 * (n + 1)
		m, okOffs := checkListOffsets(col.Offs, col.Present, n)
		if !okOffs {
			return nil, false, false
		}
		switch kind {
		case akInt, akReal:
			col.Kind = vm.VsInt
			if kind == akReal {
				col.Kind = vm.VsReal
			}
			if !c.need(8 * m) {
				return nil, false, false
			}
			col.Fixed = c.b[c.i : c.i+8*m]
			c.i += 8 * m
		case akBool:
			col.Kind = vm.VsBool
			if !c.need((m + 7) / 8) {
				return nil, false, false
			}
			col.Bits = c.b[c.i : c.i+(m+7)/8]
			c.i += (m + 7) / 8
		case akString:
			col.Kind = vm.VsString
			d := int(c.u32())
			if c.err != nil || d < 0 || d > len(raw) {
				return nil, false, false
			}
			if f == j {
				if cap(*buf) < d {
					*buf = make([]string, d)
				}
				col.Dict = (*buf)[:d]
			}
			for e := 0; e < d; e++ {
				s := c.bytes()
				if c.err != nil {
					return nil, false, false
				}
				if f == j {
					col.Dict[e] = bytesToStr(s)
				}
			}
			w := int(c.u32())
			if c.err != nil || (w != 1 && w != 2 && w != 4) || !c.need(w*m) {
				return nil, false, false
			}
			col.Codes, col.CodeWidth = c.b[c.i:c.i+w*m], w
			c.i += w * m
		default:
			return nil, false, false
		}
		if flags&listFlagValid != 0 {
			if !c.need((m + 7) / 8) {
				return nil, false, false
			}
			col.Valid = c.b[c.i : c.i+(m+7)/8]
			c.i += (m + 7) / 8
		}
		if f == j {
			if col.Kind == vm.VsString {
				// Every defined element's code must index the dictionary; an UNDEFINED one's is masked.
				for i := 0; i < m; i++ {
					if (col.Valid == nil || testBit(col.Valid, i)) && listCodeAt(col.Codes, col.CodeWidth, i) >= len(col.Dict) {
						return nil, false, false
					}
				}
				if len(col.Dict) == 0 && m > 0 {
					col.Dict = []string{""} // every element UNDEFINED: the masked codes must still index something
				}
			}
			anyOther := false
			for _, x := range oth {
				if x != 0 {
					anyOther = true
					break
				}
			}
			if anyOther {
				col.Other = oth
			}
			return col, anyOther, true
		}
	}
	return nil, false, false
}

func listCodeAt(codes []byte, w, i int) int {
	switch w {
	case 1:
		return int(codes[i])
	case 2:
		return int(binary.LittleEndian.Uint16(codes[2*i:]))
	}
	return int(binary.LittleEndian.Uint32(codes[4*i:]))
}

// checkListOffsets verifies a field's offsets: starting at 0, non-decreasing, flat for a record without
// a list. Returns the child length.
func checkListOffsets(offs, present []byte, n int) (int, bool) {
	prev := binary.LittleEndian.Uint32(offs)
	if prev != 0 {
		return 0, false
	}
	for k := 0; k < n; k++ {
		next := binary.LittleEndian.Uint32(offs[4*k+4:])
		if next < prev || (next != prev && !testBit(present, k)) {
			return 0, false
		}
		prev = next
	}
	if int(prev) > 1<<28 {
		return 0, false
	}
	return int(prev), true
}

// LoadListColumn serves a list-valued attribute of this block from its list column (vm.ListColumnSource).
// It declines -- and the executor takes the ordinary path, which hands the block to the per-record
// evaluator -- for an attribute the block keeps no column of, or one some record of the block holds as
// something the column cannot carry.
func (s *blockVecSource) LoadListColumn(name string, scope ast.AttributeScope) (*vm.ListColumn, bool) {
	if scope != ast.NoScope && scope != ast.MyScope {
		return nil, false
	}
	b := s.blk
	if b == nil || len(b.listIDs) == 0 {
		return nil, false
	}
	id, _, known := s.resolveRef(name)
	if !known {
		return nil, false
	}
	j, ok := b.listFieldOf(id)
	if !ok {
		return nil, false
	}
	raw, err := s.bc.stream(b, kindList)
	if err != nil {
		return nil, false
	}
	lc, other, ok := b.listColumn(raw, j, &s.listBuf)
	if !ok {
		return nil, false
	}
	if other && !b.listExceptions(lc, name) {
		return nil, false
	}
	return lc, true
}

// listExceptions fills lc.Except with the value of attribute name in each record of the block holding it
// as something the column cannot carry, read from the record, as the per-record path reads it. It
// declines -- and the block goes to the per-record evaluator -- where that is not cheap or not exact:
// more such records than the held-cell exception budget allows (see PER-RECORD EXCEPTIONS), a sealed
// value, or a value that is not built from literals alone (an attribute reference or a function call
// would make it depend on the rest of the ad, or on the clock).
func (b *columnarBlock) listExceptions(lc *vm.ListColumn, name string) bool {
	cs := b.native
	if cs == nil || cs.owner == nil || cs.arena == nil || lc.Other == nil {
		return false
	}
	ks := 0
	for k := 0; k < b.n; k++ {
		if testBit(lc.Other, k) {
			ks++
		}
	}
	if ks > heldExceptionFloor && ks*heldExceptionShare > b.n {
		return false
	}
	c, seg := cs.owner, cs.arena
	if seg.dictUnusable.Load() || c.shouldEncrypt(name) {
		return false
	}
	d := seg.dict.Load()
	lc.Except = make(map[int]classad.Value, ks)
	for k := 0; k < b.n; k++ {
		if !testBit(lc.Other, k) {
			continue
		}
		gk := b.segBase + k
		if gk >= cs.offsLen() {
			return false
		}
		ww, err := c.recordWire(seg, cs.offAt(gk))
		if err != nil {
			return false
		}
		node, found := c.wireLookupDict(d, wire.Ad(ww), name)
		if !found || wire.IsEncryptedNode(node) {
			return false
		}
		expr, err := c.decodeNodeDict(d, node)
		if err != nil || !literalTree(expr, 0) {
			return false
		}
		lc.Except[k] = classad.NewEvaluator(nil).Evaluate(expr)
		listExceptionEvals.Add(1)
	}
	return true
}

// listExceptionEvals counts list-column exceptions read from their records, so a test can see the path.
var listExceptionEvals atomic.Int64

// literalTree reports whether e is built from literals and operators alone, so its value is the same
// evaluated anywhere.
func literalTree(e ast.Expr, depth int) bool {
	if depth > 64 {
		return false
	}
	switch v := e.(type) {
	case *ast.IntegerLiteral, *ast.RealLiteral, *ast.StringLiteral, *ast.BooleanLiteral,
		*ast.UndefinedLiteral, *ast.ErrorLiteral:
		return true
	case *ast.ListLiteral:
		for _, x := range v.Elements {
			if !literalTree(x, depth+1) {
				return false
			}
		}
		return true
	case *ast.ParenExpr:
		return literalTree(v.Inner, depth+1)
	case *ast.UnaryOp:
		return literalTree(v.Expr, depth+1)
	case *ast.BinaryOp:
		return literalTree(v.Left, depth+1) && literalTree(v.Right, depth+1)
	}
	return false
}

// COLUMNAR PAYLOAD EXTENSION TRAILER.
//
// colExtTrailer follows the computed-value trailer, or the remnant-name trailer when there is none: the
// magic, a section count, then each section as (tag u32, bytes). A reader skips a section whose tag it
// does not know, so a later addition rides here without costing anything to a build that predates it --
// which is what the trailers before it could not offer one another (each parser wants its trailer to end
// the payload).
const (
	colExtTrailer = 0x4c525458 // "XTRL" on disk (little-endian)
	// colExtLists is the section holding the base blocks' list columns:
	//
	//	version u32 (1) | name count u32 | names (u32 length, bytes) |
	//	block count u32 | per block: field count u32 | field count name indices u32 | body (u32 length, bytes)
	//
	// where body is the block's fields (encodeListField), compressed with the region codec.
	colExtLists        = 0x5453494c // "LIST"
	colExtListsVersion = 1
)

// hasLists reports whether any base block of the segment keeps list columns.
func (cs *colSegment) hasLists() bool {
	for _, b := range cs.blocks {
		if len(b.listIDs) > 0 {
			return true
		}
	}
	return false
}

func appendExtTrailer(dst []byte, cs *colSegment) []byte {
	dst = appendU32(dst, colExtTrailer)
	dst = appendU32(dst, 1)
	dst = appendU32(dst, colExtLists)
	return appendBytes(dst, appendListSection(nil, cs.blocks))
}

func appendListSection(dst []byte, blocks []*columnarBlock) []byte {
	index := map[string]int{}
	var names []string
	for _, b := range blocks {
		for _, nm := range b.listNames {
			if _, ok := index[strings.ToLower(nm)]; !ok {
				index[strings.ToLower(nm)] = len(names)
				names = append(names, nm)
			}
		}
	}
	dst = appendU32(dst, colExtListsVersion)
	dst = appendU32(dst, uint32(len(names)))
	for _, nm := range names {
		dst = appendBytes(dst, []byte(nm))
	}
	dst = appendU32(dst, uint32(len(blocks)))
	for _, b := range blocks {
		dst = appendU32(dst, uint32(len(b.listNames)))
		for _, nm := range b.listNames {
			dst = appendU32(dst, uint32(index[strings.ToLower(nm)]))
		}
		dst = appendBytes(dst, b.listComp)
	}
	return dst
}

// readExtTrailer decodes an extension trailer occupying exactly rest onto cs. A section that does not
// parse is dropped, alone: what it describes reads the slow way.
func readExtTrailer(rest []byte, cs *colSegment, internName func(string) uint32) bool {
	c := &cursor{b: rest}
	if len(rest) < 8 || c.u32() != colExtTrailer {
		return false
	}
	ns := int(c.u32())
	if c.err != nil || ns < 0 || ns > 1<<10 {
		return false
	}
	for range ns {
		tag := c.u32()
		body := c.bytes()
		if c.err != nil {
			return false
		}
		// A version 3 body already holds the lists, and they are the only copy: a trailer section must
		// not replace them.
		if tag == colExtLists && !cs.listsInBody {
			readListSection(body, cs, internName)
		}
	}
	return c.i == len(rest)
}

// readListSection attaches a list section's columns to cs's base blocks, all or none. The bodies alias
// body and are parsed, and checked, when a reader asks for one.
func readListSection(body []byte, cs *colSegment, internName func(string) uint32) bool {
	c := &cursor{b: body}
	if c.u32() != colExtListsVersion {
		return false
	}
	nn := int(c.u32())
	if c.err != nil || nn < 0 || nn > 1<<20 {
		return false
	}
	names := make([]string, nn)
	for i := range names {
		names[i] = string(c.bytes())
	}
	nb := int(c.u32())
	if c.err != nil || nb != len(cs.blocks) {
		return false
	}
	type att struct {
		names []string
		comp  []byte
	}
	atts := make([]att, nb)
	for i := range atts {
		nf := int(c.u32())
		if c.err != nil || nf < 0 || nf > nn {
			return false
		}
		for range nf {
			ni := int(c.u32())
			if c.err != nil || ni < 0 || ni >= nn {
				return false
			}
			atts[i].names = append(atts[i].names, names[ni])
		}
		atts[i].comp = c.bytes()
		if c.err != nil || (nf > 0) != (len(atts[i].comp) > 0) {
			return false
		}
	}
	if c.err != nil || c.i != len(body) {
		return false
	}
	for i, b := range cs.blocks {
		if len(atts[i].names) == 0 {
			continue
		}
		ids := make([]uint32, len(atts[i].names))
		for j, nm := range atts[i].names {
			ids[j] = internName(nm)
		}
		b.listIDs, b.listNames, b.listComp = ids, atts[i].names, atts[i].comp
	}
	return true
}

// dropTrailerState detaches everything the payload trailers attached to cs's base blocks, for a
// trailer region that turned out not to parse.
func (cs *colSegment) dropTrailerState() {
	for _, b := range cs.blocks {
		b.ccols, b.ccBlob, b.computed, b.computedComp = nil, nil, nil, nil
		if !cs.listsInBody { // a version 3 body's lists did not come from a trailer
			b.listIDs, b.listNames, b.listComp = nil, nil, nil
		}
	}
}

// noteListFields records the attributes cs keeps list columns of, so blocks columnarized from now on
// carry a column for each (see buildBlockLists).
func (c *Collection) noteListFields(cs *colSegment) {
	for _, b := range cs.blocks {
		for _, id := range b.listIDs {
			c.listHint.Store(id, struct{}{})
		}
	}
}

// knownListFields is a snapshot of the attributes some block keeps a list column of.
func (c *Collection) knownListFields() map[uint32]struct{} {
	out := map[uint32]struct{}{}
	c.listHint.Range(func(k, _ any) bool {
		if id, ok := k.(uint32); ok {
			out[id] = struct{}{}
		}
		return true
	})
	return out
}

// anyHas reports whether any bit of a bitmap is set.
func anyHas(bits []byte) bool {
	for _, x := range bits {
		if x != 0 {
			return true
		}
	}
	return false
}
