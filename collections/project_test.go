package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func putAd(t *testing.T, c *Collection, key, text string) {
	t.Helper()
	ad, err := classad.ParseOld(text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	if err := c.Put([]byte(key), ad); err != nil {
		t.Fatal(err)
	}
}

// TestQueryProject checks the wire-native projected scan: literal attributes read
// straight from the wire, a non-literal (expression) attribute forces a decode
// fallback for that ad, and a missing attribute yields undefined.
func TestQueryProject(t *testing.T) {
	c := New(Options{})
	putAd(t, c, "1", "RequestCpus = 4\nOwner = \"alice\"\nTotal = RequestCpus * 2")
	putAd(t, c, "2", "RequestCpus = 8\nOwner = \"bob\"") // no Total

	got := map[string][]classad.Value{}
	for vals := range c.QueryProject(mustQuery(t, "RequestCpus >= 1"), []string{"RequestCpus", "Owner", "Total"}) {
		owner, _ := vals[1].StringValue()
		// Copy the values (the slice is reused across iterations).
		got[owner] = []classad.Value{vals[0], vals[1], vals[2]}
	}
	if len(got) != 2 {
		t.Fatalf("projected %d rows, want 2", len(got))
	}

	// alice: literal RequestCpus (wire-native) + Total via decode fallback (4*2=8).
	a := got["alice"]
	if v, _ := a[0].IntValue(); v != 4 {
		t.Errorf("alice RequestCpus = %v, want 4", v)
	}
	if v, _ := a[2].IntValue(); v != 8 {
		t.Errorf("alice Total (expression, decode fallback) = %v, want 8", v)
	}

	// bob: Total is missing -> undefined.
	b := got["bob"]
	if v, _ := b[0].IntValue(); v != 8 {
		t.Errorf("bob RequestCpus = %v, want 8", v)
	}
	if !b[2].IsUndefined() {
		t.Errorf("bob Total = %v, want undefined", b[2])
	}
}

// TestQueryProjectMatchesQuery cross-checks QueryProject against the full-decode
// Query path over the same constraint, for a plain literal attribute.
func TestQueryProjectMatchesQuery(t *testing.T) {
	c := New(Options{})
	for i := 0; i < 100; i++ {
		putAd(t, c, string(rune('a'+i%26))+string(rune('0'+i%10)),
			classadText(i))
	}
	q := mustQuery(t, "Cpus >= 4")

	var viaQuery int
	for ad := range c.Query(q) {
		if v, _ := ad.EvaluateAttr("Cpus").IntValue(); v >= 4 {
			viaQuery++
		}
	}
	var viaProject int
	for vals := range c.QueryProject(q, []string{"Cpus"}) {
		if v, _ := vals[0].IntValue(); v >= 4 {
			viaProject++
		}
	}
	if viaProject != viaQuery {
		t.Fatalf("QueryProject matched %d, Query matched %d", viaProject, viaQuery)
	}
}

func classadText(i int) string {
	cpus := (i % 16) + 1
	return "Cpus = " + itoa(cpus) + "\nMem = " + itoa(cpus*1024) + "\nName = \"n" + itoa(i) + "\""
}
