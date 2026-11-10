package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestArchiveSuggestsAnIndexItsDemandJustifies: an archive accumulates query demand on the same
// tracker a mutable collection does, and reports it on the same terms. It matters that this is
// asserted HERE rather than only from the layers above: the archive's own AutoTune acts on this
// tracker, so if the demand were not reaching it, a background pass would be tuning on nothing
// and the only visible symptom would be an archive that never grows an index it should have.
//
// The attributes are chosen so a wrong answer cannot pass: Owner is queried by equality and
// Shard by range, neither is configured as an index, and Pad is present in every record but
// never queried -- so "suggest everything present" and "suggest nothing" both fail.
func TestArchiveSuggestsAnIndexItsDemandJustifies(t *testing.T) {
	a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir(), SegmentSize: 1 << 12,
		ValueAttrs: []string{"N"}}) // indexed, and queried below: must NOT be re-suggested
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	const n = 600
	owners := []string{"alice", "bob", "carol"}
	for i := 0; i < n; i++ {
		ad, perr := classad.Parse(fmt.Sprintf(`[ N=%d; Owner=%q; Shard=%d; Pad="x" ]`,
			i, owners[i%len(owners)], i%16))
		if perr != nil {
			t.Fatal(perr)
		}
		if appendErr := a.Append(ad); appendErr != nil {
			t.Fatal(appendErr)
		}
	}

	const rounds = 8
	for i := 0; i < rounds; i++ {
		drainArchiveQuery(t, a, `Owner == "alice"`)
		drainArchiveQuery(t, a, `Shard > 8`)
		drainArchiveQuery(t, a, `N > 100`)
	}

	got := suggestionMap(a.SuggestIndexes(1000))

	owner, ok := got["Owner"]
	if !ok {
		t.Fatalf("Owner was queried by equality %d times and is not indexed, yet nothing was "+
			"suggested for it: the archive is not accumulating query demand (got %+v)", rounds, got)
	}
	if owner.Kind != "categorical" || owner.QueriesEq != rounds {
		t.Errorf("Owner: kind=%q eq=%d, want categorical and %d", owner.Kind, owner.QueriesEq, rounds)
	}
	shard, ok := got["Shard"]
	if !ok {
		t.Fatalf("nothing suggested for Shard after %d range queries (got %+v)", rounds, got)
	}
	if shard.Kind != "value" || shard.QueriesRange != rounds {
		t.Errorf("Shard: kind=%q rng=%d, want value and %d", shard.Kind, shard.QueriesRange, rounds)
	}
	// Demand alone is not the rule: N carries as much of it as Shard and is already indexed.
	if s, dup := got["N"]; dup {
		t.Errorf("N is a configured value index and was suggested again: %+v", s)
	}
	// Presence alone is not the rule either.
	if s, unasked := got["Pad"]; unasked {
		t.Errorf("Pad is in every record but was never queried, yet was suggested: %+v", s)
	}
}

func drainArchiveQuery(t *testing.T, a *Archive, qs string) {
	t.Helper()
	q, err := vm.Parse(qs)
	if err != nil {
		t.Fatalf("parse %q: %v", qs, err)
	}
	for range a.Query(q) {
	}
}
