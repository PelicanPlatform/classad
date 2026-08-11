package vm

import (
	"fmt"
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// The vector executor's correctness argument is differential: it runs the SAME Program as the scalar
// interpreter, so the scalar interpreter is the reference, per record. Any expression the vector
// executor accepts must produce, for every record, exactly the value Query.Eval produces for that
// record's ad -- same type, same payload, same UNDEFINED, same ERROR.
//
// That is a stronger check than comparing match counts. A count hides a record whose result went from
// UNDEFINED to FALSE, which is the mistake three-valued logic invites and the one a vectorized combine
// is most likely to make.

// testSource is a ColumnSource over explicit per-record attribute EXPRESSIONS, so a test can put an
// UNDEFINED, an ERROR, a string, or a real in one column and see what the executor does with it.
//
// Both sides of the comparison are derived from the same source text: the vector side by evaluating
// each attribute's literal into a Vec element, the scalar side by inserting the same parsed expression
// into a ClassAd. Neither side is derived from the other, so the test cannot be circular.
type testSource struct {
	recs []map[string]string
	// vals caches each attribute's evaluated literal. LoadColumn must not allocate, or an allocation
	// test of the executor would be measuring the fixture.
	vals []map[string]classad.Value
}

func newTestSource(recs []map[string]string) *testSource {
	s := &testSource{recs: recs, vals: make([]map[string]classad.Value, len(recs))}
	for i, r := range recs {
		s.vals[i] = make(map[string]classad.Value, len(r))
		for k, src := range r {
			q, err := Parse(src)
			if err != nil {
				panic(err)
			}
			s.vals[i][k] = q.Eval(classad.New())
		}
	}
	return s
}

func (s *testSource) LoadColumn(name string, scope ast.AttributeScope, dst Vec) bool {
	if scope != ast.NoScope && scope != ast.MyScope {
		return false
	}
	for i := range s.recs {
		v, ok := s.vals[i][name]
		if !ok {
			dst.St[i] = VsUndef
			continue
		}
		if !dst.setValue(i, v) {
			return false // a string column: decline, exactly as a real source would
		}
	}
	return true
}

func (s *testSource) ad(i int) *classad.ClassAd {
	ad := classad.New()
	for k, src := range s.recs[i] {
		q, err := Parse(src)
		if err != nil {
			panic(err)
		}
		ad.Insert(k, q.Expr())
	}
	return ad
}

func sameValue(a, b classad.Value) bool {
	if a.Type() != b.Type() {
		return false
	}
	switch a.Type() {
	case classad.IntegerValue:
		x, _ := a.IntValue()
		y, _ := b.IntValue()
		return x == y
	case classad.RealValue:
		x, _ := a.RealValue()
		y, _ := b.RealValue()
		return x == y || (math.IsNaN(x) && math.IsNaN(y))
	case classad.BooleanValue:
		x, _ := a.BoolValue()
		y, _ := b.BoolValue()
		return x == y
	}
	return true // undefined / error carry no payload
}

// vecCorpus is a record set built to exercise the states, not just the happy path: integers, reals,
// negatives, zero (for division), a missing attribute, an explicit UNDEFINED, a string in a numeric
// position, and a boolean.
func vecCorpus() *testSource {
	return newTestSource([]map[string]string{
		{"A": "10", "B": "3", "F": "true"},
		{"A": "0", "B": "0", "F": "false"},
		{"A": "2.5", "B": "2", "F": "true"},
		{"A": "-7", "B": "0.5", "F": "false"},
		{"B": "4", "F": "true"}, // A missing
		{"A": "5"},              // B and F missing
		{"A": "undefined", "B": "1"},
		{"A": "9223372036854775807", "B": "2"}, // MaxInt64: + and * must overflow-check
		{"A": "-9223372036854775808", "B": "-1"},
		{"A": "error", "B": "1"},
		{"A": "1152921504606846976", "B": "1152921504606846976"}, // 2^60: beyond float64 exactness
		{"A": "1.5", "B": "0"},                                   // real division by zero
	})
}

var vecExprs = []string{
	// comparisons, the dominant shape
	"A > 4", "A >= 4", "A < 4", "A <= 4", "A == 4", "A != 4",
	"A > B", "A == B", "A >= B",
	// arithmetic with the fast paths
	"A + B > 8", "A - B < 0", "A * B > 20", "A + B", "A * B", "A - B",
	// arithmetic routed to the hook
	"A / B > 2", "A % B == 0", "A / B", "A % B",
	// integers that must not become float64
	"A + B == 2305843009213693952",
	// logical combines, including operands that are not boolean
	"A > 4 && B < 4", "A > 4 || B < 4", "A > 4 && B < 4 && A != 0",
	"A > 4 || B < 4 || A == 0", "!(A > 4)", "A > 4 && !(B < 2)",
	"F && A > 4", "F || A > 4", "!F",
	"A && B",     // non-boolean logical operands: the hook's problem
	"F && 1 / 0", // a FALSE that must dominate an ERROR the vector executor computed anyway
	// unary
	"-A > 0", "-A", "-(A + B)",
	// identity operators, hook-routed
	"A =?= B", "A =!= B", "A =?= undefined", "A =!= undefined",
	// mixed nesting
	"(A + B) * 2 > A", "(A > B) == (B < A)", "A > 4 && (B == 3 || B == 4)",
	// constants
	"true", "false", "1 > 0", "A > 4 && true",
}

// TestVecMatchesScalarPerRecord is the executor's core test: same Program, per-record equality.
func TestVecMatchesScalarPerRecord(t *testing.T) {
	src := vecCorpus()
	n := len(src.recs)
	var scratch VecScratch
	declined := 0
	for _, expr := range vecExprs {
		q, err := Parse(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		got, ok := q.VecEval(src, n, &scratch)
		if !ok {
			declined++
			t.Logf("declined: %s", expr)
			continue
		}
		for i := 0; i < n; i++ {
			want := q.Eval(src.ad(i))
			if !sameValue(got.value(i), want) {
				t.Errorf("%q record %d: vector %s (state %d) != scalar %s",
					expr, i, got.value(i), got.St[i], want)
			}
		}
	}
	if declined > 3 {
		t.Errorf("%d/%d expressions declined; the corpus is meant to be mostly servable",
			declined, len(vecExprs))
	}
	t.Logf("%d expressions x %d records verified against the scalar interpreter (%d declined)",
		len(vecExprs)-declined, n, declined)
}

// TestVecDeclinesUnsupported pins what the executor refuses, so a future change that starts silently
// serving one of these has to update the test and say so.
func TestVecDeclinesUnsupported(t *testing.T) {
	src := newTestSource([]map[string]string{
		{"A": "1", "S": `"x"`},
	})
	for _, expr := range []string{
		`S == "x"`,        // string column
		`A == "x"`,        // string constant
		`A ?: 5`,          // Elvis: a per-record select, no combine opcode
		`size({1,2,3})`,   // delegated subtree (OpEvalNode)
		`strcat("a","b")`, // function returning a string
	} {
		q, err := Parse(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		if _, ok := q.VecEval(src, 1, nil); ok {
			t.Errorf("%q: expected the vector executor to decline", expr)
		}
	}
}

// TestVecShortCircuitNoOpIsSafe is the specific claim that lets && vectorize: OpShortAnd is skipped, so
// the right operand is ALWAYS evaluated, and the answer must still match. `false && (1/0)` is the case
// that would break if the combine got the truth table wrong -- short-circuit would never have computed
// the ERROR, and the vector executor always does.
func TestVecShortCircuitNoOpIsSafe(t *testing.T) {
	src := newTestSource([]map[string]string{
		{"A": "0"},  // A > 5 is FALSE
		{"A": "10"}, // A > 5 is TRUE
		{},          // A missing: A > 5 is UNDEFINED
	})
	for _, expr := range []string{
		"A > 5 && 1 / 0 > 0", // FALSE && ERROR
		"A > 5 || 1 / 0 > 0", // TRUE || ERROR
		"A > 5 && B > 1",     // ... && UNDEFINED
		"A > 5 || B > 1",     // ... || UNDEFINED
	} {
		q, err := Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := q.VecEval(src, len(src.recs), nil)
		if !ok {
			t.Fatalf("%q declined; this shape must vectorize", expr)
		}
		for i := range src.recs {
			want := q.Eval(src.ad(i))
			if !sameValue(got.value(i), want) {
				t.Errorf("%q record %d: vector %s != scalar %s", expr, i, got.value(i), want)
			}
		}
	}
}

// TestVecScratchReuse checks the stack is actually reused: a second batch through the same scratch must
// not allocate, or a per-block scan pays for its stack per block.
func TestVecScratchReuse(t *testing.T) {
	src := vecCorpus()
	q, err := Parse("A > 4 && B < 4 && A + B > 2")
	if err != nil {
		t.Fatal(err)
	}
	var scratch VecScratch
	if _, ok := q.VecEval(src, len(src.recs), &scratch); !ok {
		t.Fatal("declined")
	}
	before := testing.AllocsPerRun(20, func() { q.VecEval(src, len(src.recs), &scratch) })
	if before > 0 {
		t.Errorf("steady-state VecEval allocates %.1f times per call; the scratch should absorb it", before)
	}
}

// TestVecStackDepth guards the no-op short-circuit's stack discipline. Skipping OpShortAnd relies on it
// PEEKING rather than popping; if that ever changes, the stack would be unbalanced and this catches it
// as a decline rather than as a wrong answer.
func TestVecStackDepth(t *testing.T) {
	src := vecCorpus()
	for _, expr := range []string{
		"A > 1 && B > 1", "A > 1 || B > 1",
		"A > 1 && B > 1 && A > 2 && B > 2",
		"(A > 1 || B > 1) && (A > 2 || B > 2)",
		"A > 1 && (B > 1 || (A > 2 && B > 2))",
	} {
		q, err := Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		var s VecScratch
		if _, ok := q.VecEval(src, len(src.recs), &s); !ok {
			t.Fatalf("%q declined", expr)
		}
		if s.depth != 1 {
			t.Errorf("%q left stack depth %d, want 1", expr, s.depth)
		}
	}
}

func TestVecBatchInvariantToSize(t *testing.T) {
	// The same records split into different batch sizes must give the same answers, or a scan that
	// batches per block gets a different result than one that batches per segment.
	big := vecCorpus()
	q, err := Parse("A > 4 && B < 4")
	if err != nil {
		t.Fatal(err)
	}
	whole, ok := q.VecEval(big, len(big.recs), nil)
	if !ok {
		t.Fatal("declined")
	}
	var want []string
	for i := range big.recs {
		want = append(want, fmt.Sprint(whole.value(i)))
	}
	for _, batch := range []int{1, 2, 3, 5, 7} {
		var got []string
		for start := 0; start < len(big.recs); start += batch {
			end := start + batch
			if end > len(big.recs) {
				end = len(big.recs)
			}
			sub := newTestSource(big.recs[start:end])
			v, ok := q.VecEval(sub, end-start, nil)
			if !ok {
				t.Fatalf("batch %d declined", batch)
			}
			for i := 0; i < end-start; i++ {
				got = append(got, fmt.Sprint(v.value(i)))
			}
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("batch=%d record %d: %s != %s", batch, i, got[i], want[i])
			}
		}
	}
}
