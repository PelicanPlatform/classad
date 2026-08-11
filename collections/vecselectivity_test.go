package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// WHY THERE IS NO SELECTION PUSHDOWN YET.
//
// Is the remaining gap to the hand-written scan caused by SELECTION -- the executor loading a second
// column for records the first predicate already excluded -- or by fixed per-block overhead?
//
// The two predict different things. If it is selection, the gap widens as the first predicate gets more
// selective, because the hand-written scan skips more and the executor skips nothing. If it is fixed
// overhead, the gap stays flat. Building pushdown before knowing which would be building on a guess.
//
// The answer was FIXED OVERHEAD, mostly. The gap was 1.50x at 62% selectivity, where there is essentially
// nothing to skip, and 1.74x at 6%, where there is almost everything to skip -- so selection was worth
// about 0.24x and the other 1.50x was per-block work. Removing the literal broadcast (see
// compareVecConst) then took the gap to 1.21-1.42x, leaving selection worth ~0.18x on a highly selective
// conjunction and nothing on an unselective one.
//
// Pushdown is therefore real but small, and it is not free: restricting a load per record adds a branch
// per record to a loop that costs one or two cycles, so it can only pay where the skipped work is
// expensive -- a string region walk or a cold-tail read, not a hot numeric compare. That is the shape to
// build if this is revisited, and this benchmark is how to tell whether it worked.
//
// RequestMemory is 1024+(i%32)*512, so the thresholds below select ~100%, ~78%, ~53% and ~6%.
func BenchmarkSelectivityGap(b *testing.B) {
	c := scopeFixtureCodec(b, 60000)
	defer c.Close()
	for _, thr := range []int{0, 4096, 8192, 16000} {
		expr := fmt.Sprintf("RequestMemory > %d && RequestCpus >= 4", thr)
		q, err := vm.Parse(expr)
		if err != nil {
			b.Fatal(err)
		}
		// Both paths must actually serve it, or the comparison is meaningless.
		hw, okHW := c.CountQuery(q)
		vec, okV := c.VectorEvalCount(q)
		if !okHW || !okV {
			b.Fatalf("%s: handwritten=%v vector=%v", expr, okHW, okV)
		}
		if hw != vec {
			b.Fatalf("%s: handwritten %d != vector %d", expr, hw, vec)
		}
		if sp := lastVecSplit.Load(); sp.vecBlocks == 0 {
			b.Fatalf("%s: nothing vectorized", expr)
		}
		name := fmt.Sprintf("thr%05d_sel%d", thr, hw*100/60000)
		b.Run("vector/"+name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				c.VectorEvalCount(q)
			}
		})
		b.Run("handwritten/"+name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				c.CountQuery(q)
			}
		})
	}
}
