package vm

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// `break` AND `continue`, and the two questions worth testing about them.
//
// The first is arithmetic: a loop that leaves early or skips a trip has an answer a reader can
// work out on paper, so every case here states that answer rather than only asserting that the
// script compiled. A jump to the wrong address still compiles and still terminates.
//
// The second is BINDING -- which loop a jump names. Nesting binds the innermost, which is the
// ordinary rule; a function body binds only a loop in that body, which is not, and is the one that
// would otherwise make a function mean different things at different call sites. Both halves are
// here: the function that loops and breaks inside itself, and the bare `break` in a body called
// from a caller's loop.

// TestLoopJumpComputes checks the value a loop with a jump in it produces, against the answer
// worked out by hand.
func TestLoopJumpComputes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		// Counts to 100, except it stops at 7. The whole point: the condition is still true when
		// the loop ends.
		{"break stops early",
			"i = 0\nwhile i < 100:\n    i = i + 1\n    if i == 7:\n        break\nresult = i\n",
			"7"},
		// 1+3+5+7+9. The even trips reach `continue` before the accumulator, so they add nothing.
		{"continue skips the rest of the body",
			"i = 0\ns = 0\nwhile i < 10:\n    i = i + 1\n    if i % 2 == 0:\n        continue\n    s = s + i\nresult = s\n",
			"25"},
		// A CONTINUE MUST NOT SKIP THE CONDITION. If it jumped past the header the loop would never
		// end; the budget would stop it and the script would come back as a non-answer.
		{"continue re-tests the condition",
			"i = 0\nwhile i < 4:\n    i = i + 1\n    continue\nresult = i\n",
			"4"},
		// The only statement in the body, so the first trip is the last one and nothing in the body
		// can have moved the condition on. A loop that terminates only because its body makes
		// progress would hang here.
		{"break as the only statement",
			"i = 0\nwhile true:\n    break\nresult = i\n",
			"0"},
		// The last statement of the body, after work that did run.
		{"break as the last statement",
			"i = 0\nwhile i < 10:\n    i = i + 3\n    break\nresult = i\n",
			"3"},
		// NESTING BINDS THE INNERMOST. The inner loop stops at j == 2, so each of the three outer
		// trips adds 2: the answer is 6 and not 3 (inner break read as outer) or 30 (no break).
		{"nested break binds the inner loop",
			"i = 0\ntotal = 0\nwhile i < 3:\n    i = i + 1\n    j = 0\n    while j < 10:\n        j = j + 1\n        if j == 2:\n            break\n    total = total + j\nresult = total\n",
			"6"},
		// The same for continue: the inner one restarts the inner loop, so the outer trip count is
		// untouched and every inner loop still runs to its own condition.
		{"nested continue binds the inner loop",
			"i = 0\nseen = 0\nwhile i < 2:\n    i = i + 1\n    j = 0\n    while j < 5:\n        j = j + 1\n        if j < 3:\n            continue\n        seen = seen + 1\nresult = seen\n",
			"6"},
		// Inside an else arm rather than a then arm, which is a different landing point for the
		// If's own jumps and so a different place for the loop's jump to be confused with one.
		{"break from an else arm",
			"i = 0\nwhile i < 100:\n    if i < 5:\n        i = i + 1\n    else:\n        break\nresult = i\n",
			"5"},
		// Two jumps out of one loop, so the patch list holds more than one entry.
		{"two breaks in one loop",
			"i = 0\nwhile i < 100:\n    i = i + 1\n    if i == 20:\n        break\n    if i == 9:\n        break\nresult = i\n",
			"9"},
		// A break and a continue in the same loop, which is where a shared target would show.
		{"break and continue together",
			"i = 0\ns = 0\nwhile i < 100:\n    i = i + 1\n    if i % 3 == 0:\n        continue\n    if i == 8:\n        break\n    s = s + i\nresult = s\n",
			"19"},
		// A FUNCTION THAT LOOPS AND BREAKS INSIDE ITSELF is legal, and the expansion sits inside
		// the caller's loop without the two interfering: the caller runs its full three trips.
		{"a function breaks its own loop, called from a loop",
			"def capped(n):\n    i = 0\n    while i < n:\n        i = i + 1\n        if i == 3:\n            break\n    return i\n" +
				"k = 0\ntotal = 0\nwhile k < 3:\n    k = k + 1\n    total = total + capped(100)\nresult = total\n",
			"9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runScript(t, tc.src, "[]").String(); got != tc.want {
				t.Fatalf("result = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestLoopJumpTerminates pins that a loop whose body always breaks finishes, rather than finishing
// only because the step budget stopped it. RunLimited reports the difference the value cannot.
func TestLoopJumpTerminates(t *testing.T) {
	sp, err := ParseScript("i = 0\nwhile true:\n    i = i + 1\n    break\nresult = i\n")
	if err != nil {
		t.Fatalf("ParseScript: %v", err)
	}
	// A budget far below what a runaway loop would need, and comfortably above one trip.
	v, ok := sp.RunLimited(nil, 100)
	if !ok {
		t.Fatal("the loop did not finish within 100 steps; the break is not leaving it")
	}
	if got := v.String(); got != "1" {
		t.Fatalf("result = %s, want 1", got)
	}
}

// TestLoopJumpRefused is every way a jump has no loop to name.
func TestLoopJumpRefused(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"break at the top level", "break\nresult = 1\n", "'break' outside a loop"},
		{"continue at the top level", "continue\nresult = 1\n", "'continue' outside a loop"},
		{"break in an if outside a loop", "if MY.x:\n    break\nresult = 1\n", "'break' outside a loop"},
		{"continue in an if outside a loop", "if MY.x:\n    continue\nresult = 1\n", "'continue' outside a loop"},
		// AFTER the loop rather than inside it: the loop's body is over, so there is nothing open.
		{"break after the loop", "i = 0\nwhile i < 2:\n    i = i + 1\nbreak\nresult = i\n", "'break' outside a loop"},
		// A bare jump in a body, with no loop in the body. The call site has one, which is exactly
		// the case that must still be refused.
		{"break in a function body called from a loop",
			"def f():\n    break\n    return 1\ni = 0\nwhile i < 2:\n    i = i + f()\nresult = i\n",
			"no loop in that body"},
		{"continue in a function body called from a loop",
			"def f():\n    continue\n    return 1\ni = 0\nwhile i < 2:\n    i = i + f()\nresult = i\n",
			"no loop in that body"},
		// ... and with no loop anywhere, which gets the same message: the rule is about the body.
		{"break in a function body called outside a loop",
			"def f():\n    break\n    return 1\nresult = f()\n",
			"no loop in that body"},
		// A jump in a body AFTER a loop in that body, so the body's own stack is empty again.
		{"break after the body's own loop",
			"def f():\n    i = 0\n    while i < 2:\n        i = i + 1\n    break\n    return i\nresult = f()\n",
			"no loop in that body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseScript(tc.src)
			if err == nil {
				t.Fatalf("accepted %q", tc.src)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestLoopJumpRefusalNamesTheJumpsLine pins WHERE a refusal points, which is the only thing that
// tells an author which line to delete. A `break` in a function body is refused while the CALL is
// being lowered, so the easy mistake is to blame the call's line.
func TestLoopJumpRefusalNamesTheJumpsLine(t *testing.T) {
	_, err := ParseScript("def f():\n    break\n    return 1\ni = 0\nwhile i < 2:\n    i = i + f()\nresult = i\n")
	if err == nil {
		t.Fatal("accepted a break in a function body")
	}
	se, ok := err.(*ScriptSyntaxError)
	if !ok {
		t.Fatalf("error is %T, want *ScriptSyntaxError with a position", err)
	}
	if se.Line != 2 || se.Column != 5 {
		t.Fatalf("refusal at line %d, column %d; want line 2, column 5 -- the break, not the call", se.Line, se.Column)
	}
}

// TestLoopJumpTakesNoOperand pins the parser's own refusal, which is what keeps `break x` from
// being read as a `break` with a statement quietly dropped.
func TestLoopJumpTakesNoOperand(t *testing.T) {
	for _, src := range []string{
		"while MY.x:\n    break 1\nresult = 1\n",
		"while MY.x:\n    continue 1\nresult = 1\n",
	} {
		_, err := ParseScript(src)
		if err == nil {
			t.Fatalf("accepted %q", src)
		}
		if !strings.Contains(err.Error(), "takes no operand") {
			t.Fatalf("error = %q, want it to say the statement takes no operand", err)
		}
	}
}

// TestLoopJumpIsASoftKeyword pins that `break` and `continue` stay usable as attribute names,
// which every word in this language has to: ClassAd attribute names are case-insensitive, so a
// hard keyword would take four spellings of a name out of the ad.
func TestLoopJumpIsASoftKeyword(t *testing.T) {
	// Leading a statement in a spelling that is not all-lowercase, and in an expression in any
	// spelling at all.
	got := runScript(t, "x = Break + continue\nresult = x\n", `[Break = 3; Continue = 4]`).String()
	if got != "7" {
		t.Fatalf("result = %s, want 7", got)
	}
}

// TestLoopJumpBuilderRefusals is the builder's half: a caller using the API directly gets the same
// refusals the parser's lowering relies on.
func TestLoopJumpBuilderRefusals(t *testing.T) {
	t.Run("outside a loop", func(t *testing.T) {
		_, err := NewScript().Break().Result(mustExpr(t, "1")).Compile()
		if err == nil || !strings.Contains(err.Error(), "'break' outside a loop") {
			t.Fatalf("err = %v, want 'break' outside a loop", err)
		}
	})
	t.Run("after Result", func(t *testing.T) {
		_, err := NewScript().Result(mustExpr(t, "1")).Continue().Compile()
		if err == nil || !strings.Contains(err.Error(), "'continue' after Result") {
			t.Fatalf("err = %v, want 'continue' after Result", err)
		}
	})
}

// TestLoopJumpDeclinesVectorization is the vector half. The executor reads control flow by region
// -- an If by the jumps that end its arms, a While by the back edge at its header -- and a jump out
// of a loop from inside an arm matches neither. A jump BACK to the header from inside an arm is
// worse than unmatched: it looks exactly like the back edge, so taking it would leave the arm's
// region open and its other side unrun, and the batch would come back with an answer.
//
// So the program is declined and the caller runs the records one at a time, which is what the
// scalar cases above already check the value of.
func TestLoopJumpDeclinesVectorization(t *testing.T) {
	cases := []struct{ name, src string }{
		{"break", "i = 0\nwhile i < 20:\n    i = i + 1\n    if i > A:\n        break\nresult = i\n"},
		{"continue", "i = 0\ns = 0\nwhile i < 6:\n    i = i + 1\n    if i < A:\n        continue\n    s = s + 1\nresult = s\n"},
		// At the top level of the body, where a continue IS the back edge and the executor would
		// serve it correctly. It declines anyway: the flag is on the program, not on the shape, so
		// that the case above cannot be reached by a reader deciding this one is harmless.
		{"continue at the top of the body", "i = 0\nwhile i < A:\n    i = i + 1\n    continue\nresult = i\n"},
	}
	src := loopCorpus()
	n := len(src.recs)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp, err := ParseScript(tc.src)
			if err != nil {
				t.Fatalf("ParseScript: %v", err)
			}
			if !sp.Program().hasLoopJump {
				t.Fatal("the program does not carry hasLoopJump; this test is not testing what it says")
			}
			var scratch VecScratch
			if _, ok := sp.VecEval(src, n, &scratch); ok {
				t.Fatal("vectorized a script with a loop jump in it")
			}
		})
	}
}

// TestLoopJumpVectorizesNothingItShouldNot is the control for the decline above: the same loop
// WITHOUT the jump is served by the vector executor, so the decline is attributable to the jump and
// not to the loop.
func TestLoopJumpVectorizesNothingItShouldNot(t *testing.T) {
	sp, err := ParseScript("i = 0\nwhile i < A:\n    i = i + 1\nresult = i\n")
	if err != nil {
		t.Fatalf("ParseScript: %v", err)
	}
	if sp.Program().hasLoopJump {
		t.Fatal("a loop with no jump in it was marked as having one")
	}
	src := loopCorpus()
	n := len(src.recs)
	var scratch VecScratch
	got, ok := evalWithDeadline(t, sp, src, n, &scratch)
	if !ok {
		t.Fatal("a plain while loop declined; the decline above would then say nothing about the jump")
	}
	for i := 0; i < n; i++ {
		if want := sp.Run(src.ad(i)); !sameValue(got.value(i), want) {
			t.Fatalf("record %d: vector %s != scalar %s", i, got.value(i), want)
		}
	}
}

// TestLoopJumpPassesThePurityGate pins that a registered function may contain a jump.
//
// The gate refuses a function that reaches the subject ad, which it reads off the compiled program:
// a reference load or a delegated subtree. Neither `break` nor `continue` adds one -- they are
// jumps between instructions the body already had -- so a function using them registers like any
// other, and is called here to show the jump still does what the scalar cases say.
func TestLoopJumpPassesThePurityGate(t *testing.T) {
	cases := []struct {
		name string
		src  string
		arg  int64
		want string
	}{
		{"break", "def firstOver(n):\n    i = 0\n    while i < 1000:\n        i = i + 1\n        if i * i > n:\n            break\n    return i\n",
			50, "8"},
		{"continue", "def oddsBelow(n):\n    i = 0\n    s = 0\n    while i < n:\n        i = i + 1\n        if i % 2 == 0:\n            continue\n        s = s + i\n    return s\n",
			10, "25"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := strings.SplitN(strings.TrimPrefix(tc.src, "def "), "(", 2)[0]
			f := compileOne(t, tc.src, name)
			if got := f.Call([]classad.Value{classad.NewIntValue(tc.arg)}).String(); got != tc.want {
				t.Fatalf("Call = %s, want %s", got, tc.want)
			}
		})
	}
}
