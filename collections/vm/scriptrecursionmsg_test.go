package vm

import (
	"strings"
	"testing"
)

// THE REFUSAL HAS TO DESCRIBE THE LANGUAGE IT IS REFUSING FOR.
//
// A call to itself is compiled in tail position and nowhere else, so the two refusals say different
// things: the one an author can act on names the position, and the one they cannot does not offer
// it. A single message covering both would tell half its readers to move a call that cannot be
// moved, and the other half nothing about the move that works.

// TestSelfRecursionRefusalNamesTailPosition pins that a self-call outside tail position is told
// where the position is, since moving it there is the whole fix.
func TestSelfRecursionRefusalNamesTailPosition(t *testing.T) {
	cases := []struct{ name, src string }{
		{"feeds an operator", "def f(n):\n    if n <= 0:\n        return 0\n    return f(n - 1) + 1\n\nresult = f(3)\n"},
		{"bound to a local", "def f(n):\n    x = f(n)\n    return x\n\nresult = f(3)\n"},
		{"a condition", "def f(n):\n    if f(n) > 0:\n        return 1\n    return 2\n\nresult = f(3)\n"},
		{"a ternary's arm", "def f(n):\n    return n > 0 ? f(n) : 2\n\nresult = f(3)\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseScript(tc.src)
			if err == nil {
				t.Fatal("the call was accepted")
			}
			msg := err.Error()
			for _, want := range []string{"is recursive", "f -> f", "TAIL POSITION", "return f(...)"} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal %q does not contain %q", msg, want)
				}
			}
		})
	}
}

// TestMutualRecursionRefusalDoesNotOfferTailPosition pins the other half: a cycle through another
// function has no tail form, so the message must not send its author looking for one.
func TestMutualRecursionRefusalDoesNotOfferTailPosition(t *testing.T) {
	src := "def a(v):\n    return b(v)\n\ndef b(v):\n    return a(v)\n\nresult = a(1)\n"
	_, err := ParseScript(src)
	if err == nil {
		t.Fatal("mutual recursion was accepted")
	}
	msg := err.Error()
	for _, want := range []string{"is recursive", "->"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not contain %q", msg, want)
		}
	}
	// The phrase the self-call refusal uses to name the position an author can move a call TO.
	// Offering it here would be advice that cannot be taken.
	if strings.Contains(msg, "TAIL POSITION") {
		t.Errorf("refusal %q offers tail position for a cycle that has no tail form", msg)
	}
}

// TestTailCallIsStillCompiled guards the pair above against the refusal simply swallowing
// everything: the position the messages point at has to be one that actually works.
func TestTailCallIsStillCompiled(t *testing.T) {
	sp, err := ParseScript("def f(n, acc):\n    if n <= 1:\n        return acc\n    return f(n - 1, acc * n)\n\nresult = f(5, 1)\n")
	if err != nil {
		t.Fatalf("a tail call was refused: %v", err)
	}
	if got := sp.Run(nil).String(); got != "120" {
		t.Errorf("f(5, 1) = %s, want 120", got)
	}
}
