package classad

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// A left-deep && / || spine is what "a || b || c || ..." parses into, and it is
// the shape a machine-generated membership constraint has. Evaluating such a
// spine folds it iteratively, so its length costs no recursion depth: the only
// depth a spine charges is what its operands' own subtrees need.
//
// The tests below pin three things: the fold's answer matches the three-valued
// tables term by term, it evaluates exactly the operands the short-circuit rules
// say to evaluate, and the recursion-depth guard still fires on a cycle and on
// genuinely deep nesting.

// chainScope evaluates an expression whose attribute references are answered by
// a term table rather than an ad, recording the name of every reference the
// evaluator resolves. Term i is named T<i>.
type chainScope struct {
	terms   []Value
	visited []int
	boom    string // a reference to this name panics; "" disables it
}

func (c *chainScope) resolve(name string, _ ast.AttributeScope) Value {
	if c.boom != "" && strings.EqualFold(name, c.boom) {
		panic(fmt.Sprintf("evaluated %s, which short-circuiting must skip", name))
	}
	i, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(name), "T"))
	if err != nil || i < 0 || i >= len(c.terms) {
		return NewUndefinedValue()
	}
	c.visited = append(c.visited, i)
	return c.terms[i]
}

func (c *chainScope) eval(t *testing.T, src string) Value {
	t.Helper()
	expr, err := ParseExpr(src)
	if err != nil {
		t.Fatalf("ParseExpr(%.60q): %v", src, err)
	}
	e := NewEvaluator(nil)
	e.SetResolver(c.resolve)
	return evalRecovered(e, expr.expr)
}

// evalRecovered evaluates expr, turning a depth-guard panic into the error
// value every terminal entry point reports it as. A spine too long to evaluate
// reads as an error result here rather than as a crashed test.
func evalRecovered(e *Evaluator, expr ast.Expr) (result Value) {
	defer RecoverCyclic(&result)
	return e.Evaluate(expr)
}

// stateValue builds the Value a term of the given truth state holds.
func stateValue(s logicalState) Value {
	switch s {
	case lsTrue:
		return NewBoolValue(true)
	case lsFalse:
		return NewBoolValue(false)
	case lsUndef:
		return NewUndefinedValue()
	default:
		return NewErrorValue()
	}
}

func stateName(s logicalState) string {
	switch s {
	case lsTrue:
		return "true"
	case lsFalse:
		return "false"
	case lsUndef:
		return "undefined"
	default:
		return "error"
	}
}

// refAnd and refOr are the documented three-valued tables, written out here so
// the oracle does not borrow the implementation it checks.
func refAnd(l, r logicalState) logicalState {
	switch l {
	case lsFalse:
		return lsFalse
	case lsErr:
		return lsErr
	case lsTrue:
		return r
	default: // undefined
		switch r {
		case lsFalse:
			return lsFalse
		case lsErr:
			return lsErr
		default:
			return lsUndef
		}
	}
}

func refOr(l, r logicalState) logicalState {
	switch l {
	case lsTrue:
		return lsTrue
	case lsErr:
		return lsErr
	case lsFalse:
		return r
	default: // undefined
		switch r {
		case lsTrue:
			return lsTrue
		case lsErr:
			return lsErr
		default:
			return lsUndef
		}
	}
}

// refChain folds a chain term by term the way the documented semantics say to,
// returning the result and the indexes of the terms that get evaluated. ops[i-1]
// joins term i-1 to term i.
func refChain(states []logicalState, ops []string) (logicalState, []int) {
	acc := states[0]
	visited := []int{0}
	for i := 1; i < len(states); i++ {
		switch {
		case acc == lsErr:
			// An error left operand is an error without the right operand,
			// and stays one at every node above.
			acc = lsErr
			continue
		case acc == lsFalse && ops[i-1] == "&&":
			continue
		case acc == lsTrue && ops[i-1] == "||":
			continue
		}
		visited = append(visited, i)
		if ops[i-1] == "&&" {
			acc = refAnd(acc, states[i])
		} else {
			acc = refOr(acc, states[i])
		}
	}
	return acc, visited
}

// chainSource renders a left-deep spine of len(states) terms. Explicit
// parentheses force left association for a mixed spine, which && / || precedence
// would otherwise group to the right.
func chainSource(n int, ops []string, parens bool) string {
	src := "T0"
	for i := 1; i < n; i++ {
		if parens && i > 1 {
			src = "(" + src + ")"
		}
		src += " " + ops[i-1] + " T" + strconv.Itoa(i)
	}
	return src
}

func checkChain(t *testing.T, states []logicalState, ops []string, parens bool) {
	t.Helper()
	c := &chainScope{terms: make([]Value, len(states))}
	for i, s := range states {
		c.terms[i] = stateValue(s)
	}
	src := chainSource(len(states), ops, parens)
	got := logicalView(c.eval(t, src))
	want, wantVisited := refChain(states, ops)
	if got != want {
		t.Fatalf("%s = %s, want %s", src, stateName(got), stateName(want))
	}
	if len(c.visited) != len(wantVisited) {
		t.Fatalf("%s evaluated terms %v, want %v", src, c.visited, wantVisited)
	}
	for i := range wantVisited {
		if c.visited[i] != wantVisited[i] {
			t.Fatalf("%s evaluated terms %v, want %v", src, c.visited, wantVisited)
		}
	}
}

// TestShortCircuitSpineMatchesThreeValuedTables generates spines mixing true,
// false, undefined and error terms with mixed operators, and checks both the
// answer and which operands are evaluated against a term-by-term fold of the
// documented semantics.
func TestShortCircuitSpineMatchesThreeValuedTables(t *testing.T) {
	all := []logicalState{lsTrue, lsFalse, lsUndef, lsErr}

	// Every two-term combination, both operators, at both operand positions.
	for _, l := range all {
		for _, r := range all {
			for _, op := range []string{"&&", "||"} {
				checkChain(t, []logicalState{l, r}, []string{op}, false)
			}
		}
	}

	// Every three-term combination, every operator pairing: this puts each
	// state at the head, the middle and the tail of a spine.
	for _, a := range all {
		for _, b := range all {
			for _, cc := range all {
				for _, op1 := range []string{"&&", "||"} {
					for _, op2 := range []string{"&&", "||"} {
						checkChain(t, []logicalState{a, b, cc}, []string{op1, op2}, true)
					}
				}
			}
		}
	}

	// Longer random spines, uniform and mixed.
	rng := rand.New(rand.NewSource(20260308))
	for trial := 0; trial < 400; trial++ {
		n := 2 + rng.Intn(40)
		states := make([]logicalState, n)
		for i := range states {
			states[i] = all[rng.Intn(len(all))]
		}
		ops := make([]string, n-1)
		mixed := rng.Intn(2) == 0
		uniform := "||"
		if rng.Intn(2) == 0 {
			uniform = "&&"
		}
		for i := range ops {
			if mixed && rng.Intn(2) == 0 {
				ops[i] = "&&"
			} else if mixed {
				ops[i] = "||"
			} else {
				ops[i] = uniform
			}
		}
		checkChain(t, states, ops, mixed)
	}
}

// TestShortCircuitSpineSkipsDecidedOperands pins the property short-circuiting
// is load-bearing for: once the accumulator decides a node, that node's right
// operand is not evaluated, however long the spine is. The skipped operands here
// panic if touched.
func TestShortCircuitSpineSkipsDecidedOperands(t *testing.T) {
	const n = 5000
	for _, tc := range []struct {
		name   string
		op     string
		head   Value
		want   bool
		decide int
	}{
		{name: "or-true-head", op: "||", head: NewBoolValue(true), want: true},
		{name: "and-false-head", op: "&&", head: NewBoolValue(false), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &chainScope{terms: make([]Value, n), boom: "Boom"}
			c.terms[0] = tc.head
			src := "T0"
			for i := 1; i < n; i++ {
				src += " " + tc.op + " Boom"
			}
			got := c.eval(t, src)
			b, err := got.BoolValue()
			if err != nil {
				t.Fatalf("got %v, want a boolean", got)
			}
			if b != tc.want {
				t.Fatalf("got %v, want %v", b, tc.want)
			}
		})
	}
}

// TestShortCircuitSpineSkipsAfterError pins the error case: an error
// accumulator is an error at every node above it, and no right operand past it
// is evaluated.
func TestShortCircuitSpineSkipsAfterError(t *testing.T) {
	const n = 3000
	c := &chainScope{terms: []Value{NewErrorValue()}, boom: "Boom"}
	src := "T0"
	for i := 1; i < n; i++ {
		if i%2 == 0 {
			src = "(" + src + ") && Boom"
		} else {
			src = "(" + src + ") || Boom"
		}
	}
	if got := c.eval(t, src); !got.IsError() {
		t.Fatalf("got %v, want error", got)
	}
	// The head term is the only operand the chain evaluates.
	if len(c.visited) != 1 || c.visited[0] != 0 {
		t.Fatalf("evaluated terms %v, want only the head", c.visited)
	}
}

// TestShortCircuitSpineLengthIsNotDepth is the regression test for the cliff: a
// membership constraint answers from its terms, not from how many of them there
// are. maxEvalDepth sits at 2000, and a spine that charged one depth unit per
// term turned to error just below it.
func TestShortCircuitSpineLengthIsNotDepth(t *testing.T) {
	ad, err := Parse(`[X = 1]`)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{2, 100, 1997, 1998, 1999, 2000, 2001, 5000, 50000} {
		src := orChain(n)
		expr, err := ParseExpr(src)
		if err != nil {
			t.Fatalf("ParseExpr(%d terms): %v", n, err)
		}
		got := evalRecovered(NewEvaluator(ad), expr.expr)
		b, err := got.BoolValue()
		if err != nil {
			t.Fatalf("%d terms: got %v, want true", n, got)
		}
		if !b {
			t.Fatalf("%d terms: got false, want true", n)
		}
	}
}

// TestShortCircuitSpineNoMatch is the same chain over an ad no term matches: the
// fold runs every term and answers false.
func TestShortCircuitSpineNoMatch(t *testing.T) {
	ad, err := Parse(`[X = -1]`)
	if err != nil {
		t.Fatal(err)
	}
	expr, err := ParseExpr(orChain(50000))
	if err != nil {
		t.Fatal(err)
	}
	got := evalRecovered(NewEvaluator(ad), expr.expr)
	b, err := got.BoolValue()
	if err != nil {
		t.Fatalf("got %v, want false", got)
	}
	if b {
		t.Fatal("got true, want false")
	}
}

// TestShortCircuitSpineConjunction is the && counterpart: a long conjunction
// every term of which holds.
func TestShortCircuitSpineConjunction(t *testing.T) {
	ad, err := Parse(`[X = 1]`)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	const n = 20000
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString(" && ")
		}
		sb.WriteString("X == 1")
	}
	expr, err := ParseExpr(sb.String())
	if err != nil {
		t.Fatal(err)
	}
	got := evalRecovered(NewEvaluator(ad), expr.expr)
	if b, err := got.BoolValue(); err != nil || !b {
		t.Fatalf("got %v, want true", got)
	}
}

// TestShortCircuitGuardStillFires keeps the depth guard doing its job: a cyclic
// reference and genuinely deep nesting both resolve to error.
func TestShortCircuitGuardStillFires(t *testing.T) {
	t.Run("deep-nesting-inside-a-spine", func(t *testing.T) {
		ad, err := Parse(`[X = 1]`)
		if err != nil {
			t.Fatal(err)
		}
		// One spine term is nested far deeper than the guard allows; the
		// spine's own length does not rescue it.
		deep := strings.Repeat("!", maxEvalDepth+1) + "X"
		src := "X == 2 || X == 3 || (" + deep + ")"
		expr, err := ParseExpr(src)
		if err != nil {
			t.Fatal(err)
		}
		got := evalRecovered(NewEvaluator(ad), expr.expr)
		if !got.IsError() {
			t.Fatalf("got %v, want error", got)
		}
	})

	t.Run("cyclic-reference-in-a-spine", func(t *testing.T) {
		ad, err := Parse(`[A = B || false; B = A || false; X = 1]`)
		if err != nil {
			t.Fatal(err)
		}
		expr, err := ParseExpr("X == 2 || X == 3 || A")
		if err != nil {
			t.Fatal(err)
		}
		got := evalRecovered(NewEvaluator(ad), expr.expr)
		if !got.IsError() && !got.IsUndefined() {
			t.Fatalf("got %v, want the cycle reported as error or undefined", got)
		}
	})
}
