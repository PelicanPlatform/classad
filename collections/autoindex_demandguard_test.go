package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// demandGuardCollection is 300 ads over two attributes, one of which the caller may index.
func demandGuardCollection(t *testing.T, indexed ...string) *Collection {
	t.Helper()
	c := New(Options{Shards: 4, CategoricalAttrs: indexed})
	owners := []string{"alice", "bob", "carol", "dave", "eve"}
	for i := 0; i < 300; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ Owner=%q; Arch="X86_64"; Memory=%d ]`,
			owners[i%len(owners)], (i%20)*512))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// TestSuggestIndexesDemandGuard covers the precondition SuggestIndexes now checks before it
// decodes anything: it must agree with the full computation in both directions. Getting it wrong
// in the cheap direction is the dangerous one -- the sample would be skipped and a real suggestion
// silently lost, which looks exactly like a table nobody has queried.
func TestSuggestIndexesDemandGuard(t *testing.T) {
	t.Parallel()
	t.Run("demand on an unindexed attribute still suggests", func(t *testing.T) {
		t.Parallel()
		c := demandGuardCollection(t)
		for i := 0; i < 8; i++ {
			drainQuery(t, c, `Owner == "alice"`)
		}
		if !c.hasUnindexedDemand() {
			t.Error("the guard reports no unindexed demand after 8 queries on an unindexed attribute")
		}
		if got := suggestionMap(c.SuggestIndexes(1000)); got["Owner"].Kind != "categorical" {
			t.Errorf("Owner: got %+v, want a categorical suggestion", got["Owner"])
		}
	})
	t.Run("no demand at all", func(t *testing.T) {
		t.Parallel()
		c := demandGuardCollection(t)
		if c.hasUnindexedDemand() {
			t.Error("the guard reports unindexed demand on a collection nobody has queried")
		}
		if got := c.SuggestIndexes(1000); len(got) != 0 {
			t.Errorf("suggestions without any demand: %+v", got)
		}
	})
	t.Run("demand only on an already-indexed attribute", func(t *testing.T) {
		t.Parallel()
		c := demandGuardCollection(t, "Owner")
		for i := 0; i < 8; i++ {
			drainQuery(t, c, `Owner == "alice"`)
		}
		if c.hasUnindexedDemand() {
			t.Error("the guard counts demand on an attribute that is already indexed")
		}
		if got := c.SuggestIndexes(1000); len(got) != 0 {
			t.Errorf("suggested an index that already exists: %+v", got)
		}
	})
	t.Run("read demand is not index demand", func(t *testing.T) {
		t.Parallel()
		c := demandGuardCollection(t)
		// Pull values OUT of records without filtering on them: that drives the hot set, and
		// drove the sampling pass too, but it can never produce an index suggestion.
		for i := 0; i < 8; i++ {
			drainQuery(t, c, `true`)
		}
		if c.hasUnindexedDemand() {
			t.Error("the guard counts read demand, which SuggestIndexes ignores")
		}
		if got := c.SuggestIndexes(1000); len(got) != 0 {
			t.Errorf("suggestions from read-only demand: %+v", got)
		}
	})
}
