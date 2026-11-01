package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// BenchmarkDepthCheckCost puts the write-side depth check beside the Put it guards, over the pool
// corpus: "check" is wire.CheckDepth alone, "put" a whole Put (check included) into a persistent
// store and an in-memory one. The check's share of a Put is check/put.
func BenchmarkDepthCheckCost(b *testing.B) {
	ads := loadCorpus(b)
	b.Run("check", func(b *testing.B) {
		for i := 0; b.Loop(); i++ {
			if err := wire.CheckDepth(ads[i%len(ads)].AST()); err != nil {
				b.Fatal(err)
			}
		}
	})
	for _, persistent := range []bool{false, true} {
		b.Run(fmt.Sprintf("put/persistent=%v", persistent), func(b *testing.B) {
			c := New(Options{Shards: 4})
			if persistent {
				var err error
				if c, err = Open(Options{Shards: 4, Dir: b.TempDir()}); err != nil {
					b.Fatal(err)
				}
				defer func() { _ = c.Close() }()
			}
			for i := 0; b.Loop(); i++ {
				if err := c.Put(fmt.Appendf(nil, "k%d", i%4096), ads[i%len(ads)]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
