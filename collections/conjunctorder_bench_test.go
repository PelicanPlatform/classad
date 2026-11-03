package collections

import (
	"os"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// WHAT A CONJUNCTION'S ORDER IS WORTH, AND WHY THE OBVIOUS ANSWER IS BACKWARDS.
//
// The executor skips an operand no record needs, including its column LOAD (vecpath.go). So the
// order of a `&&` chain decides which operand's column is loaded and which is elided, and the
// natural guess is that a cheap numeric comparison belongs first, where it can reject everything
// and spare an expensive string load.
//
// MEASURED, THAT IS THE WRONG WAY ROUND. Over the committed OSPool sample, on the SAME hundred
// blocks, putting the string term first runs about 1.6x faster than putting the numeric term first.
// The string operand is not the expensive one: its column is dictionary encoded, so a constant
// equality resolves the literal to a code once per block, and a literal absent from the block's
// dictionary settles every record in it without reading any. The numeric operand has no such
// shortcut -- each record's value is read and compared.
//
// The consequence for a cost-ordering pass is that it cannot be written at the expression layer.
// Which operand is cheap is a property of the COLUMN'S FORM -- dictionary or positional, fitted
// width or escaped -- and that is storage's knowledge, not the compiler's. An ordering pass driven
// by a static syntactic cost model would confidently produce the slower order here. It depends on
// the same fact form selection needs: a Vec that knows what its column is.
//
// A fourth shape is here to keep the comparison honest. `string-alone` scans 192 blocks where the
// conjunctions scan 100, because the numeric operand prunes at the zone map whether or not it is
// evaluated first -- pruning is order independent, so the ordering question is only about what
// happens INSIDE a block that survives.
var conjunctOrderCases = []struct{ name, expr string }{
	{"cheap-alone", "UpdatesLost > 1000000"},
	{"cheap-first", `UpdatesLost > 1000000 && OpSysLongName == "nope"`},
	{"string-first", `OpSysLongName == "nope" && UpdatesLost > 1000000`},
	{"string-alone", `OpSysLongName == "nope"`},
}

// BenchmarkConjunctOrder times the four shapes. CLASSAD_BENCH_ORDER_REVERSE reverses the arms: the
// arm measured first absorbs the warm-up, and a conclusion from one direction alone is not one.
func BenchmarkConjunctOrder(b *testing.B) {
	store := poolSampleFixture(b)
	cases := conjunctOrderCases
	if os.Getenv("CLASSAD_BENCH_ORDER_REVERSE") != "" {
		cases = make([]struct{ name, expr string }, 0, len(conjunctOrderCases))
		for i := len(conjunctOrderCases) - 1; i >= 0; i-- {
			cases = append(cases, conjunctOrderCases[i])
		}
	}
	for _, c := range cases {
		q, err := vm.Parse(c.expr)
		if err != nil {
			b.Fatal(err)
		}
		coldCache(b, store)
		store.VectorEvalCount(q)
		store.VectorEvalCount(q)
		b.Run(c.name, func(b *testing.B) {
			// Assert WHICH PATH RAN before reporting anything. A shape that fell back to
			// colScope would be timing a different executor, at a different speed, and would
			// do so silently -- which would make the comparison between the arms meaningless
			// while still printing numbers. It fails rather than warns for that reason.
			if _, ok := store.VectorEvalCount(q); !ok {
				b.Fatalf("%s: the vector path declined", c.name)
			}
			sp := lastVecSplit.Load()
			if sp == nil || sp.vecBlocks == 0 {
				b.Fatalf("%s: no block took the vector path: %+v", c.name, sp)
			}
			if sp.scopeBlocks != 0 {
				b.Fatalf("%s: %d block(s) fell back to colScope", c.name, sp.scopeBlocks)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := store.VectorEvalCount(q); !ok {
					b.Fatal("vector path declined mid-benchmark")
				}
			}
			b.ReportMetric(float64(sp.vecBlocks), "blocks")
		})
	}
}
