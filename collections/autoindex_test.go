package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// drainQuery runs a query to completion so its demand is recorded.
func drainQuery(t *testing.T, c *Collection, qs string) {
	t.Helper()
	q, err := vm.Parse(qs)
	if err != nil {
		t.Fatalf("parse %q: %v", qs, err)
	}
	for range c.Query(q) {
	}
}

func suggestionMap(s []IndexSuggestion) map[string]IndexSuggestion {
	m := make(map[string]IndexSuggestion, len(s))
	for _, x := range s {
		m[x.Attr] = x
	}
	return m
}

func TestSuggestIndexes(t *testing.T) {
	c := New(Options{Shards: 4}) // no indexes configured
	owners := []string{"alice", "bob", "carol", "dave", "eve"}
	for i := 0; i < 300; i++ {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ Owner="%s"; Arch="X86_64"; Memory=%d; Rank=Cpus*2 ]`,
			owners[i%len(owners)], (i%20)*512))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}

	// Query Owner by equality and Memory by range; also query Rank (an expression-
	// valued attr) by range. Never query Arch.
	for i := 0; i < 8; i++ {
		drainQuery(t, c, `Owner == "alice"`)
		drainQuery(t, c, `Memory > 4096`)
		drainQuery(t, c, `Rank > 5`)
	}

	got := suggestionMap(c.SuggestIndexes(1000))

	if s, ok := got["Owner"]; !ok || s.Kind != "categorical" {
		t.Errorf("Owner: got %+v, want categorical", s)
	} else if s.QueriesEq != 8 || s.StringFrac != 1.0 {
		t.Errorf("Owner rationale: eq=%d strFrac=%.2f (want 8, 1.00)", s.QueriesEq, s.StringFrac)
	}
	if s, ok := got["Memory"]; !ok || s.Kind != "value" {
		t.Errorf("Memory: got %+v, want value", s)
	} else if s.QueriesRange != 8 || s.NumericFrac != 1.0 {
		t.Errorf("Memory rationale: rng=%d numFrac=%.2f (want 8, 1.00)", s.QueriesRange, s.NumericFrac)
	}
	// Arch has data but no query demand -> not suggested.
	if _, ok := got["Arch"]; ok {
		t.Errorf("Arch suggested despite no query demand")
	}
	// Rank has demand but is an expression (not a numeric literal) -> not suggested.
	if _, ok := got["Rank"]; ok {
		t.Errorf("Rank suggested despite being expression-valued (should be all-exceptions)")
	}
}

// TestSuggestIndexesExcludesConfigured verifies an already-indexed attribute is not
// re-suggested.
func TestSuggestIndexesExcludesConfigured(t *testing.T) {
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Owner"}})
	for i := 0; i < 100; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ Owner="u%d"; State="Idle" ]`, i%5))
		c.Put([]byte(fmt.Sprintf("k%d", i)), ad)
	}
	for i := 0; i < 5; i++ {
		drainQuery(t, c, `Owner == "u1"`)   // already indexed
		drainQuery(t, c, `State == "Idle"`) // not indexed
	}
	got := suggestionMap(c.SuggestIndexes(1000))
	if _, ok := got["Owner"]; ok {
		t.Errorf("Owner re-suggested though already configured")
	}
	if s, ok := got["State"]; !ok || s.Kind != "categorical" {
		t.Errorf("State: got %+v, want categorical", s)
	}
}

// TestDemandTracking checks the raw demand counters split equality vs range.
func TestDemandTracking(t *testing.T) {
	c := New(Options{Shards: 2})
	c.Put([]byte("k"), mustParse(t, `[ A=1; B="x" ]`))
	for i := 0; i < 3; i++ {
		drainQuery(t, c, `A > 0`)    // range
		drainQuery(t, c, `A == 1`)   // equality
		drainQuery(t, c, `B == "x"`) // equality
	}
	v, ok := c.demand.m.Load("a")
	if !ok {
		t.Fatal("no demand recorded for A")
	}
	d := v.(*demandCounts)
	if got := d.rng.Load(); got != 3 {
		t.Errorf("A range demand = %d, want 3", got)
	}
	if got := d.eq.Load(); got != 3 {
		t.Errorf("A equality demand = %d, want 3", got)
	}
}
