package classad

import (
	"strconv"
	"strings"
	"testing"
)

// benchChainAd is the scope the chain benchmarks evaluate against: X sits at the
// end of the search space the chains probe, so a disjunction runs its full length
// before it finds a match.
const benchChainAd = `[X = 1000; Arch = "X86_64"; OpSys = "LINUX"; Memory = 4096; Cpus = 8; Disk = 2000000]`

func benchEvalExpr(b *testing.B, src string) {
	b.Helper()
	ad, err := Parse(benchChainAd)
	if err != nil {
		b.Fatal(err)
	}
	expr, err := ParseExpr(src)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if v := NewEvaluator(ad).Evaluate(expr.expr); !v.IsBool() {
			b.Fatalf("got %v, want a boolean", v)
		}
	}
}

// BenchmarkShortCircuitPair is the two-operand case: a && whose left operand is
// not itself a && or ||.
func BenchmarkShortCircuitPair(b *testing.B) {
	benchEvalExpr(b, `Arch == "X86_64" && Memory >= 2048`)
}

// BenchmarkShortCircuitSpine4 is the shape a job's Requirements has: a handful of
// conjuncts in one left-deep spine.
func BenchmarkShortCircuitSpine4(b *testing.B) {
	benchEvalExpr(b, `Arch == "X86_64" && OpSys == "LINUX" && Memory >= 2048 && Cpus >= 1`)
}

// BenchmarkShortCircuitSpine1000 is a machine-generated membership constraint:
// a thousand-term disjunction whose match is the last term.
func BenchmarkShortCircuitSpine1000(b *testing.B) {
	benchEvalExpr(b, orChain(1000))
}

// orChain builds "X == 1 || X == 2 || ... || X == n".
func orChain(n int) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		if i > 1 {
			sb.WriteString(" || ")
		}
		sb.WriteString("X == ")
		sb.WriteString(strconv.Itoa(i))
	}
	return sb.String()
}
