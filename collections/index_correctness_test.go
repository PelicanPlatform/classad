package collections

import (
	"fmt"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// buildIndexedCorpus makes a collection indexed on Arch/State/Owner (categorical)
// and Cpus/Memory (value), populated with ads that span every extraction case:
// plain literals, an expression-valued attr, a wrong-type attr, and an absent
// attr. It returns the collection and the source ads keyed by their "ID" attr so a
// test can compute the brute-force answer.
func buildIndexedCorpus(t *testing.T) (*Collection, map[int]*classad.ClassAd) {
	t.Helper()
	c := New(Options{
		Shards:           8,
		CategoricalAttrs: []string{"Arch", "State", "Owner"},
		ValueAttrs:       []string{"Cpus", "Memory"},
	})
	arches := []string{"X86_64", "x86_64", "aarch64", "ppc64le"}
	states := []string{"Unclaimed", "Claimed", "Idle", "Owner"}
	owners := []string{"alice", "bob", "carol", "dave"}
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 400; i++ {
		var text string
		switch {
		case i%37 == 0:
			// Expression-valued indexed attrs -> exceptions.
			text = fmt.Sprintf(`[ ID = %d; Arch = "%s"; State = "%s"; Owner = "%s"; Cpus = Base + 1; Memory = 1024 * %d ]`,
				i, arches[i%len(arches)], states[i%len(states)], owners[i%len(owners)], (i%8)+1)
		case i%41 == 0:
			// Wrong-type / absent: Arch numeric, State missing.
			text = fmt.Sprintf(`[ ID = %d; Arch = %d; Owner = "%s"; Cpus = %d; Memory = %d ]`,
				i, i, owners[i%len(owners)], (i%8)+1, ((i%16)+1)*512)
		default:
			text = fmt.Sprintf(`[ ID = %d; Arch = "%s"; State = "%s"; Owner = "%s"; Cpus = %d; Memory = %d ]`,
				i, arches[i%len(arches)], states[i%len(states)], owners[i%len(owners)], (i%8)+1, ((i%16)+1)*512)
		}
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatalf("parse ad %d: %v", i, err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
		src[i] = ad
	}
	c.Reindex()
	return c, src
}

func idOf(t *testing.T, ad *classad.ClassAd) int {
	t.Helper()
	v, ok := ad.EvaluateAttrInt("ID")
	if !ok {
		t.Fatalf("ad missing ID")
	}
	return int(v)
}

// queryIDs runs the collection query and returns the sorted set of matching IDs.
func queryIDs(t *testing.T, c *Collection, q *vm.Query) []int {
	var ids []int
	for ad := range c.Query(q) {
		ids = append(ids, idOf(t, ad))
	}
	sort.Ints(ids)
	return ids
}

// bruteIDs is the ground truth: evaluate the query against every source ad.
func bruteIDs(src map[int]*classad.ClassAd, q *vm.Query) []int {
	var ids []int
	for id, ad := range src {
		if q.Matches(ad) {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

func TestIndexMatchesFullScan(t *testing.T) {
	t.Parallel()
	c, src := buildIndexedCorpus(t)
	queries := []string{
		`Arch == "X86_64"`,                     // categorical equality (case-insensitive)
		`State == "Unclaimed"`,                 // categorical equality
		`Owner == "alice" || Owner == "carol"`, // set membership
		`Arch != "aarch64"`,                    // categorical negation
		`!(Owner == "bob")`,                    // negation normalized to !=
		`Cpus >= 4`,                            // value range
		`Memory > 4096`,                        // value range
		`Cpus == 3`,                            // value equality
		`Cpus != 5`,                            // value negation
		`Memory > 2048 && Memory <= 6144`,      // two-sided range on one attr
		`Cpus >= 2 && Arch == "X86_64" && Memory > 1024`, // multi-index AND
		`Owner == "dave" && Cpus < 4`,                    // categorical + value AND
		`Cpus > 1000`,                                    // no matches
		`Rank > 5`,                                       // non-indexed attr -> full scan
		`Cpus >= 2 && Nonexistent == "x"`,                // one indexed, one non-indexed
		`Memory > 1024*3`,                                // constant folding
		`Arch =?= "X86_64"`,                              // identity: case-sensitive, only the exact case
		`Arch =?= "x86_64"`,                              // identity: the other case variant only
		`Owner =?= "alice" || Owner =?= "bob"`,           // identity OR-chain -> membership
		`Cpus =?= 3`,                                     // numeric identity
		`Arch =!= "aarch64" && Cpus >= 2`,                // isnt (not indexed) alongside an indexed probe
		`State =?= undefined`,                            // presence: absent categorical (i%41 drops State)
		`State =!= undefined`,                            // presence: defined categorical
		`Cpus =?= undefined`,                             // presence: value attr that evaluates undefined (exc)
		`Cpus =!= undefined && Memory > 1024`,            // presence + value range
		`isUndefined(State)`,                             // presence via function form
		`!isUndefined(State) && Owner == "alice"`,        // negated function form + indexed eq
		`Owner isnt undefined`,                           // presence: always-defined attr
	}
	for _, qs := range queries {
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatalf("parse query %q: %v", qs, err)
		}
		got := queryIDs(t, c, q)
		want := bruteIDs(src, q)
		if !equalInts(got, want) {
			t.Errorf("query %q:\n  index got %d matches\n  brute   %d matches\n  got=%v\n  want=%v",
				qs, len(got), len(want), got, want)
		}
	}
}

// TestMetaEqualsUsesIndex asserts the planner change directly: `=?=` (identity) is
// planned as an indexed access path (like `==`), while `=!=` (isnt) is not indexed
// and falls back to a scan. Correctness of both is covered by TestIndexMatchesFullScan.
func TestMetaEqualsUsesIndex(t *testing.T) {
	t.Parallel()
	c, _ := buildIndexedCorpus(t)
	cases := []struct {
		query       string
		wantIndexed bool
	}{
		{`Arch =?= "X86_64"`, true},   // identity on a categorical index
		{`Cpus =?= 3`, true},          // identity on a value index
		{`Arch == "X86_64"`, true},    // the == it is planned like
		{`Arch =!= "aarch64"`, false}, // isnt vs literal: deliberately not indexed
		{`State isnt undefined`, true},        // presence on a categorical index
		{`State =?= undefined`, true},         // absence on a categorical index
		{`Cpus =!= undefined`, true},          // presence on a value index
		{`isUndefined(Owner)`, true},          // presence via function form
		{`!isUndefined(Cpus)`, true},          // negated function form
		{`Nonexistent isnt undefined`, false}, // presence on an unindexed attr -> scan
	}
	for _, tc := range cases {
		q, err := vm.Parse(tc.query)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.query, err)
		}
		ex := c.ExplainQuery(q)
		gotIndexed := ex.Plan == "indexed"
		if gotIndexed != tc.wantIndexed {
			t.Errorf("query %q: plan=%q indexUsable=%d, want indexed=%v",
				tc.query, ex.Plan, ex.IndexUsable, tc.wantIndexed)
		}
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
