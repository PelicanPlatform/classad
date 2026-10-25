package vm

import (
	"fmt"
	"testing"
)

// benchDictSource builds a block-sized batch whose string columns are dictionary-ENCODED, which is what
// the storage layer hands the executor for a low-cardinality string attribute.
//
// Block-sized and low-cardinality on purpose: a dictionary pays for itself by being much smaller than the
// block, and a fixture whose dictionary is nearly one entry per record measures the parse rather than the
// comparison.
func benchDictSource(n, cardinality int) *dictSource {
	recs := make([]map[string]string, n)
	for i := range recs {
		recs[i] = map[string]string{
			// quoted: a record's value is ClassAd source. Half the records differ only in case,
			// so the dictionary has fold-equal runs rather than one entry per distinct value.
			"Owner": fmt.Sprintf("%q", fmt.Sprintf("%cser%d", "uU"[i%2], i%cardinality)),
			"Cmd":   fmt.Sprintf("%q", fmt.Sprintf("/home/user%d/run.sh", i%cardinality)),
			"Cpus":  fmt.Sprint(1 + i%8),
		}
	}
	return newDictSource(recs, "Owner", "Cmd")
}

// BenchmarkVecDictShapes times the string shapes over the EXECUTOR, with the column load out of the
// picture: the same Program, the same kernels, a batch already in vector form. The end-to-end number is
// BenchmarkVectorEval in the collections package, where a block decompression and the dictionary parse sit
// in front of this and compress every ratio.
func BenchmarkVecDictShapes(b *testing.B) {
	const n = 8192
	src := benchDictSource(n, 64)
	for _, expr := range []string{
		`Owner == "user3"`,
		`"user3" == Owner`,
		`Owner < "user5"`,
		`"user5" > Owner`,
		`Owner == Cmd`,
		`Owner =?= "user3"`,
		`Owner =?= Cmd`,
		`Owner == "user3" && Cpus > 2`,
		`Cpus > 2`,
	} {
		q, err := Parse(expr)
		if err != nil {
			b.Fatal(err)
		}
		var scratch VecScratch
		if _, ok := q.VecEval(src, n, &scratch); !ok {
			b.Fatalf("%q declined", expr)
		}
		b.Run(expr, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				q.VecEval(src, n, &scratch)
			}
		})
	}
}
