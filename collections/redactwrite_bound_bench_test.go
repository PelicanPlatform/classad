package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// BenchmarkRedactedBoundInterned times redacted SetAttribute writes, one transaction each, to records
// compaction left interned, on a store with the default segment size and a chain bound of 16 -- the
// writes that reach the bound need a whole record that carries the record's sealed values. "write" is
// the time per committed write over writes spread across 8 keys; "read-after-N" is the time per
// privileged point read of a key after N such writes to it, which is what the stored chain costs its
// readers.
func BenchmarkRedactedBoundInterned(b *testing.B) {
	if !mmapSupported {
		b.Skip("persistence is unix-only")
	}
	setup := func(b *testing.B) *Collection {
		opts := sealedParityOpts(b.TempDir())
		opts.SegmentSize = 0 // the default
		opts.DeltaMax = 16
		c, err := Open(opts)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { c.Close() })
		sealedPut(b, c, 0, sealedParityN)
		internAll(c)
		return c
	}
	write := func(b *testing.B, c *Collection, key string, v int) {
		tx := c.BeginRedacted()
		tx.SetDurable(false)
		patch := classad.New()
		patch.InsertAttr("JobStatus", int64(v))
		tx.PatchAttrs([]byte(key), patch, nil)
		if r := tx.Commit(); r.Committed != 1 {
			b.Fatalf("%s: %+v", key, r)
		}
	}
	b.Run("write", func(b *testing.B) {
		c := setup(b)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			write(b, c, fmt.Sprintf("%d.0", 7*(i%8)), i)
		}
	})
	for _, n := range []int{16, 64, 256} {
		b.Run(fmt.Sprintf("read-after-%d", n), func(b *testing.B) {
			c := setup(b)
			for i := 0; i < n; i++ {
				write(b, c, "7.0", i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := c.Get([]byte("7.0")); !ok {
					b.Fatal("7.0 missing")
				}
			}
		})
	}
}
