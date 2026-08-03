package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/klauspost/compress/zstd"
)

// A columnar block: numeric prefixes uncompressed (fixed stride), string regions and cold
// tails each a separately-zstd'd stream with per-record offsets. Reassembling record k's full
// row = prefix_k ++ strRegion_k ++ coldTail_k (exactly the original record), so a full-ad read
// must decompress the block's string+cold streams.
type colBlock struct {
	hot            []byte // record prefixes concatenated, uncompressed (stride = prefixLen)
	strComp        []byte
	coldComp       []byte
	strOff, cldOff []int // per-record cumulative offsets into the decompressed streams
	n              int
}

func buildColBlocks(s *adSchema, recs [][]byte, block int, enc *zstd.Encoder) []colBlock {
	var blocks []colBlock
	for start := 0; start < len(recs); start += block {
		end := start + block
		if end > len(recs) {
			end = len(recs)
		}
		b := colBlock{n: end - start, strOff: []int{0}, cldOff: []int{0}}
		var strCat, cldCat []byte
		for _, r := range recs[start:end] {
			prefix, str, cold := s.splitRecord(r)
			b.hot = append(b.hot, prefix...)
			strCat = append(strCat, str...)
			cldCat = append(cldCat, cold...)
			b.strOff = append(b.strOff, len(strCat))
			b.cldOff = append(b.cldOff, len(cldCat))
		}
		b.strComp = enc.EncodeAll(strCat, nil)
		b.coldComp = enc.EncodeAll(cldCat, nil)
		blocks = append(blocks, b)
	}
	return blocks
}

func BenchmarkSchemaFullAdRead(b *testing.B) {
	ads, _ := loadOSPoolAds(b)
	_, wires := encodeOSPool(b, ads)
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	prefixLen := s.escBytes + s.fixedLen
	recs := make([][]byte, len(wires))
	for i, w := range wires {
		recs[i] = s.encode(wire.Ad(w))
	}
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	dec, _ := zstd.NewReader(nil)

	// Row baseline: each record compressed on its own (point-lookup-friendly row store).
	rowComp := make([][]byte, len(recs))
	for i, r := range recs {
		rowComp[i] = enc.EncodeAll(r, nil)
	}
	countAd := func(rec []byte) int {
		n := 0
		s.forEach(rec, func(uint32, []byte) bool { n++; return true })
		return n
	}

	// Row: point read (reconstruct one full ad) and full read (all ads).
	b.Run("Row/PointRead", func(b *testing.B) {
		b.ReportAllocs()
		var buf []byte
		mid := len(recs) / 2
		for n := 0; n < b.N; n++ {
			buf, _ = dec.DecodeAll(rowComp[mid], buf[:0])
			_ = countAd(buf)
		}
	})
	b.Run("Row/FullRead_perAd", func(b *testing.B) {
		b.ReportAllocs()
		var buf []byte
		for n := 0; n < b.N; n++ {
			for i := range rowComp {
				buf, _ = dec.DecodeAll(rowComp[i], buf[:0])
				_ = countAd(buf)
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(recs)), "ns/ad")
	})

	for _, block := range []int{128, 512, 1500} {
		blocks := buildColBlocks(s, recs, block, enc)
		// which block holds the middle record
		midBlk, midLocal := (len(recs)/2)/block, (len(recs)/2)%block

		b.Run(fmt.Sprintf("Col%d/PointRead", block), func(b *testing.B) {
			b.ReportAllocs()
			var sbuf, cbuf, rec []byte
			for n := 0; n < b.N; n++ {
				bl := &blocks[midBlk]
				sbuf, _ = dec.DecodeAll(bl.strComp, sbuf[:0]) // decompress the WHOLE block's streams
				cbuf, _ = dec.DecodeAll(bl.coldComp, cbuf[:0])
				rec = rec[:0]
				rec = append(rec, bl.hot[midLocal*prefixLen:(midLocal+1)*prefixLen]...)
				rec = append(rec, sbuf[bl.strOff[midLocal]:bl.strOff[midLocal+1]]...)
				rec = append(rec, cbuf[bl.cldOff[midLocal]:bl.cldOff[midLocal+1]]...)
				_ = countAd(rec)
			}
		})
		b.Run(fmt.Sprintf("Col%d/FullRead_perAd", block), func(b *testing.B) {
			b.ReportAllocs()
			var sbuf, cbuf, rec []byte
			for n := 0; n < b.N; n++ {
				for bi := range blocks {
					bl := &blocks[bi]
					sbuf, _ = dec.DecodeAll(bl.strComp, sbuf[:0]) // once per block
					cbuf, _ = dec.DecodeAll(bl.coldComp, cbuf[:0])
					for k := 0; k < bl.n; k++ {
						rec = rec[:0]
						rec = append(rec, bl.hot[k*prefixLen:(k+1)*prefixLen]...)
						rec = append(rec, sbuf[bl.strOff[k]:bl.strOff[k+1]]...)
						rec = append(rec, cbuf[bl.cldOff[k]:bl.cldOff[k+1]]...)
						_ = countAd(rec)
					}
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(recs)), "ns/ad")
		})
	}
}
