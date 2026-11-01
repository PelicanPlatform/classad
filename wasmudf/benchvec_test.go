package wasmudf

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// These benchmarks answer the question the vector entry point exists for: what
// does one record cost when the call is made once per BATCH instead of once per
// record, and how much of the scalar path's cost was the per-call machinery
// rather than the work.
//
// Everything is reported per ELEMENT (ns/elem) as well as per operation, because
// the per-operation number for a 4096-record batch is not comparable to anything
// and the per-element number is comparable to every scalar measurement in
// bench_test.go.

func benchBatchFixture(b *testing.B, opts Options, name string, trusted bool, minArgs, maxArgs int) *UDF {
	b.Helper()
	l, err := NewLoader(context.Background(), opts)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = l.Close(context.Background()) })
	wasm, err := readFixture(name)
	if err != nil {
		b.Fatal(err)
	}
	u, err := l.LoadWith(context.Background(), name, minArgs, maxArgs, wasm,
		LoadOptions{Trusted: trusted})
	if err != nil {
		b.Fatal(err)
	}
	return u
}

func benchInts(n int) Batch {
	st := make([]uint8, n)
	vals := make([]int64, n)
	for i := range n {
		st[i], vals[i] = vsInt, int64(i)
	}
	return Batch{N: n, St: st, I: vals}
}

// BenchmarkBatchScale is the amortization curve: the same work at four batch
// sizes, so the fixed per-call cost shows up as the gap between them rather than
// having to be argued about.
func BenchmarkBatchScale(b *testing.B) {
	for _, n := range []int{64, 256, 1024, 4096} {
		for _, trusted := range []bool{false, true} {
			label := "untrusted"
			if trusted {
				label = "trusted"
			}
			b.Run(fmt.Sprintf("n=%d/%s", n, label), func(b *testing.B) {
				u := benchBatchFixture(b, Options{MaxBatch: n}, "vecdouble", trusted, 1, 1)
				in := benchInts(n)
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

// BenchmarkScalarPerRecord is the same module and the same data through the
// scalar entry point, once per record: the number BenchmarkBatchScale is measured
// against. Both arms are present because the comparison that matters is
// batch-vs-scalar at the SAME trust level -- otherwise the vector path gets
// credit for turning off the backstop.
func BenchmarkScalarPerRecord(b *testing.B) {
	const n = 4096
	for _, trusted := range []bool{false, true} {
		label := "untrusted"
		if trusted {
			label = "trusted"
		}
		b.Run(label, func(b *testing.B) {
			u := benchBatchFixture(b, Options{}, "vecdouble", trusted, 1, 1)
			args := []classad.Value{classad.NewIntValue(7)}
			for b.Loop() {
				for range n {
					if v := u.Call(args); v.IsError() {
						b.Fatal("ERROR")
					}
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
		})
	}
}

// BenchmarkBatchMaskForm is a mask-form argument: 2 bits per record across the
// boundary instead of 9 bytes. It is the representation the engine's comparisons
// already produce, so this is what a predicate feeding a UDF costs.
//
// Both trust levels, because the untrusted arm cannot show the saving. The
// interruptibility tax turns out to be charged per LOOP BACK-EDGE inside the
// guest, not once per call, so for a vectorized guest it is paid per element --
// ~21ns of it, which swamps the difference between copying 1KB and copying 36KB.
// Only the trusted arm is measuring the representation.
func BenchmarkBatchMaskForm(b *testing.B) {
	const n = 4096
	states := make([]int, n)
	for i := range states {
		states[i] = i % 4
	}
	in := maskBatch(states)
	for _, trusted := range []bool{false, true} {
		label := "untrusted"
		if trusted {
			label = "trusted"
		}
		b.Run(label, func(b *testing.B) {
			u := benchBatchFixture(b, Options{MaxBatch: n}, "vecmaskarg", trusted, 1, 1)
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

// BenchmarkBatchEscalateAll is the escape hatch's worst case: every record
// declined, so every record takes a scalar call on top of the vector one. It is
// the number that says what a badly chosen vector path costs.
//
// It is FASTER than BenchmarkScalarPerRecord/untrusted, which is not what one
// would predict from "it added a call and removed nothing", and the gap -- about
// 290ns per record -- is exactly the per-call context.WithTimeout that
// escalations no longer pay, because the whole batch shares one deadline. So the
// worst case of the escape hatch is still better than the path it falls back to,
// for a reason that has nothing to do with the escape hatch and everything to do
// with where the deadline is set up.
func BenchmarkBatchEscalateAll(b *testing.B) {
	const n = 1024
	u := benchBatchFixture(b, Options{MaxBatch: n}, "vecdeclineall", false, 1, 1)
	in := benchInts(n)
	out := &Batch{N: n, St: make([]uint8, n), I: make([]int64, n)}
	for b.Loop() {
		if !u.CallBatch([]Batch{in}, out) {
			b.Fatal("declined")
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
}

// BenchmarkBatchConstArgument is a literal plus a column, which is the commonest
// shape a UDF call in a query has. The const argument crosses as one element and
// a flag, so this measures the saving of not materializing it.
func BenchmarkBatchConstArgument(b *testing.B) {
	const n = 4096
	u := benchBatchFixture(b, Options{MaxBatch: n}, "vecconst", false, 2, 2)
	k := Batch{N: n, Const: true, St: []uint8{vsInt}, I: []int64{10}}
	xs := benchInts(n)
	out := &Batch{N: n, St: make([]uint8, n), I: make([]int64, n)}
	for b.Loop() {
		if !u.CallBatch([]Batch{k, xs}, out) {
			b.Fatal("declined")
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
}

// BenchmarkMarshalForms isolates the HOST-SIDE cost of handing a column over, for
// the two representations, with no guest in the picture.
//
// It exists because BenchmarkBatchMaskForm cannot answer the question: its two
// arms run different guests (bit extraction against a load and a multiply), so
// the difference between them is not the difference between the forms. This runs
// the same code over the same record count and differs only in the shape of the
// column, which is the only way to say what passing bitplanes through actually
// saves: 9 bytes per record copied against 2 bits.
func BenchmarkMarshalForms(b *testing.B) {
	const n = 4096
	u := benchBatchFixture(b, Options{MaxBatch: n}, "vecdouble", true, 1, 1)
	in, err := u.acquire()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { u.release(in) })

	states := make([]int, n)
	for i := range states {
		states[i] = i % 4
	}
	forms := []struct {
		name string
		arg  Batch
	}{
		{"data", benchInts(n)},
		{"mask", maskBatch(states)},
		{"const", Batch{N: n, Const: true, St: []uint8{vsInt}, I: []int64{10}}},
	}
	for k := range forms {
		tc := &forms[k]
		b.Run(tc.name, func(b *testing.B) {
			args := []Batch{tc.arg}
			for b.Loop() {
				if _, ok := writeBatchArgs(in.mem, in.base, in.size, n, args); !ok {
					b.Fatal("does not fit")
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
		})
	}
}
