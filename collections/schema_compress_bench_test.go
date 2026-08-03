package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/klauspost/compress/zstd"
)

// BenchmarkSchemaScanCompressed quantifies the scan cost when records are zstd-compressed
// (as sealed segments are), the crux of the size-vs-scan-speed trade: reading Memory from an
// UNCOMPRESSED numeric prefix pays no decompression, while a fully-compressed record must be
// decoded first.
func BenchmarkSchemaScanCompressed(b *testing.B) {
	ads, _ := loadOSPoolAds(b)
	c, wires := encodeOSPool(b, ads)
	memID, _ := c.intern.LookupID("Memory")
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	f, idx := memoryField(b, c, s)
	const threshold = 4096

	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	dec, _ := zstd.NewReader(nil)
	split := s.escBytes + s.fixedLen

	baseComp := make([][]byte, len(wires))
	recFull := make([][]byte, len(wires)) // whole schema record compressed
	recTail := make([][]byte, len(wires)) // schema record: uncompressed prefix + compressed tail
	recPrefix := make([][]byte, len(wires))
	for i, w := range wires {
		baseComp[i] = enc.EncodeAll(w, nil)
		r := s.encode(wire.Ad(w))
		recFull[i] = enc.EncodeAll(r, nil)
		recPrefix[i] = append([]byte(nil), r[:split]...) // uncompressed numeric prefix
		recTail[i] = enc.EncodeAll(r[split:], nil)       // compressed tail (unused by a numeric scan)
	}

	b.Run("BaselineCompressed_decompress+walk", func(b *testing.B) {
		b.ReportAllocs()
		var buf []byte
		for n := 0; n < b.N; n++ {
			cnt := 0
			for _, cr := range baseComp {
				buf, _ = dec.DecodeAll(cr, buf[:0])
				wire.Ad(buf).ForEach(func(id uint32, node []byte) bool {
					if id != memID {
						return true
					}
					if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitInt && lit.Int > threshold {
						cnt++
					}
					return false
				})
			}
			_ = cnt
		}
	})
	b.Run("SchemaFullyCompressed_decompress+offset", func(b *testing.B) {
		b.ReportAllocs()
		var buf []byte
		for n := 0; n < b.N; n++ {
			cnt := 0
			for _, cr := range recFull {
				buf, _ = dec.DecodeAll(cr, buf[:0])
				if !testBit(buf[:s.escBytes], idx) && readIntLE(buf[s.escBytes+f.off:], f.width, f.unsigned) > threshold {
					cnt++
				}
			}
			_ = cnt
		}
	})
	b.Run("SchemaUncompressedPrefix_offset_only", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			cnt := 0
			for _, p := range recPrefix { // read Memory with NO decompression
				if !testBit(p[:s.escBytes], idx) && readIntLE(p[s.escBytes+f.off:], f.width, f.unsigned) > threshold {
					cnt++
				}
			}
			_ = cnt
		}
	})
	_ = recTail
}
