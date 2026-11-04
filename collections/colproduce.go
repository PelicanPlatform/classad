package collections

import (
	"math"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Handing a caller the VALUES of a query's matching records, a column at a time.
//
// Every exported columnar entry before this one reduces its columns to a scalar -- a count, a
// group's accumulator, a threshold. A reader that wants the records themselves had only the record
// scan, which materializes each one, renders it to text and leaves the consumer to parse it back.
// Measured on the consumer that asked for this, rendering and re-parsing is the large majority of a
// read, and both exist only because the typed values stopped being typed somewhere in the middle.
//
// So this is the same scan with the reduction removed: the predicate decides which records survive
// and their attribute values come out in the form storage already holds them.
//
// THE SELECTION IS THE NEW PART. A count needs only how many bits are set in the match mask; a
// producer needs the values at those positions, gathered into a dense batch. Nothing above did that,
// which is why this is a file rather than another accumulator.
//
// WHICH TIER ANSWERS MUST NOT CHANGE WHAT COMES OUT, and that is a sharper constraint here than for
// the count. The grouped scan already holds the weaker form of it -- it reads the group columns
// through the block in every tier, including tiers where the PREDICATE fell back to per-record
// evaluation, so group identity cannot depend on which tier answered. A producer needs it for every
// value it emits, so the rule here is the same one: the tier decides which records match, never what
// their values are.
//
// THE BATCH IS BORROWED. Its slices are reused from batch to batch, and a string in it may point
// into the block it was read from -- see Batch. Copying instead was measured in the string predicate
// and lost; the consumer this exists for is an encoder that copies into its own buffers as it
// appends, so a copy here would be a copy to undo.

// ValueKind is the type of one produced cell.
//
// A cell carries its own kind rather than the column carrying one, because an attribute is not
// obliged to hold the same type in every record and a producer that imposed one would be inventing
// values. A consumer with a declared schema coerces; a consumer without one can see what it got.
type ValueKind uint8

const (
	ValueUndefined ValueKind = iota // the record does not have this attribute
	ValueError                      // a value this path cannot produce: an expression, a list, a nested ad, a sealed value this reader may not open
	ValueInt
	ValueReal
	ValueBool
	ValueString
)

// Column is one attribute's values for the rows of a Batch, in row order.
//
// Kind, Num and Str are parallel and all of length Batch.N. Num carries an Int verbatim, a Bool as 0
// or 1, and a Real as math.Float64bits -- which is how both classad.Value and the columnar block
// already store one, so a real reaches a consumer without a conversion in between. Int, Float, Bool
// and Text read a cell without the caller needing to know that.
type Column struct {
	Name string
	Kind []ValueKind
	Num  []int64
	Str  []string
}

// Int, Float, Bool and Text read cell i in its natural Go type. Each answers for the kind it names
// and a zero value otherwise; a caller that cares which it has reads Kind first.
func (c *Column) Int(i int) int64 { return c.Num[i] }

func (c *Column) Float(i int) float64 {
	if c.Kind[i] == ValueReal {
		return math.Float64frombits(uint64(c.Num[i]))
	}
	return float64(c.Num[i])
}

func (c *Column) Bool(i int) bool { return c.Num[i] != 0 }

func (c *Column) Text(i int) string { return c.Str[i] }

// Batch is one block's -- or one chunk of one window's -- surviving records, as columns.
//
// BORROWED, AND ONLY UNTIL THE CALLBACK RETURNS. The slices are the producer's own and are refilled
// for the next batch, so a cell read after that is some later record's.
//
// A STRING IN Str ALIASES THE BLOCK'S DECOMPRESSED REGION -- it is an unsafe.String over the
// dictionary entry, not a copy. Keeping one past the callback therefore pins that whole region for
// as long as the string lives, which on a scan of any size is a great deal more memory than the
// consumer thinks it is holding, and it stays valid only while that region does. A consumer that
// keeps anything past its callback copies it.
//
// Cols is in the order the attributes were requested, one entry each, and that order and length are
// the same for every batch of a scan -- including a batch no column could be read for, which reports
// undefined rather than a short row. An encoder with a fixed schema can therefore bind its builders
// once.
type Batch struct {
	N    int
	Cols []Column
}

// ColumnScanStats records which tiers answered, in the shape vecSplit records it for the count.
//
// It is here for the reason vecSplit is there: a producer that quietly read every value from the
// records instead of the columns would return exactly the right answer, at the speed of the path it
// was built to replace, and nothing in the result would say so. ColumnRows and RecordRows are that
// question asked directly.
type ColumnScanStats struct {
	Rows int // rows handed to the callback

	// ColumnCells and RecordCells are counted per CELL rather than per row, because a projection
	// mixes the two: a block hands over the columns its schema carries and the rest comes from the
	// record. Counting per row would have to pick one for a mixed row and would then report a
	// projection that reads six columns and one cold attribute exactly like one that reads seven
	// cold attributes.
	ColumnCells int // cells read from the block's columns
	RecordCells int // cells read from the record's own wire form

	// RecordReads is the number of records materialized from the wire -- the cost RecordCells
	// implies, paid once per row however many of its cells come from it.
	RecordReads int

	VectorBlocks int // the predicate ran as a vector over the block's columns
	ScopeBlocks  int // the executor declined this block; the predicate ran per record
	ChurnBlocks  int // mostly superseded, so per-record evaluation is cheaper
	PrunedBlocks int // a zone or dictionary ruled the block out
	EmptyBlocks  int // nothing in the block was visible to this snapshot
	RowWindows   int // no columnar block: the active segment, or one not yet columnarized
	PrunedSegs   int // the segment zone map ruled the whole window out

	// Unreadable counts records the predicate matched from the columns but whose own bytes
	// could not then be materialized. The predicate tiers already treat such a record as not
	// matching; counting it here keeps that from being a row that quietly went missing.
	Unreadable int
}

// maxProduceRows caps a batch. A columnar block is already a natural batch and smaller than this;
// the cap is for the row tier, whose window has no such bound and would otherwise hand a consumer
// one batch the size of a segment.
const maxProduceRows = 4096

// ScanColumns calls fn with the values of attrs for every visible record matching q, in batches.
//
// ok=false means this collection cannot serve the query from columns at all -- no accelerator, a
// non-native query, nothing asked for -- and the caller should scan as before. It is decided before
// any batch is produced, so ok=false always comes with no callback having run.
//
// Within a served query every tier produces values, including a window with no columnar block and a
// block the vector executor declines. A tier that contributed no rows would be a result silently
// missing records, which is worse than a slow one.
//
// fn returning false stops the scan early; that is a caller's decision and still reports ok=true,
// with Rows counting what it was given.
func (c *Collection) ScanColumns(q *vm.Query, attrs []string, fn func(*Batch) bool) (ColumnScanStats, bool) {
	return c.scanColumns(q, attrs, false, fn)
}

// ScanColumnsRedacted is ScanColumns for a reader not entitled to sealed values: the constraint is
// rewritten so a sealed attribute is undefined to it (see keylessQuery), and a sealed cell is
// produced as ValueError rather than opened.
//
// A sealed value is never stored as a column, so a column read cannot leak one; the record tier
// reads it unopened, which is where the refusal has to hold.
func (c *Collection) ScanColumnsRedacted(q *vm.Query, attrs []string, fn func(*Batch) bool) (ColumnScanStats, bool) {
	return c.scanColumns(q, attrs, true, fn)
}

// ColumnsServable reports whether ScanColumns can serve this query at all, without scanning.
//
// Every reason ScanColumns declines is knowable before it reads anything -- there is no
// accelerator, the query is not native, nothing was asked for -- which is what lets a caller that
// must commit to an answer before the first row decide up front. A relay that returned a sequence
// and only then discovered it could not serve one would be indistinguishable from a query that
// matched nothing.
//
// ScanColumns asks this same question rather than repeating the conditions, so the two cannot
// drift into disagreeing about what is servable.
func (c *Collection) ColumnsServable(q *vm.Query, attrs []string) bool {
	return c.schemaScan.Load() != nil && c.intern != nil && q != nil && q.Native() && len(attrs) > 0
}

func (c *Collection) scanColumns(q *vm.Query, attrs []string, redact bool, fn func(*Batch) bool) (ColumnScanStats, bool) {
	if fn == nil || !c.ColumnsServable(q, attrs) {
		return ColumnScanStats{}, false
	}
	st := c.schemaScan.Load()
	p := newColumnProducer(c, attrs, fn, st.cache, redact)
	p.run(q, redact)
	return p.stats, true
}

// columnProducer holds one scan's reusable buffers: the batch's columns, one loaded vector per
// attribute, and the selection. All three are sized to the widest block seen so far and refilled per
// block, so a scan allocates per shape rather than per batch.
type columnProducer struct {
	c     *Collection
	attrs []string
	fn    func(*Batch) bool
	bc    *blockCache

	// lookups reads an attribute from a record's own wire form.
	lookups []fieldLookup

	// noRead marks an attribute answered as UNDEFINED without reading anything, for either of two
	// reasons.
	//
	// The intern table does not carry the name, so no record in this collection has ever had it.
	// Saying so here keeps one unknown name out of the record tier.
	//
	// Or this reader may not open sealed values and the policy seals this attribute (see
	// keylessHides). The store may hold it in the CLEAR -- the policy can be turned on after
	// records are written -- and then the columns hold the clear value too, so reading the column
	// would hand over exactly what the reader is not entitled to. Undefined is what the keyless
	// record renderer substitutes for the same attribute, so the two readers agree.
	//
	// This is where the withholding happens, before any storage is touched, which is why the
	// sealed-node branch in nodeCell is a backstop rather than the rule.
	noRead []bool

	batch Batch
	vecs  []*vm.Vec
	vecN  int
	sel   []int32

	// fromColumn says, for the block being emitted, whether each attribute was loaded as a column.
	fromColumn []bool

	// opener is the data key for a reader entitled to sealed values, nil for one that is not.
	opener wire.Sealer

	stats   ColumnScanStats
	stopped bool
}

func newColumnProducer(c *Collection, attrs []string, fn func(*Batch) bool, bc *blockCache, redact bool) *columnProducer {
	p := &columnProducer{c: c, attrs: attrs, fn: fn, bc: bc}
	p.batch.Cols = make([]Column, len(attrs))
	p.lookups = make([]fieldLookup, len(attrs))
	p.fromColumn = make([]bool, len(attrs))
	p.noRead = make([]bool, len(attrs))
	p.opener = c.renderKey(redact)
	for i, a := range attrs {
		p.batch.Cols[i].Name = a
		id, ok := c.intern.LookupID(a)
		if !ok || (redact && c.keylessHides(a)) {
			p.noRead[i] = true
			continue
		}
		p.lookups[i] = c.attrLookup(id)
	}
	return p
}

// ensure sizes the batch's columns and the loaded vectors for a block of n records.
func (p *columnProducer) ensure(n int) {
	if cap(p.sel) < n {
		p.sel = make([]int32, 0, n)
	}
	if p.vecN < n {
		p.vecs = make([]*vm.Vec, len(p.attrs))
		for i := range p.vecs {
			p.vecs[i] = vm.NewColumnVec(n)
		}
		p.vecN = n
		for i := range p.batch.Cols {
			col := &p.batch.Cols[i]
			col.Kind = make([]ValueKind, n)
			col.Num = make([]int64, n)
			col.Str = make([]string, n)
		}
	}
}

// setCell writes one cell of column ai at row, translating the vector's element state.
func (p *columnProducer) setCell(ai, row int, state uint8, num int64, str string) {
	col := &p.batch.Cols[ai]
	switch state {
	case vm.VsInt:
		col.Kind[row], col.Num[row], col.Str[row] = ValueInt, num, ""
	case vm.VsReal:
		col.Kind[row], col.Num[row], col.Str[row] = ValueReal, num, ""
	case vm.VsBool:
		col.Kind[row], col.Num[row], col.Str[row] = ValueBool, num, ""
	case vm.VsString:
		col.Kind[row], col.Num[row], col.Str[row] = ValueString, 0, str
	case vm.VsUndef:
		col.Kind[row], col.Num[row], col.Str[row] = ValueUndefined, 0, ""
	default:
		col.Kind[row], col.Num[row], col.Str[row] = ValueError, 0, ""
	}
}

// flush hands the filled rows to the callback and resets the batch. It reports whether to continue.
func (p *columnProducer) flush(n int) bool {
	if n == 0 {
		return true
	}
	p.batch.N = n
	for i := range p.batch.Cols {
		col := &p.batch.Cols[i]
		col.Kind, col.Num, col.Str = col.Kind[:n], col.Num[:n], col.Str[:n]
	}
	keep := p.fn(&p.batch)
	for i := range p.batch.Cols {
		col := &p.batch.Cols[i]
		col.Kind, col.Num, col.Str = col.Kind[:cap(col.Kind)], col.Num[:cap(col.Num)], col.Str[:cap(col.Str)]
	}
	p.stats.Rows += n
	if !keep {
		p.stopped = true
	}
	return keep
}

// nodeCell reads an attribute's wire node as a cell, in the vector's own element states so the two
// tiers write through one translation rather than two.
//
// open is the data key, or nil for a reader not entitled to sealed values. A SEALED node is opened
// with it and read as the value it holds, and left UNDEFINED without it -- which is what the keyless
// record decode substitutes for one, so the two readers see what each of them would see from a
// record scan rather than something only this path produces.
//
// A node that is not a scalar literal is ValueError: an expression, a list, or a nested ad, all of
// which are values this path does not produce. That is distinct from UNDEFINED, which is the record
// not having the attribute, and keeping them distinct is what lets a consumer tell "no value" from
// "a value you did not get".
func nodeCell(node []byte, found bool, open wire.Sealer) (uint8, int64, string) {
	if !found {
		return vm.VsUndef, 0, ""
	}
	if wire.IsEncryptedNode(node) {
		plain, ok := wire.OpenEncryptedNode(node, open)
		if !ok {
			// No key, or it does not authenticate: undefined, never the ciphertext. A reader
			// barred from this attribute never reaches here -- noRead answered it before any
			// record was read -- so this is the backstop for a node that will not open, which
			// is corruption rather than policy.
			return vm.VsUndef, 0, ""
		}
		node = plain
	}
	lit, ok := scalarValue(node)
	if !ok {
		return vm.VsError, 0, ""
	}
	switch lit.Kind {
	case wire.LitInt:
		return vm.VsInt, lit.Int, ""
	case wire.LitReal:
		return vm.VsReal, int64(math.Float64bits(lit.Real)), ""
	case wire.LitBool:
		var i int64
		if lit.Bool {
			i = 1
		}
		return vm.VsBool, i, ""
	case wire.LitString:
		return vm.VsString, 0, lit.Str
	case wire.LitUndef:
		return vm.VsUndef, 0, ""
	}
	return vm.VsError, 0, ""
}

// fromRecord fills row from the record's own wire form, for every attribute at once.
//
// One materialized record serves every column, which is why the fallback is per BLOCK and not per
// column: an attribute the block cannot hand over costs the block's rows one wire read each, not one
// per attribute.
func (p *columnProducer) fromRecord(d *segDictHandle, ad wire.Ad, row int) {
	for ai := range p.attrs {
		if p.noRead[ai] {
			p.setCell(ai, row, vm.VsUndef, 0, "")
			continue
		}
		node, found := p.lookups[ai](d, ad)
		state, num, str := nodeCell(node, found, p.opener)
		p.setCell(ai, row, state, num, str)
	}
}

// run is VectorEvalCount's loop with the count replaced by a gather. Every tier it falls through
// gets a producing analogue, for the reason the grouped scan gives: a tier that contributed no rows
// would be a result silently missing records.
func (p *columnProducer) run(q *vm.Query, redact bool) {
	c := p.c
	sides := c.newEvalSides(q, redact)
	cs := &colScope{bc: p.bc, c: c}
	resolver := cs.resolve
	src := &blockVecSource{c: c, bc: p.bc}
	scratch := vecScratchPool.Get().(*vm.VecScratch) //nolint:errcheck // the pool's New returns exactly this type
	defer func() {
		scratch.Release()
		vecScratchPool.Put(scratch)
	}()

	var live []uint64
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			if p.stopped {
				break
			}
			// No matching record, so no row: pruning is as sound here as for the count.
			if sides.excludes(c, w) {
				p.stats.PrunedSegs++
				continue
			}
			// The CurrentTime-folded form where the window is proven free of CurrentTime.
			sd := sides.at(w)
			fq, m, fallbackM := sd.q, sd.m, sd.rm
			seg := w.seg.colblk.Load()
			if seg == nil || seg.schema() == nil {
				// The active segment, or one the accelerator has not reached: read each visible
				// record the ordinary way.
				p.stats.RowWindows++
				p.rowWindow(w, s0, fallbackM)
				continue
			}
			pruneIdxs, prunePreds := sd.tests(c, seg.schema())
			strEq := sd.stringEq(c, seg.schema())
			base := 0
			for bi, blk := range seg.blocks {
				if p.stopped {
					break
				}
				if len(prunePreds) > 0 && !blockMayMatch(blk, pruneIdxs, prunePreds) {
					p.stats.PrunedBlocks++
					base += blk.n
					continue
				}
				if len(strEq) > 0 && blk.dictPrunes(strEq, p.bc, &src.dictBuf) {
					p.stats.PrunedBlocks++
					base += blk.n
					continue
				}
				words := vm.MaskWords(blk.n)
				if cap(live) < words {
					live = make([]uint64, words)
				}
				live = live[:words]
				for i := range live {
					live[i] = 0
				}
				nLive := 0
				for k := 0; k < blk.n; k++ {
					gk := base + k
					if gk >= seg.offsLen() {
						break
					}
					if w.visible(seg.offAt(gk), s0) {
						live[k/64] |= 1 << uint(k%64)
						nLive++
					}
				}
				if nLive == 0 {
					p.stats.EmptyBlocks++
					base += blk.n
					continue
				}
				p.ensure(blk.n)
				src.blk = blk
				src.setGroups(seg, bi)
				sel := p.sel[:0]
				if nLive*minLiveDen < blk.n*minLiveNum {
					// Mostly superseded: the vector executor would evaluate every record in the
					// block, including the ones nobody can see.
					p.stats.ChurnBlocks++
					cs.setBlock(blk)
					cs.setGroups(seg, bi)
					sel = p.selectScoped(cs, resolver, m, fallbackM, w, seg, blk, base, s0, sel)
				} else if vec, ok := fq.VecEval(src, blk.n, scratch); ok {
					p.stats.VectorBlocks++
					for k := 0; k < blk.n; k++ {
						if live[k/64]&(1<<uint(k%64)) == 0 || !vec.IsTrue(k) {
							continue
						}
						sel = append(sel, int32(k))
					}
				} else {
					// This block only. Every other block still vectorizes.
					p.stats.ScopeBlocks++
					cs.setBlock(blk)
					cs.setGroups(seg, bi)
					sel = p.selectScoped(cs, resolver, m, fallbackM, w, seg, blk, base, s0, sel)
				}
				p.sel = sel
				p.emitBlock(src, w, seg, base, sel, fallbackM)
				base += blk.n
			}
		}
		releaseWindows(wins)
		if p.stopped {
			return
		}
	}
}

// selectScoped is the per-record predicate tier: the expression is evaluated against the block's
// columns one record at a time, and a record the column scope cannot answer for goes to the ordinary
// evaluator on its own. Only which records survive is decided here; their values are read the same
// way in every tier.
func (p *columnProducer) selectScoped(cs *colScope, resolver func(name string, scope ast.AttributeScope) classad.Value,
	m *vm.Matcher, fallbackM *rowMatch, w segWindow, seg *colSegment, blk *columnarBlock, base int, s0 uint64,
	sel []int32) []int32 {
	for k := 0; k < blk.n; k++ {
		gk := base + k
		if gk >= seg.offsLen() {
			break
		}
		o := seg.offAt(gk)
		if !w.visible(o, s0) {
			continue
		}
		cs.k, cs.fellBack = k, false
		v := m.EvalResolved(resolver)
		matched := false
		if cs.fellBack {
			// One record needs the ordinary evaluator; the rest of the block is unaffected.
			matched = p.c.evalOneRecord(w, o, fallbackM)
		} else {
			matched = isTrueValue(v)
		}
		if matched {
			sel = append(sel, int32(k))
		}
	}
	return sel
}

// emitBlock gathers the selected records' values and hands them to the callback.
//
// A column is taken from the block where the block carries it and from the record where it does not,
// per COLUMN and not per block: a schema holds the attributes worth holding, and a projection that
// names one outside it would otherwise give up the other six. The record is materialized once per
// row however many of its cells need it, which is what makes the mixed case cost a wire read and
// not a wire read per attribute.
func (p *columnProducer) emitBlock(src *blockVecSource, w segWindow, seg *colSegment,
	base int, sel []int32, fallbackM *rowMatch) {
	if len(sel) == 0 {
		return
	}
	needRecord := false
	for ai, name := range p.attrs {
		if p.noRead[ai] {
			// Undefined without reading anything, which is also what keeps such an attribute from
			// dragging the whole block onto the record path. See noRead.
			p.fromColumn[ai] = false
			continue
		}
		p.fromColumn[ai] = src.LoadColumn(name, ast.NoScope, p.vecs[ai])
		if !p.fromColumn[ai] {
			needRecord = true
		}
	}
	row := 0
	for _, k := range sel {
		var ad wire.Ad
		if needRecord {
			b, ok := fallbackM.adAt(w, seg.offAt(base+int(k)))
			if !ok {
				// A record that cannot be read does not match, which is what the predicate tiers
				// already do with one; counted so it is not a silent loss.
				p.stats.Unreadable++
				continue
			}
			ad = wire.Ad(b)
			p.stats.RecordReads++
		}
		for ai := range p.attrs {
			switch {
			case p.noRead[ai]:
				p.setCell(ai, row, vm.VsUndef, 0, "")
			case p.fromColumn[ai]:
				state, num, str := p.vecs[ai].Cell(int(k))
				p.setCell(ai, row, state, num, str)
				p.stats.ColumnCells++
			default:
				node, found := p.lookups[ai](w.dict(), ad)
				state, num, str := nodeCell(node, found, p.opener)
				p.setCell(ai, row, state, num, str)
				p.stats.RecordCells++
			}
		}
		row++
		if row == maxProduceRows {
			if !p.flush(row) {
				return
			}
			row = 0
		}
	}
	p.flush(row)
}

// rowWindow is the tier for a window with no columnar block: evaluate each visible record the
// ordinary way and read its values from the record itself.
func (p *columnProducer) rowWindow(w segWindow, s0 uint64, m *rowMatch) {
	p.ensure(maxProduceRows)
	d := w.dict()
	row := 0
	for off := 0; off < w.used; {
		o := uint32(off)
		if end, ok := w.dmg.skip(o); ok {
			off = int(end)
			continue
		}
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		if recIsMarker(w.data, o) || !w.visible(o, s0) {
			off += int(total)
			continue
		}
		if ww, matched := m.match(w, o); matched {
			p.fromRecord(d, wire.Ad(ww), row)
			p.stats.RecordReads++
			p.stats.RecordCells += len(p.attrs)
			row++
			if row == maxProduceRows {
				if !p.flush(row) {
					return
				}
				row = 0
			}
		}
		off += int(total)
	}
	p.flush(row)
}
