package vm

import (
	"fmt"
	"math"
	"strings"
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
	recs  []map[string]string
	vals  []map[string]classad.Value
	names map[string]string // any spelling of an attribute name -> the spelling vals is keyed by
}

func newTestSource(recs []map[string]string) *testSource {
	s := &testSource{recs: recs, vals: make([]map[string]classad.Value, len(recs)),
		names: map[string]string{}}
	for i, r := range recs {
		s.vals[i] = make(map[string]classad.Value, len(r))
		for k, src := range r {
			q, err := Parse(src)
			if err != nil {
				panic(err)
			}
			// Stored under BOTH the given spelling and the folded one, because ClassAd attribute
			// names are case-insensitive and a real source resolves them through an intern table,
			// which folds. A case-sensitive lookup here made `a != 0` serve all-undefined against a
			// record carrying `A`, where the scalar interpreter resolved it -- a harness that
			// disagreed with the product, found by FuzzVecParity within two seconds.
			//
			// Both spellings rather than only the folded one so the common lookup is an exact hit:
			// strings.ToLower allocates for a name that is not already lower-case, and
			// TestVecScratchReuse requires a steady-state LoadColumn to allocate nothing at all.
			s.vals[i][k] = q.Eval(classad.New())
			// Both spellings resolve to the stored one. A real source resolves a name through its
			// intern table, which folds; doing it with a table rather than by folding at lookup time
			// is what keeps LoadColumn allocation-free, since strings.ToLower allocates for any name
			// that is not already lower-case and TestVecScratchReuse requires zero.
			s.names[k] = k
			s.names[strings.ToLower(k)] = k
		}
	}
	return s
}

func (s *testSource) LoadColumn(name string, scope ast.AttributeScope, dst *Vec) bool {
	if scope != ast.NoScope && scope != ast.MyScope {
		return false
	}
	// Resolved once, outside the record loop. An attribute no record carries is not in the table, and
	// folding it is the only case that allocates -- it then misses every record and is served as
	// undefined, which is what a source that knows the attribute is absent everywhere should do.
	key, known := s.names[name]
	if !known {
		key = strings.ToLower(name)
	}
	for i := range s.recs {
		v, ok := s.vals[i][key]
		if !ok {
			dst.St[i] = VsUndef
			continue
		}
		if !dst.setValueData(i, v) {
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
	case classad.StringValue:
		// BYTE equality, deliberately, even though every comparison OPERATOR folds case. The question
		// here is whether the executor produced the same value the reference produced, and "user3" is
		// not the same value as "USER3" -- a kernel that resolved a dictionary code to the wrong entry
		// of a fold-equal run would otherwise pass.
		x, _ := a.StringValue()
		y, _ := b.StringValue()
		return x == y
	}
	return true // undefined / error carry no payload
}

// vecCorpus is a record set built to exercise the states, not just the happy path: integers, reals,
// negatives, zero (for division), a missing attribute, an explicit UNDEFINED, a string in a numeric
// position, and a boolean.
func vecCorpus() *testSource {
	return newTestSource([]map[string]string{
		{"A": "10", "B": "3", "F": "true", "S": `"user3"`},
		{"A": "0", "B": "0", "F": "false", "S": `"USER3"`}, // same string, different case
		{"A": "2.5", "B": "2", "F": "true", "S": `"user10"`},
		{"A": "-7", "B": "0.5", "F": "false", "S": `""`}, // empty string
		{"B": "4", "F": "true"},                          // A and S missing
		{"A": "5"},                                       // B and F missing
		{"A": "undefined", "B": "1"},
		{"A": "9223372036854775807", "B": "2"}, // MaxInt64: + and * must overflow-check
		{"A": "-9223372036854775808", "B": "-1"},
		{"A": "error", "B": "1"},
		{"A": "1152921504606846976", "B": "1152921504606846976"}, // 2^60: beyond float64 exactness
		{"A": "1.5", "B": "0"},                                   // real division by zero
		{"A": "0.0", "B": "-0.0"},                                // identity on signed zero: same value, different bits
		{"A": "-0.0", "B": "0"},                                  // ... and a real against an integer zero
		{"A": "true", "B": "1"},                                  // a boolean against the integer it is not
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
	// identity: total, type-sensitive, case-sensitive, and spelled both ways
	"A =?= B", "A =!= B", "A =?= undefined", "A =!= undefined",
	"A is B", "A isnt B", "A is undefined", "A isnt error",
	"A =?= 1", "A =?= 1.0", "A =?= true", "A =?= 0", "A =?= -0.0",
	"F =?= true", "F =?= 1", "F =!= true",
	`S =?= S`, "A =?= A", "B =?= B", "A =?= error",
	`A =?= "10"`, `S =?= 10`,
	// mixed nesting
	"(A + B) * 2 > A", "(A > B) == (B < A)", "A > 4 && (B == 3 || B == 4)",
	// constants
	"true", "false", "1 > 0", "A > 4 && true",
	// strings: every comparison operator folds case, the identity operators do not
	`S == "user3"`, `S == "USER3"`, `S != "user3"`, `S < "user5"`, `S > "user5"`,
	`S <= "user3"`, `S >= "USER3"`,
	`S =?= "user3"`, `S =!= "user3"`, `S =?= "USER3"`, `S =!= "USER3"`,
	`S == ""`, `S =?= undefined`,
	`S == "user3" || S == "user10"`, `S == "user3" && A > 4`,
	`S == A`,    // string against a number: the hook's problem
	`S < A`,     // likewise, ordered
	`S && true`, // a string as a logical operand
	// string-VALUED results, so the payload itself is compared and not just the type
	`S`, `S + 1`, `-S`,
	// strict built-in calls, applied elementwise (see callFuncVec)
	"int(A)", "real(A)", "floor(A)", "ceiling(A)", "round(A)",
	"pow(B, 2)", "quantize(A, 4)", "interval(A)", "string(A)", "bool(F)",
	"isUndefined(A)", "isError(A)", "isInteger(A)", "isReal(A)", "isBoolean(F)",
	`isString(S)`, `toUpper(S)`, `toLower(S)`, `size(S)`,
	`strcmp(S, "user3")`, `stricmp(S, "USER3")`, `substr(S, 0, 4)`,
	`regexp("^user", S)`,
	// calls combined with the native operators, and nested in each other
	"int(A) > 4", "int(floor(A)) + 1", "-int(A)", "isUndefined(A) || A > 4",
	"isUndefined(A) && F", `toUpper(S) == "USER3"`, `substr(S, 0, 4) == "user"`,
	`toUpper(toLower(S))`,
	// MASK-form arguments: a comparison or logical result carries its value in the bitplanes, not in
	// the data arrays, so a call on one is only correct if it converts first (callFuncVec's toData).
	// Without these the conversion can be deleted and every other expression here still passes.
	"isBoolean(A > 4)", "isUndefined(A > 4)", "isInteger(A > 4)", "int(A > 4)",
	"string(A > 4)", "bool(A > 4)", "isBoolean(A > 4 && B < 4)", "isBoolean(!F)",
	"int(A > 4) + 1", "string(A > 4) == \"true\"",
	// ELVIS, now a blend. A is undefined in some records and absent in others, so the fallback path
	// is exercised rather than merely present.
	"A ?: 5", "A ?: B", "A ?: 0", `S ?: "x"`, `A ?: S`, "F ?: true",
	"(A ?: 1) + (B ?: 2)", "(A ?: 1) > 4",
	// Right-nested Elvis: both operators converge on ONE instruction, so the join order matters.
	"A ?: (B ?: 9)", "A ?: (B ?: (A ?: 11))",
	// CONDITIONALS, also a blend. Every condition state: true/false, a number, undefined, a string
	// (error), and a mask-form condition.
	"ifThenElse(A > 4, 1, 2)", "ifThenElse(F, A, B)", "ifThenElse(A, 1, 2)",
	"ifThenElse(S, 1, 2)", "ifThenElse(A, A, B)", "ifThenElse(B, 1, 2)",
	`ifThenElse(A > 4, S, "none")`, "ifThenElse(A > 4, 1, 2) + 1",
	"ifThenElse(A > 4, A + B, A - B)", "-ifThenElse(F, 1, 2)",
	"ifThenElse(A > 4, 1, 2) == 1", "ifThenElse(F, 1, 2) && A > 0",
	// Nested conditionals, and the two constructs sharing a join.
	"ifThenElse(A > 4, ifThenElse(F, 1, 2), 3)",
	"ifThenElse(F, ifThenElse(A > 4, 1, 2), ifThenElse(B > 2, 3, 4))",
	"A ?: ifThenElse(F, 1, 2)", "ifThenElse(F, A ?: 1, B ?: 2)",
	"ifThenElse(A > 4, 1, 2) ?: 99", "(A ?: 1) ?: 2",
	// stringListMember over a literal list, against a string column.
	`stringListMember(S, "user3,user10")`, `stringListMember(S, "user10")`,
	`stringListMember(S, "")`, `stringListMember(S, "user3;user10", ";")`,
	`stringListMember(S, " user3 , user10 ")`, `stringListMember(S, "user3,user10") && A > 4`,
	`stringListMember(A, "user3")`, // a numeric item: ERROR
	// A CONSTANT item: the whole batch has one answer, so the kernel computes it once instead of
	// per record. Without these the const path is never entered -- every other case here reads a
	// column -- and could be deleted with the suite still green.
	`stringListMember("user3", "user3,user10")`, `stringListMember("nope", "user3,user10")`,
	`stringListMember("user3", "user3,user10") && A > 4`,
	`stringListMember(undefined, "user3")`, // const UNDEFINED item: the empty string, so no match
	// A MASK-form item. The states a mask can hold are true/false/undefined/error, and the undefined
	// one is what distinguishes a correct read from a stale one: an undefined item is the EMPTY
	// STRING and so matches nothing (false), while reading the data arrays of a mask-form vector
	// finds whatever was last there and reports ERROR.
	`stringListMember(A > 4, "true,false")`, `stringListMember(A > 4, "")`,
	`stringListMember(F && A > 4, "true")`,
	// REAL OVERFLOW, which is asymmetric in the reference: a result of +Inf is ERROR, while -Inf and
	// NaN are ordinary real values. The fast arithmetic loop has to agree with that, and pinning both
	// directions is what keeps a fix for one from breaking the other.
	"A + 1.7977e308 + 1.7977e308",   // +Inf: must be ERROR
	"1.7977e308 * 1.7977e308",       // likewise
	"A * 1.7977e308 * 1.7977e308",   //
	"0.0 - 1.7977e308 - 1.7977e308", // -Inf: must stay a real
	"A - 1.7977e308 - 1.7977e308",   //
	"-(1.7977e308 + 1.7977e308)",    // ERROR negated, not -Inf
	"(A + 1.7977e308 + 1.7977e308) > 0",
	// every argument constant: computed once, not per record
	"pow(2, 10)", "floor(3.7)", "quantize(7, 4)",
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
	if declined > 0 {
		t.Errorf("%d/%d expressions declined; the corpus is meant to be mostly servable",
			declined, len(vecExprs))
	}
	t.Logf("%d expressions x %d records verified against the scalar interpreter (%d declined)",
		len(vecExprs)-declined, n, declined)
}

// TestVecDeclinesUnsupported pins what the executor refuses, so a future change that starts silently
// serving one of these has to update the test and say so.
//
// Elvis and conditionals used to be here. They are served now, as blends, and live in vecExprs where
// every record is checked against the scalar interpreter. What is left is refused for one of three
// durable reasons: the program is not native at all (a delegated subtree), the RESULT is a value a
// Vec cannot hold (a list or a nested ad), or the built-in is nondeterministic.
func TestVecDeclinesUnsupported(t *testing.T) {
	src := newTestSource([]map[string]string{
		{"A": "1", "S": `"x"`},
	})
	for _, expr := range []string{
		`size({1,2,3})`,    // a strict call whose ARGUMENT is a delegated subtree
		`strcat("a","b")`,  // a lazy built-in: delegated, so OpEvalNode
		`eval("A + 1")`,    // resolves in the scope, which a column source is not
		`unparse(A)`,       // needs its argument's syntax, so it can never be a value operation
		`split(S)`,         // strict, but returns a LIST, which a vector cannot hold
		`splitUserName(S)`, // likewise
		`random(A)`,        // nondeterministic: never compiled natively
		`{1, 2, A}`,        // a list literal is delegated, and a vector cannot hold a list anyway
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

// TestVecIdentityOpStrings pins the operator strings the identity kernel is keyed on against what the
// COMPILER actually emits.
//
// `=?=` and `is` are one operator and the parser normalizes both to "is", so a kernel written for the
// source spelling is unreachable -- which is what had happened: every identity comparison went to the
// hook, and identicalVec, dictionary-code handling and all, had never run. A parity test cannot see
// that (the hook is correct, just slower), so the reachability is asserted directly.
func TestVecIdentityOpStrings(t *testing.T) {
	for _, expr := range []string{"A =?= B", "A is B", "A =!= B", "A isnt B"} {
		q, err := Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		if len(q.prog.ops) != 1 {
			t.Fatalf("%q compiled to ops %q, want exactly one", expr, q.prog.ops)
		}
		if op := q.prog.ops[0]; op != opIs && op != opIsnt {
			t.Errorf("%q compiles to operator %q, which binopVec routes to the hook rather than to "+
				"identicalVec", expr, op)
		}
	}
}

// identityStates is one record per VALUE, so a pair of them is one cell of the identity truth table.
var identityStates = []string{
	"0", "1", "-1", "0.0", "-0.0", "1.0", "true", "false", `"a"`, `"A"`, `""`, "undefined", "error",
}

// TestVecIdentityExhaustive runs every ordered pair of those values through `is` and `isnt`, against the
// scalar interpreter.
//
// Exhaustive rather than sampled, in the style of the short-circuit test, because identity's rules are
// about the pairs a corpus does not naturally contain: a real against the integer that equals it, two
// strings differing only in case, signed zero, and UNDEFINED against UNDEFINED -- where identity is TRUE
// while every other operator is UNDEFINED.
func TestVecIdentityExhaustive(t *testing.T) {
	var recs []map[string]string
	for _, l := range identityStates {
		for _, r := range identityStates {
			recs = append(recs, map[string]string{"L": l, "R": r})
		}
	}
	src := newTestSource(recs)
	exprs := []string{"L is R", "L isnt R", "L =?= R", "L =!= R"}
	// ... and against a LITERAL on each side, which is a separate kernel: the literal is read once
	// instead of being spread across the batch, so it has its own type and payload comparison to get
	// wrong, and `undefined is undefined` is true while every ordering operator on it is undefined.
	for _, lit := range identityStates {
		exprs = append(exprs, "L is "+lit, "L isnt "+lit, lit+" is L", lit+" isnt L")
	}
	for _, expr := range exprs {
		q, err := Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := q.VecEval(src, len(recs), nil)
		if !ok {
			t.Fatalf("%q declined", expr)
		}
		for i := range recs {
			if want := q.Eval(src.ad(i)); !sameValue(got.value(i), want) {
				t.Errorf("%q with L=%s R=%s: vector %s != scalar %s",
					expr, recs[i]["L"], recs[i]["R"], got.value(i), want)
			}
		}
	}
}

// FuzzVecParity is the differential gate on the VECTOR executor, where the corpus test is the
// hand-written one. The oracle is the scalar interpreter over the same Program and the same records,
// which is itself fuzzed against the tree-walking evaluator (FuzzDifferential).
//
// The contract it enforces is one-sided, and that is the point: DECLINING is always acceptable, so a
// mutation that makes the executor refuse more is not a failure. Serving a record with a value the
// scalar interpreter does not agree with is the only failure, because a decline falls back and a
// wrong answer does not.
func FuzzVecParity(f *testing.F) {
	for _, es := range vecExprs {
		f.Add(es)
	}
	f.Fuzz(func(t *testing.T, exprSrc string) {
		q, err := Parse(exprSrc)
		if err != nil || q == nil {
			return
		}
		// Built per iteration rather than shared: a fuzz target runs concurrently, and a source
		// shared across workers would make a failure depend on which worker got there first.
		src := vecCorpus()
		n := len(src.recs)
		var scratch VecScratch
		got, ok := q.VecEval(src, n, &scratch)
		if !ok {
			return
		}
		for i := 0; i < n; i++ {
			want := q.Eval(src.ad(i))
			if !sameValue(got.value(i), want) {
				t.Fatalf("expr=%q record %d: vector %s (state %d) != scalar %s",
					exprSrc, i, got.value(i), got.St[i], want)
			}
		}
	})
}
