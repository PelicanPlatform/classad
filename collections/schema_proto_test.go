package collections

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/klauspost/compress/zstd"
)

// This is the P1 prototype for a per-segment ClassAd schema (see the design discussion): a
// centrally-managed, sampled schema stores common, type-stable attributes in a fixed-offset
// typed blob at the front of each record (bools bit-packed, ints width-optimized by a fit
// percentile, reals f64, strings as an 8-byte SSO slot + per-record table), with the rest in
// a wire-encoded cold tail and a per-record escape bitmap for missing/exceptional values.
//
// It measures the two claims: (1) SIZE -- dropping the per-record id+type header for schema
// attributes is a net shrink for low-exception attributes, even after zstd; (2) SCAN -- a
// fixed-offset typed field scan (Memory > N) beats today's wire walk. It does not integrate
// with the store; it is a measurement harness over the real OSPool slot corpus.

type pkind uint8

const (
	pkNone pkind = iota
	pkBool
	pkInt
	pkReal
	pkString
)

type schemaAttr struct {
	id       uint32
	kind     pkind
	width    int  // int: 1/2/4/8; real/string: 8; bool: 0 (bit-packed)
	unsigned bool // int only
	boolBit  int  // bool: bit index in the fixed blob's leading bitset
	off      int  // non-bool: byte offset in the fixed blob
	escIdx   int  // bit index in the per-record escape bitmap (== position in layout order)
}

type protoSchema struct {
	attrs     []schemaAttr
	byID      map[uint32]int
	boolBytes int
	fixedLen  int
	escBytes  int
}

// intFits reports whether v fits the given width/signedness.
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

// chooseIntType picks the smallest (width, signedness) covering >= fitThresh of the sampled
// values; the rest become per-record exceptions. Unsigned is used when every sample is >= 0.
func chooseIntType(vals []int64, fitThresh float64) (int, bool) {
	unsigned := true
	for _, v := range vals {
		if v < 0 {
			unsigned = false
			break
		}
	}
	for _, w := range []int{1, 2, 4} {
		fit := 0
		for _, v := range vals {
			if intFits(v, w, unsigned) {
				fit++
			}
		}
		if float64(fit)/float64(len(vals)) >= fitThresh {
			return w, unsigned
		}
	}
	return 8, false
}

func nodeKind(node []byte) (pkind, wire.Literal) {
	lit, ok := wire.LiteralValue(node)
	if !ok {
		return pkNone, lit
	}
	switch lit.Kind {
	case wire.LitBool:
		return pkBool, lit
	case wire.LitInt:
		return pkInt, lit
	case wire.LitReal:
		return pkReal, lit
	case wire.LitString:
		return pkString, lit
	}
	return pkNone, lit
}

// buildSchema samples the ads and selects schema attributes: present with a dominant storable
// kind in >= presenceThresh of ALL ads. presenceThresh and fitThresh are the tunable knobs.
func buildSchema(wires [][]byte, presenceThresh, fitThresh float64, includeStrings bool) *protoSchema {
	n := len(wires)
	type stat struct {
		kinds   [5]int
		intVals []int64
	}
	stats := map[uint32]*stat{}
	for _, w := range wires {
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			st := stats[id]
			if st == nil {
				st = &stat{}
				stats[id] = st
			}
			k, lit := nodeKind(node)
			st.kinds[k]++
			if k == pkInt {
				st.intVals = append(st.intVals, lit.Int)
			}
			return true
		})
	}

	var chosen []schemaAttr
	for id, st := range stats {
		domK, domC := pkNone, 0
		for k := pkBool; k <= pkString; k++ {
			if st.kinds[k] > domC {
				domC, domK = st.kinds[k], pkind(k)
			}
		}
		if domK == pkNone || (domK == pkString && !includeStrings) {
			continue
		}
		if float64(domC)/float64(n) < presenceThresh {
			continue
		}
		a := schemaAttr{id: id, kind: domK}
		switch domK {
		case pkInt:
			a.width, a.unsigned = chooseIntType(st.intVals, fitThresh)
		case pkReal, pkString:
			a.width = 8
		}
		chosen = append(chosen, a)
	}

	// Layout smallest-to-largest so bools bit-pack and ints group by width: bool, int(width
	// asc), real, string; ties by id for determinism.
	classOf := func(a schemaAttr) int { return int(a.kind) }
	sort.Slice(chosen, func(i, j int) bool {
		if classOf(chosen[i]) != classOf(chosen[j]) {
			return classOf(chosen[i]) < classOf(chosen[j])
		}
		if chosen[i].width != chosen[j].width {
			return chosen[i].width < chosen[j].width
		}
		return chosen[i].id < chosen[j].id
	})

	nBool := 0
	for _, a := range chosen {
		if a.kind == pkBool {
			nBool++
		}
	}
	s := &protoSchema{byID: map[uint32]int{}, boolBytes: (nBool + 7) / 8}
	off := s.boolBytes
	bit := 0
	for i := range chosen {
		chosen[i].escIdx = i
		if chosen[i].kind == pkBool {
			chosen[i].boolBit = bit
			bit++
		} else {
			chosen[i].off = off
			off += chosen[i].width
		}
		s.byID[chosen[i].id] = i
	}
	s.attrs = chosen
	s.fixedLen = off
	s.escBytes = (len(chosen) + 7) / 8
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
	shift := uint(64 - 8*w) // sign-extend
	return int64(u<<shift) >> shift
}

// encodeRecord lays out one ad in the schema format: [escape bitmap][fixed blob][uvarint
// strTableLen][string table][cold tail]. The cold tail holds non-schema attributes and any
// schema attribute that is missing or exceptional (wrong kind / out of the chosen int range),
// each as the same uvarint(id)+node the wire form uses.
func (s *protoSchema) encodeRecord(w []byte) []byte {
	esc := make([]byte, s.escBytes)
	fixed := make([]byte, s.fixedLen)
	var strTable, cold []byte
	filled := make([]bool, len(s.attrs))

	wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
		idx, ok := s.byID[id]
		if !ok {
			cold = append(binary.AppendUvarint(cold, uint64(id)), node...)
			return true
		}
		a := &s.attrs[idx]
		k, lit := nodeKind(node)
		escape := k != a.kind || (a.kind == pkInt && !intFits(lit.Int, a.width, a.unsigned))
		if escape {
			cold = append(binary.AppendUvarint(cold, uint64(id)), node...)
			return true // filled stays false -> escape bit set below
		}
		switch a.kind {
		case pkBool:
			if lit.Bool {
				setBit(fixed[:s.boolBytes], a.boolBit)
			}
		case pkInt:
			putIntLE(fixed[a.off:], lit.Int, a.width)
		case pkReal:
			binary.LittleEndian.PutUint64(fixed[a.off:], math.Float64bits(lit.Real))
		case pkString:
			writeStrSlot(fixed[a.off:a.off+8], &strTable, lit.Str)
		}
		filled[idx] = true
		return true
	})
	for i := range s.attrs {
		if !filled[i] {
			setBit(esc, i) // missing or exceptional: not in the fixed slot
		}
	}
	rec := append([]byte{}, esc...)
	rec = append(rec, fixed...)
	rec = binary.AppendUvarint(rec, uint64(len(strTable)))
	rec = append(rec, strTable...)
	rec = append(rec, cold...)
	return rec
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

func zstdLen(b []byte) int {
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	defer enc.Close()
	return len(enc.EncodeAll(b, nil))
}

// TestSchemaSizeReport is the size half of P1: it reports raw and zstd bytes for the current
// wire encoding vs the schema encoding, over the real OSPool corpus, at a few thresholds and
// with/without strings in the schema.
func TestSchemaSizeReport(t *testing.T) {
	ads, _ := loadOSPoolAds(t)
	c, wires := encodeOSPool(t, ads)
	_ = c

	var baseRaw int
	var baseCat []byte
	for _, w := range wires {
		baseRaw += len(w)
		baseCat = append(baseCat, w...)
	}
	baseZ := zstdLen(baseCat)
	t.Logf("BASELINE (current wire): %d ads, raw=%d B (%.1f/ad), zstd=%d B",
		len(wires), baseRaw, float64(baseRaw)/float64(len(wires)), baseZ)

	// Byte attribution: where do the baseline bytes go? Header (uvarint id) vs value node,
	// split scalar (schema-eligible) vs non-scalar (expr/list -- can never be a fixed slot).
	var hdr, scalarVal, exprVal, nAttr, nExpr int
	for _, w := range wires {
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			nAttr++
			var tmp [10]byte
			hdr += binary.PutUvarint(tmp[:], uint64(id))
			if k, _ := nodeKind(node); k != pkNone {
				scalarVal += len(node)
			} else {
				exprVal += len(node)
				nExpr++
			}
			return true
		})
	}
	t.Logf("ATTRIBUTION: %d attrs (%d expr/list), header=%d B (%.1f%%), scalar-val=%d B (%.1f%%), expr-val=%d B (%.1f%%)",
		nAttr, nExpr, hdr, 100*float64(hdr)/float64(baseRaw),
		scalarVal, 100*float64(scalarVal)/float64(baseRaw), exprVal, 100*float64(exprVal)/float64(baseRaw))

	report := func(label string, presence, fit float64, strings bool) {
		s := buildSchema(wires, presence, fit, strings)
		var raw int
		var cat []byte
		for _, w := range wires {
			r := s.encodeRecord(w)
			raw += len(r)
			cat = append(cat, r...)
		}
		z := zstdLen(cat)
		nb, ni, nr, ns := 0, 0, 0, 0
		for _, a := range s.attrs {
			switch a.kind {
			case pkBool:
				nb++
			case pkInt:
				ni++
			case pkReal:
				nr++
			case pkString:
				ns++
			}
		}
		t.Logf("%-28s schema=%d attrs (%db/%di/%dr/%ds) fixed=%dB | raw=%d B (%.1f/ad, %+.1f%%) zstd=%d B (%+.1f%%)",
			label, len(s.attrs), nb, ni, nr, ns, s.fixedLen,
			raw, float64(raw)/float64(len(wires)), 100*float64(raw-baseRaw)/float64(baseRaw),
			z, 100*float64(z-baseZ)/float64(baseZ))
	}
	report("numeric p90/f95", 0.90, 0.95, false)
	report("numeric p95/f95", 0.95, 0.95, false)
	report("numeric+str p90/f95", 0.90, 0.95, true)
	report("numeric+str p95/f95", 0.95, 0.95, true)
}

// BenchmarkSchemaScanMemory is the scan half of P1: full-scan Memory > N over the corpus, the
// current wire walk vs a fixed-offset typed read.
func BenchmarkSchemaScanMemory(b *testing.B) {
	ads, _ := loadOSPoolAds(b)
	c, wires := encodeOSPool(b, ads)
	memID, ok := c.intern.LookupID("Memory")
	if !ok {
		b.Skip("no Memory attribute")
	}
	s := buildSchema(wires, 0.90, 0.95, false)
	ai, ok := s.byID[memID]
	if !ok || s.attrs[ai].kind != pkInt {
		b.Skipf("Memory not an int schema attr")
	}
	ma := s.attrs[ai]
	recs := make([][]byte, len(wires))
	for i, w := range wires {
		recs[i] = s.encodeRecord(w)
	}
	const threshold = 4096

	b.Run("WireWalk", func(b *testing.B) {
		b.ReportAllocs()
		var matched int
		for n := 0; n < b.N; n++ {
			matched = 0
			for _, w := range wires {
				wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
					if id != memID {
						return true
					}
					if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitInt && lit.Int > threshold {
						matched++
					}
					return false // found Memory; stop this ad
				})
			}
		}
		b.Logf("matched=%d/%d", matched, len(wires))
	})

	b.Run("WireLookup", func(b *testing.B) { // fair baseline: direct hot-directory accessor
		b.ReportAllocs()
		var matched int
		for n := 0; n < b.N; n++ {
			matched = 0
			for _, w := range wires {
				if node, ok := wire.Ad(w).Lookup(memID); ok {
					if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitInt && lit.Int > threshold {
						matched++
					}
				}
			}
		}
		b.Logf("matched=%d/%d", matched, len(wires))
	})

	b.Run("SchemaFixedOffset", func(b *testing.B) {
		b.ReportAllocs()
		var matched int
		for n := 0; n < b.N; n++ {
			matched = 0
			for _, r := range recs {
				if testBit(r[:s.escBytes], ma.escIdx) {
					continue // missing/exceptional: would consult the cold tail
				}
				v := readIntLE(r[s.escBytes+ma.off:], ma.width, ma.unsigned)
				if v > threshold {
					matched++
				}
			}
		}
		b.Logf("matched=%d/%d", matched, len(wires))
	})
}

// BenchmarkSchemaScanEndToEnd compares the schema fixed-offset scan against the REAL store
// scan paths for `Memory > 4096` over the same corpus: Query (decode+eval each match) and
// QueryProject (the wire-native count-where path the aggregate uses). This is the honest
// end-to-end number -- the store baselines include predicate eval and visibility.
func BenchmarkSchemaScanEndToEnd(b *testing.B) {
	ads, _ := loadOSPoolAds(b)
	c, wires := encodeOSPool(b, ads)
	memID, ok := c.intern.LookupID("Memory")
	if !ok {
		b.Skip("no Memory")
	}
	// Real, queryable store (no index -> full scan, the unselective-predicate case).
	store := New(Options{Shards: 1})
	for i, ad := range ads {
		if err := store.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			b.Fatal(err)
		}
	}
	q, err := vm.Parse("Memory > 4096")
	if err != nil {
		b.Fatal(err)
	}
	s := buildSchema(wires, 0.90, 0.95, false)
	ma := s.attrs[s.byID[memID]]
	recs := make([][]byte, len(wires))
	for i, w := range wires {
		recs[i] = s.encodeRecord(w)
	}
	const threshold = 4096

	b.Run("StoreQuery", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			cnt := 0
			for range store.Query(q) {
				cnt++
			}
			_ = cnt
		}
	})
	b.Run("StoreQueryProject", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			cnt := 0
			for range store.QueryProject(q, []string{"Memory"}) {
				cnt++
			}
			_ = cnt
		}
	})
	b.Run("SchemaFixedScan", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			cnt := 0
			for _, r := range recs {
				if testBit(r[:s.escBytes], ma.escIdx) {
					continue
				}
				if readIntLE(r[s.escBytes+ma.off:], ma.width, ma.unsigned) > threshold {
					cnt++
				}
			}
			_ = cnt
		}
	})
}
