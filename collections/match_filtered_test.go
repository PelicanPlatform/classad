package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestMatchFilteredEqualsBrute: MatchSortedRankedFiltered (job + pushed-down resource
// constraint) returns exactly the slots that a full match then a constraint filter would,
// with the constraint's index narrowing the candidate scan.
func TestMatchFilteredEqualsBrute(t *testing.T) {
	t.Parallel()
	const n = 4000
	build := func(indexed bool) *Collection {
		opts := Options{Shards: 4}
		if indexed {
			opts.ValueAttrs = []string{"Memory"}
			opts.CategoricalAttrs = []string{"State", "Arch"}
		}
		c := New(opts)
		states := []string{"Unclaimed", "Claimed", "Claimed", "Claimed"}
		for i := 0; i < n; i++ {
			c.Put([]byte(fmt.Sprintf("m%d", i)), mustAd(t, fmt.Sprintf(
				`[ Id=%d; Memory=%d; State=%q; Arch="X86_64"; Requirements=true ]`,
				i, (i%8+1)*1024, states[i%4])))
		}
		if indexed {
			c.Reindex()
		}
		return c
	}
	plain, indexed := build(false), build(true)
	job := mustAd(t, `[ RequestMemory=4096; Requirements = (TARGET.Memory >= RequestMemory) && (TARGET.Arch == "X86_64"); Rank = TARGET.Memory ]`)

	for _, tw := range []string{`State =!= "Claimed"`, `State == "Unclaimed"`, `State != "Claimed" && Memory > 2048`} {
		// Brute force: full match on plain, then filter by the constraint.
		tq, _ := vm.Parse(tw)
		want := map[int]bool{}
		for _, rm := range plain.MatchSortedRanked(job, 0) {
			if tq.Matches(rm.Ad) {
				id, _ := rm.Ad.EvaluateAttrInt("Id")
				want[int(id)] = true
			}
		}
		got, err := indexed.MatchSortedRankedFiltered(job, tw, 0)
		if err != nil {
			t.Fatal(err)
		}
		gotIDs := map[int]bool{}
		for _, rm := range got {
			id, _ := rm.Ad.EvaluateAttrInt("Id")
			gotIDs[int(id)] = true
		}
		if len(gotIDs) != len(want) {
			t.Errorf("%s: filtered got %d, brute %d", tw, len(gotIDs), len(want))
		}
		for id := range want {
			if !gotIDs[id] {
				t.Errorf("%s: missing id %d", tw, id)
			}
		}
		if len(want) == 0 || len(want) == n {
			t.Errorf("%s: expected a partial set, got %d", tw, len(want))
		}
		// Sorted by rank descending.
		var last float64 = 1 << 60
		for _, rm := range got {
			if rm.HasRank && rm.Rank > last {
				t.Errorf("%s: results not rank-sorted", tw)
			}
			last = rm.Rank
		}
	}
}
