package vm

import (
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// SELF-TAIL-RECURSION, which is the one recursive call a script may make.
//
// The behaviour under test is not "recursion works". It is that `return f(...)` inside f becomes a
// back edge to f's own entry -- the arguments computed, the parameters overwritten, the activation
// cleared -- and that NOTHING ELSE moved: every other recursive position keeps the refusal that
// names the cycle, because every other position needs the call's value where it stands and the only
// way to produce one is an expansion that has no end.

// TestScriptTailCallComputes is the behavioural table: each script is a loop written as a recursion,
// and each answer is one a reader can check by hand.
func TestScriptTailCallComputes(t *testing.T) {
	cases := []struct {
		name, src, ad, want string
	}{
		{
			name: "factorial",
			src: `def fact(n, acc):
    if n <= 1:
        return acc
    return fact(n - 1, acc * n)

result = fact(10, 1)
`,
			ad: `[]`, want: "3628800",
		},
		{
			name: "sum to n",
			src: `def sumTo(n, acc):
    if n <= 0:
        return acc
    return sumTo(n - 1, acc + n)

result = sumTo(100, 0)
`,
			ad: `[]`, want: "5050",
		},
		{
			// The fold a `for` would write, written as a recursion instead, over the native element
			// load rather than over anything a tail call had to be taught.
			name: "fold over a list",
			src: `def fold(xs, i, acc):
    if i >= size(xs):
        return acc
    return fold(xs, i + 1, acc + xs[i])

result = fold(MY.Sizes, 0, 0)
`,
			ad: `[Sizes = {1, 2, 3, 4}]`, want: "10",
		},
		{
			// An empty list: the first test is already false, so the recursion makes no trip at all.
			name: "fold over an empty list",
			src: `def fold(xs, i, acc):
    if i >= size(xs):
        return acc
    return fold(xs, i + 1, acc + xs[i])

result = fold(MY.Sizes, 0, 0)
`,
			ad: `[Sizes = {}]`, want: "0",
		},
		{
			// The tail call is the whole of an else arm, so the jump is emitted inside a branch and
			// still goes to the body's entry rather than to the end of the arm.
			name: "tail call in an else arm",
			src: `def collatz(n, steps):
    if n <= 1:
        return steps
    else:
        if n % 2 == 0:
            return collatz(n / 2, steps + 1)
        else:
            return collatz(3 * n + 1, steps + 1)

result = collatz(6, 0)
`,
			ad: `[]`, want: "8",
		},
		{
			// From inside a loop in the body. The jump leaves the loop on its way to the entry,
			// which is sound for the same reason a `break` is: there is no back edge on that path.
			name: "tail call inside a while body",
			src: `def walk(xs, i, acc):
    while i < size(xs):
        return walk(xs, i + 1, acc + xs[i])
    return acc

result = walk(MY.Sizes, 0, 0)
`,
			ad: `[Sizes = {5, 6, 7}]`, want: "18",
		},
		{
			// From inside a `for` body, which desugars onto the same `while`. The index advances
			// before the body, so a jump out of the loop skips no part of the loop's own work.
			name: "tail call inside a for body",
			src: `def shrink(n, acc):
    if n <= 0:
        return acc
    for x in {1, 2, 3}:
        if x > 2:
            return shrink(n - 1, acc + x)
    return acc

result = shrink(2, 0)
`,
			ad: `[]`, want: "6",
		},
		{
			// A `for` in the body of a tail-recursive function. Its sequence, length and index are
			// registers of the activation, so they are cleared on re-entry and rebuilt by the loop's
			// own header on the next trip.
			name: "a for loop in a tail-recursive body",
			src: `def countdown(n, acc):
    for x in {1, 2}:
        acc = acc + x
    if n <= 0:
        return acc
    return countdown(n - 1, acc)

result = countdown(2, 0)
`,
			ad: `[]`, want: "9",
		},
		{
			// Two calls to the same tail-recursive function. Each is its own expansion with its own
			// registers and its own entry, so a back edge that went to the other one's entry -- or
			// to the argument evaluation at the call site -- would be visible here.
			name: "two calls to one tail-recursive function",
			src: `def fact(n, acc):
    if n <= 1:
        return acc
    return fact(n - 1, acc * n)

result = fact(5, 1) + fact(3, 1)
`,
			ad: `[]`, want: "126",
		},
		{
			// A function the tail-recursive one calls, which is an ordinary expansion sitting inside
			// a body that runs more than once.
			name: "a tail-recursive function calling a sibling",
			src: `def twice(x):
    return x * 2

def go(n, acc):
    if n <= 0:
        return acc
    return go(n - 1, acc + twice(n))

result = go(4, 0)
`,
			ad: `[]`, want: "20",
		},
		{
			// A call folds case, and a tail call is a call: the name is resolved through the same
			// map, so the spelling at the `return` need not match the spelling at the `def`.
			name: "the call folds case",
			src: `def SumTo(n, acc):
    if n <= 0:
        return acc
    return SUMTO(n - 1, acc + n)

result = sumto(10, 0)
`,
			ad: `[]`, want: "55",
		},
		{
			// Nothing recursive at all, written so that it looks it: a calls b, b returns. The
			// refusal must not have become a refusal of any call that resembles one.
			name: "mutual-looking but not recursive",
			src: `def a(v):
    return b(v) + 1

def b(v):
    return v * 2

result = a(5)
`,
			ad: `[]`, want: "11",
		},
		{
			// The function reads the ad, which a tail call does not change: the attribute is read
			// again on every trip, from the same scope.
			name: "a body that reads the ad",
			src: `def drop(n):
    if n <= Floor:
        return n
    return drop(n - 1)

result = drop(10)
`,
			ad: `[Floor = 7]`, want: "7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wants(t, runScript(t, tc.src, tc.ad), tc.want)
		})
	}
}

// TestScriptTailCallEvaluatesEveryArgumentBeforeWritingAnyParameter is the ordering law, and the
// reason the arguments go through temporaries rather than straight into the parameters.
//
// An argument expression reads parameters -- that is what a recursion is for -- and the parameters
// are the registers being written. Writing them one at a time lets an earlier write be read by a
// later argument, so a function that swaps its own arguments would duplicate one instead. Swapping
// is the smallest case that shows it, and the answer differs for every trip count, so a single pair
// is not enough: with three trips the wrong order gives 1 where the right one gives 2.
func TestScriptTailCallEvaluatesEveryArgumentBeforeWritingAnyParameter(t *testing.T) {
	swap := `def f(n, a, b):
    if n <= 0:
        return a
    return f(n - 1, b, a)

result = f(%s, 1, 2)
`
	// One trip swaps, two swap back, three swap again.
	for _, tc := range []struct{ trips, want string }{{"1", "2"}, {"2", "1"}, {"3", "2"}, {"4", "1"}} {
		t.Run("trips "+tc.trips, func(t *testing.T) {
			wants(t, runScript(t, strings.Replace(swap, "%s", tc.trips, 1), `[]`), tc.want)
		})
	}

	// The same hazard with three parameters rotating, where storing in order would leave every
	// parameter holding the first one's value.
	rotate := `def r(n, a, b, c):
    if n <= 0:
        return a * 100 + b * 10 + c
    return r(n - 1, b, c, a)

result = r(1, 1, 2, 3)
`
	wants(t, runScript(t, rotate, `[]`), "231")
}

// TestScriptTailCallClearsTheActivationOnReentry pins what a trip round the back edge means: a fresh
// activation of the body, not another turn of a loop the author wrote.
//
// A local bound on one path and read on another has to read UNDEFINED again on the next trip, as it
// would in a second call. Without the clear it would hold what the previous trip left, which is a
// value no call to the function could have produced.
func TestScriptTailCallClearsTheActivationOnReentry(t *testing.T) {
	// `bump` is bound only on the trip where n is 3. Every later trip must read UNDEFINED, so the
	// total is one 100 and not one per remaining trip.
	src := `def go(n, acc):
    if n <= 0:
        return acc
    if n == 3:
        bump = 100
    return go(n - 1, acc + (bump ?: 0))

result = go(5, 0)
`
	wants(t, runScript(t, src, `[]`), "100")

	// The same hazard one level down, where nothing the author can write would put it right: `tag`
	// returns a value on one trip and falls off the end on the others, and its result register
	// belongs to an expansion with no name. The clear covers every register the activation
	// allocated, which is what makes this case need no separate mechanism.
	nested := `def tag(n):
    if n == 3:
        return 100
    return

def go(n, acc):
    if n <= 0:
        return acc
    return go(n - 1, acc + (tag(n) ?: 0))

result = go(5, 0)
`
	wants(t, runScript(t, nested, `[]`), "100")
}

// TestScriptTailCallRefusalsStillHold is the other half of the feature, and the more important one:
// every recursive position that is not a tail call keeps the refusal that names the cycle.
//
// A wrongly-accepted position is a script that computes something other than what it says, with
// nothing to notice it; a wrongly-refused one is a message. So the line is drawn narrow and pinned
// here, position by position.
func TestScriptTailCallRefusalsStillHold(t *testing.T) {
	cases := []struct{ name, src string }{
		{"the result feeds an operator", "def f(v):\n    return f(v) + 1\n\nresult = f(1)\n"},
		{"the result is an operand on the left", "def f(v):\n    return 1 + f(v)\n\nresult = f(1)\n"},
		{"the result is another call's argument", "def g(x):\n    return x * 2\n\ndef f(v):\n    return g(f(v))\n\nresult = f(1)\n"},
		{"the call is its own argument", "def f(v):\n    return f(f(v))\n\nresult = f(1)\n"},
		{"the result is bound to a local", "def f(v):\n    x = f(v)\n    return x\n\nresult = f(1)\n"},
		{"the result is a condition", "def f(v):\n    if f(v) > 0:\n        return 1\n    return 2\n\nresult = f(1)\n"},
		{"the result is a loop condition", "def f(v):\n    while f(v) > 0:\n        return 1\n    return 2\n\nresult = f(1)\n"},
		{"the result is subscripted", "def f(v):\n    return f(v)[0]\n\nresult = f(1)\n"},
		// Sound to accept and left out on purpose, so that the rule stays one a reader checks by
		// looking at the line rather than at a parse tree.
		{"a parenthesised call", "def f(v):\n    return (f(v))\n\nresult = f(1)\n"},
		// The call is in tail position of a function that is not the one being expanded.
		{"mutual recursion, both in tail position", "def a(v):\n    return b(v)\n\ndef b(v):\n    return a(v)\n\nresult = a(1)\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := refuses(t, tc.src)
			if !strings.Contains(msg, "recursive") {
				t.Errorf("refusal %q does not say the call is recursive", msg)
			}
			if !strings.Contains(msg, "->") {
				t.Errorf("refusal %q does not name the cycle", msg)
			}
		})
	}

	// A ternary is refused before the recursion is reached, by the rule that a construct evaluated
	// against the ad cannot call a script function at all. It is listed separately because the
	// message is the one a NON-recursive call gets there too, and reading it as the cycle refusal
	// would hide the day that rule stopped firing.
	msg := refuses(t, "def f(v):\n    return f(v) ? 1 : 2\n\nresult = f(1)\n")
	if !strings.Contains(msg, "calls f") || !strings.Contains(msg, "evaluated against the ad") {
		t.Errorf("refusal %q does not explain the delegated call", msg)
	}
}

// TestScriptTailCallWrongArityIsAnArityRefusal pins that a self-call with the wrong number of
// arguments is not read as a tail call. Both counts are in the message, which is the useful thing to
// say; "f is recursive" would not be.
func TestScriptTailCallWrongArityIsAnArityRefusal(t *testing.T) {
	msg := refuses(t, "def f(a, b):\n    return f(a)\n\nresult = f(1, 2)\n")
	for _, want := range []string{"f", "takes 2", "passes 1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q lacks %q", msg, want)
		}
	}
}

// TestScriptTailCallIsBoundedByTheStepBudget is termination, and it is the step budget's job for the
// same reason it is a `while`'s: nothing the compiler can check tells a recursion that ends from one
// that does not.
//
// The smallest runaway is a function that takes no arguments and computes nothing, where the only
// charged instruction on the back edge is the one that clears the activation. Run on another
// goroutine with a deadline, because the failure being guarded against is that it never returns --
// and a hang takes the package down without saying which test was stuck.
func TestScriptTailCallIsBoundedByTheStepBudget(t *testing.T) {
	cases := []struct{ name, src string }{
		{"no arguments", "def spin():\n    return spin()\n\nresult = spin()\n"},
		{"an argument that never changes", "def spin(n):\n    return spin(n)\n\nresult = spin(1)\n"},
		{"a test that is never true", "def spin(n):\n    if n > 1000000000:\n        return n\n    return spin(n + 1)\n\nresult = spin(0)\n"},
	}
	scope, perr := classad.Parse(`[]`)
	if perr != nil {
		t.Fatal(perr)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp, err := ParseScript(tc.src)
			if err != nil {
				t.Fatalf("ParseScript: %v", err)
			}
			type outcome struct {
				val classad.Value
				ok  bool
			}
			done := make(chan outcome, 1)
			go func() {
				v, ok := sp.RunLimited(scope, 5000)
				done <- outcome{v, ok}
			}()
			select {
			case got := <-done:
				if got.ok {
					t.Fatalf("an unbounded tail recursion finished, returning %s", got.val)
				}
				if !got.val.IsError() {
					t.Errorf("budget exhaustion gave %s, want error", got.val)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("an unbounded tail recursion did not return: a trip round the back edge is " +
					"being charged nothing, so the budget can never be reached")
			}
		})
	}

	// And a recursion that does end is not stopped by the budget, so the bound above is a bound and
	// not a refusal of tail calls in general.
	sp, err := ParseScript("def sumTo(n, acc):\n    if n <= 0:\n        return acc\n    return sumTo(n - 1, acc + n)\n\nresult = sumTo(100, 0)\n")
	if err != nil {
		t.Fatalf("ParseScript: %v", err)
	}
	got, ok := sp.RunLimited(scope, 5000)
	if !ok {
		t.Fatal("a terminating tail recursion of 100 trips ran out of a 5000-step budget")
	}
	wants(t, got, "5050")
}

// TestScriptTailCallDeclinesVectorization pins the vector executor's answer, which is to decline.
//
// That executor reads control flow by region: a While is recognised by the conditional at its
// header, and that conditional is what drops the records it has become false for from the live set.
// A function's entry carries no conditional, and the records going round a tail call are not the
// records of any one region -- a tail call written in an If arm would carry the whole batch
// backwards with the other arm unrun. There is no shape here to run in lockstep, so the batch falls
// back to the per-record path, where the cases above already say what the answer is.
//
// THE DECLINE IS MADE TWICE, which the second half of this test pins. The jump itself is a backward
// one the region walk does not recognise, so it would be refused there whatever the flag said; the
// flag refuses the PROGRAM, before an instruction is read, so the refusal does not rest on a shape
// argument that a body beginning with a `while` -- whose entry is a loop header -- invites a reader
// to re-examine.
func TestScriptTailCallDeclinesVectorization(t *testing.T) {
	src := `def down(n, acc):
    if n <= 0:
        return acc
    return down(n - 1, acc + n)

result = down(A, 0)
`
	sp, err := ParseScript(src)
	if err != nil {
		t.Fatalf("ParseScript: %v", err)
	}
	if !sp.Program().hasTailCall {
		t.Fatal("the program does not carry hasTailCall; this test is not testing what it says")
	}
	corpus := loopCorpus()
	n := len(corpus.recs)
	var scratch VecScratch
	if _, ok := evalWithDeadline(t, sp, corpus, n, &scratch); ok {
		t.Fatal("vectorized a script with a tail call in it")
	}
	// The shape alone is refused too: the back edge is not one the region walk has a region for.
	sp.Program().hasTailCall = false
	if _, ok := evalWithDeadline(t, sp, corpus, n, &scratch); ok {
		t.Fatal("the region walk served a tail call's back edge; the flag is then the only thing " +
			"standing between a batch and an answer read off a construct that is not there")
	}
	sp.Program().hasTailCall = true

	// The per-record path answers every record. The corpus carries conditions that are neither true
	// nor false -- a missing attribute, an explicit UNDEFINED, a string -- and those fault, exactly
	// as an If on the same value would; the rest sum 1..A.
	want := []string{"0", "1", "6", "15", "error", "error", "error", "3"}
	for i := 0; i < n; i++ {
		if got := sp.Run(corpus.ad(i)).String(); got != want[i] {
			t.Errorf("record %d: got %s, want %s", i, got, want[i])
		}
	}
}

// TestScriptTailCallVectorizesNothingItShouldNot is the control for the decline: a script calling a
// function with no tail call in it is served by the vector executor, so the decline above is
// attributable to the tail call rather than to there being a function at all.
func TestScriptTailCallVectorizesNothingItShouldNot(t *testing.T) {
	sp, err := ParseScript("def inc(n):\n    return n + 1\n\nresult = inc(A)\n")
	if err != nil {
		t.Fatalf("ParseScript: %v", err)
	}
	if sp.Program().hasTailCall {
		t.Fatal("a function with no tail call in it was marked as having one")
	}
	corpus := loopCorpus()
	n := len(corpus.recs)
	var scratch VecScratch
	got, ok := evalWithDeadline(t, sp, corpus, n, &scratch)
	if !ok {
		t.Fatal("a plain function call declined; the decline above would then say nothing about the tail call")
	}
	for i := 0; i < n; i++ {
		if want := sp.Run(corpus.ad(i)); !sameValue(got.value(i), want) {
			t.Fatalf("record %d: vector %s != scalar %s", i, got.value(i), want)
		}
	}
}

// TestScriptTailCallInAModule pins that a module's function may be tail-recursive, and that it still
// passes the purity gate.
//
// A module compiles one function on its own, with its parameters filled from argument VALUES rather
// than from a caller's expressions. A tail call writes those same registers, so the entry a trip
// goes back to is the body's first instruction and not an argument evaluation that does not exist.
// The gate refuses a function that reaches the subject ad; a back edge adds no reference load, so a
// tail-recursive function registers like any other.
func TestScriptTailCallInAModule(t *testing.T) {
	f := compileOne(t, "def fact(n, acc):\n    if n <= 1:\n        return acc\n    return fact(n - 1, acc * n)\n", "fact")
	if got := f.Call([]classad.Value{classad.NewIntValue(6), classad.NewIntValue(1)}).String(); got != "720" {
		t.Fatalf("fact(6, 1) = %s, want 720", got)
	}
	// Called twice, because the compiled function is shared and a trip that left a register dirty
	// would show up on the second call rather than the first.
	if got := f.Call([]classad.Value{classad.NewIntValue(4), classad.NewIntValue(1)}).String(); got != "24" {
		t.Fatalf("fact(4, 1) = %s, want 24", got)
	}

	// Non-tail self-recursion in a module is refused where it has always been refused.
	m, err := ParseScriptModule("def loop(x):\n    return loop(x) + 1\n")
	if err != nil {
		t.Fatalf("ParseScriptModule: %v", err)
	}
	if _, err := m.Compile("loop"); err == nil || !strings.Contains(err.Error(), "recursive") {
		t.Fatalf("Compile = %v, want a recursion refusal", err)
	}
}

// TestScriptTailCallFromInsideACallersLoop pins the interaction with the per-call reset: a
// tail-recursive function called from inside a `while` is expanded once, runs its own trips on each
// visit, and starts each visit with the registers it started the first one with.
func TestScriptTailCallFromInsideACallersLoop(t *testing.T) {
	src := `def sumTo(n, acc):
    if n <= 0:
        return acc
    return sumTo(n - 1, acc + n)

i = 0
total = 0
while i < 4:
    i = i + 1
    total = total + sumTo(i, 0)
result = total
`
	// 1 + 3 + 6 + 10.
	wants(t, runScript(t, src, `[]`), "20")

	// Both resets at once: the per-call one, which clears what the expansion holds each time the
	// caller's loop reaches it, and the re-entry one, which clears it each trip round the recursion.
	// `tag` returns a value on one trip of five and falls off the end on the others, so a register
	// held over either edge would show up as a second or a third 100.
	both := `def tag(n):
    if n == 3:
        return 100
    return

def go(n, acc):
    if n <= 0:
        return acc
    return go(n - 1, acc + (tag(n) ?: 0))

i = 0
total = 0
while i < 2:
    i = i + 1
    total = total + go(5, 0)
result = total
`
	wants(t, runScript(t, both, `[]`), "200")
}

// TestScriptTailCallBreaksStillBindToTheBody pins that a tail call changes nothing about which loop
// a `break` names. A body's `break` is bound by a loop in that body, and a tail call in the same
// body does not give it another one to reach for.
func TestScriptTailCallBreaksStillBindToTheBody(t *testing.T) {
	msg := refuses(t, `def f(n):
    break
    return f(n - 1)

i = 0
while i < 2:
    i = i + 1
    x = f(i)
result = i
`)
	if !strings.Contains(msg, "'break' in a function body with no loop in that body") {
		t.Errorf("refusal %q does not explain which loop a break binds to", msg)
	}

	// And a `break` that does have a loop in the body still leaves THAT loop, with the tail call
	// below it reached afterwards.
	src := `def f(n, acc):
    i = 0
    while i < 10:
        i = i + 1
        if i > 2:
            break
    acc = acc + i
    if n <= 1:
        return acc
    return f(n - 1, acc)

result = f(3, 0)
`
	// Three trips, each adding the 3 the inner loop stops at.
	wants(t, runScript(t, src, `[]`), "9")
}
