package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// benchmark: a selective query over a compressed store, with and without an index
// on the filtered attribute. Without the index every ad is decompressed and
// match-tested; with it only the (few) candidates are.
func benchSelective(b *testing.B, withIndex bool) {
	codec, err := NewZSTDCodec(nil)
	if err != nil {
		b.Fatal(err)
	}
	opts := Options{Shards: 8, Codec: codec}
	if withIndex {
		opts.CategoricalAttrs = []string{"Owner"}
	}
	c := New(opts)
	const n = 50000
	const owners = 1000 // high cardinality -> `Owner == x` matches ~n/owners = 50
	for i := 0; i < n; i++ {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ID=%d; Owner="user%d"; Arch="X86_64"; Cpus=%d; Memory=%d; State="Unclaimed" ]`,
			i, i%owners, i%16, (i%32)*512))
		if err != nil {
			b.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			b.Fatal(err)
		}
	}
	if withIndex {
		c.Reindex()
	}
	q, err := vm.Parse(`Owner == "user7"`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var matches int
	for i := 0; i < b.N; i++ {
		matches = 0
		for range c.Query(q) {
			matches++
		}
	}
	b.StopTimer()
	if matches == 0 {
		b.Fatal("expected matches")
	}
	b.ReportMetric(float64(matches), "matches")
}

func BenchmarkSelectiveWithIndex(b *testing.B) { benchSelective(b, true) }
func BenchmarkSelectiveFullScan(b *testing.B)  { benchSelective(b, false) }
