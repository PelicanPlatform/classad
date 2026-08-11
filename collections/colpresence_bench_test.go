package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// What the columnar presence path is worth against the row scan it replaces, on the reported query
// shape: count(*) where ProcId is undefined.
func BenchmarkPresenceCount(b *testing.B) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 20})
	defer c.Close()
	const n = 60000
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"user%d\"\nCmd = \"/home/user%d/run.sh\"\n"+
			"JobStatus = %d\nRequestMemory = %d\nArgs = \"--in in%d.dat --out out%d.dat\"",
			i/10, i%10, i%512, i%512, 1+i%5, 1024+(i%32)*512, i, i)
		if i%997 == 0 {
			src = fmt.Sprintf("ClusterId = %d\nOwner = \"user%d\"\nCmd = \"/home/user%d/run.sh\"", i/10, i%512, i%512)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(b, src)); err != nil {
			b.Fatal(err)
		}
	}
	q, err := vm.Parse("ProcId >= 0")
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		for range c.Query(q) {
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		b.Skip("no sealed segments")
	}
	pq, err := vm.Parse("ProcId is undefined")
	if err != nil {
		b.Fatal(err)
	}
	if _, ok := c.CountQuery(pq); !ok {
		b.Skip("columnar presence path declined")
	}

	b.Run("columnar", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, ok := c.CountQuery(pq); !ok {
				b.Fatal("declined mid-benchmark")
			}
		}
	})
	b.Run("rowScan", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			n := 0
			for range c.Query(pq) {
				n++
			}
		}
	})
}
