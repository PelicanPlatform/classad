package vm

import (
	"regexp"
	"strings"
	"testing"
)

// TestAnchoredLiteralPrefix pins the pattern shapes that do and do not yield a literal
// prefix. Every accepted case is checked against the real regexp engine below
// (TestAnchoredPrefixIsNecessary); this test is about which shapes are recognized at all.
func TestAnchoredLiteralPrefix(t *testing.T) {
	cases := []struct {
		pat  string
		want string // "" means: no prefix
	}{
		{"^ap40", "ap40"},
		{"^ap40.*gpu", "ap40"},
		{"^slot1@", "slot1@"},
		{"^abc$", "abc"},
		{"^ab+c", "ab"},       // one repetition is mandatory
		{"^ab?c", "a"},        // the b is optional
		{"^ab*c", "a"},        // likewise
		{"^ab{2}c", "a"},      // conceded without reading the bounds
		{"^a[bc]d", "a"},      // a character class ends the run
		{"^a(bc)", "a"},       // so does a group
		{"^a\\.b", "a"},       // and an escape
		{"^héllo.*", "héllo"}, // multi-byte runes survive intact
		{"abc", ""},           // unanchored: matches anywhere
		{"^", ""},             // anchor alone prunes nothing
		{"^a*bc", ""},         // the whole literal run is optional
		{"^abc|def", ""},      // an alternation after the run breaks the guarantee
		{"^(a|b)c", ""},
		{"(?i)^abc", ""}, // inline flags before the anchor: not recognized
	}
	for _, c := range cases {
		got, ok := anchoredLiteralPrefix(c.pat)
		if c.want == "" {
			if ok {
				t.Errorf("anchoredLiteralPrefix(%q) = %q, want no prefix", c.pat, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("anchoredLiteralPrefix(%q) = %q,%v; want %q", c.pat, got, ok, c.want)
		}
	}
}

// TestAnchoredPrefixIsNecessary is the property the probe rests on: every string the
// pattern matches begins with the reported literal. It is checked against Go's own regexp
// engine -- the one the ClassAd regexp built-in uses -- over an alphabet dense in the
// characters the patterns are built from, so a pattern whose prefix is wrong is caught by
// a string that matches it.
func TestAnchoredPrefixIsNecessary(t *testing.T) {
	pats := []string{
		"^ab", "^ab.*cd", "^abc$", "^ab+c", "^ab?c", "^ab*c", "^ab{2}c",
		"^a[bc]d", "^a(bc)", "^a\\.b", "^a.b", "^aa+", "^ab{0,3}c", "^abc",
	}
	alphabet := []string{"a", "b", "c", "d", "."}
	var corpus []string
	var gen func(prefix string, depth int)
	gen = func(prefix string, depth int) {
		corpus = append(corpus, prefix)
		if depth == 0 {
			return
		}
		for _, ch := range alphabet {
			gen(prefix+ch, depth-1)
		}
	}
	gen("", 6)
	for _, pat := range pats {
		lit, ok := anchoredLiteralPrefix(pat)
		if !ok {
			continue
		}
		re := regexp.MustCompile(pat)
		matched := 0
		for _, s := range corpus {
			if !re.MatchString(s) {
				continue
			}
			matched++
			if !strings.HasPrefix(s, lit) {
				t.Errorf("pattern %q matches %q, which lacks the reported prefix %q", pat, s, lit)
			}
		}
		if matched == 0 {
			t.Errorf("pattern %q matched nothing in the corpus: the check is vacuous", pat)
		}
	}
}

func TestRegexpPrefixProbeShapes(t *testing.T) {
	probeOf := func(t *testing.T, src string) (Probe, bool) {
		t.Helper()
		q, err := Parse(src)
		if err != nil {
			t.Fatalf("compile %q: %v", src, err)
		}
		ps := q.Probes()
		for _, p := range ps {
			if p.Op == "prefix" {
				return p, true
			}
		}
		return Probe{}, false
	}
	t.Run("plain", func(t *testing.T) {
		p, ok := probeOf(t, `regexp("^ap40", GlobalJobId)`)
		if !ok {
			t.Fatal("no prefix probe")
		}
		if !strings.EqualFold(p.Attr, "GlobalJobId") {
			t.Errorf("attr = %q", p.Attr)
		}
		s, _ := p.Vals[0].StringValue()
		if s != "ap40" {
			t.Errorf("prefix = %q, want ap40", s)
		}
	})
	t.Run("case-insensitive option is still a prefix", func(t *testing.T) {
		if _, ok := probeOf(t, `regexp("^ap40", GlobalJobId, "i")`); !ok {
			t.Error("the i option should still yield a prefix probe (the index folds case anyway)")
		}
	})
	t.Run("multiline option is refused", func(t *testing.T) {
		// Under "m" the anchor matches at every line start, so the literal is not a prefix
		// of the value: `regexp("^b", "a\nb", "m")` is true.
		if p, ok := probeOf(t, `regexp("^b", Host, "m")`); ok {
			t.Errorf("multiline yielded a prefix probe %+v", p)
		}
	})
	t.Run("negation yields no probe", func(t *testing.T) {
		if p, ok := probeOf(t, `!regexp("^ap40", GlobalJobId)`); ok {
			t.Errorf("negated match yielded a prefix probe %+v", p)
		}
	})
	t.Run("argument order matters", func(t *testing.T) {
		// regexp is regexp(pattern, target): a pattern in the second slot is not ours.
		if p, ok := probeOf(t, `regexp(GlobalJobId, "^ap40")`); ok {
			t.Errorf("reversed arguments yielded a prefix probe %+v", p)
		}
	})
	t.Run("non-literal pattern yields no probe", func(t *testing.T) {
		if p, ok := probeOf(t, `regexp(Pattern, GlobalJobId)`); ok {
			t.Errorf("non-literal pattern yielded a prefix probe %+v", p)
		}
	})
	t.Run("other regexp functions are not probed", func(t *testing.T) {
		if p, ok := probeOf(t, `regexpMember("^ap40", Hosts)`); ok {
			t.Errorf("regexpMember yielded a prefix probe %+v", p)
		}
	})
	t.Run("conjunction keeps both probes", func(t *testing.T) {
		q, err := Parse(`Owner == "alice" && regexp("^ap40", GlobalJobId)`)
		if err != nil {
			t.Fatal(err)
		}
		var ops []string
		for _, p := range q.Probes() {
			ops = append(ops, p.Op)
		}
		if len(ops) != 2 {
			t.Fatalf("probes = %v, want an equality and a prefix", ops)
		}
	})
}

// TestPrefixProbeIsInexact guards the consumers that answer FROM the probes rather than
// filtering with them: a prefix probe covers more than its conjunct, so a query containing
// one must never be reported as exactly equivalent to its probes.
func TestPrefixProbeIsInexact(t *testing.T) {
	q, err := Parse(`regexp("^ap40.*gpu", GlobalJobId)`)
	if err != nil {
		t.Fatal(err)
	}
	probes, exact := q.ExactProbes()
	if len(probes) != 1 || probes[0].Op != "prefix" {
		t.Fatalf("probes = %+v, want one prefix probe", probes)
	}
	if exact {
		t.Error("a prefix probe was reported exact: a count answered from it would include every ap40 ad")
	}
}
