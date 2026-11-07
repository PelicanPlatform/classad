package collections

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// A `.`-path select does not stop at the record it selects from. A member the record
// does not define CHAINS to the enclosing scope -- the ad the select was written in --
// so `Res.Mem` reads the ad's own Mem when Res has none. That is the ClassAd scoping
// rule, and the C++ reference engine does the same: `Res.Mem >= 4096` over
// `[Mem=4096; Res=[Other=1]]` is TRUE there, FALSE only when the record shadows the
// name with a smaller value.
//
// A scan must answer exactly as q.Matches does. What makes that non-trivial is that a
// scan does not evaluate the whole record: it decodes the attributes the query reads
// and evaluates against those. The chained-to attribute is one of them even though the
// query never names it, so a read set built from the names alone would evaluate the
// query against an ad that lacks an attribute the whole record has -- and silently
// return fewer rows. These tests are the differential that catches that: the same ads
// through both paths, asserting identical row sets.

// selectChainAds returns the fixture, keyed by the ad's Name, covering every shape a
// select's member lookup can take against the enclosing scope. Each is repeated enough
// times (with a distinct Name) that a scan has segments to walk, and padded with filler
// so a partial decode is doing less work than a whole one.
func selectChainAds() map[string]string {
	shapes := []struct{ name, body string }{
		// The chaining shape: Res is present and lacks Mem, so Res.Mem is the OUTER Mem.
		{"chain", `Mem = 4096; Res = [ Other = 1 ]`},
		// The non-chaining shape: Res defines Mem, so the chain is never walked.
		{"inner", `Mem = 1; Res = [ Mem = 8192 ]`},
		// A nested ad that SHADOWS the outer name with a value that does not match.
		{"shadow", `Mem = 4096; Res = [ Mem = 1 ]`},
		// The outer attribute is missing, so the chain finds nothing: undefined.
		{"noouter", `Res = [ Other = 1 ]`},
		// No record at all: the select's own base is undefined.
		{"nores", `Mem = 4096`},
		// Two levels: A.B.Mem walks B, then A, then the ad.
		{"twolevel", `Mem = 4096; A = [ B = [ C = 1 ] ]`},
		// Two levels where the MIDDLE record supplies the name.
		{"twolevelmid", `Mem = 1; A = [ Mem = 8192; B = [ C = 1 ] ]`},
		// The chained-to value is itself an expression over other attributes, so the
		// chained read must drag its own closure in with it.
		{"chainexpr", `Base = 2048; Mem = Base * 2; Res = [ Other = 1 ]`},
		// The selected member is a string, chained from the enclosing scope.
		{"chainstr", `Owner = "alice"; Res = [ Other = 1 ]`},
		// The record shadows the string name.
		{"shadowstr", `Owner = "alice"; Res = [ Owner = "bob" ]`},
	}
	out := map[string]string{}
	for _, s := range shapes {
		for i := 0; i < 20; i++ {
			name := fmt.Sprintf("%s-%02d", s.name, i)
			var b strings.Builder
			fmt.Fprintf(&b, `[ Name = "%s"; Ord = %d; %s`, name, i, s.body)
			for f := 0; f < 6; f++ {
				fmt.Fprintf(&b, `; Filler%02d = "filler %s-%d"`, f, name, f)
			}
			b.WriteString(" ]")
			out[name] = b.String()
		}
	}
	return out
}

// selectChainQueries are the constraints run over the fixture. Each reads through a
// select whose member the record may or may not define.
var selectChainQueries = []string{
	`Res.Mem >= 4096`,
	`Res.Mem < 4096`,
	`A.B.Mem >= 4096`,
	`A.B.Mem >= 4096 || Res.Mem >= 4096`,
	`Res.Owner == "alice"`,
	`!(Res.Mem >= 4096)`,
	`Res.Mem >= 4096 && Ord < 10`,
	`[ pick = Res ].pick.Mem >= 4096`,
}

// evaluatorRows is the oracle: the names of the ads q.Matches accepts, which is the
// tree-walking evaluator's own answer on the whole ad.
func evaluatorRows(t *testing.T, q *vm.Query, ads map[string]string) []string {
	t.Helper()
	var out []string
	for name, src := range ads {
		if q.Matches(mustAd(t, src)) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func namesOfAds(t *testing.T, seq func(func(*classad.ClassAd) bool)) []string {
	t.Helper()
	var out []string
	for ad := range seq {
		n, ok := ad.EvaluateAttrString("Name")
		if !ok {
			t.Fatalf("a scanned row has no Name: %s", ad.String())
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// TestSelectChainsToEnclosingScopeOnScan is the differential: every fixture ad through
// q.Matches and through the store's read paths, asserting identical row sets. The arms
// are the ways a record can be reached -- in memory, across shards with query fan-out,
// from disk, through a nested-path index on the very paths these queries read, and
// through a plain value index on the attributes they chain TO.
//
// `Res.Mem >= 4096` over the "chain" ads is the case a scan can only get right by
// carrying the enclosing Mem, so a fixture without a chaining shape proves nothing
// here. TestSelectChainIsNotVacuous holds the fixture to having one.
func TestSelectChainsToEnclosingScopeOnScan(t *testing.T) {
	t.Parallel()
	ads := selectChainAds()
	for _, arm := range []struct {
		name string
		open func(t *testing.T) *Collection
	}{
		{"memory", func(t *testing.T) *Collection { return New(Options{Shards: 1}) }},
		{"memory-sharded-parallel", func(t *testing.T) *Collection {
			return New(Options{Shards: 4, SegmentSize: 1 << 12, QueryParallelism: 4})
		}},
		{"persistent", func(t *testing.T) *Collection {
			c, err := Open(Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 12})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })
			return c
		}},
		// A nested-path index on the very paths these queries read. The index answers
		// from what it finds AT the path, which is not what the query evaluates to when
		// the record lacks the member -- so a record whose Res is present without a Mem
		// has to stay a candidate. This arm is what holds it to that.
		{"path-indexed", func(t *testing.T) *Collection {
			return New(Options{Shards: 2, SegmentSize: 1 << 12,
				ValueAttrs: []string{"Res.Mem", "A.B.Mem"}, CategoricalAttrs: []string{"Res.Owner"}})
		}},
		{"value-indexed", func(t *testing.T) *Collection {
			return New(Options{Shards: 2, SegmentSize: 1 << 12, ValueAttrs: []string{"Mem", "Ord"}})
		}},
	} {
		t.Run(arm.name, func(t *testing.T) {
			c := arm.open(t)
			for name, src := range ads {
				if err := c.Put([]byte(name), mustAd(t, src)); err != nil {
					t.Fatal(err)
				}
			}
			c.Reindex()
			for _, qs := range selectChainQueries {
				want := evaluatorRows(t, mustParseQ(t, qs), ads)
				got := namesOfAds(t, c.Query(mustParseQ(t, qs)))
				if !slices.Equal(got, want) {
					t.Errorf("%s: scan returned %d rows, the evaluator %d\n scan: %v\n eval: %v",
						qs, len(got), len(want), got, want)
				}
			}
		})
	}
}

// TestSelectChainsToEnclosingScopeColumnar runs the same differential over a
// columnarized store, where a record can be matched from its columns or rebuilt
// narrowed to what the query reads -- two more places the chained attribute could be
// left out.
func TestSelectChainsToEnclosingScopeColumnar(t *testing.T) {
	t.Parallel()
	ads := selectChainAds()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	for name, src := range ads {
		if err := c.Put([]byte(name), mustAd(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no columnar schema derived: the columnar arm would not be covered")
	}
	c.ColumnarizeSealed()
	if columnarSegmentsForTest(c) == 0 {
		t.Fatal("fixture columnarized nothing")
	}
	for _, qs := range selectChainQueries {
		want := evaluatorRows(t, mustParseQ(t, qs), ads)
		got := namesOfAds(t, c.Query(mustParseQ(t, qs)))
		if !slices.Equal(got, want) {
			t.Errorf("%s: columnar scan returned %d rows, the evaluator %d\n scan: %v\n eval: %v",
				qs, len(got), len(want), got, want)
		}
	}
}

// TestSelectChainsToEnclosingScopeRawPaths holds the raw-wire and text-projected reads
// to the same row set. They filter through the same plan the decoding scan does, so a
// seed the plan omitted would drop rows from these too.
func TestSelectChainsToEnclosingScopeRawPaths(t *testing.T) {
	t.Parallel()
	ads := selectChainAds()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	for name, src := range ads {
		if err := c.Put([]byte(name), mustAd(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	for _, qs := range selectChainQueries {
		want := evaluatorRows(t, mustParseQ(t, qs), ads)

		var raw []string
		for ra := range c.QueryRaw(mustParseQ(t, qs)) {
			ad := rawAdToClassAd(t, ra)
			n, ok := ad.EvaluateAttrString("Name")
			if !ok {
				t.Fatalf("%s: a raw row has no Name", qs)
			}
			raw = append(raw, n)
		}
		sort.Strings(raw)
		if !slices.Equal(raw, want) {
			t.Errorf("%s: QueryRaw returned %v, the evaluator %v", qs, raw, want)
		}

		var proj []string
		for ra := range c.QueryRawProjected(mustParseQ(t, qs), []string{"Name"}, true, false) {
			ad := rawAdToClassAd(t, ra)
			n, ok := ad.EvaluateAttrString("Name")
			if !ok {
				t.Fatalf("%s: a projected row has no Name", qs)
			}
			proj = append(proj, n)
		}
		sort.Strings(proj)
		if !slices.Equal(proj, want) {
			t.Errorf("%s: QueryRawProjected returned %v, the evaluator %v", qs, proj, want)
		}
	}
}

// TestSelectChainPathIndexStaysSound holds the nested-path index to the same row set as
// the evaluator, and -- the half that keeps it from passing for the wrong reason --
// proves the index was actually consulted.
//
// The index is keyed on what it finds AT the path. A record whose Res is present but
// defines no Mem evaluates `Res.Mem` as the ad's own Mem, which the entry at that path
// cannot know, so such a record must stay a candidate for every probe on it rather than
// be posted as absent. If it were posted as absent the chaining rows would be dropped
// here and nowhere else.
func TestSelectChainPathIndexStaysSound(t *testing.T) {
	t.Parallel()
	ads := selectChainAds()
	c := New(Options{Shards: 1, SegmentSize: 1 << 12, ValueAttrs: []string{"Res.Mem"}})
	for name, src := range ads {
		if err := c.Put([]byte(name), mustAd(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	const qs = `Res.Mem >= 4096`
	want := evaluatorRows(t, mustParseQ(t, qs), ads)
	var stats ScanStats
	var got []string
	for row := range c.QueryProjectStats(mustParseQ(t, qs), []string{"Name"}, &stats) {
		n, _ := row[0].StringValue()
		got = append(got, n)
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("path-indexed read returned %v, the evaluator %v", got, want)
	}
	t.Logf("path index: %d records, %d candidates, segments indexed/skipped/unindexed %d/%d/%d",
		len(ads), stats.IndexCandidates, stats.SegmentsIndexed, stats.SegmentsIndexSkipped, stats.SegmentsUnindexed)
	if stats.SegmentsIndexed == 0 {
		t.Fatalf("no segment was answered from the path index, so this proves nothing about it: %+v", stats)
	}
	if stats.IndexCandidates >= len(ads) {
		t.Errorf("the index narrowed nothing: %d candidates of %d records", stats.IndexCandidates, len(ads))
	}
}

// TestSelectChainIsNotVacuous pins that the fixture actually contains the shape the
// bug was about: a query that chains, one that is shadowed, and one that resolves
// inside the record, each matching a different set. Without this a later edit could
// leave every query matching nothing and the differential would still pass.
func TestSelectChainIsNotVacuous(t *testing.T) {
	t.Parallel()
	ads := selectChainAds()
	rows := evaluatorRows(t, mustParseQ(t, `Res.Mem >= 4096`), ads)
	kinds := map[string]int{}
	for _, n := range rows {
		kinds[n[:strings.LastIndex(n, "-")]]++
	}
	// "chain" matches only by chaining to the enclosing Mem; "inner" matches on the
	// record's own Mem; "chainexpr" chains to an Mem that is itself an expression.
	for _, want := range []string{"chain", "inner", "chainexpr"} {
		if kinds[want] == 0 {
			t.Errorf("fixture no longer matches any %q ad on `Res.Mem >= 4096`: %v", want, kinds)
		}
	}
	// "shadow" defines Res.Mem = 1, so it must NOT match, or the chain is being
	// preferred over the record's own member.
	if kinds["shadow"] != 0 {
		t.Errorf("a shadowing ad matched `Res.Mem >= 4096`: the record's own member must win: %v", kinds)
	}
	for _, n := range []string{"noouter", "nores"} {
		if kinds[n] != 0 {
			t.Errorf("%q matched `Res.Mem >= 4096` with nothing to resolve Mem to: %v", n, kinds)
		}
	}
}

// selectValueAd is ad i of a fixture whose STORED expressions reach the ad through a
// select: X reads Res.Mem, which Res does not define, so it is the ad's own Mem. A
// projection of X must ship Mem with it, or X evaluates to undefined on the projected
// row while it is a number on the whole ad.
func selectValueAd(i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[ Name = "p%05d"; Ord = %d; Mem = %d; Slack = %d; Deep = %d`,
		i, i, 1024*(1+i%8), i%17, 100+i%23)
	b.WriteString(`; Res = [ Other = 1 ]`)
	b.WriteString(`; A = [ B = [ C = 1 ] ]`)
	b.WriteString(`; X = Res.Mem`)
	b.WriteString(`; Y = Res.Mem + Slack`)
	b.WriteString(`; Z = A.B.Deep * 2`)
	b.WriteString(`; S = [ inner = Res.Mem ].inner`)
	for f := 0; f < 6; f++ {
		fmt.Fprintf(&b, `; Filler%02d = "filler %05d-%d"`, f, i, f)
	}
	b.WriteString(" ]")
	return b.String()
}

// TestProjectionChasesRefsThroughSelects holds a projected row's evaluation of a
// select-valued attribute to the whole ad's, over the text and wire projections and
// over plain and columnarized segments.
//
// The attribute the chain resolves to is part of X's closure even though no expression
// outside the select names it, so the reference walk has to contribute the select's
// member or the row ships Res without Mem.
func TestProjectionChasesRefsThroughSelects(t *testing.T) {
	t.Parallel()
	const n = 400
	projected := []string{"Name", "X", "Y", "Z", "S"}
	// The two key encodings reach the reference walk through different code: an
	// in-memory collection stores interned ids, a persistent one inline names.
	for _, arm := range []struct {
		name     string
		columnar bool
		open     func(t *testing.T) *Collection
	}{
		{"interned", false, func(t *testing.T) *Collection {
			return New(Options{Shards: 1, SegmentSize: 1 << 13})
		}},
		{"inline", false, nil},
		{"inline+columnar", true, nil},
	} {
		columnar := arm.columnar
		t.Run(arm.name, func(t *testing.T) {
			c := func() *Collection {
				if arm.open != nil {
					return arm.open(t)
				}
				c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 13})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.Close() })
				return c
			}()
			for i := 0; i < n; i++ {
				if err := c.Put([]byte(fmt.Sprintf("p%05d", i)), mustAd(t, selectValueAd(i))); err != nil {
					t.Fatal(err)
				}
			}
			if columnar {
				if !c.BuildAndEnableSchemaScan(4096, 8) {
					t.Fatal("no columnar schema derived: the columnar arm would not be covered")
				}
				c.ColumnarizeSealed()
				if columnarSegmentsForTest(c) == 0 {
					t.Fatal("fixture columnarized nothing")
				}
			}

			// The oracle: each attribute evaluated on the whole stored ad.
			want := map[string]map[string]string{}
			for ad := range c.Scan() {
				nm, ok := ad.EvaluateAttrString("Name")
				if !ok {
					t.Fatal("a stored ad has no Name")
				}
				want[nm] = evalAll(ad, projected[1:]...)
			}
			if len(want) != n {
				t.Fatalf("scan returned %d ads, want %d", len(want), n)
			}
			for _, v := range want {
				for a, got := range v {
					if got == "undefined" || got == "error" {
						t.Fatalf("the fixture's %s does not evaluate on the whole ad (%s): the test would pass vacuously", a, got)
					}
				}
				break
			}

			check := func(path string, ad *classad.ClassAd) {
				t.Helper()
				nm, ok := ad.EvaluateAttrString("Name")
				if !ok {
					t.Fatalf("%s: a projected row has no Name", path)
				}
				for a, w := range want[nm] {
					if got := ad.EvaluateAttr(a).String(); got != w {
						t.Fatalf("%s: %s %s = %s on the projected row, %s on the whole ad (row: %s)",
							path, nm, a, got, w, ad.String())
					}
				}
			}

			rows := 0
			for ra := range c.ScanRawProjected(projected, true, false) {
				check("text", rawAdToClassAd(t, ra))
				rows++
			}
			if rows != n {
				t.Fatalf("text projection returned %d rows, want %d", rows, n)
			}
			wrows := 0
			for row := range c.QueryRawWireRefs(mustParseQ(t, "true"), projected, false) {
				m := wireRowMap(t, row)
				var b strings.Builder
				for k, v := range m {
					fmt.Fprintf(&b, "%s = %s\n", k, v)
				}
				ad, err := classad.ParseOld(b.String())
				if err != nil {
					t.Fatalf("wire row does not parse: %v\n%s", err, b.String())
				}
				check("wire", ad)
				wrows++
			}
			if wrows != n {
				t.Fatalf("wire projection returned %d rows, want %d", wrows, n)
			}
		})
	}
}

// TestMatchChasesSelectsIntoTheSlotClosure is the matchmaking counterpart: a slot
// expression that reads `Res.Memory` where Res has no Memory reads the SLOT's Memory,
// so every match path that evaluates the slot against a closure of it must carry that
// attribute. The hot-header and closure-decode paths build exactly such a partial ad.
//
// classad.MatchClassAd (bruteMatch) is the oracle, as it is for the other match
// invariants here.
func TestMatchChasesSelectsIntoTheSlotClosure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		job   string
		slots []string
		want  map[int]matchResult
	}{
		{
			// Requirements reads a member the record does not define, so it is the
			// slot's own Memory. Id 3's record shadows it with a value that passes.
			name: "requirements-through-select",
			job:  `[ Requirements = true; Rank = 1 ]`,
			slots: []string{
				`[ Id = 1; Memory = 10; Res = [ Other = 1 ]; Requirements = Res.Memory > 5 ]`,
				`[ Id = 2; Memory = 4; Res = [ Other = 1 ]; Requirements = Res.Memory > 5 ]`,
				`[ Id = 3; Memory = 4; Res = [ Memory = 10 ]; Requirements = Res.Memory > 5 ]`,
			},
			want: map[int]matchResult{1: {1, true}, 3: {1, true}},
		},
		{
			// The job's Rank reads a slot attribute whose value is a select, so the
			// rank top-up has to chase the chained-to attribute as well.
			name: "rank-through-select",
			job:  `[ Requirements = true; Rank = TARGET.Score ]`,
			slots: []string{
				`[ Id = 1; Memory = 10; Res = [ Other = 1 ]; Score = Res.Memory; Requirements = true ]`,
				`[ Id = 2; Memory = 20; Res = [ Memory = 7 ]; Score = Res.Memory; Requirements = true ]`,
			},
			want: map[int]matchResult{1: {10, true}, 2: {7, true}},
		},
		{
			// Two levels, and the chained-to attribute is itself an expression.
			name: "deep-select",
			job:  `[ Requirements = true; Rank = 1 ]`,
			slots: []string{
				`[ Id = 1; Base = 3; Memory = Base * 4; A = [ B = [ C = 1 ] ]; Requirements = A.B.Memory > 5 ]`,
				`[ Id = 2; Base = 1; Memory = Base * 4; A = [ B = [ C = 1 ] ]; Requirements = A.B.Memory > 5 ]`,
			},
			want: map[int]matchResult{1: {1, true}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := bruteMatch(t, tc.job, tc.slots)
			if diff := diffMatchSets(want, tc.want, true); diff != "" {
				t.Fatalf("MatchClassAd differs from the case's expectation:\n%s", diff)
			}
			// The case has to tell something apart, or every path agrees vacuously:
			// either the chain decides which slots match, or it decides their ranks.
			if len(want) == 0 {
				t.Fatal("the case matches no slot: nothing is being compared")
			}
			if len(want) == len(tc.slots) {
				ranks := map[float64]bool{}
				for _, r := range want {
					ranks[r.rank] = true
				}
				if len(ranks) < 2 {
					t.Fatalf("every slot matches with the same rank: the case does not discriminate (%v)", want)
				}
			}
			for name, c := range targetDepColls(t, tc.slots, []string{"Memory"}) {
				checkEntryPoints(t, name, c, tc.job, want)
				for _, k := range []int{1, 2} { // deferred top-N, where closure decode runs
					got := c.MatchSortedRanked(mustAd(t, tc.job), k)
					if len(got) != min(k, len(want)) {
						t.Fatalf("%s limit %d: got %d matches, want %d", name, k, len(got), min(k, len(want)))
					}
					for _, rm := range got {
						id, _ := rm.Ad.EvaluateAttrInt("Id")
						w, ok := want[int(id)]
						if !ok || w.hasRank != rm.HasRank || (w.hasRank && w.rank != rm.Rank) {
							t.Fatalf("%s limit %d: Id %d rank %v/%v, MatchClassAd %v (matched %v)",
								name, k, id, rm.Rank, rm.HasRank, w, ok)
						}
					}
				}
			}
		})
	}
}

// TestQueryOverSelectValuedAttribute constrains an attribute whose STORED value is a
// select. The scan resolves such an attribute by decoding it and then everything it
// references, and on a columnarized segment it may match a record rebuilt narrowed to
// that set -- which is admitted only when the narrowed ad is closed under those
// references. A reference walk that stopped at the selected record would call the
// narrowed ad closed while it was missing the attribute the chain resolves to, and
// decide the query on an ad that evaluates X differently from the whole record.
func TestQueryOverSelectValuedAttribute(t *testing.T) {
	t.Parallel()
	const n = 600
	ad := func(i int) string {
		var b strings.Builder
		fmt.Fprintf(&b, `[ Name = "q%05d"; Ord = %d; Mem = %d`, i, i, 1024*(1+i%8))
		b.WriteString(`; Res = [ Other = 1 ]`)
		b.WriteString(`; X = Res.Mem`)         // chains: the ad's own Mem
		b.WriteString(`; W = [ Mem = 7 ].Mem`) // does not chain: the literal's member
		for f := 0; f < 10; f++ {
			fmt.Fprintf(&b, `; Filler%02d = "filler %05d-%d"`, f, i, f)
		}
		b.WriteString(" ]")
		return b.String()
	}
	for _, columnar := range []bool{false, true} {
		name := "plain"
		if columnar {
			name = "columnar"
		}
		t.Run(name, func(t *testing.T) {
			c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 13})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })
			srcs := map[string]string{}
			for i := 0; i < n; i++ {
				src := ad(i)
				srcs[fmt.Sprintf("q%05d", i)] = src
				if err := c.Put([]byte(fmt.Sprintf("q%05d", i)), mustAd(t, src)); err != nil {
					t.Fatal(err)
				}
			}
			if columnar {
				if !c.BuildAndEnableSchemaScan(4096, 8) {
					t.Fatal("no columnar schema derived: the columnar arm would not be covered")
				}
				c.ColumnarizeSealed()
				if columnarSegmentsForTest(c) == 0 {
					t.Fatal("fixture columnarized nothing")
				}
			}
			for _, qs := range []string{`X >= 4096`, `X < 4096`, `W == 7`, `X >= 4096 && W == 7`} {
				want := evaluatorRows(t, mustParseQ(t, qs), srcs)
				if len(want) == 0 {
					t.Fatalf("%s matches no ad: the comparison is vacuous", qs)
				}
				got := namesOfAds(t, c.Query(mustParseQ(t, qs)))
				if !slices.Equal(got, want) {
					t.Errorf("%s: scan returned %d rows, the evaluator %d", qs, len(got), len(want))
				}
			}
		})
	}
}

// TestSelectChainDoesNotReachASealedValue: making the chain work makes a sealed
// attribute reachable through a select -- `Res.Secret` reads the ad's Secret when the
// record has none -- so the reader that may not see that value must not be able to
// match on it either. It reads through the keyless wire context, which hands back
// undefined for a withheld attribute whatever form it is stored in, so the chained read
// is withheld exactly where a direct one is.
//
// The entitled arm is what keeps this honest: before the chain worked, BOTH readers got
// nothing and the test would have passed on a store that never resolved the select at
// all.
func TestSelectChainDoesNotReachASealedValue(t *testing.T) {
	t.Parallel()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 13,
		DataKey: sealedRefKey(), EncryptedAttrs: []string{"Secret"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	for i := 0; i < 50; i++ {
		src := fmt.Sprintf(`[ Name = "s%03d"; Secret = %d; Res = [ Other = 1 ] ]`, i, 1000+i)
		if err := c.Put([]byte(fmt.Sprintf("s%03d", i)), mustAd(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	count := func(seq func(func(*classad.ClassAd) bool)) int {
		n := 0
		for range seq {
			n++
		}
		return n
	}
	for _, qs := range []string{`Secret == 1007`, `Res.Secret == 1007`} {
		if got := count(c.Query(mustParseQ(t, qs))); got != 1 {
			t.Errorf("%s: an entitled reader matched %d records, want 1", qs, got)
		}
		if got := count(c.QueryRedacted(mustParseQ(t, qs))); got != 0 {
			t.Errorf("%s: a reader with no key matched %d records on a sealed value, want 0", qs, got)
		}
	}
}
