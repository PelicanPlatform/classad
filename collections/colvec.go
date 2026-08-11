package collections

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Vectorized evaluation of an arbitrary constraint over columnar blocks.
//
// colScope made any native expression columnar, but per RECORD: one interpreter dispatch per operator
// per record, each producing a boxed value. The hand-written scans (colmulti/colstatsmulti) are ~10x
// that, and the reason is not that they are hand-written -- it is that they work a COLUMN at a time, so
// dispatch is amortized over a block and the inner loops are contiguous. vm's vector executor
// generalizes that to any expression it can lower; this file is the bridge that feeds it columns.
//
// The two pieces divide as: vm.VecEval knows the expression and nothing about storage, blockVecSource
// knows the storage and nothing about the expression. So the column reads here are the only place that
// touches block layout, and they reduce to loadIntBatch -- the width-typed batch load -- plus the
// escape flag.
//
// NO FLOAT MATERIALIZATION. A real's slot already holds math.Float64bits and vm.Vec stores a real as
// those same bits, so loading a real column is a copy, not a conversion. That was the spike's specific
// recommendation and it is why LoadColumn hands loadIntBatch the vector's payload slice directly.
//
// DECLINING IS PER BLOCK, not per query. A block whose schema lacks the attribute, or holds it as a
// string, or has an escaped value that is an expression, falls back to colScope for THAT BLOCK; every
// other block still vectorizes. A schema change or a handful of odd records therefore costs a slope
// rather than a cliff.

// vecSplit records how the last VectorEvalCount divided its work: blocks served as vectors, blocks that
// declined to colScope, and windows with no block at all.
//
// Tests and benchmarks assert against it because a scan that silently declined every block would look
// exactly like a correct vectorized scan that is mysteriously no faster -- which is how three earlier
// measurements in this package produced plausible and wrong conclusions.
type vecSplit struct {
	vecBlocks    int
	scopeBlocks  int // the executor declined: a string column, an expression in the cold tail, Elvis
	churnBlocks  int // mostly-superseded: colScope is cheaper than evaluating records nobody can see
	prunedBlocks int // a zone ruled the block out; no records were examined at all
	emptyBlocks  int // nothing in the block was visible to this snapshot
	rowWindows   int
}

// vecScratchPool reuses the vector stack across queries. Without it each call allocated its own stack
// -- about as much as colScope allocates in total, which at a millisecond per query is real GC pressure
// on a busy table.
var vecScratchPool = sync.Pool{New: func() any { return new(vm.VecScratch) }}

// minLiveNum/minLiveDen is the visible fraction below which a block goes to colScope instead. The vector
// executor evaluates EVERY record in a block, including superseded ones, while colScope evaluates only
// the visible ones -- so on a heavily updated table the vectorized scan can do mostly wasted work. It is
// worth roughly 8x per record, so it stays ahead while more than an eighth of a block is visible; the
// guard trips at a quarter, leaving margin rather than sitting on the crossover.
const (
	minLiveNum = 1
	minLiveDen = 4
)

var lastVecSplit atomic.Pointer[vecSplit]

// blockVecSource feeds one columnar block's columns to the vector executor.
type blockVecSource struct {
	c   *Collection
	blk *columnarBlock
	bc  *blockCache
}

// LoadColumn resolves an attribute to a field of this block's schema and loads it as a vector.
//
// ok=false declines the block. An attribute the schema does not carry lives in the cold tail, which is
// a per-record read with a decompression behind it -- exactly what a vector load is meant to avoid --
// so it declines rather than pretending to be fast.
func (s *blockVecSource) LoadColumn(name string, scope ast.AttributeScope, dst *vm.Vec) bool {
	if scope != ast.NoScope && scope != ast.MyScope {
		return false
	}
	id, ok := s.c.intern.LookupID(name)
	if !ok {
		return false
	}
	idx, ok := s.blk.schema.byID[id]
	if !ok {
		return false
	}
	f := s.blk.schema.fields[idx]
	switch {
	case f.kind == akBool:
		return s.loadBool(idx, f, id, dst)
	case f.kind == akString:
		return s.loadStr(idx, id, dst)
	case numericKind(f.kind):
		return s.loadNum(idx, f, id, dst)
	}
	return false
}

// loadStr loads a string column, aliasing the block's decompressed string region rather than copying.
//
// The region is POSITIONAL -- uvarint(len)+bytes per non-escaped string field in schema order -- so
// reaching a field means walking the string fields before it, per record. That walk is the same one
// colScope does; what the vector path removes is the per-operator resolver call and the boxed value
// around it, not the walk.
//
// Copying is what a string predicate cannot afford: one allocation per record made a string comparison
// slower in the columnar path than in the row path, which is why this aliases and why Vec's elements are
// released back to the pool (see VecScratch.Release).
func (s *blockVecSource) loadStr(idx int, id uint32, dst *vm.Vec) bool {
	b := s.blk
	dst.Mask = false
	region, err := s.bc.stream(b, kindStr)
	if err != nil {
		return false
	}
	escFree := b.escapeFree(idx)
	for k := 0; k < b.n; k++ {
		esc := b.escapeAt(k)
		if !escFree && testBit(esc, idx) {
			dst.St[k] = vm.VsUndef // repaired from the cold tail below
			continue
		}
		rec := region[b.strOff[k]:b.strOff[k+1]]
		found := false
		for i := range b.schema.fields {
			if b.schema.fields[i].kind != akString || testBit(esc, i) {
				continue
			}
			l, n := binary.Uvarint(rec)
			if n <= 0 || int(l)+n > len(rec) {
				return false
			}
			if i == idx {
				if l == 0 {
					dst.SetString(k, "")
				} else {
					dst.SetString(k, unsafe.String(&rec[n], int(l)))
				}
				found = true
				break
			}
			rec = rec[n+int(l):]
		}
		if !found {
			return false
		}
	}
	if escFree {
		return true
	}
	return s.fixEscapes(idx, id, dst)
}

// loadNum loads a numeric column, then repairs the escaped elements from the cold tail.
func (s *blockVecSource) loadNum(idx int, f adField, id uint32, dst *vm.Vec) bool {
	dst.Mask = false
	if !s.blk.loadIntBatch(idx, s.bc, dst.I) {
		return false
	}
	st := vm.VsInt
	if f.kind == akReal {
		st = vm.VsReal
	}
	for k := 0; k < s.blk.n; k++ {
		dst.St[k] = st
	}
	if s.blk.escapeFree(idx) {
		return true
	}
	return s.fixEscapes(idx, id, dst)
}

// loadBool loads a bit-packed boolean column. The bool bitset sits immediately after the escape bitmap in
// each record's hot region, so this is a strided read of uncompressed memory.
//
// Where the column has no escape it is delivered as a MASK, built a word at a time: a stored bit becomes a
// result bit with no per-element state byte in between, which is the shortest path storage-to-kernel in the
// whole executor. A column WITH escapes goes to the data form instead, because an escaped value need not be
// a boolean at all -- the attribute may hold a number or a string on some records -- and a mask cannot
// represent that. Converting later, if a logical operator wants one, then applies the operand truthiness
// the reference applies.
func (s *blockVecSource) loadBool(idx int, f adField, id uint32, dst *vm.Vec) bool {
	b := s.blk
	if b.escapeFree(idx) {
		for w := range dst.Hi {
			start := w * 64
			end := start + 64
			if end > b.n {
				end = b.n
			}
			var lo uint64
			for k := start; k < end; k++ {
				base := k*b.hotStride + b.schema.escBytes
				if testBit(b.hot[base:base+b.schema.boolBytes], f.boolBit) {
					lo |= 1 << uint(k-start)
				}
			}
			dst.Hi[w], dst.Lo[w] = 0, lo
		}
		dst.Mask = true
		return true
	}
	dst.Mask = false
	for k := 0; k < b.n; k++ {
		base := k*b.hotStride + b.schema.escBytes
		dst.SetBool(k, testBit(b.hot[base:base+b.schema.boolBytes], f.boolBit))
	}
	return s.fixEscapes(idx, id, dst)
}

// fixEscapes overwrites the elements whose value did not fit its slot. An escape means the value is
// MISSING or UNSTORABLE: missing is UNDEFINED, and any scalar literal is read from the cold tail --
// including one of the wrong type for the column, since a number where the schema expects a string is
// exactly what escaping is for. Only a computed expression declines, because only the ordinary
// evaluator can resolve one against the rest of the ad.
//
// Reading the value as whatever type it actually is, rather than insisting it match the column, is what
// keeps a handful of odd records from declining a whole block: the kernels route a mismatched type
// through the parity hook and get the reference's answer for it.
func (s *blockVecSource) fixEscapes(idx int, id uint32, dst *vm.Vec) bool {
	for k := 0; k < s.blk.n; k++ {
		if !testBit(s.blk.escapeAt(k), idx) {
			continue
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
			return false // a computed expression
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
		case wire.LitUndef:
			dst.St[k] = vm.VsUndef
		default:
			dst.St[k] = vm.VsError
		}
	}
	return true
}

// VectorEvalCount counts records satisfying q, evaluating it a column at a time.
//
// ok=false only when there is no columnar state at all; individual blocks that cannot be vectorized
// are served by colScope, and segments without a block by a row walk, so the answer is always complete.
func (c *Collection) VectorEvalCount(q *vm.Query) (int, bool) {
	st := c.schemaScan.Load()
	if st == nil || c.intern == nil || q == nil || !q.Native() {
		return 0, false
	}
	m := q.Matcher()
	fallbackM := q.Matcher()
	cs := &colScope{bc: st.cache, c: c}
	resolver := cs.resolve
	src := &blockVecSource{c: c, bc: st.cache}
	scratch := vecScratchPool.Get().(*vm.VecScratch)
	defer func() {
		scratch.Release()
		vecScratchPool.Put(scratch)
	}()
	var live []uint64
	var split vecSplit
	defer func() { lastVecSplit.Store(&split) }()
	count := 0
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			seg := w.seg.colblk.Load()
			if seg == nil || seg.schema() == nil {
				split.rowWindows++
				count += c.rowEvalWindow(w, s0, fallbackM)
				continue
			}
			pruneIdxs, prunePreds := c.zonePrunableTests(q, seg.schema())
			base := 0
			for _, blk := range seg.blocks {
				if len(prunePreds) > 0 && !blockMayMatch(blk, pruneIdxs, prunePreds) {
					split.prunedBlocks++
					base += blk.n
					continue
				}
				// Visibility first, once. The vectorized path needs the mask anyway to count, and
				// knowing how much of the block is live is what decides whether vectorizing it is
				// worth doing at all.
				// Visibility as a BITMAP, so counting the survivors is a popcount per 64 records
				// rather than a branch per record, and so the result mask can be ANDed with it
				// directly instead of consulted element by element.
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
					if gk >= len(seg.offs) {
						break
					}
					o := seg.offs[gk]
					if recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
						live[k/64] |= 1 << uint(k%64)
						nLive++
					}
				}
				if nLive == 0 {
					split.emptyBlocks++
					base += blk.n
					continue
				}
				if nLive*minLiveDen < blk.n*minLiveNum {
					split.churnBlocks++
					cs.setBlock(blk)
					count += c.countBlockScoped(cs, resolver, m, fallbackM, w, seg, blk, base, s0)
					base += blk.n
					continue
				}
				src.blk = blk
				vec, ok := q.VecEval(src, blk.n, scratch)
				if !ok {
					// This block only. Every other block still vectorizes.
					split.scopeBlocks++
					cs.setBlock(blk)
					count += c.countBlockScoped(cs, resolver, m, fallbackM, w, seg, blk, base, s0)
					base += blk.n
					continue
				}
				split.vecBlocks++
				count += vec.CountTrue(live)
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return count, true
}
