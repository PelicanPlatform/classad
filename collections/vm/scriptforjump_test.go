package vm_test

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// A `for` desugars into a counted loop whose index advances BEFORE the body, and `continue`
// jumps to the loop header. Those two decisions are only jointly observable: with the advance
// after the body, a `continue` would skip it and the loop would never finish. Each construct's
// own tests pass either way, so this file is where the pair is pinned.
//
// Every case runs under an explicit budget, so "did not finish" is reported as itself rather
// than as a hung test.
func runForJump(t *testing.T, src, adsrc string) (string, bool) {
	t.Helper()
	sp, err := vm.ParseScript(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ad, perr := classad.Parse(adsrc)
	if perr != nil {
		t.Fatalf("ad: %v", perr)
	}
	v, ok := sp.RunLimited(ad, 20000)
	return v.String(), ok
}

func TestForWithLoopJumps(t *testing.T) {
	cases := []struct {
		name, src, ad, want string
	}{
		{
			"continue advances to the next element",
			"s = 0\nfor v in MY.L:\n    if v % 2 == 0:\n        continue\n    s = s + v\nresult = s\n",
			"[ L = {1,2,3,4,5} ]", "9",
		},
		{
			"break leaves the for",
			"s = 0\nfor v in MY.L:\n    if v > 3:\n        break\n    s = s + v\nresult = s\n",
			"[ L = {1,2,3,4,5} ]", "6",
		},
		{
			"break binds the inner for",
			"n = 0\nfor a in MY.L:\n    for b in MY.L:\n        break\n    n = n + 1\nresult = n\n",
			"[ L = {1,2,3} ]", "3",
		},
		{
			"break in a for nested in a while binds the for",
			"i = 0\nn = 0\nwhile i < 3:\n    i = i + 1\n    for v in MY.L:\n        break\n    n = n + 1\nresult = n\n",
			"[ L = {1,2} ]", "3",
		},
		{
			"continue in the outer of two fors",
			"n = 0\nfor a in MY.L:\n    if a == 2:\n        continue\n    for b in MY.L:\n        n = n + 1\nresult = n\n",
			"[ L = {1,2,3} ]", "6",
		},
		{
			"continue as the last statement of the body",
			"n = 0\nfor v in MY.L:\n    n = n + 1\n    continue\nresult = n\n",
			"[ L = {1,2,3,4} ]", "4",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := runForJump(t, c.src, c.ad)
			if !ok {
				t.Fatal("did not finish within the budget: a loop jump reached the header " +
					"without the index having advanced")
			}
			if got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}
