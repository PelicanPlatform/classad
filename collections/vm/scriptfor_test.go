package vm

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// runFor parses a script, runs it against an ad, and reports the value and the fault.
func runFor(t *testing.T, src, adSrc string) (classad.Value, bool, *Fault) {
	t.Helper()
	sp, err := ParseScript(src)
	if err != nil {
		t.Fatalf("ParseScript(%q): %v", src, err)
	}
	var ad *classad.ClassAd
	if adSrc != "" {
		ad, err = classad.Parse(adSrc)
		if err != nil {
			t.Fatal(err)
		}
	}
	return sp.RunWithFault(ad)
}

func wantInt(t *testing.T, got classad.Value, want int64, what string) {
	t.Helper()
	iv, err := got.IntValue()
	if err != nil || iv != want {
		t.Errorf("%s = %s, want %d", what, describe(got), want)
	}
}

// TestForVisitsEveryElement is the loop doing its job, over the shapes a sequence comes in.
func TestForVisitsEveryElement(t *testing.T) {
	ad := `[L = {1, 2, 3}; Names = {"a", "b"}; Mixed = {1, "x", undefined}]`
	cases := []struct {
		name, src string
		want      int64
	}{
		{"sum", "total = 0\nfor x in MY.L:\n    total += x\nresult = total\n", 6},
		{"count", "n = 0\nfor x in MY.Names:\n    n += 1\nresult = n\n", 2},
		{"literal sequence", "total = 0\nfor x in {10, 20, 30}:\n    total += x\nresult = total\n", 60},
		{"nested", "n = 0\nfor x in MY.L:\n    for y in MY.L:\n        n += 1\nresult = n\n", 9},
		{"branch in the body", "n = 0\nfor x in MY.L:\n    if x > 1:\n        n += 1\nresult = n\n", 2},
		{"while in the body", "n = 0\nfor x in MY.L:\n    i = 0\n    while i < x:\n        i += 1\n        n += 1\nresult = n\n", 6},
		{"for in a while", "n = 0\ni = 0\nwhile i < 2:\n    for x in MY.L:\n        n += 1\n    i += 1\nresult = n\n", 6},
		{"elements of any type", "n = 0\nfor x in MY.Mixed:\n    n += 1\nresult = n\n", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, f := runFor(t, tc.src, ad)
			if !ok || f != nil {
				t.Fatalf("ok=%v fault=%v", ok, f)
			}
			wantInt(t, got, tc.want, "result")
		})
	}
}

// TestForOverAnEmptySequenceIsNotAFault pins the boundary between "nothing to do" and "this is not
// a sequence": an empty list runs the body zero times and the script carries on.
func TestForOverAnEmptySequenceIsNotAFault(t *testing.T) {
	got, ok, f := runFor(t, "n = 7\nfor x in MY.Empty:\n    n = 0\nresult = n\n", `[Empty = {}]`)
	if !ok || f != nil {
		t.Fatalf("ok=%v fault=%v; an empty list is not a fault", ok, f)
	}
	wantInt(t, got, 7, "result")
}

// TestForOverSomethingThatIsNotAListFaults is the other side of that boundary.
//
// A string is in the table deliberately: size("abc") is 3, so a loop that trusted size() alone would
// run three times and bind ERROR each trip rather than say that a string is not a sequence. So is an
// absent attribute, which is the misspelling the fault exists to make visible.
func TestForOverSomethingThatIsNotAListFaults(t *testing.T) {
	ad := `[S = "abc"; N = 5; B = true; Rec = [K = 1]; R = 1.5]`
	for _, seq := range []string{"MY.S", "MY.N", "MY.B", "MY.Rec", "MY.R", "MY.Missing", `"abc"`, "7", "undefined", "error"} {
		src := "n = 0\nfor x in " + seq + ":\n    n += 1\nresult = n\n"
		got, ok, f := runFor(t, src, ad)
		if !ok {
			t.Errorf("for x in %s: the budget ran out; it should fault", seq)
			continue
		}
		if f == nil {
			t.Errorf("for x in %s = %s with no fault; a sequence that is not a list must fault",
				seq, describe(got))
			continue
		}
		if !got.IsError() {
			t.Errorf("for x in %s = %s, want error", seq, describe(got))
		}
		if !strings.Contains(f.Where, "for") {
			t.Errorf("for x in %s faulted at %q, which does not name the loop", seq, f.Where)
		}
	}
}

// TestForEvaluatesTheSequenceOnce is the property that makes the loop a loop over one list rather
// than over whatever the expression says each trip.
//
// It is checked POSITIONALLY rather than by counting instructions: the sequence is one instruction
// either way, and the question is whether it sits inside the back edge. Everything from the loop
// header to the back edge runs once per trip, so a sequence load in there is a sequence evaluated
// per trip.
func TestForEvaluatesTheSequenceOnce(t *testing.T) {
	sp, err := ParseScript("n = 0\nfor x in MY.Requests:\n    n += 1\nresult = n\n")
	if err != nil {
		t.Fatal(err)
	}
	p := sp.Program()
	header := -1
	for _, c := range p.conds {
		if c.loop {
			header = int(c.header)
		}
	}
	if header < 0 {
		t.Fatal("the for emitted no loop")
	}
	loads := 0
	for ip, in := range p.code {
		if in.Op != OpLoadRef || !strings.EqualFold(p.refs[in.A].name, "Requests") {
			continue
		}
		loads++
		if ip >= header {
			t.Errorf("the sequence is loaded at %d, at or after the loop header %d: it would be "+
				"evaluated once per trip", ip, header)
		}
	}
	if loads != 1 {
		t.Errorf("the sequence is loaded %d times, want 1", loads)
	}

	ad, err := classad.Parse(`[Requests = {1, 1, 1}]`)
	if err != nil {
		t.Fatal(err)
	}
	wantInt(t, sp.Run(ad), 3, "trip count")
}

// TestForBindsAnOrdinaryLocal pins what NAME is: a local like any other, visible after the loop and
// function-scoped inside a def.
func TestForBindsAnOrdinaryLocal(t *testing.T) {
	ad := `[L = {1, 2, 3}]`
	t.Run("visible after the loop", func(t *testing.T) {
		got, _, _ := runFor(t, "for x in MY.L:\n    pass\nresult = x\n", ad)
		wantInt(t, got, 3, "x after the loop")
	})
	t.Run("undefined when the loop ran zero times", func(t *testing.T) {
		got, _, _ := runFor(t, "for x in MY.Empty:\n    pass\nresult = x\n", `[Empty = {}]`)
		if !got.IsUndefined() {
			t.Errorf("x = %s, want undefined", describe(got))
		}
	})
	t.Run("rebinds a name already bound", func(t *testing.T) {
		got, _, _ := runFor(t, "x = 99\nfor x in MY.L:\n    pass\nresult = x\n", ad)
		wantInt(t, got, 3, "x")
	})
	t.Run("shadows the attribute of the same name", func(t *testing.T) {
		got, _, _ := runFor(t, "for x in MY.L:\n    pass\nresult = x + MY.x\n", `[L = {1}; x = 10]`)
		wantInt(t, got, 11, "local + attribute")
	})
	t.Run("function-scoped", func(t *testing.T) {
		src := "def total(seq):\n    t = 0\n    for e in seq:\n        t += e\n    return t\n\n" +
			"e = 100\nresult = total(MY.L) + e\n"
		got, _, _ := runFor(t, src, ad)
		wantInt(t, got, 106, "the body's e did not leak into the caller's")
	})
}

// TestForIsASoftKeyword pins that `for` has not been taken away from attribute names, which ClassAd
// case-insensitivity makes a requirement rather than a nicety.
func TestForIsASoftKeyword(t *testing.T) {
	cases := []struct {
		name, src string
		want      int64
	}{
		{"capitalized attribute", "result = MY.For\n", 5},
		{"capitalized local", "For = 1\nresult = For + MY.For\n", 6},
		{"quoted lowercase local", "'for' = 2\nresult = 'for' + MY.For\n", 7},
		{"in an expression", "result = MY.For + 1\n", 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := runFor(t, tc.src, `[For = 5]`)
			wantInt(t, got, tc.want, "result")
		})
	}
}

// TestForIsBoundedByTheStepBudget pins termination at the existing bound: the loop adds no second
// limit, and a long enough sequence runs out of steps rather than forever.
func TestForIsBoundedByTheStepBudget(t *testing.T) {
	sp, err := ParseScript("n = 0\nfor x in MY.L:\n    n += x\nresult = n\n")
	if err != nil {
		t.Fatal(err)
	}
	ad, err := classad.Parse(`[L = {1, 1, 1, 1, 1, 1, 1, 1, 1, 1}]`)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := sp.RunLimited(ad, 5); ok {
		t.Errorf("a ten-element loop finished within 5 steps (%s)", describe(v))
	}
	v, ok := sp.RunLimited(ad, DefaultStepBudget)
	if !ok {
		t.Fatal("the loop did not finish within the default budget")
	}
	wantInt(t, v, 10, "result")
}

// TestForBuilderAndParserAgree pins that the parser is a transcription: the same loop built through
// the builder API computes the same thing.
func TestForBuilderAndParserAgree(t *testing.T) {
	seq, err := parser.ParseExpr("MY.L")
	if err != nil {
		t.Fatal(err)
	}
	zero, _ := parser.ParseExpr("0")
	add, _ := parser.ParseExpr("total + x")
	total, _ := parser.ParseExpr("total")
	built, err := NewScript().Let("total", zero).
		For("x", seq, func(s *Script) { s.Let("total", add) }).
		Result(total).Compile()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseScript("total = 0\nfor x in MY.L:\n    total = total + x\nresult = total\n")
	if err != nil {
		t.Fatal(err)
	}
	ad, err := classad.Parse(`[L = {4, 5, 6}]`)
	if err != nil {
		t.Fatal(err)
	}
	a, b := built.Run(ad), parsed.Run(ad)
	if !valuesEqual(a, b) {
		t.Errorf("builder = %s, parser = %s", describe(a), describe(b))
	}
	wantInt(t, a, 15, "result")
}

// TestForRefusesAfterResult pins the builder guard, which the parser's own ordering rule hides.
func TestForRefusesAfterResult(t *testing.T) {
	one, _ := parser.ParseExpr("1")
	seq, _ := parser.ParseExpr("{1, 2}")
	_, err := NewScript().Result(one).For("x", seq, nil).Compile()
	if err == nil {
		t.Fatal("expected a refusal for For after Result")
	}
	if !strings.Contains(err.Error(), "For after Result") {
		t.Errorf("refusal %q does not say what happened", err)
	}
}

// TestForRefusesAnEmptyName pins the other builder guard.
func TestForRefusesAnEmptyName(t *testing.T) {
	seq, _ := parser.ParseExpr("{1, 2}")
	one, _ := parser.ParseExpr("1")
	_, err := NewScript().For("", seq, nil).Result(one).Compile()
	if err == nil {
		t.Fatal("expected a refusal for an empty loop variable name")
	}
	if !strings.Contains(err.Error(), "empty loop variable name") {
		t.Errorf("refusal %q does not say what happened", err)
	}
}

// TestForDoesNotLeakItsOwnRegistersIntoTheBody is the scoping guard: the sequence, the length and
// the index live in registers the body cannot name.
func TestForDoesNotLeakItsOwnRegistersIntoTheBody(t *testing.T) {
	// The loop's registers are named for the register they start at, so the first loop in a script
	// with one prior local uses for#1. A body that spelled that name reaches the AD, not the
	// register -- which is undefined here, so the sum stays 3 rather than picking up a length.
	src := "n = 0\nfor x in MY.L:\n    n += 1\n    probe = 'for#1.n'\nresult = n\n"
	got, ok, f := runFor(t, src, `[L = {1, 2, 3}]`)
	if !ok || f != nil {
		t.Fatalf("ok=%v fault=%v", ok, f)
	}
	wantInt(t, got, 3, "result")
	sp, err := ParseScript(src)
	if err != nil {
		t.Fatal(err)
	}
	// The probe resolved against the AD, so the name is in the program's read set rather than
	// bound to the loop's register.
	found := false
	for _, name := range sp.Program().ReadAttrs() {
		if strings.EqualFold(name, "for#1.n") {
			found = true
		}
	}
	if !found {
		t.Error("the loop's own register name was readable from the body; it must not be")
	}
}

// TestForFaultNamesTheConstructTheAuthorWrote pins the relabelling, ordinals included.
//
// A `for` is lowered onto the counted `while`, which is what records the fault site. A diagnostic
// that said "while #2" for a loop the author wrote as a `for` would send them to the wrong line,
// and the ordinals of both kinds have to keep counting their own kind.
func TestForFaultNamesTheConstructTheAuthorWrote(t *testing.T) {
	src := `i = 0
while i < 1:
    i += 1
for a in MY.BadA:
    pass
j = 0
while j < 1:
    j += 1
for b in MY.BadB:
    pass
result = i
`
	sp, err := ParseScript(src)
	if err != nil {
		t.Fatal(err)
	}
	sites := sp.Program().faultSites
	want := []struct {
		kind string
		ord  int
	}{{"while", 1}, {"for", 1}, {"while", 2}, {"for", 2}}
	if len(sites) != len(want) {
		t.Fatalf("recorded %d fault sites, want %d", len(sites), len(want))
	}
	for i, w := range want {
		if sites[i].kind != w.kind || sites[i].ord != w.ord {
			t.Errorf("site %d is %s #%d, want %s #%d", i, sites[i].kind, sites[i].ord, w.kind, w.ord)
		}
	}
	// And the report a run produces names the first one that fires, which is the first `for`.
	_, _, f := runFor(t, src, `[BadA = "no"; BadB = {}]`)
	if f == nil || f.Where != "for #1 condition" {
		t.Errorf("fault = %v, want the first for", f)
	}
}

// TestForUsesTheWhileLowering states the reuse as a property rather than as a comment: a `for` and
// the counted `while` it desugars to compile to the same instruction sequence.
func TestForUsesTheWhileLowering(t *testing.T) {
	asFor, err := ParseScript("total = 0\nfor x in MY.L:\n    total += x\nresult = total\n")
	if err != nil {
		t.Fatal(err)
	}
	// There is exactly one back edge, and it is the one While emits.
	loops := 0
	for _, c := range asFor.Program().conds {
		if c.loop {
			loops++
		}
	}
	if loops != 1 {
		t.Errorf("a for emitted %d loops, want 1", loops)
	}
}
