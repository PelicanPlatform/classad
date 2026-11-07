package collections

import (
	"reflect"
	"strings"
	"testing"
)

// The composite half of index provenance: what a store reads out to persist, and the two
// ways it puts it back. The single-attribute tests for the same thing are in
// autoindex_test.go; these mirror them tuple by tuple.

// TestCompositeAttrsReportsConfiguredAndAuto: CompositeAttrs is the whole set, as a saved
// config has to rebuild it, and AutoCompositeNames is the subset the tuner made. The
// distinction is the entire point -- a persisted set with no subset marked restores every
// composite as a human's.
func TestCompositeAttrsReportsConfiguredAndAuto(t *testing.T) {
	t.Parallel()
	c := New(Options{
		Shards:           4,
		CategoricalAttrs: []string{"Owner", "Arch", "Universe"},
		CompositeAttrs:   [][]string{{"Owner", "Universe"}},
	})
	putTupleAds(t, c, 60)
	if len(c.spec.Load().comps) != 1 {
		t.Fatalf("the configured composite is missing; nothing below could tell it from an auto one")
	}
	if added := c.addCompositesAuto([][]string{{"Arch", "Owner"}}); len(added) != 1 {
		t.Fatalf("the auto composite was not installed: %v", added)
	}

	all := c.CompositeAttrs()
	if len(all) != 2 {
		t.Fatalf("CompositeAttrs = %v, want both tuples", all)
	}
	auto := c.AutoCompositeNames()
	if len(auto) != 1 || !reflect.DeepEqual(auto[0], []string{"Arch", "Owner"}) {
		t.Fatalf("AutoCompositeNames = %v, want only the auto tuple Arch+Owner", auto)
	}
}

// TestAutoCompositeNamesIsEmptyWithoutAnAutoComposite pairs with the test above: the
// "subset" assertion there would read the same if AutoCompositeNames simply returned
// everything, so this is the case where it must return nothing at all.
func TestAutoCompositeNamesIsEmptyWithoutAnAutoComposite(t *testing.T) {
	t.Parallel()
	c := New(Options{
		Shards:           4,
		CategoricalAttrs: []string{"Owner", "Arch"},
		CompositeAttrs:   [][]string{{"Owner", "Arch"}},
	})
	putTupleAds(t, c, 60)
	if len(c.CompositeAttrs()) != 1 {
		t.Fatalf("the configured composite is missing; the assertion below would be vacuous")
	}
	if auto := c.AutoCompositeNames(); len(auto) != 0 {
		t.Fatalf("AutoCompositeNames = %v on a collection whose only composite is configured", auto)
	}
}

// TestMarkAutoCompositesRestoresProvenance is the archive's restore path: the composite is
// already there, configured, and only its provenance is missing.
func TestMarkAutoCompositesRestoresProvenance(t *testing.T) {
	t.Parallel()
	c := New(Options{
		Shards:           4,
		CategoricalAttrs: []string{"Owner", "Arch"},
		CompositeAttrs:   [][]string{{"Owner", "Arch"}},
	})
	putTupleAds(t, c, 60)
	if auto, ok := compositeProvenance(c)["Owner+Arch"]; !ok || auto {
		t.Fatalf("the composite is missing or already auto before the mark: %v", compositeProvenance(c))
	}

	c.MarkAutoComposites([][]string{{"Owner", "Arch"}})
	if auto, ok := compositeProvenance(c)["Owner+Arch"]; !ok || !auto {
		t.Fatalf("composite not marked auto: %v", compositeProvenance(c))
	}
	// And the mark is what a drop reads: the composite is now the tuner's to remove.
	drops := c.SuggestCompositeDrops()
	if len(drops) != 1 || !drops[0].Auto {
		t.Fatalf("SuggestCompositeDrops = %+v, want one entry marked Auto", drops)
	}
}

// TestMarkAutoCompositesMatchesTupleAsASet: a saved tuple and the live composite name the
// same components, and the mark has to land whichever order each is written in.
//
// The fixture is built so this cannot pass for the wrong reason. The composite is configured
// as (Owner, Arch), whose SORTED spelling is (Arch, Owner) -- so the order it carries is not
// the order a set comparison canonicalizes to, and a mark that compared ordered keys would
// miss it even when handed the composite's own tuple back, which is exactly what a round
// trip through a saved configuration hands it.
func TestMarkAutoCompositesMatchesTupleAsASet(t *testing.T) {
	t.Parallel()
	configured := []string{"Owner", "Arch"}
	if strings.ToLower(configured[0]) < strings.ToLower(configured[1]) {
		t.Fatalf("the fixture %v is already in sorted order, so an ordered comparison would match it too", configured)
	}
	for _, tc := range []struct {
		name string
		mark []string
	}{
		{"the composite's own order, as a saved config returns it", []string{"Owner", "Arch"}},
		{"the other order, folded", []string{"arch", "owner"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New(Options{
				Shards:           4,
				CategoricalAttrs: []string{"Owner", "Arch"},
				CompositeAttrs:   [][]string{configured},
			})
			putTupleAds(t, c, 60)
			if _, ok := compositeProvenance(c)["Owner+Arch"]; !ok {
				t.Fatal("the configured composite is missing; the mark below would have nothing to match")
			}
			c.MarkAutoComposites([][]string{tc.mark})
			if auto := compositeProvenance(c)["Owner+Arch"]; !auto {
				t.Fatalf("mark with %v did not take: %v", tc.mark, compositeProvenance(c))
			}
		})
	}
}

// TestMarkAutoCompositesIgnoresUnknownTuple: a saved tuple whose composite did not come back
// -- a component that is no longer indexed takes its composite with it -- marks nothing, and
// in particular does not mark some other composite.
func TestMarkAutoCompositesIgnoresUnknownTuple(t *testing.T) {
	t.Parallel()
	c := New(Options{
		Shards:           4,
		CategoricalAttrs: []string{"Owner", "Arch"},
		CompositeAttrs:   [][]string{{"Owner", "Arch"}},
	})
	putTupleAds(t, c, 60)
	if len(c.CompositeAttrs()) != 1 {
		t.Fatal("the configured composite is missing; there would be nothing to mis-mark")
	}

	c.MarkAutoComposites([][]string{{"Owner", "Universe"}})
	if auto := c.AutoCompositeNames(); len(auto) != 0 {
		t.Fatalf("a tuple naming no configured composite marked %v", auto)
	}
}

// TestMarkAutoCompositesDoesNotMoveTheGeneration: provenance is not part of what a sidecar
// was built under. If marking bumped the signature, every sealed segment in an archive would
// read as stale at every open and be reindexed to record who asked for the composite.
func TestMarkAutoCompositesDoesNotMoveTheGeneration(t *testing.T) {
	t.Parallel()
	c := New(Options{
		Shards:           4,
		CategoricalAttrs: []string{"Owner", "Arch"},
		CompositeAttrs:   [][]string{{"Owner", "Arch"}},
	})
	putTupleAds(t, c, 60)
	before := c.spec.Load().gen

	c.MarkAutoComposites([][]string{{"Owner", "Arch"}})
	if len(c.AutoCompositeNames()) != 1 {
		t.Fatalf("the mark did not take, so an unchanged generation says nothing: %v", compositeProvenance(c))
	}
	if after := c.spec.Load().gen; after != before {
		t.Errorf("index generation moved on a provenance-only change: %d -> %d", before, after)
	}
}

// TestAddAutoCompositesRestoresWithProvenance is the mutable store's restore path: nothing
// configured the composite, so it is added back, and the add is what carries the mark.
func TestAddAutoCompositesRestoresWithProvenance(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Owner", "Arch"}})
	putTupleAds(t, c, 60)
	if len(c.CompositeAttrs()) != 0 {
		t.Fatal("a composite is already configured; the add below would not be the thing under test")
	}

	if !c.AddAutoComposites([][]string{{"Owner", "Arch"}}) {
		t.Fatal("AddAutoComposites installed nothing")
	}
	if auto := c.AutoCompositeNames(); len(auto) != 1 {
		t.Fatalf("AutoCompositeNames after the add = %v, want the one tuple", auto)
	}
}

// TestAddAutoCompositesRefusesUnindexedComponent: a restored tuple whose component index did
// not come back leaves the composite with nothing to fall back to for everything it cannot
// answer, so it is refused rather than installed.
func TestAddAutoCompositesRefusesUnindexedComponent(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Owner"}})
	putTupleAds(t, c, 60)

	if c.AddAutoComposites([][]string{{"Owner", "Arch"}}) {
		t.Fatalf("a tuple with an unindexed component was installed: %v", c.CompositeAttrs())
	}
	if n := len(c.CompositeAttrs()); n != 0 {
		t.Fatalf("%d composites configured, want 0", n)
	}
}
