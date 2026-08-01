package collections

import (
	"slices"
	"testing"
)

// TestArchiveIndexIntrospection covers what `.indexes <archive>` reports: the configured
// index attributes and the zone-mapped ones. Zone maps cover the explicit ZoneAttrs plus
// every value-indexed attribute, which is what makes a range query prune whole segments.
func TestArchiveIndexIntrospection(t *testing.T) {
	t.Parallel()
	a, _ := buildArchive(t, t.TempDir(), 400, ArchiveOptions{
		CategoricalAttrs: []string{"Owner", "JobStatus"},
		ValueAttrs:       []string{"Memory"},
		ZoneAttrs:        []string{"CompletionDate"},
	})
	defer a.Close()

	cat, val := a.IndexedAttrs()
	if !slices.Contains(cat, "Owner") || !slices.Contains(cat, "JobStatus") {
		t.Errorf("categorical indexes = %v, want Owner and JobStatus", cat)
	}
	if !slices.Contains(val, "Memory") {
		t.Errorf("value indexes = %v, want Memory", val)
	}
	zones := a.ZoneAttrs()
	for _, want := range []string{"CompletionDate", "Memory"} {
		if !slices.Contains(zones, want) {
			t.Errorf("zone attrs = %v, want %s (explicit zone attrs plus value-indexed ones)", zones, want)
		}
	}
	if slices.Contains(zones, "Owner") {
		t.Errorf("zone attrs = %v: a categorical index is not zone-mapped", zones)
	}
}

// TestArchiveAddIndexZonesAndReach pins the two things an operator needs to know when
// indexing an existing archive: a value index added at runtime becomes zone-mapped like one
// configured at creation, and it does NOT reach already-sealed segments until a rewrite.
func TestArchiveAddIndexZonesAndReach(t *testing.T) {
	t.Parallel()
	a, src := buildArchive(t, t.TempDir(), 400, ArchiveOptions{
		CategoricalAttrs: []string{"Owner"},
	})
	defer a.Close()
	if got := a.ZoneAttrs(); len(got) != 0 {
		t.Fatalf("zone attrs = %v, want none (no value or zone attrs configured)", got)
	}

	if !a.AddIndex(nil, []string{"CompletionDate"}) {
		t.Fatal("AddIndex reported no change")
	}
	if got := a.ZoneAttrs(); !slices.Contains(got, "CompletionDate") {
		t.Errorf("zone attrs = %v, want CompletionDate: a runtime value index should be zoned too", got)
	}

	// The segments sealed before the add keep their old index set.
	stale, sealed := a.StaleIndexSegments()
	if sealed == 0 {
		t.Fatal("expected several sealed segments")
	}
	if stale != sealed {
		t.Errorf("stale/sealed = %d/%d, want every already-sealed segment stale", stale, sealed)
	}

	// A rewrite re-encodes every segment, so all sidecars are rebuilt on the current set.
	a.Rewrite()
	if stale, sealed = a.StaleIndexSegments(); stale != 0 {
		t.Errorf("after rewrite stale/sealed = %d/%d, want 0 stale", stale, sealed)
	}

	// Correctness is unchanged throughout: the new index is a pruning aid, not a filter.
	for _, qs := range []string{
		`CompletionDate > 1700000100`,
		`CompletionDate > 1700000100 && CompletionDate < 1700000200`,
		`Owner == "carol" && CompletionDate >= 1700000300`,
	} {
		got := archiveQueryIDs(t, a, qs)
		want := bruteIDs(src, mustQuery(t, qs))
		if !equalInts(got, want) {
			t.Errorf("%s: got %d ads, want %d", qs, len(got), len(want))
		}
	}
}
