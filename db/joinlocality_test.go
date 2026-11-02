package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// IS THE CARTESIAN LOOP LIMITED BY DATA LOCALITY?
//
// With no usable equality -- a non-equi ON, or CROSS JOIN -- every left row is paired with
// every right row, and the right side is held whole while the left is streamed over it. That
// is a doubly-nested loop in left-major order, so the ENTIRE right side is walked once per
// left row. If it does not fit in cache, each pass re-pulls it from memory, and tiling the
// loop (hold a block of right resident, sweep a block of left over it) would pay.
//
// The sharp test is to hold the PAIR COUNT constant and vary how big the held side is. Pair
// count fixed means the same number of condition evaluations, the same comparisons, the same
// everything except how far the inner loop walks before it comes back. So:
//
//	flat ns/pair  => the working set fits; there is no locality problem and tiling buys nothing
//	rising        => the held side is being streamed from memory; tiling is worth writing
//
// Row width is varied too, because width is what decides whether the held side fits: the same
// row COUNT at 4 attributes and at 60 is two different working sets, and the projected read
// (joinread.go) made the common case the narrow one.

func localityFixture(tb testing.TB, left, right, width int) *Catalog {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	lt, err := cat.CreateTable("l")
	if err != nil {
		tb.Fatal(err)
	}
	rt, err := cat.CreateTable("r")
	if err != nil {
		tb.Fatal(err)
	}
	pad := func(b *strings.Builder, tag string) {
		for k := 0; k < width-2; k++ {
			fmt.Fprintf(b, `; %s%02d = "padding attribute value"`, tag, k)
		}
	}
	for i := 0; i < left; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, `[ Lo = %d; Hi = %d`, i, i+8)
		pad(&b, "LA")
		b.WriteString(" ]")
		ad, perr := classad.Parse(b.String())
		if perr != nil {
			tb.Fatal(perr)
		}
		if werr := lt.Put(fmt.Sprintf("l%d", i), ad); werr != nil {
			tb.Fatal(werr)
		}
	}
	for i := 0; i < right; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, `[ V = %d; W = %d`, i, i*2)
		pad(&b, "RA")
		b.WriteString(" ]")
		ad, perr := classad.Parse(b.String())
		if perr != nil {
			tb.Fatal(perr)
		}
		if werr := rt.Put(fmt.Sprintf("r%d", i), ad); werr != nil {
			tb.Fatal(werr)
		}
	}
	return cat
}

// BenchmarkCartesianLocality holds the pair count constant and grows the held side.
func BenchmarkCartesianLocality(b *testing.B) {
	const pairs = 2_000_000

	// right row counts, with left chosen to keep left*right == pairs.
	shapes := []int{125, 500, 2000, 8000, 32000}

	for _, width := range []int{4, 60} {
		for _, right := range shapes {
			left := pairs / right
			actual := left * right // integer division may not land exactly on pairs
			name := fmt.Sprintf("w%d/held=%d/stream=%d", width, right, left)
			b.Run(name, func(b *testing.B) {
				cat := localityFixture(b, left, right, width)
				spec := JoinSpec{
					Kind: JoinCross,
					// A band condition: no equality, so every pair is examined and the
					// condition alone decides. Narrow enough that few rows are produced,
					// so the measurement is the LOOP and not the output.
					Left:  JoinSide{Table: "l", Alias: "l", Project: []string{"Lo"}},
					Right: JoinSide{Table: "r", Alias: "r", Project: []string{"V"}},
					Cond:  []string{"r.V >= l.Lo && r.V < l.Hi"},
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					var st JoinStats
					seq, err := cat.Join(spec, JoinLimits{
						MaxHashRows: 1 << 20, MaxPairs: 1 << 30, MaxRows: 1 << 30,
					}, &st)
					if err != nil {
						b.Fatal(err)
					}
					n := 0
					for range seq {
						n++
					}
					if st.Err != nil {
						b.Fatal(st.Err)
					}
					if st.Pairs != actual {
						b.Fatalf("examined %d pairs, want %d: the shapes are not comparable",
							st.Pairs, actual)
					}
					if n == 0 {
						b.Fatal("no rows produced; the arm is measuring rejection only")
					}
				}
				// ns/op is for the whole join; the comparable number is per pair.
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(actual), "ns/pair")
			})
		}
	}
}
