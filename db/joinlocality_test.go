package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// IS THE CARTESIAN LOOP LIMITED BY DATA LOCALITY? NO -- AND HERE IS THE CONTROL.
//
// With no usable equality the right side is held whole and the left streamed over it, so the
// held side is walked once per left row. That is the classic shape for blocking an inner
// loop, and the question is whether it costs anything in practice.
//
// It does not. With the per-pair WORK and the SELECTIVITY held constant, the per-pair cost
// does not rise across a 262x change in how much is held -- it falls slightly, because the
// shapes that hold more stream fewer rows and so spend less of each pair on the read:
//
//	held:    125    256    512   1024   2048   4096   8192  32768
//	ns/pair: 35.2   32.5   32.0   31.8   28.1   28.9   31.9   27.9
//
// WHY THIS BENCHMARK LOOKS THE WAY IT DOES, which is the part worth keeping. An earlier
// version used `r.V >= l.Lo && r.V < l.Hi` and showed a 1.81x rise, which read as exactly the
// cache effect blocking exists to fix. It was an artifact of the fixture. With few rows held,
// almost every left row has an Lo past the whole right range, so the first comparison is
// false and the condition SHORT-CIRCUITS after one operand; with many held it is almost
// always true and both operands run. The shapes were not doing equal work per pair, and the
// difference in how much of the condition ran was read as locality.
//
// Three things were tried against that wrong conclusion before the fixture was suspected, and
// each is a reason to trust this one: disabling the GC left the rise intact, so it was not
// collection cost; a read-only baseline accounted for under 15 ns of a 179 ns spread, so it
// was not the larger shapes reading more rows; and an actual blocked-loop implementation,
// swept from 8 to 512 rows per block, moved nothing at any size. That last one is what forced
// the fixture to be re-examined -- transposing the loop takes the right side from 2M touches
// to 32K, and a real locality problem could not have been indifferent to that.
//
// So the condition here is `r.Sel == l.Sel` with Sel the same modulus on both sides: one
// comparison per pair whatever the shape, and the same fraction of pairs surviving. Keep it
// that way. A condition whose cost or selectivity varies with the shape measures the fixture.

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
		fmt.Fprintf(&b, `[ Lo = %d; Hi = %d; Sel = %d`, i, i+8, i%1000)
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
		fmt.Fprintf(&b, `[ V = %d; W = %d; Sel = %d`, i, i*2, i%1000)
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
	shapes := []int{125, 256, 512, 1024, 2048, 4096, 8192, 32768}

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
					Left:  JoinSide{Table: "l", Alias: "l", Project: []string{"Sel"}},
					Right: JoinSide{Table: "r", Alias: "r", Project: []string{"Sel"}},
					Cond:  []string{"r.Sel == l.Sel"},
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

// BenchmarkCartesianReadBaseline is the control BenchmarkCartesianLocality needs.
//
// Holding the PAIR count constant does NOT hold constant how many ROWS are read: held=125
// with 16000 streamed reads 16125 rows, while held=32768 with 61 streamed reads 32829 --
// about twice as many. Reading is not free, so part of the per-pair rise across that sweep is
// simply the larger shapes reading more, and attributing all of it to the loop would be
// wrong. This measures the read alone for the same shapes, to be subtracted.
func BenchmarkCartesianReadBaseline(b *testing.B) {
	const pairs = 2_000_000
	for _, held := range []int{125, 2048, 32768} {
		stream := pairs / held
		actual := held * stream
		b.Run(fmt.Sprintf("held=%d/stream=%d", held, stream), func(b *testing.B) {
			cat := localityFixture(b, stream, held, 4)
			b.ResetTimer()
			for b.Loop() {
				n := 0
				for _, tbl := range []string{"l", "r"} {
					d, ok := cat.Table(tbl)
					if !ok {
						b.Fatalf("no table %s", tbl)
					}
					seq, err := d.Query("true")
					if err != nil {
						b.Fatal(err)
					}
					for range seq {
						n++
					}
				}
				if n != held+stream {
					b.Fatalf("read %d rows, want %d", n, held+stream)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(actual), "ns/pair-equiv")
		})
	}
}
