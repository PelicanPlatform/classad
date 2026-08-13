package collections

import (
	"fmt"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// fastMatches returns the multiset of ads (by String) the fast-path Query yields.
func fastMatches(c *Collection, q *vm.Query) []string {
	var out []string
	for ad := range c.Query(q) {
		out = append(out, ad.String())
	}
	sort.Strings(out)
	return out
}

// refMatches returns the multiset of ads the reference (full decode of every ad,
// then match) yields.
func refMatches(c *Collection, q *vm.Query) []string {
	var out []string
	for ad := range c.Scan() {
		if q.Matches(ad) {
			out = append(out, ad.String())
		}
	}
	sort.Strings(out)
	return out
}

func assertSameMatches(t *testing.T, c *Collection, exprs []string) {
	t.Helper()
	// The reference decodes every ad and renders each match. Doing that per expression means N full
	// decodes and N renders of the same collection, and on the real corpus (8000 ads, 6 expressions,
	// most of them matching nearly everything) that made this the slowest test in the module by 2.5x --
	// 188s under -race, 10% of the suite. Decoding and rendering ONCE and re-filtering per expression
	// asserts exactly the same thing: same ads, same texts, same sort, same comparison.
	type refAd struct {
		ad   *classad.ClassAd
		text string
	}
	var all []refAd
	for ad := range c.Scan() {
		all = append(all, refAd{ad, ad.String()})
	}
	for _, e := range exprs {
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatalf("parse %q: %v", e, err)
		}
		fast := fastMatches(c, q)
		var ref []string
		for _, a := range all {
			if q.Matches(a.ad) {
				ref = append(ref, a.text)
			}
		}
		sort.Strings(ref)
		if len(fast) != len(ref) {
			t.Errorf("query %q: fast matched %d, reference %d", e, len(fast), len(ref))
			continue
		}
		for i := range fast {
			if fast[i] != ref[i] {
				t.Errorf("query %q: match set differs at %d:\n fast=%s\n  ref=%s", e, i, fast[i], ref[i])
				break
			}
		}
	}
}

// TestQueryFastPathTransitive validates the partial-decode fast path against a
// full-decode reference on synthetic ads with derived (chained) attributes, MY
// scope, and an eval() fallback.
func TestQueryFastPathTransitive(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4})
	for i := 0; i < 400; i++ {
		// C -> B -> A: querying C must transitively decode B and A.
		src := fmt.Sprintf(`[Id=%d; A=%d; B=A+1; C=B+1; Owner=%q; Big=(A>100)]`,
			i, i, []string{"alice", "bob"}[i%2])
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	assertSameMatches(t, c, []string{
		`A > 100`,
		`C > 100`,          // transitive: C references B references A
		`B == A + 1`,       // reads A and B
		`MY.C > MY.A`,      // MY-scoped self references
		`Owner == "alice"`, // simple string
		`C > 100 && Big`,   // Big is itself a derived boolean expr
		`Missing > 0`,      // absent attribute -> undefined -> no match
		`eval("C") > 100`,  // eval() -> PartialSafe=false -> full-decode fallback
		`A > 50 || Owner == "bob"`,
	})
}

// TestQueryFastPathReal validates the fast path against real ads.
func TestQueryFastPathReal(t *testing.T) {
	t.Parallel()
	sample := loadCorpus(t)
	c := populate(t, sample, 8000)
	assertSameMatches(t, c, []string{
		`Cpus >= 1`,
		`Memory > 1000`,
		`Cpus >= 2 && Memory > 4000`,
		`Arch == "X86_64" || Arch == "x86_64"`,
		`Cpus >= 1 && Memory > 1000 && Disk > 0`,
		`Missing == 1`,
	})
}
