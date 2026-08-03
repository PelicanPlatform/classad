package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// BenchmarkSchemaScanStore measures the columnar accelerated count vs the store's real
// QueryProject count-where path for Memory>4096, over the OSPool corpus loaded into a store
// with sealed segments.
func BenchmarkSchemaScanStore(b *testing.B) {
	ads, _ := loadOSPoolAds(b)
	store := New(Options{Shards: 1, SegmentSize: 512 << 10}) // seal ~50 ads/segment
	for i, ad := range ads {
		if err := store.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			b.Fatal(err)
		}
	}
	wires := allSegmentWiresB(b, store)
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	memID, _ := store.intern.LookupID("Memory")
	memIdx, ok := s.byID[memID]
	if !ok || s.fields[memIdx].kind != akInt {
		b.Skip("Memory not an int schema field")
	}
	store.EnableSchemaScan(s, []int{memIdx})

	q, _ := vm.Parse("Memory > 4096")
	match := func(v int64) bool { return v > 4096 }

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
	b.Run("SchemaColumnarScan", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			_ = store.schemaScanIntCount(s, memIdx, match)
		}
	})
}

func allSegmentWiresB(b *testing.B, c *Collection) [][]byte {
	var ws [][]byte
	var buf []byte
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 {
				continue
			}
			for off := 0; off < seg.used; {
				o := uint32(off)
				total := recTotalLen(seg.data, o)
				if total == 0 {
					break
				}
				if !recIsMarker(seg.data, o) {
					if w, err := seg.codec.Decompress(buf[:0], recAd(seg.data, o)); err == nil {
						buf = w
						ws = append(ws, append([]byte(nil), w...))
					}
				}
				off += int(total)
			}
		}
	}
	return ws
}
