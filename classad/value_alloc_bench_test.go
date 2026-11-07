package classad

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/parser"
)

// Benchmarks isolating the two things that change independently in the Value
// shrink work: the SIZE of a Value, and the inline folded prefix.
//
// Size shows up in two quite different ways, which earlier rounds of this work
// conflated:
//
//   - A []Value packs elements at exactly sizeof, so a 16-slot VM stack is
//     16*sizeof rounded ONCE to a Go size class. Every byte off sizeof counts.
//   - An individually heap-allocated Value is rounded to a size class on its
//     own, and Go's classes run ... 24, 32, 48, 64 ... with NOTHING at 40. So a
//     40-byte Value still occupies a 48-byte slot and saves nothing when boxed,
//     while a 32-byte one is a real class and saves a third.
//
// BenchmarkValueBoxed and BenchmarkValueSliceAlloc separate those two, so the
// B/op column states which kind of win a given layout actually delivers.

// valueSink is package-level so that &v escapes and the Value is heap-allocated
// individually rather than kept in a register or on the stack.
var valueSink *Value

// BenchmarkValueBoxed allocates ONE Value on the heap per iteration. Its B/op is
// the size class, not sizeof: expect 48 at both 48 and 40 bytes, and 32 at 32.
func BenchmarkValueBoxed(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v := NewStringValue("alice")
		valueSink = &v
	}
}

// BenchmarkValueBoxedSlice allocates a slice of individually boxed Values, the
// shape a cache or an index of Values takes. Each element pays the size class.
var boxSliceSink []*Value

func BenchmarkValueBoxedSlice(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := make([]*Value, 16)
		for j := range s {
			v := NewIntValue(int64(j))
			s[j] = &v
		}
		boxSliceSink = s
	}
}

// BenchmarkValueSliceAlloc allocates a contiguous []Value, the shape the VM's
// evaluation stack and every materialized list take. Here sizeof is what counts.
var sliceSink []Value

func BenchmarkValueSliceAlloc(b *testing.B) {
	for _, n := range []int{16, 256} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sliceSink = make([]Value, n)
			}
		})
	}
}

// BenchmarkNewStringValue isolates the COST side of the inline prefix: the
// prefix is computed once per string Value constructed, and string Values are
// constructed far more often than they are compared. If the prefix is going to
// lose anywhere, it loses here.
func BenchmarkNewStringValue(b *testing.B) {
	cases := []struct{ name, s string }{
		{"short", "alice"},
		{"exactly8", "X86_6464"},
		{"long", "slot1@host.example.org"},
		{"empty", ""},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				valueSink2 = NewStringValue(tc.s)
			}
		})
	}
}

var valueSink2 Value

// BenchmarkStringCompareEval is the BENEFIT side of the inline prefix, measured
// through the real evaluation path (so it works identically on an arm that has
// no prefix at all). The sub-benchmarks are chosen to bracket the prefix:
//
//   - differs-early: the prefix decides, and nothing dereferences the strings.
//     This is the prefix's best case.
//   - differs-late: the strings share their first 8 bytes, so the prefix check
//     is pure overhead on top of the full comparison. Its worst case.
//   - equal: prefix matches and the full comparison runs anyway.
//   - case-differs: equal case-insensitively, so compareStringsFold must run;
//     a RAW prefix would wrongly reject here, which is why the prefix is folded.
func BenchmarkStringCompareEval(b *testing.B) {
	cases := []struct{ name, x, y string }{
		{"differs-early", "Unclaimed", "Idle"},
		{"differs-late", "slot1@host.example.org", "slot1@host.example.net"},
		{"equal", "X86_64", "X86_64"},
		{"case-differs", "UNCLAIMED", "unclaimed"},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			ad, err := Parse(fmt.Sprintf("[X = %q; Y = %q]", tc.x, tc.y))
			if err != nil {
				b.Fatal(err)
			}
			expr, err := parser.ParseExpr("X == Y")
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				v := NewEvaluator(ad).Evaluate(expr)
				if !v.IsBool() {
					b.Fatal("expected a boolean")
				}
			}
		})
	}
}

// BenchmarkStringCompareValues drives the comparison directly at the Value level
// over a mixed corpus, without the surrounding evaluator work that dominates
// BenchmarkStringCompareEval. If the prefix pays anywhere, the margin is widest
// here -- so a null result here is a strong null result.
func BenchmarkStringCompareValues(b *testing.B) {
	corpus := []string{
		"Unclaimed", "Idle", "Owner", "Matched", "Claimed", "Preempting",
		"X86_64", "x86_64", "LINUX", "WINDOWS",
		"slot1@host.example.org", "slot2@host.example.org", "slot1@host.example.net",
		"alice", "bob", "carol", "dave",
	}
	vals := make([]Value, len(corpus))
	for i, s := range corpus {
		vals[i] = NewStringValue(s)
	}
	ev := NewEvaluator(nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := vals[i%len(vals)]
		c := vals[(i*7+3)%len(vals)]
		_ = ev.ApplyBinaryOp("==", a, c)
	}
}
