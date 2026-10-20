package collections

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// matchEntryPoint runs one Collection match entry point and returns the matched slots
// as Id -> (rank, hasRank). Entry points that do not report a rank leave it zero.
type matchEntryPoint struct {
	name   string
	ranked bool // the entry point reports Rank
	run    func(c *Collection, job *classad.ClassAd) ([]*classad.ClassAd, []RankedMatch)
}

var matchEntryPoints = []matchEntryPoint{
	{"Match", false, func(c *Collection, job *classad.ClassAd) ([]*classad.ClassAd, []RankedMatch) {
		var out []*classad.ClassAd
		for ad := range c.Match(job) {
			out = append(out, ad)
		}
		return out, nil
	}},
	{"MatchSorted", false, func(c *Collection, job *classad.ClassAd) ([]*classad.ClassAd, []RankedMatch) {
		return c.MatchSorted(job, 0), nil
	}},
	{"MatchSorted/limit", false, func(c *Collection, job *classad.ClassAd) ([]*classad.ClassAd, []RankedMatch) {
		// A limit at least the slot count returns every match through the deferred
		// (wire-native survivor) path.
		return c.MatchSorted(job, 1<<20), nil
	}},
	{"MatchSortedRanked", true, func(c *Collection, job *classad.ClassAd) ([]*classad.ClassAd, []RankedMatch) {
		return nil, c.MatchSortedRanked(job, 0)
	}},
	{"MatchSortedRanked/limit", true, func(c *Collection, job *classad.ClassAd) ([]*classad.ClassAd, []RankedMatch) {
		return nil, c.MatchSortedRanked(job, 1<<20)
	}},
	{"MatchSortedRankedFiltered", true, func(c *Collection, job *classad.ClassAd) ([]*classad.ClassAd, []RankedMatch) {
		rm, err := c.MatchSortedRankedFiltered(job, `Id >= 0`, 0)
		if err != nil {
			panic(err)
		}
		return nil, rm
	}},
}

type matchResult struct {
	rank    float64
	hasRank bool
}

// bruteMatch is the reference: classad.MatchClassAd over fresh copies of the ORIGINAL
// job and slot ads, for every slot.
func bruteMatch(t *testing.T, jobText string, slots []string) map[int]matchResult {
	t.Helper()
	out := map[int]matchResult{}
	for _, st := range slots {
		job, slot := mustAd(t, jobText), mustAd(t, st)
		m := classad.NewMatchClassAd(job, slot)
		if !m.Match() {
			continue
		}
		id, ok := slot.EvaluateAttrInt("Id")
		if !ok {
			t.Fatalf("slot without Id: %s", st)
		}
		r, hr := m.EvaluateRankLeft()
		out[int(id)] = matchResult{rank: r, hasRank: hr}
	}
	return out
}

// checkEntryPoints asserts every match entry point returns exactly want (and, where it
// reports one, the same Rank) for jobText over c.
func checkEntryPoints(t *testing.T, label string, c *Collection, jobText string, want map[int]matchResult) {
	t.Helper()
	for _, ep := range matchEntryPoints {
		ads, ranked := ep.run(c, mustAd(t, jobText))
		got := map[int]matchResult{}
		add := func(ad *classad.ClassAd, r matchResult) {
			id, ok := ad.EvaluateAttrInt("Id")
			if !ok {
				t.Fatalf("%s %s: matched ad without Id", label, ep.name)
			}
			if _, dup := got[int(id)]; dup {
				t.Fatalf("%s %s: Id %d returned twice", label, ep.name, id)
			}
			got[int(id)] = r
		}
		for _, ad := range ads {
			add(ad, matchResult{})
		}
		for _, rm := range ranked {
			add(rm.Ad, matchResult{rank: rm.Rank, hasRank: rm.HasRank})
		}
		if diff := diffMatchSets(got, want, ep.ranked); diff != "" {
			t.Fatalf("%s %s: job %s\n%s", label, ep.name, jobText, diff)
		}
	}
}

func diffMatchSets(got, want map[int]matchResult, ranked bool) string {
	var b strings.Builder
	ids := map[int]bool{}
	for id := range got {
		ids[id] = true
	}
	for id := range want {
		ids[id] = true
	}
	sorted := make([]int, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Ints(sorted)
	for _, id := range sorted {
		g, inGot := got[id]
		w, inWant := want[id]
		switch {
		case inGot && !inWant:
			fmt.Fprintf(&b, "  Id %d returned, MatchClassAd rejects it\n", id)
		case !inGot && inWant:
			fmt.Fprintf(&b, "  Id %d missing, MatchClassAd matches it\n", id)
		case ranked && (g.hasRank != w.hasRank || (w.hasRank && g.rank != w.rank)):
			fmt.Fprintf(&b, "  Id %d rank %v/%v, MatchClassAd %v/%v\n", id, g.rank, g.hasRank, w.rank, w.hasRank)
		}
	}
	return b.String()
}

// TestMatchLeftBiasedOr: ClassAd || is left-biased with respect to ERROR -- ERROR || TRUE
// is ERROR, TRUE || ERROR is TRUE -- so a job whose Requirements errors before reaching a
// true operand must not match, however cheap the true operand is. Likewise a nested
// && whose first operand errors makes the enclosing || an error.
func TestMatchLeftBiasedOr(t *testing.T) {
	t.Parallel()
	slots := []string{`[ Id = 1; Name = "s1"; Foo = "str"; Bar = true; Baz = false; Requirements = true ]`}
	cases := []struct {
		req   string
		match bool
	}{
		{`(TARGET.Foo * 2 > 1) || TARGET.Bar`, false},
		{`TARGET.Bar || (TARGET.Foo * 2 > 1)`, true},
		{`TARGET.Foo || TARGET.Bar`, false},
		{`((TARGET.Foo * 2 > 1) && TARGET.Baz) || TARGET.Bar`, false},
		{`(TARGET.Baz && (TARGET.Foo * 2 > 1)) || TARGET.Bar`, true},
		{`!((TARGET.Foo * 2 > 1) && TARGET.Baz)`, false},
		{`!(TARGET.Baz && (TARGET.Foo * 2 > 1))`, true},
	}
	for _, opts := range []Options{{}, {Shards: 4, CategoricalAttrs: []string{"Bar", "Baz", "Foo"}}} {
		c := New(opts)
		for i, s := range slots {
			if err := c.Put([]byte(fmt.Sprintf("s%d", i)), mustAd(t, s)); err != nil {
				t.Fatal(err)
			}
		}
		c.Reindex()
		for _, tc := range cases {
			jobText := `[ Requirements = ` + tc.req + `; Rank = 1 ]`
			want := bruteMatch(t, jobText, slots)
			if (len(want) == 1) != tc.match {
				t.Fatalf("MatchClassAd(%s) = %v, test expects %v", tc.req, len(want) == 1, tc.match)
			}
			checkEntryPoints(t, "left-bias", c, jobText, want)
		}
	}
}

// matchDiffAtoms are job-side operands whose value over the slot corpus below covers
// TRUE, FALSE, UNDEFINED, ERROR and non-boolean (string, integer) results, both
// wire-native (literal slot attributes) and not (a function call, an expression-valued
// slot attribute).
var matchDiffAtoms = []string{
	`TARGET.A`, `TARGET.B`, `(TARGET.N > 2)`, `(TARGET.N * 2 > 1)`, `(TARGET.S == "x")`,
	`(TARGET.E > 0)`, `(size(TARGET.S) > 1)`, `(TARGET.N >= RequestN)`, `MY.JobFlag`,
	`true`, `false`, `undefined`, `error`, `"str"`, `TARGET.Missing`,
}

// randChain builds a random &&/|| expression of depth at most depth over the atoms,
// with parenthesized nested chains and the occasional negation.
func randChain(r *rand.Rand, depth int) string {
	if depth == 0 || r.Intn(4) == 0 {
		a := matchDiffAtoms[r.Intn(len(matchDiffAtoms))]
		if r.Intn(6) == 0 {
			return "!" + a
		}
		return a
	}
	op := " && "
	if r.Intn(2) == 0 {
		op = " || "
	}
	n := 2 + r.Intn(3)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = randChain(r, depth-1)
	}
	e := "(" + strings.Join(parts, op) + ")"
	if r.Intn(8) == 0 {
		return "!" + e
	}
	return e
}

// matchDiffSlots is a corpus whose attributes take every value class the atoms
// distinguish, including absent and expression-valued ones.
func matchDiffSlots() []string {
	as := []string{`true`, `false`, `"str"`, `1`, `0`, `undefined`, `error`}
	ns := []string{`0`, `1`, `3`, `"str"`, `2.5`}
	ss := []string{`"x"`, `"yy"`, `3`}
	var out []string
	id := 0
	for i, a := range as {
		for j, n := range ns {
			for k, s := range ss {
				b := as[(i+j+k)%len(as)]
				req := `true`
				if (i+j+k)%5 == 0 {
					req = `TARGET.RequestN > 0`
				}
				out = append(out, fmt.Sprintf(`[ Id = %d; A = %s; B = %s; N = %s; S = %s; E = N + 1; Requirements = %s ]`,
					id, a, b, n, s, req))
				id++
			}
		}
	}
	// Absent attributes.
	out = append(out,
		fmt.Sprintf(`[ Id = %d; Requirements = true ]`, id),
		fmt.Sprintf(`[ Id = %d; A = true; N = 5; Requirements = true ]`, id+1))
	return out
}

// TestMatchEqualsMatchClassAdDifferential: for random &&/|| chains (nested, negated)
// whose operands evaluate to TRUE, FALSE, UNDEFINED, ERROR or a non-boolean, every
// match entry point returns exactly the slots -- and Ranks -- that classad.MatchClassAd
// returns on the original job and slot ads. Covers an unindexed collection, an indexed
// one (index-estimated conjunct ordering and index-candidate visits), and forced
// parallel fan-out.
func TestMatchEqualsMatchClassAdDifferential(t *testing.T) {
	t.Parallel()
	slots := matchDiffSlots()
	build := func(opts Options, parallel bool) *Collection {
		c := New(opts)
		for i, s := range slots {
			if err := c.Put([]byte(fmt.Sprintf("s%d", i)), mustAd(t, s)); err != nil {
				t.Fatal(err)
			}
		}
		c.Reindex()
		if parallel {
			c.parallelMinBytes = 0
		}
		return c
	}
	colls := []struct {
		name string
		c    *Collection
	}{
		{"plain", build(Options{Shards: 1, QueryParallelism: 1}, false)},
		{"indexed", build(Options{Shards: 4, CategoricalAttrs: []string{"A", "B", "S"}, ValueAttrs: []string{"N"}}, false)},
		{"parallel", build(Options{Shards: 4, QueryParallelism: 4}, true)},
	}
	r := rand.New(rand.NewSource(20261019))
	jobs := 300
	if testing.Short() {
		jobs = 60
	}
	for i := 0; i < jobs; i++ {
		req := randChain(r, 3)
		jobText := fmt.Sprintf(`[ RequestN = 2; JobFlag = %s; Requirements = %s; Rank = TARGET.N ]`,
			[]string{"true", "false", `"str"`, "undefined"}[i%4], req)
		want := bruteMatch(t, jobText, slots)
		for _, cc := range colls {
			checkEntryPoints(t, cc.name, cc.c, jobText, want)
		}
	}
}
