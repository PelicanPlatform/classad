package collections

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// nestedCorpus is the source text of the ads every nested-path test runs against. Each one is
// a shape the path walk has to classify, and the comment says what `Res.Mem` evaluates to:
// the index must agree with that, or hand the scan a candidate set that still contains it.
var nestedCorpus = []string{
	`[ ID = 0;  Owner = "alice"; Res = [ Mem = 2048; Tag = "t1" ] ]`,      // 2048
	`[ ID = 1;  Owner = "bob";   Res = [ Mem = 4096; Tag = "t2" ] ]`,      // 4096
	`[ ID = 2;  Owner = "alice"; Res = [ Mem = 8192; Tag = "T1" ] ]`,      // 8192, mixed-case tag
	`[ ID = 3;  Owner = "carol"; Res = [ Mem = 4096.0; Tag = "t3" ] ]`,    // real, not int
	`[ ID = 4;  Owner = "alice" ]`,                                        // Res absent -> undefined
	`[ ID = 5;  Owner = "bob";   Res = 4096 ]`,                            // Res not a record -> error
	`[ ID = 6;  Owner = "carol"; Res = "4096" ]`,                          // ditto, a string
	`[ ID = 7;  Owner = "alice"; Res = { [ Mem = 4096 ], [ Mem = 1 ] } ]`, // a list -> list projection
	`[ ID = 8;  Owner = "bob";   Res = [ ] ]`,                             // empty record, no chain -> undefined
	`[ ID = 9;  Owner = "carol"; Mem = 4096; Res = [ Other = 1 ] ]`,       // CHAINS to the enclosing Mem -> 4096
	`[ ID = 10; Owner = "alice"; Mem = "4096"; Res = [ Other = 1 ] ]`,     // chains to a string
	`[ ID = 11; Owner = "bob";   Base = 2048; Res = [ Mem = Base * 2 ] ]`, // expression -> 4096
	`[ ID = 12; Owner = "carol"; Res = [ Mem = undefined ] ]`,             // undefined literal
	`[ ID = 13; Owner = "alice"; Res = [ Mem = error ] ]`,                 // error literal
	`[ ID = 14; Owner = "bob";   Res = [ Mem = "4096"; Tag = "t1" ] ]`,    // a string where others hold numbers
	`[ ID = 15; Owner = "carol"; Res = [ Mem = true; Tag = true ] ]`,      // booleans
	`[ ID = 16; Owner = "alice"; Res = [ Mem = { 1, 2 } ] ]`,              // a list value
	`[ ID = 17; Owner = "bob";   Res = [ Mem = [ X = 7 ] ] ]`,             // a record value; Res.Mem.X = 7
	`[ ID = 18; Owner = "carol"; Res = [ Deep = [ X = 7 ] ] ]`,            // three-level path
	`[ ID = 19; Owner = "alice"; Res = [ Deep = [ X = 9 ]; Mem = 0 ] ]`,   // Res.Mem = 0 (falsy)
	`[ ID = 20; Owner = "bob";   Res = [ Mem = -4096 ] ]`,                 // negative: a folded unary minus
	`[ ID = 21; Owner = "carol"; Res = [ Mem = 4096; Tag = "t2" ] ]`,
	`[ ID = 22; Owner = "alice"; Res = [ Mem = 2048; Tag = "t2" ] ]`,
	`[ ID = 23; Owner = "bob";   Deep = [ X = 7 ]; Res = [ Mem = 1 ] ]`, // Res.Deep.X chains to Deep -> 7
}

// nestedQueries are run over every corpus, indexed and not: the operators an index answers
// differently (range, equality, membership, identity, presence, truthiness), at every depth,
// alone and conjoined with a top-level attribute.
var nestedQueries = []string{
	`Res.Mem >= 4096`,
	`Res.Mem > 4096`,
	`Res.Mem == 4096`,
	`Res.Mem != 4096`,
	`Res.Mem < 4096`,
	`Res.Mem == 4096.0`,
	`Res.Mem =?= 4096`,
	`Res.Mem =!= 4096`,
	`Res.Mem is undefined`,
	`Res.Mem isnt undefined`,
	`isUndefined(Res.Mem)`,
	`!isUndefined(Res.Mem)`,
	`!(Res.Mem == 4096)`,
	`!(Res.Mem <= 4096)`,
	`Res.Mem == 2048 || Res.Mem == 8192`,
	`member(Res.Mem, { 2048, 8192 })`,
	`Res.Mem`,
	`!Res.Mem`,
	`Res.Tag == "t1"`,
	`Res.Tag != "t1"`,
	`Res.Tag =?= "t1"`,
	`Res.Tag =!= "t1"`,
	`Res.Tag == "t1" || Res.Tag == "t2"`,
	`Res.Tag is undefined`,
	`Res.Tag isnt undefined`,
	`Res.Deep.X == 7`,
	`Res.Deep.X is undefined`,
	`Res.Mem.X == 7`,
	`Owner == "alice" && Res.Mem >= 4096`,
	`Owner == "alice" || Res.Mem >= 4096`,
	`Res.Mem >= 2048 && Res.Tag == "t2"`,
	`Res.Mem > 1024 * 4`,
	`Res.Nope == 1`,
}

// nestedPathIndexes is the index configuration under test: three nested paths, one of each
// kind, plus a top-level attribute so mixed conjunctions are exercised too.
var (
	nestedCatAttrs = []string{"Owner", "Res.Tag"}
	nestedValAttrs = []string{"Res.Mem", "Res.Deep.X"}
)

// chainingIDs are the corpus ads whose nested path resolves by CHAINING out of the nested
// record to an attribute of the enclosing ad: `[ Mem = 4096; Res = [ Other = 1 ] ]` has
// `Res.Mem` = 4096 under the reference evaluator (classad.Evaluator.selectAttr links the
// nested ad's scope to the selecting one), while a collection scan reads it as undefined.
//
// That divergence is the store's, not this index's, and it predates this change: on
// origin/main an UNINDEXED collection holding only that ad returns no row for
// `Res.Mem >= 4096` while q.Matches on the same ad is true. So these ads stay in the corpus --
// they are exactly the shape the index must not decide for itself, and it posts them as
// exceptions so the scan has the last word -- but the reference evaluator is not used as the
// oracle for them. The oracle that covers them is the unindexed collection.
var chainingIDs = map[int]bool{9: true, 10: true, 23: true}

func parseNestedCorpus(t *testing.T) map[int]*classad.ClassAd {
	t.Helper()
	src := map[int]*classad.ClassAd{}
	for _, text := range nestedCorpus {
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatalf("parse %s: %v", text, err)
		}
		src[idOf(t, ad)] = ad
	}
	return src
}

// withoutChaining is the corpus minus the ads whose path chains (see chainingIDs), for the
// comparisons that use the reference evaluator as the oracle.
func withoutChaining(src map[int]*classad.ClassAd) map[int]*classad.ClassAd {
	out := make(map[int]*classad.ClassAd, len(src))
	for id, ad := range src {
		if !chainingIDs[id] {
			out[id] = ad
		}
	}
	return out
}

// fillNested puts the corpus into c, repeated reps times with distinct keys so the
// collection is big enough for the planner to prefer an index over a scan.
func fillNested(t *testing.T, c *Collection, src map[int]*classad.ClassAd, reps int) {
	t.Helper()
	ids := make([]int, 0, len(src))
	for id := range src {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for r := 0; r < reps; r++ {
		for _, id := range ids {
			if err := c.Put([]byte(fmt.Sprintf("k%d-%d", r, id)), src[id]); err != nil {
				t.Fatal(err)
			}
		}
	}
	c.Reindex()
}

// nestedIDs is queryIDs with the per-rep duplicates collapsed, so an indexed and an unindexed
// collection filled with different repetition counts still compare.
func nestedIDs(t *testing.T, c *Collection, q *vm.Query) []int {
	t.Helper()
	seen := map[int]bool{}
	for ad := range c.Query(q) {
		seen[idOf(t, ad)] = true
	}
	out := make([]int, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// TestNestedPathIndexedEqualsUnindexed is the correctness bar: for every query, the ads a
// collection with the nested paths indexed returns are exactly the ads an identically
// populated collection with no index returns. The unindexed collection is the oracle because
// it is the same scan the indexed read falls back to -- an index may only remove candidates
// that cannot match, never change what matching means.
func TestNestedPathIndexedEqualsUnindexed(t *testing.T) {
	src := parseNestedCorpus(t)
	indexed := New(Options{Shards: 4, CategoricalAttrs: nestedCatAttrs, ValueAttrs: nestedValAttrs})
	plain := New(Options{Shards: 4})
	fillNested(t, indexed, src, 20)
	fillNested(t, plain, src, 20)
	for _, qs := range nestedQueries {
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatalf("parse %q: %v", qs, err)
		}
		want := nestedIDs(t, plain, q)
		if got := nestedIDs(t, indexed, q); !equalInts(got, want) {
			t.Fatalf("%q: indexed read returned %v, unindexed scan returned %v", qs, got, want)
		}
	}
}

// TestNestedPathMatchesEvaluator is the second oracle: over the corpus whose paths do not
// chain (see chainingIDs), an indexed read returns exactly the ads the reference evaluator
// matches. Without it the test above could agree with a scan that is itself wrong.
func TestNestedPathMatchesEvaluator(t *testing.T) {
	src := withoutChaining(parseNestedCorpus(t))
	indexed := New(Options{Shards: 4, CategoricalAttrs: nestedCatAttrs, ValueAttrs: nestedValAttrs})
	fillNested(t, indexed, src, 20)
	for _, qs := range nestedQueries {
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatalf("parse %q: %v", qs, err)
		}
		want := bruteIDs(src, q)
		if got := nestedIDs(t, indexed, q); !equalInts(got, want) {
			t.Fatalf("%q: indexed read returned %v, evaluator says %v", qs, got, want)
		}
	}
}

// TestNestedPathIndexEngages is the other half: the feature must actually be used. Without it
// the correctness test above would pass against an index that never answers anything.
func TestNestedPathIndexEngages(t *testing.T) {
	src := parseNestedCorpus(t)
	indexed := New(Options{Shards: 4, CategoricalAttrs: nestedCatAttrs, ValueAttrs: nestedValAttrs})
	plain := New(Options{Shards: 4})
	fillNested(t, indexed, src, 40)
	fillNested(t, plain, src, 40)
	for _, qs := range []string{`Res.Mem >= 4096`, `Res.Mem == 2048`, `Res.Tag == "t1"`, `Res.Deep.X == 7`} {
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatal(err)
		}
		ex := indexed.ExplainQuery(q)
		if ex.Plan != "indexed" {
			t.Fatalf("%q: plan %q, want indexed (%s)", qs, ex.Plan, ex.Reason)
		}
		if ex.EstVisited >= int64(ex.TotalAds) {
			t.Fatalf("%q: EstVisited %d of %d -- the index pruned nothing", qs, ex.EstVisited, ex.TotalAds)
		}
		base := plain.ExplainQuery(q)
		if base.Plan == "indexed" {
			t.Fatalf("%q: the unindexed collection also planned indexed; the comparison is vacuous", qs)
		}
		if ex.EstVisited >= base.EstVisited {
			t.Fatalf("%q: EstVisited %d indexed vs %d unindexed", qs, ex.EstVisited, base.EstVisited)
		}
	}
}

// TestNestedPathProbeExtraction pins the probe a nested constraint yields, since everything
// downstream keys off its shape.
func TestNestedPathProbeExtraction(t *testing.T) {
	cases := []struct {
		expr string
		attr string
		path []string
	}{
		{`Res.Mem >= 4096`, "Res.Mem", []string{"Res", "Mem"}},
		{`4096 <= Res.Mem`, "Res.Mem", []string{"Res", "Mem"}},
		{`MY.Res.Mem == 1`, "Res.Mem", []string{"Res", "Mem"}}, // a MY-scoped root is the same path
		{`Res.Deep.X == 7`, "Res.Deep.X", []string{"Res", "Deep", "X"}},
		{`(Res).Mem == 7`, "Res.Mem", []string{"Res", "Mem"}},
		{`Memory >= 4096`, "Memory", nil},
	}
	for _, tc := range cases {
		q, err := vm.Parse(tc.expr)
		if err != nil {
			t.Fatalf("%q: %v", tc.expr, err)
		}
		probes := q.Probes()
		if len(probes) != 1 {
			t.Fatalf("%q: %d probes, want 1", tc.expr, len(probes))
		}
		p := probes[0]
		if tc.path == nil {
			if p.Nested() || p.Attr != tc.attr {
				t.Fatalf("%q: probe %+v, want a top-level probe on %q", tc.expr, p, tc.attr)
			}
			continue
		}
		if p.Attr != tc.attr || !p.Nested() || len(p.Path) != len(tc.path) {
			t.Fatalf("%q: probe %+v, want attr %q path %v", tc.expr, p, tc.attr, tc.path)
		}
		for i := range tc.path {
			if p.Path[i] != tc.path[i] {
				t.Fatalf("%q: probe path %v, want %v", tc.expr, p.Path, tc.path)
			}
		}
	}
}

// TestNestedPathJoinAliasNotProbed: `j.Owner` over a declared join alias is the OTHER side of
// a join, not a nested record of this ad, and must not be probed as a path -- this
// collection's index knows nothing about the other side.
func TestNestedPathJoinAliasNotProbed(t *testing.T) {
	q, err := vm.ParseAliases(`j.Owner == "alice"`, []string{"j"})
	if err != nil {
		t.Fatal(err)
	}
	if ps := q.Probes(); len(ps) != 0 {
		t.Fatalf("alias select yielded probes %+v, want none", ps)
	}
	// The same text with no alias declared IS a nested path, which is what makes the alias
	// list -- not the syntax -- the thing that decides.
	q2, err := vm.Parse(`j.Owner == "alice"`)
	if err != nil {
		t.Fatal(err)
	}
	ps := q2.Probes()
	if len(ps) != 1 || !ps[0].Nested() {
		t.Fatalf("unaliased select yielded %+v, want one nested probe", ps)
	}
}

// TestNestedPathCollidesWithDottedAttribute: an ad may carry an attribute literally NAMED
// "Res.Mem", whose id is the path index's id. The query means the path, and must get the
// path's records -- never the attribute's.
func TestNestedPathCollidesWithDottedAttribute(t *testing.T) {
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 40; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ ID = %d; Res = [ Mem = %d ] ]`, i, (i%2)*4096))
		if err != nil {
			t.Fatal(err)
		}
		// The decoy: the dotted name holds the OPPOSITE value, so reading it in place of the
		// path inverts the answer rather than merely blurring it.
		ad.InsertAttr("Res.Mem", int64((1-(i%2))*4096))
		src[i] = ad
	}
	c := New(Options{Shards: 2, ValueAttrs: []string{"Res.Mem"}})
	plain := New(Options{Shards: 2})
	fillNested(t, c, src, 20)
	fillNested(t, plain, src, 20)
	q, err := vm.Parse(`Res.Mem == 4096`)
	if err != nil {
		t.Fatal(err)
	}
	want := bruteIDs(src, q)
	if got := nestedIDs(t, plain, q); !equalInts(got, want) {
		t.Fatalf("unindexed scan returned %v, evaluator says %v", got, want)
	}
	if len(want) == 0 {
		t.Fatal("corpus matches nothing; the test proves nothing")
	}
	if got := nestedIDs(t, c, q); !equalInts(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestNestedPathFlatProbeDeclines: a probe on a top-level attribute whose NAME is the dotted
// path must not be answered from the path index's postings. The query language cannot spell
// such a reference, so it is checked at the resolution point that would.
func TestNestedPathFlatProbeDeclines(t *testing.T) {
	c := New(Options{Shards: 1, ValueAttrs: []string{"Res.Mem"}, CategoricalAttrs: []string{"Owner"}})
	spec := c.spec.Load()
	if _, ok := c.probeAttrID(spec, vm.Probe{Attr: "Res.Mem", Op: "=="}); ok {
		t.Fatal("a flat probe resolved to the nested path's index")
	}
	if _, ok := c.probeAttrID(spec, vm.Probe{Attr: "Res.Mem", Op: "==", Path: []string{"Res", "Mem"}}); !ok {
		t.Fatal("the nested probe did not resolve to its own index")
	}
	// A path spelled differently is a different index, even though its id is the same.
	if _, ok := c.probeAttrID(spec, vm.Probe{Attr: "Res.Mem", Op: "==", Path: []string{"Res", "Mem", "X"}}); ok {
		t.Fatal("a probe on a longer path resolved to a shorter path's index")
	}
	if _, ok := c.probeAttrID(spec, vm.Probe{Attr: "Owner", Op: "=="}); !ok {
		t.Fatal("a top-level probe stopped resolving")
	}
}

// TestNestedPathPersistent runs the differential over a persistent collection, whose records
// store attribute names inline rather than interned -- a separate descent through the record.
func TestNestedPathPersistent(t *testing.T) {
	src := parseNestedCorpus(t)
	indexed, err := Open(Options{Shards: 4, Dir: t.TempDir(), CategoricalAttrs: nestedCatAttrs, ValueAttrs: nestedValAttrs})
	if err != nil {
		t.Fatal(err)
	}
	defer indexed.Close()
	plain, err := Open(Options{Shards: 4, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	fillNested(t, indexed, src, 20)
	fillNested(t, plain, src, 20)
	engaged := false
	for _, qs := range nestedQueries {
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatal(err)
		}
		want := nestedIDs(t, plain, q)
		if got := nestedIDs(t, indexed, q); !equalInts(got, want) {
			t.Fatalf("%q: indexed read returned %v, unindexed scan returned %v", qs, got, want)
		}
		if indexed.ExplainQuery(q).Plan == "indexed" {
			engaged = true
		}
	}
	if !engaged {
		t.Fatal("no query planned indexed: the inline path walk was never exercised")
	}
}

// TestNestedPathColumnarized runs the differential again after the sealed segments are
// columnarized, which re-keys their records by a per-segment dictionary -- the third way a
// path has to be walked, and the one where a missing dictionary entry must read as an
// exception rather than as an absence.
func TestNestedPathColumnarized(t *testing.T) {
	src := parseNestedCorpus(t)
	indexed := New(Options{Shards: 4, CategoricalAttrs: nestedCatAttrs, ValueAttrs: nestedValAttrs})
	plain := New(Options{Shards: 4})
	fillNested(t, indexed, src, 60)
	fillNested(t, plain, src, 60)
	indexed.Compact()
	indexed.ColumnarizeSealed()
	indexed.Reindex()
	for _, qs := range nestedQueries {
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatal(err)
		}
		want := nestedIDs(t, plain, q)
		if got := nestedIDs(t, indexed, q); !equalInts(got, want) {
			t.Fatalf("%q: returned %v, want %v", qs, got, want)
		}
	}
}

// TestNestedPathAddDropIndex: a path index added at runtime answers the same query the same
// way, and dropping it returns the collection to a scan without changing the answer.
func TestNestedPathAddDropIndex(t *testing.T) {
	src := parseNestedCorpus(t)
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Owner"}})
	fillNested(t, c, src, 40)
	q, err := vm.Parse(`Res.Mem >= 4096`)
	if err != nil {
		t.Fatal(err)
	}
	want := nestedIDs(t, c, q) // before any index exists: the scan's own answer
	if len(want) == 0 {
		t.Fatal("corpus matches nothing; the test proves nothing")
	}
	if !c.AddIndex(nil, []string{"Res.Mem"}) {
		t.Fatal("AddIndex reported no change")
	}
	c.Reindex()
	if ex := c.ExplainQuery(q); ex.Plan != "indexed" {
		t.Fatalf("after AddIndex: plan %q (%s)", ex.Plan, ex.Reason)
	}
	if got := nestedIDs(t, c, q); !equalInts(got, want) {
		t.Fatalf("after AddIndex: %v, want %v", got, want)
	}
	if cat, val := c.IndexedAttrs(); !slices.Contains(val, "Res.Mem") {
		t.Fatalf("IndexedAttrs %v / %v does not name the path", cat, val)
	}
	if !c.DropIndex("Res.Mem") {
		t.Fatal("DropIndex reported no change")
	}
	c.Reindex()
	if ex := c.ExplainQuery(q); ex.Plan == "indexed" {
		t.Fatalf("after DropIndex: still planning indexed (%s)", ex.Reason)
	}
	if got := nestedIDs(t, c, q); !equalInts(got, want) {
		t.Fatalf("after DropIndex: %v, want %v", got, want)
	}
}

// TestNestedPathNodeClassification pins what the path walk decides for each shape, directly
// rather than through a query. The distinction it pins -- a member MISSING from a record that
// is there is an exception, not an absence -- has no observable effect on this store today,
// because the collection scan does not chain a select out of a nested record either (see
// chainingIDs), so only this test holds the index to the reference semantics. If the scan is
// ever made to chain, reading that case as an absence would start dropping matching records,
// and the index would already be right.
func TestNestedPathNodeClassification(t *testing.T) {
	cases := []struct {
		ad   string
		path string
		want idxNodeState
		why  string
	}{
		{`[ Res = [ Mem = 4096 ] ]`, "Res.Mem", idxValue, "member present as a literal"},
		{`[ Res = [ Mem = "x" ] ]`, "Res.Mem", idxValue, "a literal of another type is still a value"},
		{`[ Other = 1 ]`, "Res.Mem", idxAbsent, "root absent: the select is undefined"},
		{`[ Res = 5 ]`, "Res.Mem", idxUnknown, "root is not a record: the select is an error, not undefined"},
		{`[ Res = "x" ]`, "Res.Mem", idxUnknown, "ditto for a string root"},
		{`[ Res = { [ Mem = 1 ] } ]`, "Res.Mem", idxUnknown, "a list root projects to a list"},
		{`[ Res = Other ]`, "Res.Mem", idxUnknown, "an expression root may evaluate to anything"},
		{`[ Res = [ Other = 1 ] ]`, "Res.Mem", idxUnknown, "member missing: the select chains out of the record"},
		{`[ Res = [ ] ]`, "Res.Mem", idxUnknown, "ditto for an empty record"},
		{`[ Res = [ Mem = Base ] ]`, "Res.Mem", idxValue, "an expression member is a value the caller classifies"},
		{`[ Res = [ Deep = [ X = 7 ] ] ]`, "Res.Deep.X", idxValue, "three levels"},
		{`[ Res = [ Deep = 7 ] ]`, "Res.Deep.X", idxUnknown, "an intermediate that is not a record"},
		{`[ Res = [ Mem = 1 ] ]`, "Res.Deep.X", idxUnknown, "intermediate missing: chains"},
		{`[ Mem = 4096 ]`, "Res.Mem", idxAbsent, "the enclosing attribute alone is not the path"},
	}
	for _, tc := range cases {
		ad, err := classad.Parse(tc.ad)
		if err != nil {
			t.Fatalf("%s: %v", tc.ad, err)
		}
		// Both record encodings: interned ids (an in-memory collection) and inline names (a
		// persistent one) are separate descents through the record.
		table := wire.NewInternTable()
		interned := newIndexSpec(table, nil, []string{tc.path})
		inline := newInlineIndexSpec(nil, []string{tc.path})
		// A compacted persistent segment is a third encoding again: its records carry
		// SEGMENT-LOCAL ids resolved through the segment's own dictionary, paired with the
		// inline spec the collection configured (compact.go interns only when c.inline).
		segTable := wire.NewInternTable()
		segRec := wire.Encode(nil, ad.AST(), segTable)
		segDict := &segDictHandle{data: appendSegDict(nil, segTable.Names())}
		for _, enc := range []struct {
			name string
			spec *indexSpec
			buf  []byte
			dict *segDictHandle
		}{
			{"interned", interned, wire.Encode(nil, ad.AST(), table), nil},
			{"inline", inline, wire.EncodeInline(nil, ad.AST()), nil},
			{"segment-dict", inline, segRec, segDict},
		} {
			id := enc.spec.valIDs[0]
			_, got := enc.spec.indexNode(wire.Ad(enc.buf), id, enc.dict)
			if got != tc.want {
				t.Errorf("%s %s [%s]: state %d, want %d (%s)", enc.name, tc.ad, tc.path, got, tc.want, tc.why)
			}
		}
	}
}

// TestNestedPathUnprobedShapes: the select shapes an index over THIS ad cannot satisfy must
// yield no probe at all, rather than a path probe naming something the query did not say.
func TestNestedPathUnprobedShapes(t *testing.T) {
	for _, expr := range []string{
		`TARGET.Res.Mem == 1`, // the other ad of a match: this collection's index says nothing
		`Res[0].Mem == 1`,     // a subscripted root is not a path
		`Res.Mem[0] == 1`,
		`(Res.Mem + 1) == 2`, // a computed value, not an attribute
	} {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatalf("%q: %v", expr, err)
		}
		for _, p := range q.Probes() {
			if p.Nested() {
				t.Errorf("%q yielded nested probe %+v", expr, p)
			}
		}
	}
}

// TestNestedPathZoneMapDeclines: a zone map holds one [min,max] per top-level attribute. When
// an ad carries an attribute literally NAMED like a configured path, that attribute's zone is
// filed under the path's id, and a nested probe read against it would rule out segments that
// are full of matches -- a prune happens before the scan, so nothing re-verifies it.
func TestNestedPathZoneMapDeclines(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	a, err := OpenArchive(ArchiveOptions{
		Dir:         t.TempDir(),
		SegmentSize: 1 << 12,
		// The zone is on the dotted ATTRIBUTE; the index is on the PATH. Both resolve to one id.
		ZoneAttrs:  []string{"Res.Mem"},
		ValueAttrs: []string{"Res.Mem"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	want := 0
	for i := 0; i < 400; i++ {
		mem := (i % 4) * 4096
		ad, perr := classad.Parse(fmt.Sprintf(`[ N = %d; Res = [ Mem = %d ]; Pad = %q ]`, i, mem, strings.Repeat("p", 48)))
		if perr != nil {
			t.Fatal(perr)
		}
		// Every ad's dotted attribute is 0, so a zone map built from it spans [0,0] and
		// "rules out" every segment for `Res.Mem >= 4096`.
		ad.InsertAttr("Res.Mem", 0)
		if aerr := a.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
		if mem >= 4096 {
			want++
		}
	}
	q, err := vm.Parse(`Res.Mem >= 4096`)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for range a.Query(q) {
		got++
	}
	if got != want {
		t.Fatalf("got %d rows, want %d: a zone map on the dotted attribute pruned the path's segments", got, want)
	}
}

// TestNestedPathColumnarDeclines is TestNestedPathZoneMapDeclines for the COLUMNAR prefilters.
// A columnarized segment keeps a per-block zone for each schema field, and a field is one
// top-level attribute: when an ad carries attributes literally named like the configured
// paths, their fields sit under the paths' ids. Prefiltering a nested probe against them
// skips blocks without reading a row, so nothing re-verifies the skip.
func TestNestedPathColumnarDeclines(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	c, err := Open(Options{
		Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16,
		CategoricalAttrs: []string{"Res.Tag"}, ValueAttrs: []string{"Res.Mem"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	wantMem, wantTag := 0, 0
	for i := 0; i < 3000; i++ {
		mem, tag := (i%4)*4096, fmt.Sprintf("t%d", i%4)
		ad, perr := classad.Parse(fmt.Sprintf(`[ N = %d; Res = [ Mem = %d; Tag = %q ]; Pad = %q ]`,
			i, mem, tag, strings.Repeat("p", 48)))
		if perr != nil {
			t.Fatal(perr)
		}
		// The decoys are constant, so a block zone built from them spans a single value and
		// rules every block out for the queries below.
		ad.InsertAttr("Res.Mem", 0)
		ad.InsertAttrString("Res.Tag", "nothing")
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
		if mem >= 4096 {
			wantMem++
		}
		if tag == "t1" {
			wantTag++
		}
	}
	c.Reindex()
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Skip("no columnar schema was derived")
	}
	for c.ColumnarizeSealed() > 0 {
	}
	c.Reindex()
	for _, tc := range []struct {
		expr string
		want int
	}{
		{`Res.Mem >= 4096`, wantMem},
		{`Res.Tag == "t1"`, wantTag},
	} {
		q, qerr := vm.Parse(tc.expr)
		if qerr != nil {
			t.Fatal(qerr)
		}
		got := 0
		for range c.Query(q) {
			got++
		}
		if got != tc.want {
			t.Errorf("%q: got %d rows, want %d: a columnar zone on the dotted attribute pruned the path's blocks", tc.expr, got, tc.want)
		}
	}
}

// TestNestedPathSplitAndShape pins the two rules that decide what an index name MEANS, below
// the level a query can reach: which names are paths, and that one id never means both a path
// and an attribute.
func TestNestedPathSplitAndShape(t *testing.T) {
	for name, want := range map[string][]string{
		"Res.Mem":      {"Res", "Mem"},
		"Res.Deep.X":   {"Res", "Deep", "X"},
		"Memory":       nil,
		"a..b":         nil, // an empty component names no member
		".Mem":         nil,
		"Res.":         nil,
		"":             nil,
		"Res.Mem.Deep": {"Res", "Mem", "Deep"},
	} {
		got := splitAttrPath(name)
		if !samePath(got, want) || (len(want) == 0 && got != nil) {
			t.Errorf("splitAttrPath(%q) = %v, want %v", name, got, want)
		}
	}
	// One id, two meanings: the second one in is refused, so a resolution can never read one
	// index's postings for the other. Not reachable through Options today -- a dotted name
	// always configures a path -- but it is the invariant probeAttrID's shape check relies on.
	s := &indexSpec{cat: map[uint32]struct{}{}, val: map[uint32]struct{}{}}
	p := attrPath{comps: []string{"Res", "Mem"}}
	if !s.setShape(7, &p) {
		t.Fatal("the first path on an id was refused")
	}
	if !s.setShape(7, &attrPath{comps: []string{"res", "mem"}}) {
		t.Fatal("the same path spelled in another case was refused")
	}
	if s.setShape(7, &attrPath{comps: []string{"Res", "Other"}}) {
		t.Fatal("a different path on the same id was accepted")
	}
	if s.setShape(7, nil) {
		t.Fatal("a top-level attribute on a path's id was accepted")
	}
	s.val[9], s.valIDs = struct{}{}, []uint32{9}
	if s.setShape(9, &p) {
		t.Fatal("a path on an id already indexed as an attribute was accepted")
	}
}

// TestNestedPathEncryptedRootRefused: an encrypted attribute is stored opaque, so a path
// INSIDE it could never match -- every record would be an exception. The existing refusal
// ("cannot be both encrypted and indexed") has to look at the path's root to see that.
func TestNestedPathEncryptedRootRefused(t *testing.T) {
	_, dataKey := deriveDataKey(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("New accepted a path index inside an encrypted attribute")
			}
		}()
		New(Options{Shards: 1, DataKey: dataKey, EncryptedAttrs: []string{"Res"}, ValueAttrs: []string{"Res.Mem"}})
	}()
	c := New(Options{Shards: 1, DataKey: dataKey, ValueAttrs: []string{"Res.Mem"}})
	if err := c.SetEncryptedAttrs([]string{"Res"}); err == nil {
		t.Error("SetEncryptedAttrs sealed the root of an indexed path")
	}
	if !c.AddIndex(nil, []string{"Other.X"}) {
		t.Error("AddIndex refused a path whose root is not encrypted")
	}
}

// TestNestedPathSignatureStability: the index generation is what tells a sidecar on disk from
// one built under a different configuration, and it is persisted. A configuration with no
// nested paths must sign exactly as it did before paths existed, or the first Reindex after
// an upgrade rebuilds every sidecar in the store for nothing.
func TestNestedPathSignatureStability(t *testing.T) {
	table := wire.NewInternTable()
	flat := newIndexSpec(table, []string{"Owner"}, []string{"Memory"})
	// The pre-path signature of that same id set, computed the way signature() did before
	// paths were added: categorical ids, a separator, value ids.
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	mix := func(v uint64) {
		for i := 0; i < 8; i++ {
			h ^= (v >> (8 * i)) & 0xff
			h *= prime64
		}
	}
	mix(uint64(table.Intern("Owner")))
	mix(^uint64(0))
	mix(uint64(table.Intern("Memory")))
	if flat.gen != h {
		t.Fatalf("a path-free spec signs %#x, want the pre-path %#x", flat.gen, h)
	}
	withPath := newIndexSpec(table, []string{"Owner"}, []string{"Memory", "Res.Mem"})
	if withPath.gen == flat.gen {
		t.Fatal("adding a path index did not change the generation")
	}
}
