package wasmudf

import (
	"fmt"
	"testing"
)

// These benchmarks answer whether a COLUMN with one state is cheaper than a
// column with a state per element, and what the difference is made of. They run
// the same fixture shape and report the same ns/elem as benchvec_test.go, so the
// numbers are directly comparable to the version 1 ones.
//
// Four guests, one function (doubling an integer), so that the pieces can be
// separated:
//
//	vecdouble     version 1: a state byte and a branch per element
//	colvecdouble  version 2: one state, branch-free, one record per back-edge
//	colvecunroll  version 2: branch-free, EIGHT records per back-edge
//	colvecsimd    version 2: the same eight records as four i64x2 lanes
//
// The gap from the first to the second is what removing the per-element type
// costs the host and the guest. The gap from the second to the third is what
// back-edges cost, which on an interruptible runtime is most of everything. The
// gap from the third to the fourth is what SIMD is worth, and it is the smallest
// of the three -- stated here rather than left to be rediscovered, because the
// reason to want a columnar ABI is the first two gaps and not this one.

func benchColInts(n int) Batch {
	b := benchInts(n)
	return b
}

// benchColMixed is a column with one record in a hundred that does not fit, which
// is the shape the escape hatch is for: those records cross nothing and take the
// scalar path.
func benchColMixed(n int, every int) Batch {
	b := benchInts(n)
	for i := every; i < n; i += every {
		b.St[i] = vsUndef
	}
	return b
}

func BenchmarkColumnarGuests(b *testing.B) {
	const n = 4096
	for _, g := range []string{"vecdouble", "colvecdouble", "colvecunroll", "colvecsimd"} {
		for _, trusted := range []bool{false, true} {
			label := "untrusted"
			if trusted {
				label = "trusted"
			}
			b.Run(fmt.Sprintf("%s/%s", g, label), func(b *testing.B) {
				u := benchBatchFixture(b, Options{MaxBatch: n}, g, trusted, 1, 1)
				in := benchColInts(n)
				out := &Batch{N: n, St: make([]uint8, n), I: make([]int64, n)}
				for b.Loop() {
					if !u.CallBatch([]Batch{in}, out) {
						b.Fatal("declined")
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
			})
		}
	}
}

// BenchmarkColumnarScale is the amortization curve for the columnar form, the
// same four widths BenchmarkBatchScale uses.
func BenchmarkColumnarScale(b *testing.B) {
	for _, n := range []int{64, 256, 1024, 4096} {
		for _, trusted := range []bool{false, true} {
			label := "untrusted"
			if trusted {
				label = "trusted"
			}
			b.Run(fmt.Sprintf("n=%d/%s", n, label), func(b *testing.B) {
				u := benchBatchFixture(b, Options{MaxBatch: n}, "colvecsimd", trusted, 1, 1)
				in := benchColInts(n)
				out := &Batch{N: n, St: make([]uint8, n), I: make([]int64, n)}
				for b.Loop() {
					if !u.CallBatch([]Batch{in}, out) {
						b.Fatal("declined")
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
			})
		}
	}
}

// BenchmarkColumnarEscalationRate is the cost of the thing the columnar form
// trades away: a record that does not fit the column's state is not represented,
// it is escalated. At some rate of exceptions that costs more than the form
// saves, and this is where to read it off.
func BenchmarkColumnarEscalationRate(b *testing.B) {
	const n = 4096
	for _, every := range []int{0, 1024, 256, 64, 16, 4} {
		name := "none"
		if every > 0 {
			name = fmt.Sprintf("1in%d", every)
		}
		// Both guests, because version 1 escalates the same records: vecdouble's
		// own typed path declines anything that is not an integer. So the question
		// is not what escalation costs, it is at what rate the columnar form stops
		// being the cheaper way to reach the same answer.
		for _, g := range []string{"colvecsimd", "vecdouble"} {
			b.Run(g+"/"+name, func(b *testing.B) {
				u := benchBatchFixture(b, Options{MaxBatch: n}, g, true, 1, 1)
				in := benchColInts(n)
				if every > 0 {
					in = benchColMixed(n, every)
				}
				out := &Batch{N: n, St: make([]uint8, n), I: make([]int64, n)}
				for b.Loop() {
					if !u.CallBatch([]Batch{in}, out) {
						b.Fatal("declined")
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
			})
		}
	}
}

// BenchmarkColumnarUniformityScan is the host's side of the bargain: before it
// can say a column has one state it has to know, and this is what knowing costs
// when it is not declared.
//
//	declared  Batch.Uniform set, nothing is looked at
//	uniform   the word-at-a-time scan finding what it hoped to find
//	mixed     the scan failing, then the histogram and the exception mask
//
// The version 1 path's own scan (carriesStrings) is the baseline, because it is
// the pass this replaces rather than adds to.
func BenchmarkColumnarUniformityScan(b *testing.B) {
	const n = 4096
	u := benchBatchFixture(b, Options{MaxBatch: n}, "colvecdouble", true, 1, 1)
	in, err := u.acquire()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { u.release(in) })

	uniform := benchColInts(n)
	declared := benchColInts(n)
	declared.Uniform, declared.State = true, vsInt
	mixed := benchColMixed(n, 64)

	for _, tc := range []struct {
		name string
		arg  Batch
	}{{"declared", declared}, {"uniform", uniform}, {"mixed", mixed}} {
		b.Run(tc.name, func(b *testing.B) {
			args := []Batch{tc.arg}
			for b.Loop() {
				if !u.planColumns(in, n, args) {
					b.Fatal("refused")
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
		})
	}
	b.Run("v1-carriesStrings", func(b *testing.B) {
		for b.Loop() {
			if uniform.carriesStrings() {
				b.Fatal("strings")
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
	})
}

// BenchmarkColumnarHostSides isolates the two host passes the form changes, with
// no guest in the picture: writing a column in and reading a result column out.
func BenchmarkColumnarHostSides(b *testing.B) {
	const n = 4096
	u := benchBatchFixture(b, Options{MaxBatch: n}, "colvecdouble", true, 1, 1)
	in, err := u.acquire()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { u.release(in) })
	args := []Batch{benchColInts(n)}
	if !u.planColumns(in, n, args) {
		b.Fatal("refused")
	}
	out := &Batch{N: n, St: make([]uint8, n), I: make([]int64, n)}
	declined := make([]uint64, maskWords(n))

	b.Run("write/columnar", func(b *testing.B) {
		for b.Loop() {
			if _, ok := writeColBatchArgs(in.mem, in.base, in.size, n, args, in.plan, in.hostDeclined); !ok {
				b.Fatal("does not fit")
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
	})
	b.Run("write/v1-data", func(b *testing.B) {
		for b.Loop() {
			if _, ok := writeBatchArgs(in.mem, in.base, in.size, n, args); !ok {
				b.Fatal("does not fit")
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
	})

	fc, ok := writeColBatchArgs(in.mem, in.base, in.size, n, args, in.plan, in.hostDeclined)
	if !ok {
		b.Fatal("does not fit")
	}
	in.mem.WriteByte(fc.resSt, vsInt)
	b.Run("read/columnar", func(b *testing.B) {
		for b.Loop() {
			if !readColBatchResult(in.mem, fc, n, declined, out) {
				b.Fatal("bad result")
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
	})

	fd, ok := writeBatchArgs(in.mem, in.base, in.size, n, args)
	if !ok {
		b.Fatal("does not fit")
	}
	fillBytes(in.mem, fd.resSt, uint32(n), vsInt)
	b.Run("read/v1-data", func(b *testing.B) {
		for b.Loop() {
			if !readBatchResult(in.mem, fd, n, declined, out) {
				b.Fatal("bad result")
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
	})
}
