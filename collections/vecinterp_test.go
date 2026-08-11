package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// Would a vectorized BYTECODE INTERPRETER match hand-written vectorized code?
//
// The dispatch argument says yes: an interpreter dispatches once per opcode per BATCH, so with a batch
// of N the per-record dispatch cost is divided by N, and the kernels are the same loops either way.
// That is why production columnar engines (DuckDB, Velox) are vectorized interpreters rather than
// per-shape code or JITs.
//
// The cost it pays is MATERIALIZATION. An interpreter evaluates one opcode at a time, so
// `Memory > Cpus * 512` becomes: multiply into a temp vector, then compare the temp -- two passes over
// N elements plus a temp. Hand-written code FUSES it into one pass with no temp, which is what the
// spike did. This measures that penalty, and how it moves with vector width, since a vector that
// spills out of L1 makes the extra pass much more expensive than one that does not.

// --- kernels, as a bytecode interpreter would have them: one op, one pass, no knowledge of context ---

func kMulConst(dst, a []float64, k float64) {
	for i := range a {
		dst[i] = a[i] * k
	}
}

func kGT(dst []bool, a, b []float64) {
	for i := range a {
		dst[i] = a[i] > b[i]
	}
}

func kGTConst(dst []bool, a []float64, k float64) {
	for i := range a {
		dst[i] = a[i] > k
	}
}

func kGEConst(dst []bool, a []float64, k float64) {
	for i := range a {
		dst[i] = a[i] >= k
	}
}

func kAnd(dst, a, b []bool) {
	for i := range a {
		dst[i] = a[i] && b[i]
	}
}

func kAndValid(dst, valid []bool) {
	for i := range dst {
		dst[i] = dst[i] && valid[i]
	}
}

func kCount(sel []bool) int {
	n := 0
	for _, v := range sel {
		if v {
			n++
		}
	}
	return n
}

// vecInterpArith is `Memory > Cpus * 512` the way an interpreter would run it: separate kernel calls
// with a materialized temp, over a chunk of `width` records at a time. width <= 0 means whole block.
func (c *Collection) vecInterpArith(memName, cpusName string, factor float64, width int, fallback *vm.Matcher) (int, bool) {
	st := c.schemaScan.Load()
	if st == nil {
		return 0, false
	}
	memID, ok1 := c.intern.LookupID(memName)
	cpusID, ok2 := c.intern.LookupID(cpusName)
	if !ok1 || !ok2 {
		return 0, false
	}
	count := 0
	var memV, cpuV, tmp []float64
	var memOK, cpuOK, sel, sel2 []bool
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			sch := cs.schema()
			if sch == nil {
				count += c.rowEvalWindow(w, s0, fallback)
				continue
			}
			mi, ok1 := sch.byID[memID]
			ci, ok2 := sch.byID[cpusID]
			if !ok1 || !ok2 {
				releaseWindows(wins)
				return 0, false
			}
			base := 0
			for _, blk := range cs.blocks {
				var ok bool
				memV, memOK, ok = loadVec(blk, mi, st.cache, memV, memOK)
				if !ok {
					releaseWindows(wins)
					return 0, false
				}
				cpuV, cpuOK, ok = loadVec(blk, ci, st.cache, cpuV, cpuOK)
				if !ok {
					releaseWindows(wins)
					return 0, false
				}
				step := width
				if step <= 0 || step > blk.n {
					step = blk.n
				}
				if cap(tmp) < step {
					tmp = make([]float64, step)
					sel = make([]bool, step)
					sel2 = make([]bool, step)
				}
				for lo := 0; lo < blk.n; lo += step {
					hi := lo + step
					if hi > blk.n {
						hi = blk.n
					}
					m := hi - lo
					t, s1, s2 := tmp[:m], sel[:m], sel2[:m]
					// visibility as a vector, so the kernels below see a plain selection
					for k := 0; k < m; k++ {
						gk := base + lo + k
						vis := gk < len(cs.offs)
						if vis {
							o := cs.offs[gk]
							vis = recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0
						}
						s2[k] = vis
					}
					kMulConst(t, cpuV[lo:hi], factor) // opcode 1: temp = Cpus * 512
					kGT(s1, memV[lo:hi], t)           // opcode 2: sel = Memory > temp
					kAndValid(s1, memOK[lo:hi])       // validity
					kAndValid(s1, cpuOK[lo:hi])
					kAnd(s1, s1, s2) // visibility
					count += kCount(s1)
				}
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return count, true
}

// vecInterpTwoRanges is `A > a && B >= b` interpreter-style: two compares into two bitmaps, then AND.
func (c *Collection) vecInterpTwoRanges(aName string, aMin float64, bName string, bMin float64, width int, fallback *vm.Matcher) (int, bool) {
	st := c.schemaScan.Load()
	if st == nil {
		return 0, false
	}
	aID, ok1 := c.intern.LookupID(aName)
	bID, ok2 := c.intern.LookupID(bName)
	if !ok1 || !ok2 {
		return 0, false
	}
	count := 0
	var aV, bV []float64
	var aOK, bOK, s1, s2, s3 []bool
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			sch := cs.schema()
			if sch == nil {
				count += c.rowEvalWindow(w, s0, fallback)
				continue
			}
			ai, ok1 := sch.byID[aID]
			bi, ok2 := sch.byID[bID]
			if !ok1 || !ok2 {
				releaseWindows(wins)
				return 0, false
			}
			base := 0
			for _, blk := range cs.blocks {
				var ok bool
				aV, aOK, ok = loadVec(blk, ai, st.cache, aV, aOK)
				if !ok {
					releaseWindows(wins)
					return 0, false
				}
				bV, bOK, ok = loadVec(blk, bi, st.cache, bV, bOK)
				if !ok {
					releaseWindows(wins)
					return 0, false
				}
				step := width
				if step <= 0 || step > blk.n {
					step = blk.n
				}
				if cap(s1) < step {
					s1 = make([]bool, step)
					s2 = make([]bool, step)
					s3 = make([]bool, step)
				}
				for lo := 0; lo < blk.n; lo += step {
					hi := lo + step
					if hi > blk.n {
						hi = blk.n
					}
					m := hi - lo
					x, y, z := s1[:m], s2[:m], s3[:m]
					for k := 0; k < m; k++ {
						gk := base + lo + k
						vis := gk < len(cs.offs)
						if vis {
							o := cs.offs[gk]
							vis = recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0
						}
						z[k] = vis
					}
					kGTConst(x, aV[lo:hi], aMin)
					kGEConst(y, bV[lo:hi], bMin)
					kAnd(x, x, y)
					kAndValid(x, aOK[lo:hi])
					kAndValid(x, bOK[lo:hi])
					kAnd(x, x, z)
					count += kCount(x)
				}
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return count, true
}

// TestVecInterpAgrees keeps the timings meaningful: interpreter-style plans must agree with the fused
// ones and with the row path, at every vector width.
func TestVecInterpAgrees(t *testing.T) {
	c := scopeFixtureCodec(t, 20000)
	defer c.Close()
	aq, err := vm.Parse("RequestMemory > RequestCpus * 512")
	if err != nil {
		t.Fatal(err)
	}
	tq, err := vm.Parse("RequestMemory > 4096 && RequestCpus >= 4")
	if err != nil {
		t.Fatal(err)
	}
	wantA := rowTruth(t, c, "RequestMemory > RequestCpus * 512")
	wantT := rowTruth(t, c, "RequestMemory > 4096 && RequestCpus >= 4")
	for _, width := range []int{0, 256, 1024, 2048} {
		got, ok := c.vecInterpArith("RequestMemory", "RequestCpus", 512, width, aq.Matcher())
		if !ok || got != wantA {
			t.Errorf("arith width=%d: got %d ok=%v, want %d", width, got, ok, wantA)
		}
		got2, ok := c.vecInterpTwoRanges("RequestMemory", 4096, "RequestCpus", 4, width, tq.Matcher())
		if !ok || got2 != wantT {
			t.Errorf("tworanges width=%d: got %d ok=%v, want %d", width, got2, ok, wantT)
		}
	}
	t.Logf("interpreter-style plans agree at every width (arith=%d tworanges=%d)", wantA, wantT)
}

// BenchmarkVecInterpVsFused answers the design question: what does per-opcode interpretation with
// materialized temporaries cost against a hand-fused single pass, and how does vector width move it?
func BenchmarkVecInterpVsFused(b *testing.B) {
	c := scopeFixtureCodec(b, 60000)
	defer c.Close()
	aq, _ := vm.Parse("RequestMemory > RequestCpus * 512")
	tq, _ := vm.Parse("RequestMemory > 4096 && RequestCpus >= 4")
	aM, tM := aq.Matcher(), tq.Matcher()

	b.Run("arith/fusedHandWritten", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.vecCountMemGtCpusTimes512("RequestMemory", "RequestCpus", 512, aM)
		}
	})
	for _, w := range []int{0, 256, 1024, 2048, 8192} {
		name := "wholeBlock"
		if w > 0 {
			name = fmt.Sprintf("width%d", w)
		}
		b.Run("arith/interp/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c.vecInterpArith("RequestMemory", "RequestCpus", 512, w, aM)
			}
		})
	}
	b.Run("tworanges/fusedHandWritten", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.vecCountTwoRanges("RequestMemory", 4096, "RequestCpus", 4, tM)
		}
	})
	for _, w := range []int{0, 1024} {
		name := "wholeBlock"
		if w > 0 {
			name = fmt.Sprintf("width%d", w)
		}
		b.Run("tworanges/interp/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c.vecInterpTwoRanges("RequestMemory", 4096, "RequestCpus", 4, w, tM)
			}
		})
	}
}

// vecLoadOnly does the column loads and nothing else, to attribute the time.
func (c *Collection) vecLoadOnly(aName, bName string) (int, bool) {
	st := c.schemaScan.Load()
	if st == nil {
		return 0, false
	}
	aID, _ := c.intern.LookupID(aName)
	bID, _ := c.intern.LookupID(bName)
	touched := 0
	var aV, bV []float64
	var aOK, bOK []bool
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			sch := cs.schema()
			if sch == nil {
				continue
			}
			ai, ok1 := sch.byID[aID]
			bi, ok2 := sch.byID[bID]
			if !ok1 || !ok2 {
				continue
			}
			for _, blk := range cs.blocks {
				aV, aOK, _ = loadVec(blk, ai, st.cache, aV, aOK)
				bV, bOK, _ = loadVec(blk, bi, st.cache, bV, bOK)
				touched += len(aV) + len(bV)
			}
		}
		releaseWindows(wins)
	}
	return touched, true
}

// BenchmarkVecWhereTimeGoes attributes the cost: if loading the columns is most of it, then the
// expression machinery -- interpreted or fused -- is not what to optimize, and the lever is the LOAD.
func BenchmarkVecWhereTimeGoes(b *testing.B) {
	c := scopeFixtureCodec(b, 60000)
	defer c.Close()
	aq, _ := vm.Parse("RequestMemory > RequestCpus * 512")
	aM := aq.Matcher()
	b.Run("loadColumnsOnly", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.vecLoadOnly("RequestMemory", "RequestCpus")
		}
	})
	b.Run("loadPlusFusedEval", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.vecCountMemGtCpusTimes512("RequestMemory", "RequestCpus", 512, aM)
		}
	})
	b.Run("loadPlusInterpEval", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.vecInterpArith("RequestMemory", "RequestCpus", 512, 1024, aM)
		}
	})
}
