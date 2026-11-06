package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// BenchmarkListCountPool times list-filtered counts over the committed pool sample, tiled, columnarized
// and reopened, against a scalar control. It reports how each count's blocks were served.
func BenchmarkListCountPool(b *testing.B) {
	sample := loadCorpus(b)
	dir := b.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 8 << 20, GroupSchemaCount: -1})
	if err != nil {
		b.Fatal(err)
	}
	n := envInt(envWorkingN, 4000)
	for i := 0; i < n; i += 512 {
		var batch []AdUpdate
		for j := i; j < i+512 && j < n; j++ {
			batch = append(batch, AdUpdate{Key: []byte(fmt.Sprintf("ad-%d", j)), Ad: sample[j%len(sample)]})
		}
		if uerr := c.Update(batch); uerr != nil {
			b.Fatal(uerr)
		}
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		b.Fatal("no schema")
	}
	c.ColumnarizeSealed()
	if cerr := c.Close(); cerr != nil {
		b.Fatal(cerr)
	}
	c, err = Open(Options{Dir: dir, Shards: 1, SegmentSize: 8 << 20, GroupSchemaCount: -1})
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	c.BuildAndEnableSchemaScan(4096, 8)
	for _, e := range []string{"Cpus > 4", `member("Claimed", ChildState)`, "size(ChildCpus) > 8", `member("2050", ExecuteVolumes)`} {
		q, err := vm.Parse(e)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(e, func(b *testing.B) {
			for range b.N {
				c.CountQuery(q)
			}
			if sp := lastVecSplit.Load(); sp != nil {
				b.ReportMetric(float64(sp.vecBlocks), "vecblocks")
				b.ReportMetric(float64(sp.scopeBlocks), "scopeblocks")
				b.ReportMetric(float64(sp.rowWindows), "rowwindows")
			}
		})
	}
}
