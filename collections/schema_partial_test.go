package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/klauspost/compress/zstd"
)

// Partial-decompression experiment: compress the fixed numeric prefixes of a block of records
// SEPARATELY from the string/cold tails (a column-group), so a numeric scan decompresses only
// the small prefix block -- not the strings. Tests whether this recovers both size (everything
// compressed) and scan speed (decompress only the tiny numeric column, once per block).

func splitPrefixes(s *adSchema, wires [][]byte) (prefixes, tails []byte, prefixLen int, tailLens []int) {
	prefixLen = s.escBytes + s.fixedLen
	for _, w := range wires {
		r := s.encode(wire.Ad(w))
		prefixes = append(prefixes, r[:prefixLen]...)
		tails = append(tails, r[prefixLen:]...)
		tailLens = append(tailLens, len(r)-prefixLen)
	}
	return
}

func TestSchemaPartialDecompressionSize(t *testing.T) {
	ads, _ := loadOSPoolAds(t)
	_, wires := encodeOSPool(t, ads)
	var baseCat []byte
	for _, w := range wires {
		baseCat = append(baseCat, w...)
	}
	baseZ := zstdLen(baseCat)

	for _, block := range []int{len(wires), 512, 128} {
		s := buildAdSchema(wires, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
		prefixLen := s.escBytes + s.fixedLen
		var prefixZ, tailZ, prefixRaw int
		for start := 0; start < len(wires); start += block {
			end := start + block
			if end > len(wires) {
				end = len(wires)
			}
			p, tl, _, _ := splitPrefixes(s, wires[start:end])
			prefixRaw += len(p)
			prefixZ += zstdLen(p) // numeric column-group, compressed per block
			tailZ += zstdLen(tl)
		}
		total := prefixZ + tailZ
		t.Logf("block=%-5d prefixLen=%dB | prefix raw=%dKB zstd=%dKB | tail zstd=%dKB | TOTAL=%dKB vs baseline %dKB (%+.1f%%)",
			block, prefixLen, prefixRaw/1024, prefixZ/1024, tailZ/1024, total/1024, baseZ/1024,
			100*float64(total-baseZ)/float64(baseZ))
	}
}

func BenchmarkSchemaPartialDecompression(b *testing.B) {
	ads, _ := loadOSPoolAds(b)
	c, wires := encodeOSPool(b, ads)
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	f, idx := memoryField(b, c, s)
	const threshold = 4096
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	dec, _ := zstd.NewReader(nil)

	for _, block := range []int{len(wires), 512, 128} {
		// Pre-compress each block's prefix column-group.
		prefixLen := s.escBytes + s.fixedLen
		type blk struct {
			comp []byte
			n    int
		}
		var blocks []blk
		for start := 0; start < len(wires); start += block {
			end := start + block
			if end > len(wires) {
				end = len(wires)
			}
			p, _, _, _ := splitPrefixes(s, wires[start:end])
			blocks = append(blocks, blk{enc.EncodeAll(p, nil), end - start})
		}
		b.Run(fmt.Sprintf("block=%d_decompress-prefix+strided", block), func(b *testing.B) {
			b.ReportAllocs()
			var buf []byte
			for n := 0; n < b.N; n++ {
				cnt := 0
				for _, bl := range blocks {
					buf, _ = dec.DecodeAll(bl.comp, buf[:0]) // decompress ONLY the numeric column-group
					for i := 0; i < bl.n; i++ {
						base := i * prefixLen
						if !testBit(buf[base:base+s.escBytes], idx) &&
							readIntLE(buf[base+s.escBytes+f.off:], f.width, f.unsigned) > threshold {
							cnt++
						}
					}
				}
				_ = cnt
			}
		})
	}
}
