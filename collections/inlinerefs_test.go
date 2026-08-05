package collections

import (
	"slices"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// newInlineColl opens a persistent (inline-name) collection, the storage a daemon runs and
// the one where reference chasing used to be a no-op.
func newInlineColl(t *testing.T) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir() + "/store", Shards: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if !c.inline {
		t.Skip("this store is not inline-name; the test targets that representation")
	}
	return c
}

// projectedNames returns the attribute names present in the one projected ad.
func projectedNames(t *testing.T, c *Collection, projection []string, chaseRefs bool) []string {
	t.Helper()
	var names []string
	n := 0
	for ra := range c.ScanRawProjected(projection, chaseRefs, false) {
		n++
		for _, e := range ra.Exprs {
			if name, _, ok := strings.Cut(string(e), " = "); ok {
				names = append(names, strings.TrimSpace(name))
			}
		}
	}
	if n != 1 {
		t.Fatalf("got %d ads, want 1", n)
	}
	slices.Sort(names)
	return names
}

// A projected expression's references come along, so the projected ad evaluates to the
// same answer the whole ad does. Without chasing, Req loses Memory and goes undefined --
// which is what a persistent store did regardless of the flag.
func TestInlineChaseRefsCarriesReferencedAttributes(t *testing.T) {
	c := newInlineColl(t)
	putAd(t, c, "a", "Memory = 2048\nReq = Memory > 1024\n")

	if got, want := projectedNames(t, c, []string{"Req"}, false), []string{"Req"}; !slices.Equal(got, want) {
		t.Errorf("without chasing got %v, want %v (the projection must be served exactly)", got, want)
	}
	if got, want := projectedNames(t, c, []string{"Req"}, true), []string{"Memory", "Req"}; !slices.Equal(got, want) {
		t.Errorf("with chasing got %v, want %v", got, want)
	}
}

// The projected ad must actually evaluate, which is the contract the flag promises.
func TestInlineChaseRefsProjectionEvaluates(t *testing.T) {
	c := newInlineColl(t)
	putAd(t, c, "big", "Memory = 2048\nReq = Memory > 1024\n")

	for ra := range c.ScanRawProjected([]string{"Req"}, true, false) {
		ad, err := classad.ParseOld(strings.Join(exprStrings(ra), "\n") + "\n")
		if err != nil {
			t.Fatalf("parsing the projected ad: %v", err)
		}
		b, err := ad.EvaluateAttr("Req").BoolValue()
		if err != nil || !b {
			t.Errorf("Req evaluated to %v, want true", ad.EvaluateAttr("Req"))
		}
	}
}

// References are followed transitively, and each pass adds strictly more, so a chain
// terminates rather than looping.
func TestInlineChaseRefsIsTransitive(t *testing.T) {
	c := newInlineColl(t)
	putAd(t, c, "a", "Base = 4\nMid = Base * 2\nTop = Mid > 4\n")

	got := projectedNames(t, c, []string{"Top"}, true)
	if want := []string{"Base", "Mid", "Top"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v (the chain was not followed all the way)", got, want)
	}
}

// A self-referential or mutually-referential ad must not spin: the wanted set only grows,
// and is capped by the ad's attribute count.
func TestInlineChaseRefsTerminatesOnCycles(t *testing.T) {
	c := newInlineColl(t)
	putAd(t, c, "a", "X = Y + 1\nY = X + 1\nZ = X\n")

	got := projectedNames(t, c, []string{"Z"}, true)
	if want := []string{"X", "Y", "Z"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Only references that resolve against this ad are chased; TARGET. and PARENT. name a
// different ad and must not drag attributes in.
func TestInlineChaseRefsIgnoresOtherScopes(t *testing.T) {
	c := newInlineColl(t)
	putAd(t, c, "a", "Memory = 2048\nReq = TARGET.Memory > 1024\n")

	if got, want := projectedNames(t, c, []string{"Req"}, true), []string{"Req"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v (a TARGET. reference must not pull in a local attribute)", got, want)
	}
}

// A reference to a private attribute is not a way around redaction.
func TestInlineChaseRefsRespectsRedaction(t *testing.T) {
	c := newInlineColl(t)
	putAd(t, c, "a", "ClaimId = \"secret\"\nReq = ClaimId =!= undefined\n")

	for ra := range c.ScanRawProjected([]string{"Req"}, true, true) {
		for _, e := range ra.Exprs {
			if strings.Contains(strings.ToLower(string(e)), "claimid = ") {
				t.Errorf("a redacted private attribute was pulled in as a reference: %q", e)
			}
		}
	}
}

// Chasing costs nothing when the projected attributes are literals: same result either way.
func TestInlineChaseRefsNoopForLiterals(t *testing.T) {
	c := newInlineColl(t)
	putAd(t, c, "a", "Owner = \"alice\"\nMemory = 2048\n")

	plain := projectedNames(t, c, []string{"Owner"}, false)
	chased := projectedNames(t, c, []string{"Owner"}, true)
	if !slices.Equal(plain, chased) {
		t.Errorf("literal projection differs: plain %v vs chased %v", plain, chased)
	}
}

func exprStrings(ra RawAd) []string {
	out := make([]string, 0, len(ra.Exprs))
	for _, e := range ra.Exprs {
		out = append(out, string(e))
	}
	return out
}
