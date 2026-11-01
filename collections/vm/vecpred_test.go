package vm

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

// PREDICATION, held to the same law as everything else here: the same compiled script, run over a
// batch and run per record, must agree value for value.
//
// vecCorpus is the right fixture for this and not merely a convenient one. Its A column is a missing
// attribute in one record, an explicit UNDEFINED in another, an ERROR in a third and a boolean in a
// fourth, so a condition written over A is neither true nor false for several records -- which is the
// case predication has to get right and the case a happy-path corpus would never produce.

func mustExpr(t *testing.T, src string) ast.Expr {
	t.Helper()
	e, err := parser.ParseExpr(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return e
}

// predShape is one script built through the API, since If takes blocks rather than text.
type predShape struct {
	name  string
	build func(t *testing.T, s *Script) *Script
}

func predShapes() []predShape {
	let := func(name, src string) func(*testing.T, *Script) {
		return func(t *testing.T, s *Script) { s.Let(name, mustExpr(t, src)) }
	}
	block := func(f func(*testing.T, *Script), t *testing.T) func(*Script) {
		if f == nil {
			return nil
		}
		return func(s *Script) { f(t, s) }
	}
	return []predShape{
		// Both arms bind the same local, so the result is whichever arm that record took.
		{"both arms", func(t *testing.T, s *Script) *Script {
			return s.If(mustExpr(t, "A > 4"),
				block(let("x", "1"), t), block(let("x", "2"), t)).
				Result(mustExpr(t, "x"))
		}},
		// Only a THEN arm, so the else path leaves x unbound -- UNDEFINED -- which is the shape the
		// compiler cannot otherwise produce and the one the masked store exists for.
		{"then only", func(t *testing.T, s *Script) *Script {
			return s.If(mustExpr(t, "A > 4"), block(let("x", "10"), t), nil).
				Result(mustExpr(t, "x"))
		}},
		// Only an ELSE arm.
		{"else only", func(t *testing.T, s *Script) *Script {
			return s.If(mustExpr(t, "A > 4"), nil, block(let("x", "20"), t)).
				Result(mustExpr(t, "x"))
		}},
		// Bound BEFORE the If and conditionally rebound: the lanes the store does not reach must keep
		// the earlier value, which is exactly what an unmasked store would destroy.
		{"conditional rebind", func(t *testing.T, s *Script) *Script {
			s.Let("x", mustExpr(t, "A"))
			return s.If(mustExpr(t, "A > 4"), block(let("x", "x + 100"), t), nil).
				Result(mustExpr(t, "x"))
		}},
		// The arm's value depends on the record, so a store cannot be folded to a constant.
		{"per-record arms", func(t *testing.T, s *Script) *Script {
			return s.If(mustExpr(t, "A > B"),
				block(let("x", "A - B"), t), block(let("x", "B - A"), t)).
				Result(mustExpr(t, "x"))
		}},
		// A STRING in a local, stored under a predicate: the destination is forced to data form and
		// the payload has to survive it.
		{"string arms", func(t *testing.T, s *Script) *Script {
			return s.If(mustExpr(t, "A > 4"),
				block(let("x", "S"), t), block(let("x", `"other"`), t)).
				Result(mustExpr(t, "x"))
		}},
		// NESTED, so the inner region's masks are built from the outer's.
		{"nested", func(t *testing.T, s *Script) *Script {
			return s.If(mustExpr(t, "A > 4"), func(in *Script) {
				in.If(mustExpr(t, "B > 2"),
					block(let("x", "1"), t), block(let("x", "2"), t))
			}, block(let("x", "3"), t)).
				Result(mustExpr(t, "x"))
		}},
		// A nested condition that FAULTS inside an arm. This is the case the file comment in
		// vecpred.go is about: a record faulting here must not come back alive at the OUTER join.
		{"nested fault", func(t *testing.T, s *Script) *Script {
			s.Let("x", mustExpr(t, "0"))
			return s.If(mustExpr(t, "B > 0"), func(in *Script) {
				in.If(mustExpr(t, "A > 4"), block(let("x", "1"), t), block(let("x", "2"), t))
			}, block(let("x", "3"), t)).
				Result(mustExpr(t, "x"))
		}},
		// Two Ifs in sequence, so the live mask has to be restored between them and not merely
		// narrowed once.
		{"sequential", func(t *testing.T, s *Script) *Script {
			s.Let("x", mustExpr(t, "0"))
			s.If(mustExpr(t, "A > 4"), block(let("x", "x + 1"), t), nil)
			return s.If(mustExpr(t, "B > 2"), block(let("x", "x + 10"), t), nil).
				Result(mustExpr(t, "x"))
		}},
		// An If that binds nothing: it still splits and rejoins, and still faults.
		{"empty arms", func(t *testing.T, s *Script) *Script {
			s.Let("x", mustExpr(t, "A"))
			return s.If(mustExpr(t, "A > 4"), nil, nil).Result(mustExpr(t, "x"))
		}},
		// The result expression reads a local bound in both arms AND an attribute, so the join's live
		// mask has to be whole again.
		{"join is whole", func(t *testing.T, s *Script) *Script {
			return s.If(mustExpr(t, "A > 4"),
				block(let("x", "1"), t), block(let("x", "2"), t)).
				Result(mustExpr(t, "x + B"))
		}},
		// A condition that is a STRING for some records, which is neither true nor false and so faults
		// rather than choosing an arm.
		{"string condition", func(t *testing.T, s *Script) *Script {
			return s.If(mustExpr(t, "S"), block(let("x", "1"), t), block(let("x", "2"), t)).
				Result(mustExpr(t, "x"))
		}},
	}
}

// TestVecPredicateMatchesScalarPerRecord is the law.
func TestVecPredicateMatchesScalarPerRecord(t *testing.T) {
	src := vecCorpus()
	n := len(src.recs)
	var scratch VecScratch
	for _, sh := range predShapes() {
		sp, err := sh.build(t, NewScript()).Compile()
		if err != nil {
			t.Fatalf("%s: compile: %v", sh.name, err)
		}
		got, ok := sp.VecEval(src, n, &scratch)
		if !ok {
			t.Errorf("%s declined; an If is meant to be servable", sh.name)
			continue
		}
		for i := 0; i < n; i++ {
			want := sp.Run(src.ad(i))
			if !sameValue(got.value(i), want) {
				t.Errorf("%s record %d: vector %s (state %d) != scalar %s",
					sh.name, i, got.value(i), got.St[i], want)
			}
		}
	}
	t.Logf("%d predicated scripts x %d records verified against the register executor",
		len(predShapes()), n)
}

// TestVecPredicateFaultsAreReached is the guard against a vacuous law. Every shape above would agree
// with the scalar executor if no record ever faulted, and then nothing here would be testing the
// fault plane at all. This pins that the corpus actually produces faults, and that they are the
// records whose condition is neither true nor false.
func TestVecPredicateFaultsAreReached(t *testing.T) {
	src := vecCorpus()
	n := len(src.recs)
	sp, err := NewScript().
		If(mustExpr(t, "A > 4"), func(s *Script) { s.Let("x", mustExpr(t, "1")) },
			func(s *Script) { s.Let("x", mustExpr(t, "2")) }).
		Result(mustExpr(t, "x")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := sp.VecEval(src, n, nil)
	if !ok {
		t.Fatal("declined")
	}
	faults := 0
	for i := 0; i < n; i++ {
		if got.value(i).IsError() {
			faults++
		}
	}
	if faults == 0 {
		t.Fatal("no record faulted; the fault plane is untested by every other test here")
	}
	t.Logf("%d of %d records faulted on `A > 4`", faults, n)
}

// TestVecPredicateStoreIsMasked pins the primitive directly, because the parity law alone could be
// satisfied by a store that overwrote everything if the oracle happened to agree.
//
// `x = A` then `if A > 4 { x = 999 }`: the records that did NOT take the branch must still hold A,
// and the ones that did must hold 999. An unmasked store gives 999 everywhere.
func TestVecPredicateStoreIsMasked(t *testing.T) {
	src := vecCorpus()
	n := len(src.recs)
	sp, err := NewScript().
		Let("x", mustExpr(t, "A")).
		If(mustExpr(t, "A > 4"), func(s *Script) { s.Let("x", mustExpr(t, "999")) }, nil).
		Result(mustExpr(t, "x")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := sp.VecEval(src, n, nil)
	if !ok {
		t.Fatal("declined")
	}
	var kept, written int
	for i := 0; i < n; i++ {
		want := sp.Run(src.ad(i))
		if !sameValue(got.value(i), want) {
			t.Errorf("record %d: vector %s != scalar %s", i, got.value(i), want)
		}
		if x, err := got.value(i).IntValue(); err == nil && x == 999 {
			written++
		} else if !got.value(i).IsError() {
			kept++
		}
	}
	if written == 0 || kept == 0 {
		t.Errorf("%d records written and %d kept; the test needs both to say anything", written, kept)
	}
}

// TestVecPredicateScratchSurvivesADecline is the test for the ownership rule in resetPredicates.
//
// A While declines PART-WAY THROUGH: the executor serves its branch, enters the then arm, and only
// then meets the back edge. So the scratch is left with an open region whose arm masks are in flight
// and whose live mask aliases one of them. If reset released an aliased mask twice, the pool would
// hand the same backing array to two masks that are supposed to be different -- and the next script
// through that scratch would get a wrong answer rather than a crash.
//
// So: decline, then run a correct script through the same scratch and demand the same answer a fresh
// scratch gives.
func TestVecPredicateScratchSurvivesADecline(t *testing.T) {
	src := vecCorpus()
	n := len(src.recs)
	var scratch VecScratch

	loop, err := NewScript().
		While(mustExpr(t, "A > 4"), func(s *Script) { s.Let("x", mustExpr(t, "A - 1")) }).
		Result(mustExpr(t, "x")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loop.VecEval(src, n, &scratch); ok {
		t.Fatal("the loop was served; this test needs it to decline part-way through")
	}

	// Nested, so the reused scratch has to produce several masks that are genuinely distinct.
	sp, err := NewScript().
		Let("x", mustExpr(t, "0")).
		If(mustExpr(t, "B > 0"), func(in *Script) {
			in.If(mustExpr(t, "A > 4"),
				func(s *Script) { s.Let("x", mustExpr(t, "1")) },
				func(s *Script) { s.Let("x", mustExpr(t, "2")) })
		}, func(s *Script) { s.Let("x", mustExpr(t, "3")) }).
		Result(mustExpr(t, "x")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	reused, ok := sp.VecEval(src, n, &scratch)
	if !ok {
		t.Fatal("the nested script declined through the reused scratch")
	}
	fresh, ok := sp.VecEval(src, n, nil)
	if !ok {
		t.Fatal("the nested script declined through a fresh scratch")
	}
	for i := 0; i < n; i++ {
		if !sameValue(reused.value(i), fresh.value(i)) {
			t.Errorf("record %d: reused scratch gave %s, fresh scratch gave %s",
				i, reused.value(i), fresh.value(i))
		}
		if want := sp.Run(src.ad(i)); !sameValue(reused.value(i), want) {
			t.Errorf("record %d: reused scratch gave %s, scalar gave %s", i, reused.value(i), want)
		}
	}
}

// TestVecPredicateFaultsDoNotRevive pins the rule the vecpred.go file comment is about, directly
// rather than through a corpus.
//
// A record whose INNER condition faults must stay dead at the OUTER join. The outer region computed
// its arm masks before the inner fault happened, so rebuilding the live mask from them -- which is
// what the join does -- would bring the record back unless the fault was cleared from them too.
//
// Stated as "the faulted records are exactly the ones the scalar executor faults", because that is
// the claim; counting them would pass with the wrong records in the set.
func TestVecPredicateFaultsDoNotRevive(t *testing.T) {
	src := vecCorpus()
	n := len(src.recs)
	// x is bound before the If and read after it, so a revived record would show its PRE-If value
	// rather than ERROR -- a plausible-looking answer, which is why this is worth pinning.
	sp, err := NewScript().
		Let("x", mustExpr(t, "-1")).
		If(mustExpr(t, "B > 0"), func(in *Script) {
			in.If(mustExpr(t, "A > 4"),
				func(s *Script) { s.Let("x", mustExpr(t, "1")) },
				func(s *Script) { s.Let("x", mustExpr(t, "2")) })
		}, nil).
		Result(mustExpr(t, "x + 0")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := sp.VecEval(src, n, nil)
	if !ok {
		t.Fatal("declined")
	}
	var vecErr, scalarErr []int
	for i := 0; i < n; i++ {
		if got.value(i).IsError() {
			vecErr = append(vecErr, i)
		}
		if sp.Run(src.ad(i)).IsError() {
			scalarErr = append(scalarErr, i)
		}
	}
	if !sameInts(vecErr, scalarErr) {
		t.Errorf("faulted records: vector %v, scalar %v", vecErr, scalarErr)
	}
	if len(scalarErr) == 0 {
		t.Fatal("no record faulted; this test is not testing what it says")
	}
	// And a record that faulted INSIDE the inner region is among them, which is the revival case. The
	// inner condition is only reached where the outer one was true.
	inner := 0
	for _, i := range scalarErr {
		ad := src.ad(i)
		if q, err := Parse("B > 0"); err == nil && q.Eval(ad).String() == "true" {
			inner++
		}
	}
	if inner == 0 {
		t.Fatal("every fault came from the OUTER condition; the revival case is untested")
	}
	t.Logf("%d faults, %d of them from the inner condition", len(scalarErr), inner)
}

// TestVecPredicateLiveMaskIsRestored pins that the live mask after an If is the UNION of its arms and
// not either one of them.
//
// It needs its own fixture, because vecCorpus cannot see this: no record of it takes the else path of
// `A > 4` and the then path of `B > 2`, so a join that kept only the then mask agrees with the scalar
// executor on every record of it. The records here are chosen to cover all four combinations, which
// is the only thing that makes a second If depend on the first join.
func TestVecPredicateLiveMaskIsRestored(t *testing.T) {
	src := newTestSource([]map[string]string{
		{"A": "10", "B": "9"}, // then, then
		{"A": "10", "B": "0"}, // then, else
		{"A": "0", "B": "9"},  // else, THEN -- the combination vecCorpus lacks
		{"A": "0", "B": "0"},  // else, else
	})
	n := len(src.recs)
	sp, err := NewScript().
		Let("x", mustExpr(t, "0")).
		If(mustExpr(t, "A > 4"), func(s *Script) { s.Let("x", mustExpr(t, "x + 1")) }, nil).
		If(mustExpr(t, "B > 2"), func(s *Script) { s.Let("x", mustExpr(t, "x + 10")) }, nil).
		Result(mustExpr(t, "x")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := sp.VecEval(src, n, nil)
	if !ok {
		t.Fatal("declined")
	}
	// Stated outright as well as differentially: a join that dropped the else arm would give record 2
	// zero instead of ten, and the scalar oracle would catch it -- but only with this fixture, so the
	// expected values are written down to say what the fixture is for.
	for i, want := range []string{"11", "1", "10", "0"} {
		if got.value(i).String() != want {
			t.Errorf("record %d: %s, want %s", i, got.value(i), want)
		}
		if scalar := sp.Run(src.ad(i)); !sameValue(got.value(i), scalar) {
			t.Errorf("record %d: vector %s != scalar %s", i, got.value(i), scalar)
		}
	}
}

// TestVecPredicateOverADictionaryColumn covers the representation hazard a masked store has and an
// ordinary store does not.
//
// An unmasked store takes the whole vector, dictionary and all, so a local can hold dictionary CODES.
// A masked store then has to write a value from somewhere else into some of its lanes -- and a code
// means nothing outside the dictionary that assigned it. So the destination is materialized first.
// Without that the surviving lanes keep codes with no resolver and read as ERROR, which is the same
// failure a blend had.
func TestVecPredicateOverADictionaryColumn(t *testing.T) {
	src := dictCorpus()
	n := len(src.recs)
	for _, tc := range []struct{ name, cond, then string }{
		// The destination holds codes; the arm writes a literal string.
		{"literal into codes", "A > 3", `"replaced"`},
		// The arm writes codes from a DIFFERENT dictionary into a destination holding codes.
		{"other dictionary", "A > 3", "D2"},
		// The arm writes a number, so the destination holds both strings and numbers.
		{"number into codes", "A > 3", "A"},
	} {
		sp, err := NewScript().
			Let("x", mustExpr(t, "D")).
			If(mustExpr(t, tc.cond), func(s *Script) { s.Let("x", mustExpr(t, tc.then)) }, nil).
			Result(mustExpr(t, "x")).Compile()
		if err != nil {
			t.Fatal(err)
		}
		got, ok := sp.VecEval(src, n, nil)
		if !ok {
			t.Errorf("%s declined", tc.name)
			continue
		}
		for i := 0; i < n; i++ {
			want := sp.Run(src.ad(i))
			if !sameValue(got.value(i), want) {
				t.Errorf("%s record %d: vector %s (state %d) != scalar %s",
					tc.name, i, got.value(i), got.St[i], want)
			}
		}
	}
}

// TestVecPredicateAdoptsADictionary is the guard against a vacuous version of the rule above.
// Resolving every incoming code is always CORRECT, so a masked store that never adopted a dictionary
// would pass the test above while materializing a string per record -- the cost the dictionary exists
// to avoid. Here the destination is unbound when the store happens, so it has no dictionary of its
// own and must take the source's.
func TestVecPredicateAdoptsADictionary(t *testing.T) {
	src := dictCorpus()
	n := len(src.recs)
	sp, err := NewScript().
		If(mustExpr(t, "A > 3"), func(s *Script) { s.Let("x", mustExpr(t, "D")) }, nil).
		Result(mustExpr(t, "x")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := sp.VecEval(src, n, nil)
	if !ok {
		t.Fatal("declined")
	}
	for i := 0; i < n; i++ {
		if want := sp.Run(src.ad(i)); !sameValue(got.value(i), want) {
			t.Errorf("record %d: vector %s != scalar %s", i, got.value(i), want)
		}
	}
	if got.Dict == nil {
		t.Fatal("the store did not adopt the source's dictionary; every code is being materialized")
	}
	coded := 0
	for i := 0; i < n; i++ {
		if got.St[i] == VsDictCode {
			coded++
		}
	}
	if coded == 0 {
		t.Error("no element came through as a code")
	}
}

// TestVecPredicateSetupIsOnlyAnOptimization pins that the hasStatements flag cannot cause a wrong
// answer or a panic, only a late setup.
//
// The flag exists to keep the predication planes off the expression path, where they cost about 2% of
// a scan. A future construct that emits a branch and forgets to set it would otherwise meet a nil
// live mask -- an index panic, inside a daemon. So the branch sets the planes up itself if they are
// missing, and this runs a branching program with the flag deliberately cleared.
func TestVecPredicateSetupIsOnlyAnOptimization(t *testing.T) {
	src := vecCorpus()
	n := len(src.recs)
	sp, err := NewScript().
		If(mustExpr(t, "A > 4"), func(s *Script) { s.Let("x", mustExpr(t, "1")) },
			func(s *Script) { s.Let("x", mustExpr(t, "2")) }).
		Result(mustExpr(t, "x")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if !sp.Program().hasStatements {
		t.Fatal("If did not set hasStatements; this test is not testing what it says")
	}
	sp.Program().hasStatements = false
	got, ok := sp.VecEval(src, n, nil)
	if !ok {
		t.Fatal("declined with the flag cleared")
	}
	for i := 0; i < n; i++ {
		if want := sp.Run(src.ad(i)); !sameValue(got.value(i), want) {
			t.Errorf("record %d: vector %s != scalar %s", i, got.value(i), want)
		}
	}
}

// TestVecPredicateScratchMixesScriptsAndExpressions runs an expression, then a branching script, then
// an expression again, through ONE scratch. The predication planes are set up and torn down per
// batch now, so the order in which a scratch sees the two kinds is a real state transition.
func TestVecPredicateScratchMixesScriptsAndExpressions(t *testing.T) {
	src := vecCorpus()
	n := len(src.recs)
	var scratch VecScratch
	q, err := Parse("A > 4")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NewScript().
		Let("x", mustExpr(t, "0")).
		If(mustExpr(t, "A > 4"), func(s *Script) { s.Let("x", mustExpr(t, "1")) }, nil).
		Result(mustExpr(t, "x")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		ev, ok := q.VecEval(src, n, &scratch)
		if !ok {
			t.Fatalf("round %d: the expression declined", round)
		}
		for i := 0; i < n; i++ {
			if want := q.Eval(src.ad(i)); !sameValue(ev.value(i), want) {
				t.Errorf("round %d expression record %d: %s != %s", round, i, ev.value(i), want)
			}
		}
		sv, ok := sp.VecEval(src, n, &scratch)
		if !ok {
			t.Fatalf("round %d: the script declined", round)
		}
		for i := 0; i < n; i++ {
			if want := sp.Run(src.ad(i)); !sameValue(sv.value(i), want) {
				t.Errorf("round %d script record %d: %s != %s", round, i, sv.value(i), want)
			}
		}
	}
}
