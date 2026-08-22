package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// SPIKE: is there anything for SIMD to do?
//
// Go 1.26 has simd/archsimd (GOEXPERIMENT=simd) and it is AMD64-ONLY -- every operation file is
// _amd64.go -- with an architecture-agnostic package expected in 1.27. So before anyone writes an
// intrinsic, the question worth answering is whether the vectorized scan is even limited by the
// arithmetic SIMD would widen.
//
// This decomposes the scan into the phases it actually runs, over identical data, so the answer is a
// measurement rather than an argument:
//
//	visibility  -- establish which records a snapshot can see (recSeq/recSuperseded per record)
//	+loads      -- and load each referenced column into a vector
//	+eval       -- and run the expression kernels over those vectors
//	full        -- and count, which is what VectorEvalCount does
//
// SIMD can only touch the arithmetic inside +eval. If that band is a small share of the total, widening
// it eight ways cannot matter, and the profitable work is wherever the time actually is.

const (
	phaseVisibility = iota
	phaseLoads
	phaseEval
)

// vecScanPhase runs the vectorized scan but stops after the named phase, so the phases' costs subtract.
func (c *Collection) vecScanPhase(q *vm.Query, phase int) int {
	st := c.schemaScan.Load()
	if st == nil {
		return -1
	}
	src := &blockVecSource{c: c, bc: st.cache}
	scratch := &vm.VecScratch{}
	attrs := q.ReadAttrs()
	var probe *vm.Vec
	touched := 0
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			seg := w.seg.colblk.Load()
			if seg == nil || seg.schema() == nil {
				continue
			}
			base := 0
			for _, blk := range seg.blocks {
				nLive := 0
				for k := 0; k < blk.n; k++ {
					gk := base + k
					if gk >= seg.offsLen() {
						break
					}
					o := seg.offAt(gk)
					if recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
						nLive++
					}
				}
				touched += nLive
				if phase == phaseVisibility || nLive == 0 {
					base += blk.n
					continue
				}
				src.blk = blk
				if probe == nil || cap(probe.I) < blk.n {
					probe = &vm.Vec{
						I: make([]int64, blk.n), S: make([]string, blk.n), St: make([]uint8, blk.n),
						Hi: make([]uint64, vm.MaskWords(blk.n)), Lo: make([]uint64, vm.MaskWords(blk.n)),
					}
				}
				if phase == phaseLoads {
					for _, a := range attrs {
						src.LoadColumn(a, ast.NoScope, probe)
					}
					base += blk.n
					continue
				}
				vec, ok := q.VecEval(src, blk.n, scratch)
				if !ok {
					base += blk.n
					continue
				}
				_ = vec
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return touched
}

// BenchmarkVecPhases attributes the vectorized scan's time to visibility, column loads, expression
// evaluation and counting. The evaluation band is the only one SIMD could widen.
func BenchmarkVecPhases(b *testing.B) {
	c := scopeFixtureCodec(b, 60000)
	defer c.Close()
	for _, expr := range []string{
		"RequestMemory > 4096",
		"RequestMemory > 4096 && RequestCpus >= 4",
		"RequestMemory > RequestCpus * 512",
		"RequestMemory > 4096 && (JobStatus == 1 || JobStatus == 4)",
		`Owner == "user3"`,
	} {
		q, err := vm.Parse(expr)
		if err != nil {
			b.Fatal(err)
		}
		if n := c.vecScanPhase(q, phaseVisibility); n <= 0 {
			b.Fatalf("%q: visibility pass saw %d records", expr, n)
		}
		b.Run("1visibility/"+expr, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				c.vecScanPhase(q, phaseVisibility)
			}
		})
		b.Run("2loads/"+expr, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				c.vecScanPhase(q, phaseLoads)
			}
		})
		b.Run("3eval/"+expr, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				c.vecScanPhase(q, phaseEval)
			}
		})
		b.Run("4full/"+expr, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				c.VectorEvalCount(q)
			}
		})
	}
}

// BenchmarkVecEvalScaling separates the cost of a COMPARISON from the cost of a logical COMBINE, by
// measuring the eval phase as conjuncts are added. Each extra conjunct adds one comparison and one
// combine, so the slope says what a combine costs.
//
// It matters for deciding whether SIMD is the right lever. A comparison is arithmetic over loaded
// columns, which is what SIMD widens. A combine is pure three-valued logic over the executor's boolean
// representation -- one state byte plus an eight-byte payload per element -- which no instruction set
// fixes: it is a representation problem, and a bitmap would collapse it to a word-wise AND over 64
// records at a time in ordinary Go.
func BenchmarkVecEvalScaling(b *testing.B) {
	c := scopeFixtureCodec(b, 60000)
	defer c.Close()
	for _, tc := range []struct {
		name string
		expr string
	}{
		{"1cmp_0combine", "RequestMemory > 0"},
		{"2cmp_1combine", "RequestMemory > 0 && RequestCpus > 0"},
		{"3cmp_2combine", "RequestMemory > 0 && RequestCpus > 0 && JobStatus > 0"},
		{"4cmp_3combine", "RequestMemory > 0 && RequestCpus > 0 && JobStatus > 0 && ProcId >= 0"},
	} {
		q, err := vm.Parse(tc.expr)
		if err != nil {
			b.Fatal(err)
		}
		b.Run("loads/"+tc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				c.vecScanPhase(q, phaseLoads)
			}
		})
		b.Run("eval/"+tc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				c.vecScanPhase(q, phaseEval)
			}
		})
	}
}
