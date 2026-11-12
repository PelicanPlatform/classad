package classad

import "testing"

// TestBareScopeKeywordsResolveToAds pins the old-ClassAd-semantics keywords
// that name an ad rather than an attribute. The table is what condor_submit
// reports for the same expressions: each `request_<tag> = <keyword>` below was
// run through `condor_submit -dry-run`, and a tag whose keyword resolves to an
// ad gets NO `TARGET.<tag> >= Request<tag>` clause in Requirements, because
// SubmitHash::SetRequirements admits only an integer, a real or undefined.
//
// PARENT is in the table precisely because it is undefined at the top level --
// it was the one keyword the Go side already agreed with, and only by accident.
func TestBareScopeKeywordsResolveToAds(t *testing.T) {
	cases := []struct {
		expr    string
		wantAd  bool
		comment string
	}{
		{"MY", true, "the ad the reference is evaluated in"},
		{"my", true, "keywords are case-insensitive"},
		{"SELF", true, "a synonym for MY"},
		{"ROOT", true, "the outermost ad of the scope chain"},
		{"TOPLEVEL", true, "a synonym for ROOT"},
		{"PARENT", false, "no enclosing scope at the top level"},
		{"TARGET", false, "not a scope keyword; an ordinary lookup"},
	}
	for _, c := range cases {
		ad, err := Parse("[RequestA0 = " + c.expr + "; Other = 1]")
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.expr, err)
		}
		v := ad.EvaluateAttr("RequestA0")
		if gotAd := v.IsClassAd(); gotAd != c.wantAd {
			t.Errorf("%s: IsClassAd() = %v, want %v (%s); value = %s",
				c.expr, gotAd, c.wantAd, c.comment, v.String())
		}
		// Whatever else it is, a keyword naming an ad must not read as
		// undefined: that is the value condor_submit still builds a
		// requirements clause for, which is how this diverged.
		if c.wantAd && v.IsUndefined() {
			t.Errorf("%s: evaluated to undefined, want an ad", c.expr)
		}
	}
}

// TestBareScopeKeywordShadowedByAttribute checks the lookup order: the
// reference engine tries an ordinary lookup at each scope level before it
// considers the keyword, so an attribute of the same name wins.
func TestBareScopeKeywordShadowedByAttribute(t *testing.T) {
	ad, err := Parse("[my = 7; self = 8; root = 9; parent = 10; A = my; B = self; C = root; D = parent]")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for attr, want := range map[string]int64{"A": 7, "B": 8, "C": 9, "D": 10} {
		v := ad.EvaluateAttr(attr)
		got, err := v.IntValue()
		if err != nil {
			t.Errorf("%s: IntValue() error %v (value = %s)", attr, err, v.String())
			continue
		}
		if got != want {
			t.Errorf("%s = %d, want %d (the attribute, not the scope keyword)", attr, got, want)
		}
	}
}

// TestBareParentResolvesToEnclosingAd covers the other half of PARENT: inside a
// nested ad it names the enclosing one, where at the top level it is undefined.
// The scope chain is established by the select operator, so the reference has
// to be reached through one.
func TestBareParentResolvesToEnclosingAd(t *testing.T) {
	ad, err := Parse("[Outer = 1; Inner = [Got = PARENT]; Reached = Inner.Got]")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := ad.EvaluateAttr("Reached")
	if !v.IsClassAd() {
		t.Fatalf("Inner.Got = %s, want the enclosing ad", v.String())
	}
	// The enclosing ad is the outer one, which is the ad that defines Outer.
	inner, err := v.ClassAdValue()
	if err != nil {
		t.Fatalf("ClassAdValue: %v", err)
	}
	if got := inner.EvaluateAttr("Outer"); got.IsUndefined() {
		t.Errorf("PARENT resolved to an ad without Outer: %s", v.String())
	}
}
