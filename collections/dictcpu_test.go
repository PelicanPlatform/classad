package collections

import (
	"fmt"
	"testing"
)

// Does the trained dictionary cost CPU on the streams it does not help?
//
// The size argument against a dictionary-less codec for the cold-numeric region was that the region
// gains nothing from a dictionary (0.0% to +0.7%), which is ~100 bytes per 2.7 MB -- not worth a
// second encoder. That argument only weighed BYTES. A dictionary is not free at runtime: the encoder
// has to seed its window with the dictionary content on every EncodeAll, and the decoder has to
// reference it on every DecodeAll. Cold-numeric is decompressed on every cold-column scan, so if
// that per-call cost is real it lands on a hot path, and the trade is different.
//
// Measured per REGION KIND so the comparison is not confounded: a dictionary that costs CPU on
// cold-numeric while saving 5-7% on the cold tail is two separate decisions.

// dictCPUFixture returns raw regions by kind, plus a dictionary trained on records.
func dictCPUFixture(tb testing.TB) (map[streamKind][][]byte, []byte) {
	tb.Helper()
	ads := realOSPoolAds(tb, 20000)
	c := New(Options{Shards: 1, SegmentSize: 1 << 20})
	for i, ad := range ads {
		if err := c.Put([]byte(fmt.Sprintf("slot%d", i)), ad); err != nil {
			tb.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		tb.Skip("no sealed segments")
	}
	dict, err := TrainDict(c.CollectSamples(2000))
	if err != nil {
		tb.Fatal(err)
	}
	out := map[streamKind][][]byte{}
	for _, b := range c.sampleableBlocks(0) {
		for _, k := range []streamKind{kindColdNum, kindStr, kindCold} {
			if raw, err := b.regionRaw(k); err == nil && len(raw) > 0 {
				out[k] = append(out[k], raw)
			}
		}
	}
	return out, dict
}

var dictCPUKinds = []struct {
	kind streamKind
	name string
}{{kindColdNum, "coldNumeric"}, {kindStr, "strings"}, {kindCold, "coldTail"}}

// BenchmarkRegionCompressDict measures compression CPU per region kind, with and without the
// dictionary.
func BenchmarkRegionCompressDict(b *testing.B) {
	regions, dict := dictCPUFixture(b)
	plain, err := NewZSTDCodec(nil)
	if err != nil {
		b.Fatal(err)
	}
	trained, err := NewZSTDCodec(dict)
	if err != nil {
		b.Fatal(err)
	}
	for _, kv := range dictCPUKinds {
		payloads := regions[kv.kind]
		if len(payloads) == 0 {
			continue
		}
		bytes := 0
		for _, p := range payloads {
			bytes += len(p)
		}
		for _, cs := range []struct {
			name string
			cd   Codec
		}{{"noDict", plain}, {"dict", trained}} {
			b.Run(kv.name+"/"+cs.name, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(bytes))
				var dst []byte
				for i := 0; i < b.N; i++ {
					for _, p := range payloads {
						dst = cs.cd.Compress(dst[:0], p)
					}
				}
			})
		}
	}
}

// BenchmarkRegionDecompressDict measures DECOMPRESSION CPU per region kind. This is the one that
// matters: a cold-column scan decompresses the cold-numeric region, and a reconstruct or an escaped
// read decompresses the string and cold-tail regions. Each payload is compressed by the SAME codec
// that decompresses it, which is how it works in the store.
func BenchmarkRegionDecompressDict(b *testing.B) {
	regions, dict := dictCPUFixture(b)
	plain, err := NewZSTDCodec(nil)
	if err != nil {
		b.Fatal(err)
	}
	trained, err := NewZSTDCodec(dict)
	if err != nil {
		b.Fatal(err)
	}
	for _, kv := range dictCPUKinds {
		payloads := regions[kv.kind]
		if len(payloads) == 0 {
			continue
		}
		rawBytes := 0
		for _, p := range payloads {
			rawBytes += len(p)
		}
		for _, cs := range []struct {
			name string
			cd   Codec
		}{{"noDict", plain}, {"dict", trained}} {
			comp := make([][]byte, len(payloads))
			for i, p := range payloads {
				comp[i] = cs.cd.Compress(nil, p)
			}
			b.Run(kv.name+"/"+cs.name, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(rawBytes))
				var dst []byte
				for i := 0; i < b.N; i++ {
					for _, c := range comp {
						var err error
						dst, err = cs.cd.Decompress(dst[:0], c)
						if err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}
