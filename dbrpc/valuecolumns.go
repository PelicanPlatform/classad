package dbrpc

import (
	"encoding/binary"

	"github.com/PelicanPlatform/classad/collections"
)

// Encoding a value frame STRAIGHT FROM THE PRODUCER'S COLUMNS.
//
// The column source already hands this package typed columns -- collections.Batch holds, per
// attribute, a kind per cell and a parallel []int64 / []string. The frame it has to produce holds,
// per column, a kind, a validity bitmap, the dense values and the exceptions. Those are the same
// shape. Going through classad.Value in between boxed every cell of an already-columnar batch and
// then immediately took it apart again to write the dense arrays.
//
// Num carries an Int verbatim, a Bool as 0 or 1 and a Real as math.Float64bits, and the frame
// writes an int as uint64(n) and a real as math.Float64bits(f) -- so for both kinds the dense value
// is uint64(col.Num[i]) and there is no conversion anywhere in the middle.
//
// STRINGS ARE NOT COPIED. A batch's string aliases the block's decompressed region and is valid only
// until the callback returns, which is exactly when this runs: the bytes go into the frame while the
// block is still pinned. The row-at-a-time path has to intern them instead, because its encoder
// outlives the batch it read from.

// encodeBatchFrame writes one frame carrying rows [lo,hi) of b, appended to dst.
//
// It answers cell for cell with what appendBatch plus encode produce for the same batch -- the same
// kinds, the same dense values, the same exceptions, byte for byte. TestBatchEncodePathsAgree holds
// the two to that, because they are now two implementations of one wire format and nothing else
// would notice them drifting apart.
func (e *valueEncoder) encodeBatchFrame(dst []byte, b *collections.Batch, lo, hi int) []byte {
	n := hi - lo
	dst = putI32(dst, int32(n))
	dst = putI32(dst, int32(len(e.cols)))
	validBytes := (n + 7) / 8
	for ci := range e.cols {
		var col *collections.Column
		if ci < len(b.Cols) {
			col = &b.Cols[ci]
		}
		kind := batchColumnKind(col, lo, hi)
		var codes []int32
		var dictStrs []string
		if kind == KindString && e.dict {
			if codes, dictStrs = e.buildBatchStringDict(col, lo, hi); dictStrs != nil {
				kind = KindStringDict
			}
		}
		dst = append(dst, byte(kind))

		valid := grow(e.valid, validBytes)
		clear(valid)
		e.valid = valid
		dense := e.ints[:0]
		strs := e.strs[:0]
		boolBits := e.boolBits[:0]
		nbool := 0
		excRows := e.excRows[:0]

		for i := 0; i < n; i++ {
			ck := batchKindAt(col, lo+i)
			packed := false
			switch kind {
			case KindInt:
				if ck == collections.ValueInt {
					dense = append(dense, col.Num[lo+i])
					packed = true
				}
			case KindReal:
				if ck == collections.ValueReal {
					// Already math.Float64bits in Num; the frame wants exactly that.
					dense = append(dense, col.Num[lo+i])
					packed = true
				}
			case KindBool:
				if ck == collections.ValueBool {
					if nbool%8 == 0 {
						boolBits = append(boolBits, 0)
					}
					if col.Num[lo+i] != 0 {
						boolBits[nbool>>3] |= 1 << (uint(nbool) & 7)
					}
					nbool++
					packed = true
				}
			case KindString:
				if ck == collections.ValueString {
					strs = append(strs, col.Str[lo+i])
					packed = true
				}
			case KindStringDict:
				packed = ck == collections.ValueString
			}
			if packed {
				setBit(valid, i)
			} else {
				excRows = append(excRows, i)
			}
		}
		dst = putBytes(dst, valid)
		switch kind {
		case KindInt, KindReal:
			dst = putI32(dst, int32(len(dense)))
			for _, v := range dense {
				dst = putU64(dst, uint64(v))
			}
		case KindBool:
			dst = putI32(dst, int32(nbool))
			dst = putBytes(dst, boolBits)
		case KindString:
			dst = putI32(dst, int32(len(strs)))
			for _, s := range strs {
				dst = putStr(dst, s)
			}
		case KindStringDict:
			dst = putI32(dst, int32(len(codes)))
			dst = putI32(dst, int32(len(dictStrs)))
			for _, s := range dictStrs {
				dst = putStr(dst, s)
			}
			for _, c := range codes {
				dst = binary.AppendUvarint(dst, uint64(c))
			}
		default:
			dst = putI32(dst, 0)
		}
		dst = putI32(dst, int32(len(excRows)))
		for _, i := range excRows {
			dst = putI32(dst, int32(i))
			dst = appendBatchExc(dst, col, lo+i)
		}
		e.ints, e.strs, e.boolBits, e.excRows = dense, strs, boolBits, excRows
	}
	return dst
}

// batchKindAt is the cell's kind, or undefined where the batch has no such column. A batch always
// carries one entry per requested attribute, so the guard is for a short Cols rather than a short
// row; appendBatch reports undefined in the same case.
func batchKindAt(col *collections.Column, i int) collections.ValueKind {
	if col == nil {
		return collections.ValueUndefined
	}
	return col.Kind[i]
}

// batchColumnKind picks the kind the column packs: the most common scalar kind among its cells, as
// chooseKind does over values. Most common rather than first-seen, so one odd row at the front
// cannot demote a column that is otherwise uniform.
func batchColumnKind(col *collections.Column, lo, hi int) ValueKind {
	if col == nil {
		return KindNone
	}
	var counts [5]int
	for i := lo; i < hi; i++ {
		switch col.Kind[i] {
		case collections.ValueInt:
			counts[KindInt]++
		case collections.ValueReal:
			counts[KindReal]++
		case collections.ValueBool:
			counts[KindBool]++
		case collections.ValueString:
			counts[KindString]++
		}
	}
	best, bestN := KindNone, 0
	for k := KindInt; k <= KindString; k++ {
		if counts[k] > bestN {
			best, bestN = k, counts[k]
		}
	}
	return best
}

// appendBatchExc writes one cell that did not pack, in the exception encoding appendExcValue uses.
//
// The kind mapping is appendBatch's: a batch kind this package does not name is an ERROR, which is
// what the row-at-a-time path produces for the same cell (its switch ends in NewErrorValue). A batch
// has no list or nested-ad kind, so excExpr never arises here.
func appendBatchExc(b []byte, col *collections.Column, i int) []byte {
	switch batchKindAt(col, i) {
	case collections.ValueUndefined:
		return append(b, excUndefined)
	case collections.ValueInt:
		return putU64(append(b, excInt), uint64(col.Num[i]))
	case collections.ValueReal:
		return putU64(append(b, excReal), uint64(col.Num[i]))
	case collections.ValueBool:
		x := byte(0)
		if col.Num[i] != 0 {
			x = 1
		}
		return append(append(b, excBool), x)
	case collections.ValueString:
		return putStr(append(b, excString), col.Str[i])
	default:
		return append(b, excError)
	}
}

// buildBatchStringDict is buildStringDict over a batch column: the same payoff test, the same
// varint codes, reading kinds and strings instead of values.
func (e *valueEncoder) buildBatchStringDict(col *collections.Column, lo, hi int) ([]int32, []string) {
	if col == nil {
		return nil, nil
	}
	if e.dictIndex == nil {
		e.dictIndex = make(map[string]int32, 16)
	} else {
		clear(e.dictIndex)
	}
	index := e.dictIndex
	dict := e.dictStrs[:0]
	codes := e.dictCodes[:0]
	plain, dictBytes, codeBytes := 0, 0, 0
	for i := lo; i < hi; i++ {
		if col.Kind[i] != collections.ValueString {
			continue
		}
		str := col.Str[i]
		plain += len(str) + 2
		c, ok := index[str]
		if !ok {
			c = int32(len(dict))
			index[str] = c
			dict = append(dict, str)
			dictBytes += len(str) + 2
		}
		codes = append(codes, c)
		codeBytes += uvarintLen(uint64(c))
	}
	e.dictCodes, e.dictStrs = codes, dict
	if len(codes) == 0 || dictBytes+codeBytes >= plain {
		return nil, nil
	}
	return codes, dict
}

// The accumulator: the producer's columns, OWNED, so a frame can span several batches.
//
// collections emits one batch per columnar block -- about 400 rows on a typical segment -- while a
// frame carries ValueBatchRows. Framing per batch therefore fragments the stream: measured over a
// 160,000-row six-column read it turned 79 frames into 383, and the per-frame work on both sides
// cost 3.5x the allocations it saved. So batches are accumulated until they fill a frame.
//
// It is shaped as collections.Column because that is exactly what it holds, which lets the same
// frame writer serve both: a borrowed batch straight from the producer, or this owned copy of
// several. Kinds and numbers are bulk-copied; only strings are touched per cell, and only because
// the accumulator outlives the batch whose region they alias.
func (e *valueEncoder) accumulate(b *collections.Batch, lo, hi int) {
	if e.acc == nil {
		e.acc = make([]collections.Column, len(e.cols))
	}
	for ci := range e.acc {
		dst := &e.acc[ci]
		if ci >= len(b.Cols) {
			// No such column in this batch: undefined for every row, as appendBatch reports.
			for i := lo; i < hi; i++ {
				dst.Kind = append(dst.Kind, collections.ValueUndefined)
				dst.Num = append(dst.Num, 0)
				dst.Str = append(dst.Str, "")
			}
			continue
		}
		src := &b.Cols[ci]
		dst.Kind = append(dst.Kind, src.Kind[lo:hi]...)
		dst.Num = append(dst.Num, src.Num[lo:hi]...)
		for i := lo; i < hi; i++ {
			s := ""
			if src.Kind[i] == collections.ValueString {
				s = e.intern(src.Str[i])
			}
			dst.Str = append(dst.Str, s)
		}
	}
	e.accRows += hi - lo
}

// accReset drops the accumulated rows, keeping the capacity for the next frame.
func (e *valueEncoder) accReset() {
	for ci := range e.acc {
		e.acc[ci].Kind = e.acc[ci].Kind[:0]
		e.acc[ci].Num = e.acc[ci].Num[:0]
		e.acc[ci].Str = e.acc[ci].Str[:0]
	}
	e.accRows = 0
}
