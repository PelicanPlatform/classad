package vm

import (
	"errors"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// mustAd parses a scope for a test.
func mustAd(t *testing.T, src string) *classad.ClassAd {
	t.Helper()
	ad, err := classad.Parse(src)
	if err != nil {
		t.Fatalf("parse scope %s: %v", src, err)
	}
	return ad
}

// mustScript parses and compiles a script for a test.
func mustScript(t *testing.T, src string) *ScriptProgram {
	t.Helper()
	sp, err := ParseScript(src)
	if err != nil {
		t.Fatalf("parse script:\n%s\n%v", src, err)
	}
	return sp
}

// defaultAndFloor is the script the language was designed around: read an attribute, supply a
// default when it is absent, and clamp it to a floor. It is three statements and uses every piece of
// the surface syntax that is not a loop.
const defaultAndFloor = `# Effective memory request, with a floor and an explicit absence case.
want = MY.RequestMemory
if want is undefined:
    want = 2048
if want < 512:
    want = 512
result = want
`

func TestParseScriptDefaultAndFloor(t *testing.T) {
	sp := mustScript(t, defaultAndFloor)
	cases := []struct {
		scope string
		want  string
	}{
		{`[RequestMemory = 4096]`, "4096"},
		{`[RequestMemory = 100]`, "512"},
		{`[RequestMemory = 512]`, "512"},
		{`[]`, "2048"},
		{`[Other = 1]`, "2048"},
	}
	for _, tc := range cases {
		if got := sp.Run(mustAd(t, tc.scope)).String(); got != tc.want {
			t.Errorf("against %s: got %s, want %s", tc.scope, got, tc.want)
		}
	}
}

// TestParseScriptMatchesBuilder is the test that says the parser is a transcription and not a second
// implementation. The two scripts are built by completely different routes -- one from text, one
// from Go calls -- and must be the same program, down to the locals it allocates and the value it
// computes against every scope.
func TestParseScriptMatchesBuilder(t *testing.T) {
	parsed := mustScript(t, defaultAndFloor)

	wantExpr, err := parser.ParseExpr("MY.RequestMemory")
	if err != nil {
		t.Fatal(err)
	}
	isUndef, err := parser.ParseExpr("want is undefined")
	if err != nil {
		t.Fatal(err)
	}
	belowFloor, err := parser.ParseExpr("want < 512")
	if err != nil {
		t.Fatal(err)
	}
	c2048, _ := parser.ParseExpr("2048")
	c512, _ := parser.ParseExpr("512")
	readWant, _ := parser.ParseExpr("want")

	built, err := NewScript().
		Let("want", wantExpr).
		If(isUndef, func(s *Script) { s.Let("want", c2048) }, nil).
		If(belowFloor, func(s *Script) { s.Let("want", c512) }, nil).
		Result(readWant).
		Compile()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if a, b := parsed.Locals(), built.Locals(); strings.Join(a, ",") != strings.Join(b, ",") {
		t.Errorf("locals differ: parsed %v, built %v", a, b)
	}
	if a, b := parsed.RegProgram().Len(), built.RegProgram().Len(); a != b {
		t.Errorf("instruction counts differ: parsed %d, built %d", a, b)
	}
	for _, src := range []string{
		`[RequestMemory = 4096]`, `[RequestMemory = 100]`, `[]`,
		`[RequestMemory = "huge"]`, `[RequestMemory = RequestMemory]`,
	} {
		scope := mustAd(t, src)
		a, b := parsed.Run(scope), built.Run(scope)
		if !valuesEqual(a, b) {
			t.Errorf("against %s: parsed %s, built %s", src, describe(a), describe(b))
		}
	}
}

// TestParseScriptWhile exercises the one construct an expression cannot express at all: a loop whose
// trip count depends on the value it is computing.
func TestParseScriptWhile(t *testing.T) {
	// Halve until it fits, counting the halvings -- two locals carried across the back edge.
	sp := mustScript(t, `size = MY.Bytes
steps = 0
while size > 1024:
    size = size / 2
    steps += 1
result = steps
`)
	cases := []struct {
		scope string
		want  string
	}{
		{`[Bytes = 1024]`, "0"},
		{`[Bytes = 2048]`, "1"},
		{`[Bytes = 1048576]`, "10"},
	}
	for _, tc := range cases {
		if got := sp.Run(mustAd(t, tc.scope)).String(); got != tc.want {
			t.Errorf("against %s: got %s, want %s", tc.scope, got, tc.want)
		}
	}

	// A loop that never runs leaves its locals exactly as the statements before it left them.
	sp2 := mustScript(t, "n = 5\nwhile false:\n    n = 0\nresult = n\n")
	if got := sp2.Run(mustAd(t, `[]`)).String(); got != "5" {
		t.Errorf("while false: got %s, want 5", got)
	}
}

// TestParseScriptElifChain checks that an elif cascade picks exactly one arm, including the case
// where no arm matches and the chain has no else.
func TestParseScriptElifChain(t *testing.T) {
	sp := mustScript(t, `tier = "none"
if MY.Cpus >= 64:
    tier = "huge"
elif MY.Cpus >= 16:
    tier = "large"
elif MY.Cpus >= 4:
    tier = "medium"
else:
    tier = "small"
result = tier
`)
	cases := []struct{ scope, want string }{
		{`[Cpus = 128]`, `"huge"`},
		{`[Cpus = 16]`, `"large"`},
		{`[Cpus = 4]`, `"medium"`},
		{`[Cpus = 1]`, `"small"`},
	}
	for _, tc := range cases {
		if got := sp.Run(mustAd(t, tc.scope)).String(); got != tc.want {
			t.Errorf("against %s: got %s, want %s", tc.scope, got, tc.want)
		}
	}

	// `pass` is a suite that does nothing, which is a block and not a missing one. It is the way to
	// say that a case is handled by doing nothing on purpose.
	sp3 := mustScript(t, "t = 1\nif MY.Cpus > 2:\n    pass\nelse:\n    t = 2\nresult = t\n")
	if got := sp3.Run(mustAd(t, `[Cpus = 4]`)).String(); got != "1" {
		t.Errorf("pass arm: got %s, want 1", got)
	}
	if got := sp3.Run(mustAd(t, `[Cpus = 1]`)).String(); got != "2" {
		t.Errorf("else arm: got %s, want 2", got)
	}

	// No else, and no arm taken: the local keeps the value the statements before the chain gave it.
	sp2 := mustScript(t, "t = 0\nif MY.Cpus > 100:\n    t = 1\nelif MY.Cpus > 50:\n    t = 2\nresult = t\n")
	if got := sp2.Run(mustAd(t, `[Cpus = 4]`)).String(); got != "0" {
		t.Errorf("no arm taken: got %s, want 0", got)
	}
}

// TestParseScriptBindingModel is the builder's binding-model test asked again through the syntax,
// because the syntax is where a reader forms the expectation that the compiler then has to meet.
func TestParseScriptBindingModel(t *testing.T) {
	scope := mustAd(t, `[ x = 10; Owner = "alice" ]`)
	cases := []struct{ src, want string }{
		// The right-hand side reads the ATTRIBUTE x; the result reads the LOCAL.
		{"x = x + 1\nresult = x\n", "11"},
		// A scoped reference is always the attribute, however the name is bound.
		{"x = x + 1\nresult = MY.x\n", "10"},
		// Locals fold case exactly as attribute names do.
		{"Foo = 41 + 1\nresult = fOO\n", "42"},
		// An unbound name is an attribute, exactly as in a bare expression.
		{"other = 1\nresult = Owner\n", `"alice"`},
		// Rebinding: the second statement reads the first binding.
		{"x = 1\nx = x + 1\nx = x + 1\nresult = x\n", "3"},
		// Augmented assignment is the same rule written shorter: the first `+=` reads the attribute.
		{"x += 1\nresult = x\n", "11"},
		{"y = 2\ny *= 5\nresult = y\n", "10"},
	}
	for _, tc := range cases {
		if got := mustScript(t, tc.src).Run(scope).String(); got != tc.want {
			t.Errorf("%q => %s, want %s", tc.src, got, tc.want)
		}
	}
}

// TestParseScriptSoftKeywords pins that a statement keyword is a keyword only where a statement can
// start, and only in lowercase. ClassAd attribute names are case-insensitive, so an ad in the wild
// may well have an attribute called `If` or `Result`, and a script that could not read one would be
// a script that could not be used on real data.
func TestParseScriptSoftKeywords(t *testing.T) {
	scope := mustAd(t, `[ If = 7; Result = 9; For = 11; While = 13 ]`)
	cases := []struct{ src, want string }{
		{"a = MY.If + MY.Result\nresult = a\n", "16"},
		// Not statement-leading, so they lex as ordinary identifiers.
		{"a = 1 + if\nresult = a\n", "8"},
		{"a = while + for\nresult = a\n", "24"},
		// Capitalized, so not keywords even in statement-leading position: these are locals.
		{"If = 1\nResult = If + 1\nresult = Result\n", "2"},
		// The escape hatch for a local that really must be spelled in lowercase.
		{"'for' = 100\nresult = 'for' + 1\n", "101"},
	}
	for _, tc := range cases {
		if got := mustScript(t, tc.src).Run(scope).String(); got != tc.want {
			t.Errorf("%q => %s, want %s", tc.src, got, tc.want)
		}
	}
}

// TestParseScriptLineStructure covers the lexical contract: where a statement ends, and the three
// ways it can be made not to end there.
func TestParseScriptLineStructure(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			// An ad literal across lines, semicolons and all. The newline inside `[` emits no
			// statement end, which is the whole reason one bracket counter covers all three.
			"ad literal spans lines",
			"result = MY.Tries < [\n    MaxRetries = 3;\n    OnFailure = \"hold\"\n].MaxRetries\n",
			"true",
		},
		{
			"list literal spans lines",
			"sizes = {\n    1,\n    2,\n    3\n}\nresult = size(sizes)\n",
			"3",
		},
		{
			"call spans lines",
			"n = strcat(\n    \"a\",\n    \"b\"\n)\nresult = n\n",
			`"ab"`,
		},
		{
			"explicit line join",
			"n = 1 + \\\n    2\nresult = n\n",
			"3",
		},
		{
			"hash comment at end of line",
			"n = 1  # the count\nresult = n + 1 # and one more\n",
			"2",
		},
		{
			"classad comments survive",
			"n = 1 // one\nresult = n /* plus */ + 1\n",
			"2",
		},
		{
			"blank and comment-only lines carry no indentation",
			"n = 1\n\n   # indented comment, not a block\n\nresult = n\n",
			"1",
		},
		{
			// A block comment in front of the code is whitespace, not indentation: the statement
			// after it is still in column 1, and a rule that counted it would put the next line in
			// a block that does not exist.
			"leading block comment is not indentation",
			"/* note */ n = 1\nif n > 0:\n    /* here too */ n = 2\nresult = n\n",
			"2",
		},
		{
			"CRLF line endings",
			"n = 1\r\nif n > 0:\r\n    n = 2\r\nresult = n\r\n",
			"2",
		},
	}
	scope := mustAd(t, `[Tries = 1]`)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustScript(t, tc.src).Run(scope).String(); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestParseScriptRejectsAmbiguousIndentation is the tab rule, which exists because a misread indent
// does not produce a wrong value -- it produces a statement in a different block, which is a
// different program. A width guess would make that silent.
func TestParseScriptRejectsAmbiguousIndentation(t *testing.T) {
	cases := []struct{ name, src string }{
		{"tab indents a block", "if MY.x:\n\ty = 1\nresult = y\n"},
		{"tab after spaces", "if MY.x:\n    y = 1\n  \tz = 2\nresult = y\n"},
		{"tab before spaces", "if MY.x:\n\t    y = 1\nresult = y\n"},
		{"tab indents a top-level statement", "x = 1\n\ty = 2\nresult = y\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseScript(tc.src)
			if err == nil {
				t.Fatal("expected a rejection")
			}
			if !strings.Contains(err.Error(), "tab in indentation") {
				t.Errorf("wrong complaint: %v", err)
			}
		})
	}

	// A tab that is NOT in the indentation is ordinary whitespace, because the ambiguity is about
	// where a block begins and nothing else.
	if got := mustScript(t, "x\t=\t1\nresult\t=\tx\n").Run(mustAd(t, `[]`)).String(); got != "1" {
		t.Errorf("tab inside a line: got %s, want 1", got)
	}
}

// TestParseScriptErrors checks that every refusal names a position and says what was expected. A
// parser whose errors say only "syntax error" is one people route around.
func TestParseScriptErrors(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		line     int
		col      int
		contains string
	}{
		{"missing block colon", "if MY.x > 1\n    y = 1\nresult = y\n", 1, 12, "expected ':' at the end of the 'if' header"},
		{"missing while colon", "while MY.x\n    y = 1\nresult = y\n", 1, 11, "expected ':' at the end of the 'while' header"},
		{"elvis is not a block colon", "if MY.x ?:\n    y = 1\nresult = y\n", 1, 11, "expected ':' at the end of the 'if' header"},
		{"no suite", "if MY.x > 1:\nresult = 1\n", 1, 13, "expected an indented block after 'if'"},
		{"no condition", "if :\n    y = 1\nresult = y\n", 1, 3, "expected a condition after 'if'"},
		{"stray indent", "x = 1\n    y = 2\nresult = y\n", 2, 5, "unexpected indent"},
		{"first line indented", "    x = 1\nresult = x\n", 1, 5, "unexpected indent"},
		{"bad dedent", "if MY.x:\n    y = 1\n  z = 2\nresult = y\n", 3, 3, "unindent does not match any outer indentation level"},
		{"elif with no if", "x = 1\nelif MY.x:\n    y = 1\nresult = y\n", 2, 1, "'elif' without a matching 'if'"},
		{"else with no if", "x = 1\nelse:\n    y = 1\nresult = y\n", 2, 1, "'else' without a matching 'if'"},
		{"else with trailing junk", "if MY.x:\n    y = 1\nelse y:\n    y = 2\nresult = y\n", 3, 5, "expected ':' directly after 'else'"},
		{"no result", "x = 1\n", 2, 1, "the script has no 'result' statement"},
		{"empty script", "\n# nothing here\n", 1, 1, "the script is empty"},
		{"statements after result", "result = 1\nx = 2\n", 2, 1, "no statements may follow 'result'"},
		{"result inside a block", "if MY.x:\n    result = 1\nresult = 2\n", 2, 5, "'result' must be a top-level statement"},
		{"not a statement", "x = 1\nfoo(x)\nresult = x\n", 2, 1, "expected a statement"},
		// A comparison is an expression, and an expression on its own is not a statement. These two
		// are the mistakes that look most like assignments, so the operator has to be told apart
		// from the assignment rather than merely found.
		{"equality is not an assignment", "x = 1\nx == 2\nresult = x\n", 2, 1, "expected a statement"},
		{"comparison is not an assignment", "x = 1\nx >= 2\nresult = x\n", 2, 1, "expected a statement"},
		{"ad write", "MY.Foo = 1\nresult = 1\n", 1, 1, "writing to the ad is not implemented"},
		{"dynamic write", "self[\"a\"] = 1\nresult = 1\n", 1, 1, "writing to the ad is not implemented"},
		{"for without in", "for x MY.L:\n    y = x\nresult = y\n", 1, 7, "expected 'in' after the loop variable"},
		{"for without a variable", "for in MY.L:\n    y = 1\nresult = y\n", 1, 5, "expected a loop variable before 'in'"},
		{"for without a sequence", "for x in:\n    y = 1\nresult = y\n", 1, 9, "expected a sequence after 'in'"},
		{"for without a colon", "for x in MY.L\n    y = x\nresult = y\n", 1, 14, "expected ':' at the end of the 'for' header"},
		{"for without a body", "for x in MY.L:\nresult = 1\n", 1, 15, "expected an indented block after 'for'"},
		{"for target is not a name", "for x.y in MY.L:\n    z = 1\nresult = z\n", 1, 6, "expected 'in' after the loop variable"},
		{"break outside a loop", "break\nresult = 1\n", 1, 1, "'break' outside a loop"},
		{"continue outside a loop", "continue\nresult = 1\n", 1, 1, "'continue' outside a loop"},
		{"junk after break", "while MY.x:\n    break 1\nresult = 1\n", 2, 10, "expected end of line after 'break'"},
		{"junk after continue", "while MY.x:\n    continue 1\nresult = 1\n", 2, 13, "expected end of line after 'continue'"},
		{"def inside a block", "if MY.x:\n    def f(a):\n        return a\nresult = 1\n", 2, 5, "'def' must be a top-level statement"},
		{"return outside a function", "return 1\nresult = 1\n", 1, 1, "'return' outside a function body"},
		{"junk after pass", "if MY.x:\n    pass 1\nresult = 1\n", 2, 9, "expected end of line after 'pass'"},
		{"unclosed bracket", "x = (1 + 2\nresult = x\n", 1, 5, "unclosed '('"},
		{"unterminated string", "x = \"abc\nresult = x\n", 1, 5, "unterminated string literal"},
		{"block comment spans lines", "x = 1 /* two\nlines */ + 1\nresult = x\n", 1, 7, "may not span a line break"},
		{"expression error carries the script position", "x = 1\ny = 2\nresult = y +\n", 3, 12, "syntax error"},
		{"quoted name hint", "x = 'alice' +\nresult = x\n", 1, 13, "quoted attribute name"},
		{"local in a conditional", "x = MY.Cpus\nresult = x ? 1 : 2\n", 2, 1, `reads the local "x"`},
		{"local in a list literal", "x = MY.Cpus\nresult = {x, 1}\n", 2, 1, `reads the local "x"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseScript(tc.src)
			if err == nil {
				t.Fatal("expected a rejection")
			}
			var se *ScriptSyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("not a *ScriptSyntaxError: %T %v", err, err)
			}
			if se.Line != tc.line || se.Column != tc.col {
				t.Errorf("position %d:%d, want %d:%d (%v)", se.Line, se.Column, tc.line, tc.col, err)
			}
			if !strings.Contains(se.Msg, tc.contains) {
				t.Errorf("message %q does not mention %q", se.Msg, tc.contains)
			}
			// An error over a line of source renders that line with a caret under the column, so a
			// caller that prints it needs to know nothing about the script's layout. An error past
			// the end of the input has no line to point at and renders as a sentence.
			if rendered := err.Error(); se.Source != "" && !strings.Contains(rendered, "^") {
				t.Errorf("no caret in rendered error:\n%s", rendered)
			}
		})
	}
}

// TestParseScriptUnterminatedAtEndOfInput covers the lexical failures that run off the end of the
// source rather than off the end of a line. They take a different branch from their mid-file
// equivalents -- the scanner stops because there is no more input, not because it found a break --
// and that branch is the one a file whose last line was being edited will reach.
func TestParseScriptUnterminatedAtEndOfInput(t *testing.T) {
	cases := []struct{ name, src, contains string }{
		{"unclosed bracket, no trailing newline", "x = (1 + 2", "unclosed '('"},
		{"unterminated string, no trailing newline", `x = "abc`, "unterminated string literal"},
		{"unterminated quoted name, no trailing newline", "x = 'abc", "unterminated quoted attribute name"},
		{"unterminated quoted name at a line break", "x = 'abc\nresult = x\n", "unterminated quoted attribute name"},
		{"unterminated block comment", "x = 1 /* oops", "unterminated /* comment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseScript(tc.src)
			if err == nil {
				t.Fatal("expected a rejection")
			}
			var se *ScriptSyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("not a *ScriptSyntaxError: %T %v", err, err)
			}
			if se.Line < 1 || se.Column < 1 {
				t.Errorf("error without a position: %d:%d", se.Line, se.Column)
			}
			if !strings.Contains(se.Msg, tc.contains) {
				t.Errorf("message %q does not mention %q", se.Msg, tc.contains)
			}
		})
	}
}

// TestParseScriptAssignmentTargets covers what may and may not stand on the left of an assignment.
// The quoted form is the only way to bind a local whose name is a lowercase keyword, so its failure
// modes are worth pinning as precisely as its successes.
func TestParseScriptAssignmentTargets(t *testing.T) {
	bad := []struct{ name, src, contains string }{
		{"two words", "x y = 1\nresult = x\n", "expected a local name"},
		{"nothing at all", "= 1\nresult = 1\n", "expected a local name"},
		{"empty quoted name", "'' = 1\nresult = 1\n", "expected a quoted attribute name"},
		{"trailing junk after a quoted name", "'a'b = 1\nresult = 1\n", "expected a quoted attribute name"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseScript(tc.src)
			if err == nil {
				t.Fatal("expected a rejection")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("message %v does not mention %q", err, tc.contains)
			}
		})
	}

	good := []struct{ name, src, want string }{
		// The assignment operator is found at bracket depth zero and outside quotes, so neither an
		// `=` inside a string nor the ones inside an ad literal can be mistaken for it.
		{"equals inside a string", "x = \"a=b\"\nresult = x\n", `"a=b"`},
		{"equals inside an ad literal", "policy = [ a = 1; b = 2 ]\nresult = isClassAd(policy)\n", "true"},
		{"escaped quote in a quoted name", "'a\\'b' = 41\nresult = 'a\\'b' + 1\n", "42"},
		// A comment with no newline after it is the end of the input, not the end of a line.
		{"comment at end of input", "result = 1\n# done", "1"},
		// Nor does the last statement need a line terminator to be a statement.
		{"no trailing newline", "x = 41\nresult = x + 1", "42"},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustScript(t, tc.src).Run(mustAd(t, `[]`)).String(); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestScriptLocalCaptureIsTyped checks the refusal a caller might want to recognize rather than
// print: a local read from inside a construct that is evaluated against the ad.
func TestScriptLocalCaptureIsTyped(t *testing.T) {
	// Through the builder, where there is no position to attach and the error arrives bare.
	x, err := parser.ParseExpr("MY.Cpus")
	if err != nil {
		t.Fatal(err)
	}
	res, err := parser.ParseExpr("x ? 1 : 2")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewScript().Let("x", x).Result(res).Compile()
	var lc *LocalCaptureError
	if !errors.As(err, &lc) {
		t.Fatalf("got %v, want a *LocalCaptureError", err)
	}
	if lc.Local != "x" {
		t.Errorf("Local = %q, want x", lc.Local)
	}
	// Printed bare it names the package, the local and the subtree, because a builder caller has no
	// line to be told about and the subtree is the only handle they have on it.
	if msg := lc.Error(); !strings.HasPrefix(msg, "vm: script: ") || !strings.Contains(msg, "x ? 1 : 2") {
		t.Errorf("Error() = %q", msg)
	}

	// The same expression with no local in it compiles, because nothing is captured: the delegated
	// subtree reads attributes, which is what a delegated subtree is for.
	res2, err := parser.ParseExpr("Cpus ? 1 : 2")
	if err != nil {
		t.Fatal(err)
	}
	if _, cerr := NewScript().Let("x", x).Result(res2).Compile(); cerr != nil {
		t.Errorf("a conditional over attributes should compile: %v", cerr)
	}

	// And a scoped reference is an attribute, so it is not a capture either.
	res3, err := parser.ParseExpr("MY.x ? 1 : 2")
	if err != nil {
		t.Fatal(err)
	}
	if _, cerr := NewScript().Let("x", x).Result(res3).Compile(); cerr != nil {
		t.Errorf("MY.x inside a conditional should compile: %v", cerr)
	}
}

// TestParseScriptFaults checks that a condition which is neither true nor false stops the script and
// is reported as a fault, rather than being quietly treated as false. A script runs once, so a
// branch silently not taken leaves no second record to notice.
func TestParseScriptFaults(t *testing.T) {
	sp := mustScript(t, "n = 0\nif MY.Missing > 1:\n    n = 1\nresult = n\n")
	val, ok, fault := sp.RunWithFault(mustAd(t, `[]`))
	if !ok {
		t.Fatal("the script should have finished")
	}
	if fault == nil {
		t.Fatalf("expected a fault, got value %s", describe(val))
	}
	if fault.Class != FaultCondition {
		t.Errorf("fault class %v, want %v", fault.Class, FaultCondition)
	}
	if !val.IsError() {
		t.Errorf("a faulted script yields %s, want error", describe(val))
	}

	// A while whose condition is a string is the same fault: no truthiness rule covers it.
	sp2 := mustScript(t, "n = 0\nwhile MY.Owner:\n    n = 1\nresult = n\n")
	if _, _, f := sp2.RunWithFault(mustAd(t, `[Owner = "alice"]`)); f == nil {
		t.Error("a string condition should fault")
	}
}

// TestParseScriptReduction asks the reduction law of the parser: a script of local bindings and a
// result equals classad.Eval of the inlined expression. It is the same law the builder is fuzzed
// against, which is the point -- the parser inherits an oracle rather than acquiring one.
func TestParseScriptReduction(t *testing.T) {
	scope := mustAd(t, `[Cpus = 4; Memory = 2048; Owner = "alice"; R = 2.5; Flag = true]`)
	// The local names are deliberately unlikely: the law holds only when the bindings capture no
	// name the operands themselves use, and that is its precondition rather than a bug to find.
	cases := []struct{ a, b, result string }{
		{"Cpus", "Memory", "zzA + zzB"},
		{"Cpus + 1", "Cpus / 0", "zzA + zzB"},
		{"Missing", "Cpus", "zzA ?: zzB"},
		{"undefined", "error", "zzA =?= zzB"},
		{"toUpper(Owner)", "Owner", "zzA + zzB"},
		{"Cpus", "Memory", "(zzA + zzB) > zzA"},
		{"Cpus", "Cpus", "zzA + zzA + zzB"},
		{"Flag", "Cpus > 2", "zzA && zzB"},
		{"9223372036854775807", "1", "zzA + zzB"},
		{"Cpus", "Memory", "Cpus + zzA + zzB"},
	}
	for _, tc := range cases {
		src := "zzA = " + tc.a + "\nzzB = " + tc.b + "\nresult = " + tc.result + "\n"
		sp, err := ParseScript(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		inlined := strings.NewReplacer("zzA", "("+tc.a+")", "zzB", "("+tc.b+")").Replace(tc.result)
		ref, err := parser.ParseExpr(inlined)
		if err != nil {
			t.Fatalf("parse inlined %q: %v", inlined, err)
		}
		got, want := sp.Run(scope), refEval(scope, ref)
		if !valuesEqual(got, want) {
			t.Errorf("script %q = %s, inlined %q = %s", src, describe(got), inlined, describe(want))
		}
	}
}

// FuzzParseScript checks only that the parser terminates with either a program or a positioned
// error, never a panic. The interesting invariants are elsewhere; this guards the part of a parser
// that is reached by input nobody wrote on purpose.
func FuzzParseScript(f *testing.F) {
	for _, s := range []string{
		defaultAndFloor,
		"result = 1\n",
		"x = 1\nwhile x < 10:\n    x += 1\nresult = x\n",
		"if MY.a:\n    b = 1\nelif MY.c:\n    b = 2\nelse:\n    b = 3\nresult = b\n",
		"x = [\n a = 1;\n b = 2\n]\nresult = x.a\n",
		"x = \\\n 1\nresult = x\n",
		"#\n:\n'\n\"\n/*\n\t\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		sp, err := ParseScript(src)
		switch {
		case err != nil:
			var se *ScriptSyntaxError
			if errors.As(err, &se) {
				if se.Line < 1 || se.Column < 1 {
					t.Fatalf("error without a position: %d:%d %q", se.Line, se.Column, se.Msg)
				}
			}
		case sp == nil:
			t.Fatal("neither a program nor an error")
		default:
			sp.RunLimited(mustAd(t, `[Cpus = 4; Owner = "alice"]`), 10000)
		}
	})
}
