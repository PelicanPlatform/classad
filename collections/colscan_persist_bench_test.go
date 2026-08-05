package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// BenchmarkSchemaScanPersistedReopen measures the REAL persisted path: a store whose columnar
// blocks were written at seal and RELOADED (mmap-backed, zero-copy) on reopen -- the accelerator
// nobody rebuilt. Two accelerated scans bracket the cost model:
//
//   - Cpus > 4: Cpus fits a byte, so no value escapes the fitted int width -- this is the pure
//     hot-column strided read over the mmap (fast, few allocs), the zero-heap best case.
//   - Memory > 4096: ~5% of Memory values fall outside the fitted int width and ESCAPE to the cold
//     tail, where the current scan reconstructs the whole record to read one field -- the dominant
//     cost when a numeric attribute has a heavy tail. (Optimization: read just the escaped field.)
//
// Both are compared to the row QueryProject count. Also asserts the reopen did ZERO block rebuilds.
func BenchmarkSchemaScanPersistedReopen(b *testing.B) {
	if !mmapSupported {
		b.Skip("persistence is unix-only")
	}
	ads, _ := loadOSPoolAds(b)
	if len(ads) == 0 {
		b.Skip("no corpus")
	}
	dir := b.TempDir()
	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 512 << 10})
		if err != nil {
			b.Fatal(err)
		}
		return c
	}
	c := open()
	// Enable schema-scan early (after a small sample) so every segment that seals afterward
	// persists its block -- the steady state a long-running daemon reaches.
	for i := 0; i < 20 && i < len(ads); i++ {
		c.Put([]byte(fmt.Sprintf("k%d", i)), ads[i])
	}
	mq, _ := vm.Parse("true")
	for i := 0; i < 20; i++ {
		for range c.QueryProject(mq, []string{"Memory"}) {
		}
	}
	if !c.BuildAndEnableSchemaScan(2000, 4) {
		b.Skip("schema-scan not enabled (Memory not stable int?)")
	}
	for i := 20; i < len(ads); i++ {
		c.Put([]byte(fmt.Sprintf("k%d", i)), ads[i])
	}
	c.Close()

	before := colSegmentBuilds.Load()
	c2 := open()
	defer c2.Close()
	if built := colSegmentBuilds.Load() - before; built != 0 {
		b.Fatalf("reopen rebuilt %d columnar blocks (want 0 -- must reload from disk)", built)
	}

	mem, _ := vm.Parse("Memory > 4096")
	cpus, _ := vm.Parse("Cpus > 4")

	b.Run("RowQueryProject_Memory", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			cnt := 0
			for range c2.QueryProject(mem, []string{"Memory"}) {
				cnt++
			}
			_ = cnt
		}
	})
	b.Run("Columnar_Memory_withEscapes", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			if _, ok := c2.CountQuery(mem); !ok {
				b.Fatal("CountQuery declined")
			}
		}
	})
	b.Run("Columnar_Cpus_noEscapes", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			if _, ok := c2.CountQuery(cpus); !ok {
				b.Fatal("CountQuery declined")
			}
		}
	})
}
