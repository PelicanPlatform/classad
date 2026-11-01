package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// BenchmarkDeltaChainUnopened completes every delta chain of an encrypted store whose bases were
// compacted into interned segments and whose patches are inline deltas -- the chain the plain splice
// refuses -- once per op, for each sealedUse:
//
//	opened:   decode every version with the key, merge, re-encode and seal again (what every reader
//	          paid before a keyless reader stated its use)
//	unopened: rewrite the interned base to inline names with its sealed values copied, then splice
//
// Each sub-benchmark builds its own store, so run the two arms as separate processes, alternating
// which goes first, and compare medians.
func BenchmarkDeltaChainUnopened(b *testing.B) {
	for _, arm := range []struct {
		name string
		use  sealedUse
	}{{"opened", sealedOpened}, {"unopened", sealedUnopened}} {
		b.Run(arm.name, func(b *testing.B) {
			c, err := Open(sealedParityOpts(b.TempDir()))
			if err != nil {
				b.Fatal(err)
			}
			defer c.Close()
			tx := c.Begin()
			for i := 0; i < sealedParityN; i++ {
				tx.Put([]byte(fmt.Sprintf("%d.0", i)), mustAd(b, sealedParityAd(i, false)))
			}
			if r := tx.Commit(); r.Conflicted() {
				b.Fatal("put conflicted")
			}
			internAll(c)
			tx = c.Begin()
			for i := 0; i < sealedParityN; i += 3 {
				patch := classad.New()
				patch.InsertAttr("JobStatus", int64(sealedParityJobStatus(i, true)))
				patch.InsertAttrString("Owner", sealedParityOwner(i, true))
				tx.PatchAttrs([]byte(fmt.Sprintf("%d.0", i)), patch, nil)
			}
			if r := tx.Commit(); r.Conflicted() {
				b.Fatal("patch conflicted")
			}
			type ref struct {
				r  recRef
				s0 uint64
			}
			var refs []ref
			var held [][]segWindow
			for _, sh := range c.shards {
				s0, wins := sh.snapshot()
				held = append(held, wins)
				forEachVisibleRef(s0, wins, func(r recRef) bool {
					if recIsDelta(r.w.data, r.off) {
						refs = append(refs, ref{r, s0})
					}
					return true
				})
			}
			defer func() {
				for _, w := range held {
					releaseWindows(w)
				}
			}()
			if len(refs) == 0 {
				b.Fatal("no delta records")
			}
			var buf []byte
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				for i := range refs {
					w, err := c.wireAt(refs[i].r, refs[i].s0, buf, arm.use)
					if err != nil {
						b.Fatal(err)
					}
					buf = w
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(refs)), "ns/chain")
		})
	}
}
