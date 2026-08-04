package collections

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/klauspost/compress/zstd"
)

// Measurement harness for the per-segment schema (adschema.go), over the real OSPool slot
// corpus: it reports raw+zstd size vs the current wire encoding and benchmarks a fixed-offset
// Memory>N scan vs the wire walk / hot-directory Lookup / real store scan. Skips without
// testdata/ospool_slots.ldif (like ospool_spike_test).

func zstdLen(b []byte) int {
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	defer enc.Close()
	return len(enc.EncodeAll(b, nil))
}

func TestSchemaSizeReport(t *testing.T) {
	ads, _ := loadOSPoolAds(t)
	_, wires := encodeOSPool(t, ads)

	var baseRaw int
	var baseCat []byte
	for _, w := range wires {
		baseRaw += len(w)
		baseCat = append(baseCat, w...)
	}
	baseZ := zstdLen(baseCat)
	t.Logf("BASELINE (current wire): %d ads, raw=%d B (%.1f/ad), zstd=%d B",
		len(wires), baseRaw, float64(baseRaw)/float64(len(wires)), baseZ)

	// Byte attribution: header (uvarint id) vs value node, split scalar vs expr/list.
	var hdr, scalarVal, exprVal, nAttr, nExpr int
	for _, w := range wires {
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			nAttr++
			var tmp [10]byte
			hdr += binary.PutUvarint(tmp[:], uint64(id))
			if k, _ := nodeKind(node); k != akNone {
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

	report := func(label string, opts adSchemaOpts) {
		s := buildAdSchema(wires, opts)
		var raw, hotBytes int
		var cat, tailCat []byte
		split := s.escBytes + s.fixedLen // numeric hot prefix | compressible tail
		for _, w := range wires {
			r := s.encode(wire.Ad(w))
			raw += len(r)
			cat = append(cat, r...)
			hotBytes += split
			tailCat = append(tailCat, r[split:]...)
		}
		zAll := zstdLen(cat)                  // whole record compressed
		zSplit := hotBytes + zstdLen(tailCat) // hot prefix uncompressed + tail compressed
		var nb, ni, nr, ns int
		for _, f := range s.fields {
			switch f.kind {
			case akBool:
				nb++
			case akInt:
				ni++
			case akReal:
				nr++
			case akString:
				ns++
			}
		}
		t.Logf("%-22s schema=%d (%db/%di/%dr/%ds) hot=%dB | raw %+.1f%% | zstd-all %+.1f%% | zstd hot+tail %+.1f%%",
			label, len(s.fields), nb, ni, nr, ns, split,
			100*float64(raw-baseRaw)/float64(baseRaw),
			100*float64(zAll-baseZ)/float64(baseZ),
			100*float64(zSplit-baseZ)/float64(baseZ))
	}
	report("numeric p90/f95", adSchemaOpts{Presence: 0.90, Fit: 0.95})
	report("numeric p95/f95", adSchemaOpts{Presence: 0.95, Fit: 0.95})
	report("numeric+str p90/f95", adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	report("numeric+str p95/f95", adSchemaOpts{Presence: 0.95, Fit: 0.95, Strings: true})
	report("numeric+str p80/f95", adSchemaOpts{Presence: 0.80, Fit: 0.95, Strings: true})
}

// memoryField resolves Memory as an int schema field for the scan benchmarks.
func memoryField(tb testing.TB, c *Collection, s *adSchema) (adField, int) {
	memID, ok := c.intern.LookupID("Memory")
	if !ok {
		tb.Skip("no Memory attribute")
	}
	idx, ok := s.byID[memID]
	if !ok || s.fields[idx].kind != akInt {
		tb.Skip("Memory is not an int schema field")
	}
	return s.fields[idx], idx
}

func BenchmarkSchemaScanMemory(b *testing.B) {
	ads, _ := loadOSPoolAds(b)
	c, wires := encodeOSPool(b, ads)
	memID, _ := c.intern.LookupID("Memory")
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.90, Fit: 0.95})
	f, idx := memoryField(b, c, s)
	recs := make([][]byte, len(wires))
	for i, w := range wires {
		recs[i] = s.encode(wire.Ad(w))
	}
	const threshold = 4096

	b.Run("WireWalk", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			matched := 0
			for _, w := range wires {
				wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
					if id != memID {
						return true
					}
					if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitInt && lit.Int > threshold {
						matched++
					}
					return false
				})
			}
			_ = matched
		}
	})
	b.Run("WireLookup", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			matched := 0
			for _, w := range wires {
				if node, ok := wire.Ad(w).Lookup(memID); ok {
					if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitInt && lit.Int > threshold {
						matched++
					}
				}
			}
			_ = matched
		}
	})
	b.Run("SchemaFixedOffset", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			matched := 0
			for _, r := range recs {
				if testBit(r[:s.escBytes], idx) {
					continue
				}
				if readIntLE(r[s.escBytes+f.off:], f.width, f.unsigned) > threshold {
					matched++
				}
			}
			_ = matched
		}
	})
}

// BenchmarkSchemaScanEndToEnd compares the schema fixed-offset scan against the REAL store
// scan paths for Memory > 4096: Query (decode+eval) and QueryProject (the count-where path).
func BenchmarkSchemaScanEndToEnd(b *testing.B) {
	ads, _ := loadOSPoolAds(b)
	c, wires := encodeOSPool(b, ads)
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
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.90, Fit: 0.95})
	f, idx := memoryField(b, c, s)
	recs := make([][]byte, len(wires))
	for i, w := range wires {
		recs[i] = s.encode(wire.Ad(w))
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
				if testBit(r[:s.escBytes], idx) {
					continue
				}
				if readIntLE(r[s.escBytes+f.off:], f.width, f.unsigned) > threshold {
					cnt++
				}
			}
			_ = cnt
		}
	})
}
