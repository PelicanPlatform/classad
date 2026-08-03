package collections

import (
	"encoding/binary"
	"math"
	"sort"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// A per-segment ClassAd schema stores the segment's common, type-stable attributes in a
// fixed-offset typed blob at the front of each record, so a scan or match reads them by offset
// with no per-record wire walk. ClassAds are schemaless, but a segment of like ads (every Slot
// ad has Memory:int, Cpus:int, ...) is nearly a schema; sampling recovers it.
//
// Record layout:
//
//		[escape bitmap][fixed blob][uvarint strTableLen][string table][cold tail]
//
//	  - escape bitmap: one bit per schema field, set when this record's value is missing or
//	    exceptional (wrong kind, or an int outside the field's chosen width) -- then it is not
//	    in the fixed slot; an exceptional value is carried in the cold tail, a missing one is
//	    simply absent.
//	  - fixed blob: bools bit-packed in a leading bitset, then ints (grouped by ascending width),
//	    reals, and string slots -- each schema field at a fixed offset.
//	  - string table: bytes for tabled (>7 byte) strings; an 8-byte string slot is inline for
//	    <=7 bytes else an (offset,length) into this table.
//	  - cold tail: every non-schema attribute, and every escaped schema field, as the same
//	    uvarint(id)+node the wire form uses (self-delimiting via wire.NodeLen).
//
// This is the internal record encoding only; ads decode back to the wire/ClassAd form on the
// way out. Numeric fields are the fast path; strings/cold are correctness, not speed.
type adKind uint8

const (
	akNone adKind = iota // not a stored scalar (a computed expression, list, undefined, ...)
	akBool
	akInt
	akReal
	akString
)

// adField is one schema attribute's placement and type.
type adField struct {
	id       uint32
	kind     adKind
	width    int  // int: 1/2/4/8; real/string: 8; bool: 0 (bit-packed)
	unsigned bool // int only
	boolBit  int  // bool: bit index in the fixed blob's leading bitset
	off      int  // non-bool: byte offset in the fixed blob
	// the escape-bitmap bit index equals the field's position in s.fields.
}

// adSchema is the resolved layout for a segment.
type adSchema struct {
	fields    []adField
	byID      map[uint32]int
	boolBytes int
	fixedLen  int
	escBytes  int
}

// adSchemaOpts tunes schema construction.
type adSchemaOpts struct {
	// Presence: a field is included when its dominant storable kind is present in at least this
	// fraction of ALL sampled ads (bounds per-record waste from missing/exceptional slots).
	Presence float64
	// Fit: an int field's width is the smallest covering at least this fraction of its sampled
	// values; the rest escape. So one 1000-CPU slot does not force every Cpus to 8 bytes.
	Fit float64
	// Strings includes string attributes as 8-byte SSO slots (a per-record string table). Off
	// by default: with no cross-record dedup this can regress compressed size.
	Strings bool
}

// intFits reports whether v fits the given width and signedness.
func intFits(v int64, w int, unsigned bool) bool {
	if unsigned {
		if v < 0 {
			return false
		}
		switch w {
		case 1:
			return v <= 0xFF
		case 2:
			return v <= 0xFFFF
		case 4:
			return v <= 0xFFFFFFFF
		}
		return true
	}
	switch w {
	case 1:
		return v >= -128 && v <= 127
	case 2:
		return v >= -32768 && v <= 32767
	case 4:
		return v >= math.MinInt32 && v <= math.MaxInt32
	}
	return true
}

// chooseIntWidth picks the smallest (width, signedness) covering >= fit of the sampled values;
// the rest become per-record exceptions. Unsigned is used when every sample is non-negative.
func chooseIntWidth(vals []int64, fit float64) (int, bool) {
	unsigned := true
	for _, v := range vals {
		if v < 0 {
			unsigned = false
			break
		}
	}
	for _, w := range []int{1, 2, 4} {
		ok := 0
		for _, v := range vals {
			if intFits(v, w, unsigned) {
				ok++
			}
		}
		if len(vals) > 0 && float64(ok)/float64(len(vals)) >= fit {
			return w, unsigned
		}
	}
	return 8, false
}

// nodeKind maps a wire node to its storable scalar kind (akNone for a computed expression).
func nodeKind(node []byte) (adKind, wire.Literal) {
	lit, ok := wire.LiteralValue(node)
	if !ok {
		return akNone, lit
	}
	switch lit.Kind {
	case wire.LitBool:
		return akBool, lit
	case wire.LitInt:
		return akInt, lit
	case wire.LitReal:
		return akReal, lit
	case wire.LitString:
		return akString, lit
	}
	return akNone, lit
}

// buildAdSchema samples wire ads and resolves a schema: attributes whose dominant storable kind
// is present in >= opts.Presence of the ads, laid out smallest-to-largest.
func buildAdSchema(sample [][]byte, opts adSchemaOpts) *adSchema {
	n := len(sample)
	type stat struct {
		kinds   [5]int
		intVals []int64
	}
	stats := map[uint32]*stat{}
	for _, w := range sample {
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			st := stats[id]
			if st == nil {
				st = &stat{}
				stats[id] = st
			}
			k, lit := nodeKind(node)
			st.kinds[k]++
			if k == akInt {
				st.intVals = append(st.intVals, lit.Int)
			}
			return true
		})
	}

	var fields []adField
	for id, st := range stats {
		domK, domC := akNone, 0
		for k := akBool; k <= akString; k++ {
			if st.kinds[k] > domC {
				domC, domK = st.kinds[k], adKind(k)
			}
		}
		if domK == akNone || (domK == akString && !opts.Strings) {
			continue
		}
		if n == 0 || float64(domC)/float64(n) < opts.Presence {
			continue
		}
		f := adField{id: id, kind: domK}
		switch domK {
		case akInt:
			f.width, f.unsigned = chooseIntWidth(st.intVals, opts.Fit)
		case akReal, akString:
			f.width = 8
		}
		fields = append(fields, f)
	}

	// Layout: bool, int (width asc), real, string; ties by id for determinism. Grouping bools
	// first lets them bit-pack; grouping ints by width keeps the blob dense.
	sort.Slice(fields, func(i, j int) bool {
		if fields[i].kind != fields[j].kind {
			return fields[i].kind < fields[j].kind
		}
		if fields[i].width != fields[j].width {
			return fields[i].width < fields[j].width
		}
		return fields[i].id < fields[j].id
	})

	nBool := 0
	for _, f := range fields {
		if f.kind == akBool {
			nBool++
		}
	}
	s := &adSchema{byID: make(map[uint32]int, len(fields)), boolBytes: (nBool + 7) / 8}
	off, bit := s.boolBytes, 0
	for i := range fields {
		if fields[i].kind == akBool {
			fields[i].boolBit = bit
			bit++
		} else {
			fields[i].off = off
			off += fields[i].width
		}
		s.byID[fields[i].id] = i
	}
	s.fields = fields
	s.fixedLen = off
	s.escBytes = (len(fields) + 7) / 8
	return s
}

func setBit(b []byte, i int)       { b[i>>3] |= 1 << uint(i&7) }
func testBit(b []byte, i int) bool { return b[i>>3]&(1<<uint(i&7)) != 0 }

func putIntLE(dst []byte, v int64, w int) {
	u := uint64(v)
	for i := 0; i < w; i++ {
		dst[i] = byte(u >> (8 * uint(i)))
	}
}

func readIntLE(src []byte, w int, unsigned bool) int64 {
	var u uint64
	for i := 0; i < w; i++ {
		u |= uint64(src[i]) << (8 * uint(i))
	}
	if unsigned || w == 8 {
		return int64(u)
	}
	shift := uint(64 - 8*w) // sign-extend from w bytes
	return int64(u<<shift) >> shift
}

// writeStrSlot encodes an 8-byte string slot: high byte 0..7 = inline length (bytes in
// slot[0:len]); high byte 0xFF = tabled (offset uint32 in slot[0:4], length uint24 in
// slot[4:7], appended to the per-record table).
func writeStrSlot(slot []byte, table *[]byte, s string) {
	if len(s) <= 7 {
		copy(slot[:7], s)
		slot[7] = byte(len(s))
		return
	}
	off := len(*table)
	*table = append(*table, s...)
	binary.LittleEndian.PutUint32(slot[0:4], uint32(off))
	slot[4], slot[5], slot[6] = byte(len(s)), byte(len(s)>>8), byte(len(s)>>16)
	slot[7] = 0xFF
}

func readStrSlot(slot, table []byte) string {
	if slot[7] != 0xFF {
		return string(slot[:slot[7]])
	}
	off := binary.LittleEndian.Uint32(slot[0:4])
	l := int(slot[4]) | int(slot[5])<<8 | int(slot[6])<<16
	return string(table[off : off+uint32(l)])
}

// encode lays one wire ad out in the schema record format.
func (s *adSchema) encode(w wire.Ad) []byte {
	esc := make([]byte, s.escBytes)
	fixed := make([]byte, s.fixedLen)
	var strTable, cold []byte
	filled := make([]bool, len(s.fields))

	w.ForEach(func(id uint32, node []byte) bool {
		idx, ok := s.byID[id]
		if !ok {
			cold = append(binary.AppendUvarint(cold, uint64(id)), node...)
			return true
		}
		f := &s.fields[idx]
		k, lit := nodeKind(node)
		if k != f.kind || (f.kind == akInt && !intFits(lit.Int, f.width, f.unsigned)) {
			cold = append(binary.AppendUvarint(cold, uint64(id)), node...) // escaped -> cold tail
			return true                                                    // filled stays false
		}
		switch f.kind {
		case akBool:
			if lit.Bool {
				setBit(fixed[:s.boolBytes], f.boolBit)
			}
		case akInt:
			putIntLE(fixed[f.off:], lit.Int, f.width)
		case akReal:
			binary.LittleEndian.PutUint64(fixed[f.off:], math.Float64bits(lit.Real))
		case akString:
			writeStrSlot(fixed[f.off:f.off+8], &strTable, lit.Str)
		}
		filled[idx] = true
		return true
	})
	for i := range s.fields {
		if !filled[i] {
			setBit(esc, i) // missing or exceptional: not in the fixed slot
		}
	}
	rec := make([]byte, 0, s.escBytes+s.fixedLen+len(strTable)+len(cold)+4)
	rec = append(rec, esc...)
	rec = append(rec, fixed...)
	rec = binary.AppendUvarint(rec, uint64(len(strTable)))
	rec = append(rec, strTable...)
	rec = append(rec, cold...)
	return rec
}

// forEach yields every attribute of a schema record as (id, wire node), reconstructing a node
// for each non-escaped fixed field and replaying the cold tail. Returns false if the record is
// malformed or fn stopped early. scratch, if non-nil, is reused to build synthesized nodes.
func (s *adSchema) forEach(rec []byte, fn func(id uint32, node []byte) bool) bool {
	if len(rec) < s.escBytes+s.fixedLen {
		return false
	}
	esc := rec[:s.escBytes]
	fixed := rec[s.escBytes : s.escBytes+s.fixedLen]
	p := s.escBytes + s.fixedLen
	strLen, m := binary.Uvarint(rec[p:])
	if m <= 0 {
		return false
	}
	p += m
	if p+int(strLen) > len(rec) {
		return false
	}
	strTable := rec[p : p+int(strLen)]
	cold := rec[p+int(strLen):]

	var scratch []byte
	for i := range s.fields {
		if testBit(esc, i) {
			continue // escaped/missing: in the cold tail (exceptional) or absent (missing)
		}
		f := &s.fields[i]
		scratch = scratch[:0]
		switch f.kind {
		case akBool:
			scratch = wire.AppendBoolNode(scratch, testBit(fixed[:s.boolBytes], f.boolBit))
		case akInt:
			scratch = wire.AppendIntNode(scratch, readIntLE(fixed[f.off:], f.width, f.unsigned))
		case akReal:
			scratch = wire.AppendRealNode(scratch, math.Float64frombits(binary.LittleEndian.Uint64(fixed[f.off:])))
		case akString:
			scratch = wire.AppendStringNode(scratch, readStrSlot(fixed[f.off:f.off+8], strTable))
		}
		if !fn(f.id, scratch) {
			return true
		}
	}
	for len(cold) > 0 {
		id, m := binary.Uvarint(cold)
		if m <= 0 {
			return false
		}
		cold = cold[m:]
		nl, ok := wire.NodeLen(cold)
		if !ok || nl > len(cold) {
			return false
		}
		if !fn(uint32(id), cold[:nl]) {
			return true
		}
		cold = cold[nl:]
	}
	return true
}
