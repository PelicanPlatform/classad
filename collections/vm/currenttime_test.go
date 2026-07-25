package vm

import (
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// TestCurrentTimeProbe verifies a query referencing CurrentTime folds it to a constant at
// compile time, so `CompletionDate > CurrentTime - 86400` yields an indexable range probe on
// CompletionDate (not a non-prunable attr-vs-expr comparison). This is what lets the archive
// prune whole segments/zones for "jobs in the last day" queries.
func TestCurrentTimeProbe(t *testing.T) {
	before := time.Now().Unix()
	q, err := Parse(`CompletionDate > CurrentTime - 86400`)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().Unix()

	probes := q.Probes()
	if len(probes) != 1 {
		t.Fatalf("got %d probes, want 1 (CurrentTime should fold to a constant threshold)", len(probes))
	}
	p := probes[0]
	if p.Attr != "CompletionDate" || p.Op != ">" {
		t.Fatalf("probe = {%s %s}, want {CompletionDate >}", p.Attr, p.Op)
	}
	if len(p.Vals) != 1 {
		t.Fatalf("probe has %d values, want 1", len(p.Vals))
	}
	n, err := p.Vals[0].IntValue()
	if err != nil {
		t.Fatalf("probe threshold is not an integer literal: %v", err)
	}
	if n < before-86400 || n > after-86400 {
		t.Errorf("probe threshold = %d, want ~now-86400 (in [%d, %d])", n, before-86400, after-86400)
	}
}

// TestCurrentTimeMatch verifies a compiled query with CurrentTime matches ads correctly
// (recent kept, old rejected).
func TestCurrentTimeMatch(t *testing.T) {
	now := time.Now().Unix()
	q, err := Parse(`CompletionDate > CurrentTime - 86400`)
	if err != nil {
		t.Fatal(err)
	}
	recent, _ := classad.Parse(`[ CompletionDate = ` + itoa(now-3600) + ` ]`) // 1h ago
	old, _ := classad.Parse(`[ CompletionDate = ` + itoa(now-2*86400) + ` ]`) // 2d ago
	if !q.Matches(recent) {
		t.Error("recent ad (1h ago) should match CompletionDate > CurrentTime - 86400")
	}
	if q.Matches(old) {
		t.Error("old ad (2d ago) should not match CompletionDate > CurrentTime - 86400")
	}
}

// TestNonCurrentTimeUnchanged sanity-checks that a query without CurrentTime still compiles
// and its probes are unaffected (the fold is gated on referencing CurrentTime).
func TestNonCurrentTimeUnchanged(t *testing.T) {
	q, err := Parse(`Memory > 2048`)
	if err != nil {
		t.Fatal(err)
	}
	if p := q.Probes(); len(p) != 1 || p[0].Attr != "Memory" || p[0].Op != ">" {
		t.Fatalf("Memory probe unexpectedly changed: %v", p)
	}
}
