package vm

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// regexpPrefixProbe recognizes `regexp("^literal...", attr)` -- an anchored regular
// expression whose match therefore begins with a known literal string -- and yields a
// "prefix" probe on attr. A categorical index answers it by walking the run of sorted
// keys that start with the literal (see catUsable), which is the only index shape a
// pattern match can use at all.
//
// The probe is a NECESSARY condition, never a sufficient one: `^ap40.*gpu` prunes to the
// ads whose value starts with "ap40" and the store then evaluates the whole pattern over
// them. That is the usual probe contract, so everything after the literal prefix -- the
// rest of the pattern, its case sensitivity, its character classes -- costs selectivity
// and nothing else. probeIsExact reports a prefix probe as inexact for the consumers that
// answer FROM probes rather than filtering with them.
//
// Only the two-argument form and the case/dotall options are recognized. MULTILINE IS
// REFUSED: under "m" the `^` matches at every line start, so `regexp("^b", "a\nb", "m")`
// is TRUE for a value that does not begin with "b", and the probe would be a filter that
// drops matching records.
func (pc probeCtx) regexpPrefixProbe(fc *ast.FunctionCall) (Probe, bool) {
	if !strings.EqualFold(fc.Name, "regexp") || len(fc.Args) < 2 || len(fc.Args) > 3 {
		return Probe{}, false
	}
	if len(fc.Args) == 3 {
		opts, ok := stringLit(fc.Args[2])
		if !ok || strings.ContainsAny(opts, "mM") {
			return Probe{}, false
		}
	}
	pat, ok := stringLit(fc.Args[0])
	if !ok {
		return Probe{}, false
	}
	t, ok := pc.probeTargetOf(fc.Args[1])
	if !ok {
		return Probe{}, false
	}
	lit, ok := anchoredLiteralPrefix(pat)
	if !ok {
		return Probe{}, false
	}
	return Probe{Attr: t.name, Op: "prefix", Vals: []classad.Value{classad.NewStringValue(lit)}, Path: t.path}, true
}

// anchoredLiteralPrefix returns the literal string every match of pat must begin with, or
// ok=false when the pattern guarantees no such prefix.
//
// It reads the pattern rather than the compiled program, so it is deliberately simple and
// deliberately pessimistic: pat must start with `^`, and the prefix is the run of ordinary
// characters that follows, ending at the first regular-expression metacharacter.
//
// A metacharacter that can make the PRECEDING character optional gives that character back:
// `^ab?c` and `^ab*c` and `^ab{0,2}c` all guarantee only "a". `+` does not, since one
// repetition is mandatory, so `^ab+c` keeps "ab". `{` is treated as optional-making without
// reading its bounds -- `^ab{2}c` really does guarantee "ab", but parsing the repetition
// counts is more pattern syntax than this is worth, and conceding the character costs only
// selectivity.
//
// An alternation anywhere in the pattern abandons the whole attempt: `^abc|def` matches
// "def", which the leading `^abc` says nothing about. Checking for `|` across the entire
// pattern -- not just the literal run -- is what makes that sound, because the alternation
// that breaks the guarantee sits after the run, not inside it.
func anchoredLiteralPrefix(pat string) (string, bool) {
	if !strings.HasPrefix(pat, "^") || strings.ContainsRune(pat, '|') {
		return "", false
	}
	var lit []rune
	for _, r := range pat[1:] {
		if !isRegexpMeta(r) {
			lit = append(lit, r)
			continue
		}
		// A quantifier governs the character just appended, which is therefore not
		// guaranteed to appear at all.
		if (r == '*' || r == '?' || r == '{') && len(lit) > 0 {
			lit = lit[:len(lit)-1]
		}
		break
	}
	if len(lit) == 0 {
		return "", false // `^` alone, or a quantified first character: prunes nothing
	}
	return string(lit), true
}

// isRegexpMeta reports whether r ends a literal run in a pattern. `}` and `]` are listed
// too: they are literal outside their constructs, but a run that reached one in a pattern
// this simple-minded is better abandoned than guessed at.
func isRegexpMeta(r rune) bool {
	switch r {
	case '\\', '.', '+', '*', '?', '(', ')', '[', ']', '{', '}', '^', '$', '|':
		return true
	}
	return false
}

// stringLit returns a string literal argument's value.
func stringLit(e ast.Expr) (string, bool) {
	if s, ok := unparen(e).(*ast.StringLiteral); ok {
		return s.Value, true
	}
	return "", false
}
