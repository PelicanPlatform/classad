package vm

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// THE BATCH-INVARIANCE LAW: how the records were divided into batches must not be observable.
//
// The same records, through the same program, must give the same answer and cost the same whether
// they arrive as one batch or as several — at any sizes, including a final batch smaller than the
// rest, and including a split that falls between two trips of a loop for some record.
//
// This is the law that the two state-leak bugs found so far would have failed. One was a binding
// surviving into the next batch through a reused scratch; the other was a mask released twice after
// a loop declined part-way, so the pool handed the same array to two masks that were meant to be
// different. Both were caught by a test written for that specific bug after reasoning my way to it.
// A law catches the next one without my having thought of it first.
//
// THE SCRATCH IS SHARED ACROSS THE SPLIT BATCHES ON PURPOSE. A fresh scratch per batch cannot leak
// anything and would make this vacuous; reuse is also what the scan actually does, since the whole
// point of a scratch is to amortize across blocks.

// vecRunner is one program, runnable as a batch and as a record.
type vecRunner struct {
	name   string
	batch  func(src ColumnSource, n int, s *VecScratch) (*Vec, bool)
	record func(ad *classad.ClassAd) classad.Value
}

// invarianceRunners builds the programs the law runs over: expressions, predicated scripts, and
// loops — because the state that can leak differs for each. An expression leaves nothing behind; a
// script leaves bindings; a loop leaves bindings, a live mask, and a region stack.
func invarianceRunners(t *testing.T) (exprs []vecRunner, loops []vecRunner) {
	t.Helper()
	for _, src := range append(append([]string{}, vecExprs...), chargeShapes...) {
		q, err := Parse(src)
		if err != nil {
			continue
		}
		exprs = append(exprs, vecRunner{
			name:   src,
			batch:  func(c ColumnSource, n int, s *VecScratch) (*Vec, bool) { return q.VecEval(c, n, s) },
			record: func(ad *classad.ClassAd) classad.Value { return q.Eval(ad) },
		})
	}
	for _, sh := range predShapes() {
		sp, err := sh.build(t, NewScript()).Compile()
		if err != nil {
			t.Fatalf("%s: %v", sh.name, err)
		}
		exprs = append(exprs, vecRunner{
			name:   "script/" + sh.name,
			batch:  func(c ColumnSource, n int, s *VecScratch) (*Vec, bool) { return sp.VecEval(c, n, s) },
			record: func(ad *classad.ClassAd) classad.Value { return sp.Run(ad) },
		})
	}
	for _, sh := range loopShapes() {
		sp, err := sh.build(t, NewScript()).Compile()
		if err != nil {
			t.Fatalf("%s: %v", sh.name, err)
		}
		loops = append(loops, vecRunner{
			name:   "loop/" + sh.name,
			batch:  func(c ColumnSource, n int, s *VecScratch) (*Vec, bool) { return sp.VecEval(c, n, s) },
			record: func(ad *classad.ClassAd) classad.Value { return sp.Run(ad) },
		})
	}
	return exprs, loops
}

// splitsOf returns batch-size patterns for n records: every record alone, pairs, a long batch and a
// short one in both orders, and an uneven pattern that leaves a partial final batch.
func splitsOf(n int) [][]int {
	ones := make([]int, n)
	for i := range ones {
		ones[i] = 1
	}
	var pairs []int
	for i := 0; i < n; i += 2 {
		if i+2 <= n {
			pairs = append(pairs, 2)
		} else {
			pairs = append(pairs, 1)
		}
	}
	out := [][]int{ones, pairs}
	if n > 1 {
		out = append(out, []int{n - 1, 1}, []int{1, n - 1})
	}
	// Uneven, so the last batch is a different size from the others: a partial final batch is a
	// shape a scan always produces and a fixture rarely does.
	var uneven []int
	for left, k := n, 3; left > 0; k++ {
		take := k
		if take > left {
			take = left
		}
		uneven = append(uneven, take)
		left -= take
	}
	return append(out, uneven)
}

// runSplit evaluates the records in the given batch sizes through ONE scratch, returning the
// per-record values and the total charge.
func runSplit(r vecRunner, recs []map[string]string, sizes []int) ([]classad.Value, int64, bool) {
	var vals []classad.Value
	var charge int64
	var scratch VecScratch
	at := 0
	for _, size := range sizes {
		sub := newTestSource(recs[at : at+size])
		v, ok := r.batch(sub, size, &scratch)
		if !ok {
			return nil, 0, false
		}
		for i := 0; i < size; i++ {
			vals = append(vals, v.value(i))
		}
		charge += scratch.charge
		at += size
	}
	return vals, charge, true
}

// TestVecBatchInvariance is the law.
func TestVecBatchInvariance(t *testing.T) {
	exprs, loops := invarianceRunners(t)
	for _, tc := range []struct {
		corpus  []map[string]string
		runners []vecRunner
	}{
		{vecCorpus().recs, exprs},
		{loopCorpus().recs, loops},
	} {
		n := len(tc.corpus)
		for _, r := range tc.runners {
			whole, wholeCharge, ok := runSplit(r, tc.corpus, []int{n})
			if !ok {
				continue // declined as a whole; the split runs are then not comparable
			}
			for _, sizes := range splitsOf(n) {
				got, charge, ok := runSplit(r, tc.corpus, sizes)
				if !ok {
					// A decline at one batch size and not another is itself a difference the law
					// forbids: what the executor can serve must not depend on how records were grouped.
					t.Errorf("%s: served as one batch of %d, declined as %v", r.name, n, sizes)
					continue
				}
				for i := range got {
					if !sameValue(got[i], whole[i]) {
						t.Errorf("%s split %v record %d: %s, but %s as one batch",
							r.name, sizes, i, got[i], whole[i])
					}
				}
				if charge != wholeCharge {
					t.Errorf("%s split %v: charged %d, but %d as one batch", r.name, sizes, charge, wholeCharge)
				}
			}
		}
	}
	t.Logf("%d programs x %d split patterns", len(exprs)+len(loops), len(splitsOf(15)))
}

// TestVecBatchInvarianceIsNotVacuous guards the law itself. If every program declined, or every
// corpus were one record, the loop above would pass having compared nothing.
func TestVecBatchInvarianceIsNotVacuous(t *testing.T) {
	exprs, loops := invarianceRunners(t)
	if len(exprs) < 20 || len(loops) < 4 {
		t.Fatalf("%d expression runners and %d loop runners; the law is running over almost nothing",
			len(exprs), len(loops))
	}
	served := 0
	for _, r := range loops {
		if _, _, ok := runSplit(r, loopCorpus().recs, []int{len(loopCorpus().recs)}); ok {
			served++
		}
	}
	if served < len(loops) {
		t.Errorf("only %d of %d loop programs were served as a whole batch", served, len(loops))
	}
	// And the splits really do divide the records.
	for _, sizes := range splitsOf(8) {
		total := 0
		for _, s := range sizes {
			total += s
		}
		if total != 8 {
			t.Errorf("split %v covers %d records, want 8", sizes, total)
		}
	}
}

// TestSetLiveDerivesTheWholeBatchFlag pins the one field of the live set that nothing else can.
//
// liveAll says "the mask is the whole batch", and a store consults it to take the vector wholesale
// instead of copying lane by lane. It is a pure optimization: setting it false always is CORRECT and
// simply slower, so no behavioural test can catch that — and a mutation proved it, surviving the
// entire suite. What a test can pin is the derivation itself, which is the thing that was getting
// out of step when six places maintained it by hand.
func TestSetLiveDerivesTheWholeBatchFlag(t *testing.T) {
	const n = 70 // more than one mask word, so the tail bits are in play
	var s VecScratch
	s.n = n
	full := make([]uint64, maskWords(n))
	fillLive(full, n)
	s.setLive(full)
	if !s.liveAll || s.liveCount != n {
		t.Errorf("a full mask gave liveAll=%v count=%d, want true and %d", s.liveAll, s.liveCount, n)
	}
	// One record short of the batch: the fast path must NOT be taken, or that record's lane is
	// written by a store it was not live for.
	one := make([]uint64, maskWords(n))
	fillLive(one, n)
	one[0] &^= 1
	s.setLive(one)
	if s.liveAll || s.liveCount != n-1 {
		t.Errorf("a mask missing one record gave liveAll=%v count=%d, want false and %d",
			s.liveAll, s.liveCount, n-1)
	}
	empty := make([]uint64, maskWords(n))
	s.setLive(empty)
	if s.liveAll || s.liveCount != 0 {
		t.Errorf("an empty mask gave liveAll=%v count=%d, want false and 0", s.liveAll, s.liveCount)
	}
	// And the tail beyond n must not count: a word is 64 bits and the batch is 70, so the last word
	// has 58 bits that are not records.
	all := make([]uint64, maskWords(n))
	for i := range all {
		all[i] = ^uint64(0)
	}
	s.setLive(all)
	if s.liveCount == n {
		t.Error("a mask with its tail set counted exactly n records; the tail is being counted as live " +
			"and happens to agree only because fillLive clears it")
	}
}
