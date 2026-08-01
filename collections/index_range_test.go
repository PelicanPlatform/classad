package collections

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/classad"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/RoaringBitmap/roaring/v2"
)

// TestCoalesceRanges pins the planner's range-merging rules: opposite bounds on one
// attribute become one two-sided probe, same-side bounds tighten, and everything else
// passes through in input order.
func TestCoalesceRanges(t *testing.T) {
	t.Parallel()
	val := func(op string, v float64) usableProbe {
		return usableProbe{attrID: 1, op: op, fvals: []float64{v}}
	}
	cat := func(op string, s string) usableProbe {
		return usableProbe{attrID: 2, cat: true, op: op, svals: []string{s}}
	}

	tests := []struct {
		name string
		in   []usableProbe
		want []usableProbe
	}{{
		name: "two sided band",
		in:   []usableProbe{val(">", 1024), val("<", 4096)},
		want: []usableProbe{{attrID: 1, op: ">", fvals: []float64{1024}, hiOp: "<", hiVal: 4096}},
	}, {
		// The upper bound arriving first must still normalize to lower-bound-first.
		name: "upper bound first",
		in:   []usableProbe{val("<=", 4096), val(">=", 1024)},
		want: []usableProbe{{attrID: 1, op: ">=", fvals: []float64{1024}, hiOp: "<=", hiVal: 4096}},
	}, {
		name: "tighten same side",
		in:   []usableProbe{val(">", 1024), val(">", 2048), val("<", 4096)},
		want: []usableProbe{{attrID: 1, op: ">", fvals: []float64{2048}, hiOp: "<", hiVal: 4096}},
	}, {
		// At an equal bound the exclusive comparison is the tighter one.
		name: "equal bound prefers exclusive",
		in:   []usableProbe{val(">=", 1024), val(">", 1024), val("<=", 4096), val("<", 4096)},
		want: []usableProbe{{attrID: 1, op: ">", fvals: []float64{1024}, hiOp: "<", hiVal: 4096}},
	}, {
		// Same-side bounds subsume: the tighter one is the whole constraint, and the
		// result stays one-sided (no upper bound was ever given).
		name: "same side tightens to the stricter bound",
		in:   []usableProbe{val(">", 1024), val(">", 512)},
		want: []usableProbe{val(">", 1024)},
	}, {
		name: "same side tightens regardless of order",
		in:   []usableProbe{val(">", 512), val(">", 1024)},
		want: []usableProbe{val(">", 1024)},
	}, {
		name: "same side upper bounds tighten",
		in:   []usableProbe{val("<", 4096), val("<", 8192)},
		want: []usableProbe{val("<", 4096)},
	}, {
		name: "same side equal value prefers exclusive",
		in:   []usableProbe{val(">=", 1024), val(">", 1024)},
		want: []usableProbe{val(">", 1024)},
	}, {
		name: "three same-side bounds collapse to one",
		in:   []usableProbe{val(">", 512), val(">", 2048), val(">", 1024)},
		want: []usableProbe{val(">", 2048)},
	}, {
		name: "one sided untouched",
		in:   []usableProbe{val(">", 1024)},
		want: []usableProbe{val(">", 1024)},
	}, {
		name: "empty band survives as a band",
		in:   []usableProbe{val(">", 4096), val("<", 1024)},
		want: []usableProbe{{attrID: 1, op: ">", fvals: []float64{4096}, hiOp: "<", hiVal: 1024}},
	}, {
		name: "non-range probes pass through in order",
		in:   []usableProbe{cat("==", "alice"), val(">", 1024), val("==", 7), val("<", 4096)},
		want: []usableProbe{
			cat("==", "alice"),
			{attrID: 1, op: ">", fvals: []float64{1024}, hiOp: "<", hiVal: 4096},
			val("==", 7),
		},
	}, {
		name: "different attributes do not merge",
		in:   []usableProbe{val(">", 1024), {attrID: 9, op: "<", fvals: []float64{4096}}},
		want: []usableProbe{val(">", 1024), {attrID: 9, op: "<", fvals: []float64{4096}}},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := coalesceRanges(append([]usableProbe(nil), tc.in...))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d probes %+v, want %d %+v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if !probeEqual(got[i], tc.want[i]) {
					t.Errorf("probe %d: got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func probeEqual(a, b usableProbe) bool {
	if a.attrID != b.attrID || a.cat != b.cat || a.op != b.op || a.hiOp != b.hiOp || a.hiVal != b.hiVal {
		return false
	}
	if len(a.fvals) != len(b.fvals) || len(a.svals) != len(b.svals) {
		return false
	}
	for i := range a.fvals {
		if a.fvals[i] != b.fvals[i] {
			return false
		}
	}
	for i := range a.svals {
		if a.svals[i] != b.svals[i] {
			return false
		}
	}
	return true
}

// TestRangeBandQueryResults is the correctness anchor for coalescing: a two-sided range
// query must return exactly the ads a brute-force scan returns, including the type
// exceptions the index cannot post (a string-valued Memory) and empty bands.
func TestRangeBandQueryResults(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"Memory", "Cpus"}})
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 4000; i++ {
		text := fmt.Sprintf(`[ ID=%d; Owner="u%d"; Memory=%d; Cpus=%d ]`, i, i%20, (i%64+1)*128, i%16)
		if i%53 == 0 { // exception: Memory is not a number, so it cannot be posted
			text = fmt.Sprintf(`[ ID=%d; Owner="u%d"; Memory="lots"; Cpus=%d ]`, i, i%20, i%16)
		}
		ad := mustAd(t, text)
		src[i] = ad
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	queries := []string{
		`Memory > 1024 && Memory < 4096`,
		`Memory >= 1024 && Memory <= 4096`,
		`Memory > 1024 && Memory <= 1024`, // empty band
		`Memory > 8192 && Memory < 1024`,  // inverted: empty band
		`Memory >= 128 && Memory <= 8192`, // whole span
		`Memory > 1024 && Memory > 2048 && Memory < 4096`,
		`Memory > 1024 && Memory < 4096 && Cpus >= 4 && Cpus < 12`,
		`Memory > 1024 && Memory < 4096 && Owner == "u3"`,
		`Memory > 99999 && Memory < 999999`, // above every value
		`(Memory > 1024 && Memory < 2048) || (Cpus > 2 && Cpus < 6)`,
	}
	for _, qs := range queries {
		indexedVsBrute(t, c, src, qs)
	}
}

// TestRangeBandPrunesKeyRun checks the pruning win: a band that falls in a gap between
// the indexed values admits NO candidates, even though each half-open side on its own
// admits half the segment. Per-segment [min,max] stats cannot see into the gap, so this
// is the key-run scan doing the work -- which only a coalesced probe can bound at both ends.
func TestRangeBandPrunesKeyRun(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1, ValueAttrs: []string{"Memory"}})
	// Memory is 0 or 10000 -- nothing in between -- so the band (100, 9000) is empty
	// while `> 100` and `< 9000` each match half the segment.
	for i := 0; i < 2000; i++ {
		mem := 0
		if i%2 == 0 {
			mem = 10000
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[ ID=%d; Memory=%d ]`, i, mem))); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	si := firstSegIndex(t, c)
	plan := func(qs string) []usableProbe { return planOf(t, c, qs) }
	for _, qs := range []string{`Memory > 100`, `Memory < 9000`} {
		if si.candidateOffsets(plan(qs)).IsEmpty() {
			t.Errorf("%s: expected candidates (half the segment matches)", qs)
		}
	}
	band := plan(`Memory > 100 && Memory < 9000`)
	if len(band) != 1 || !band[0].twoSided() {
		t.Fatalf("expected one two-sided probe, got %+v", band)
	}
	if cand := si.candidateOffsets(band); !cand.IsEmpty() {
		t.Errorf("the band (100, 9000) falls in the value gap: want no candidates, got %d", cand.GetCardinality())
	}
	// Sanity: the pruning is not the index lying -- the query really returns nothing.
	bandQ, err := vm.Parse(`Memory > 100 && Memory < 9000`)
	if err != nil {
		t.Fatal(err)
	}
	if ids := queryIDs(t, c, bandQ); len(ids) != 0 {
		t.Errorf("band query returned %d ads, want 0", len(ids))
	}

	// A band entirely below the segment's span must still skip the whole prefix: the
	// coalesced probe has to test BOTH bounds against the segment stats, or it would
	// lose a skip the separate `< 100` probe would have found on its own.
	below := plan(`Memory > -10 && Memory < -1`)
	if len(below) != 1 || !below[0].twoSided() {
		t.Fatalf("expected one two-sided probe, got %+v", below)
	}
	if !si.skipsPrefix(below) {
		t.Error("a band below every value should skip the indexed prefix")
	}
}

// TestRangeBandExplain checks that .explain attributes the band's selectivity to each
// half rather than reporting two barely-selective one-sided probes.
func TestRangeBandExplain(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1, ValueAttrs: []string{"Memory"}})
	for i := 0; i < 2000; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[ ID=%d; Memory=%d ]`, i, i))); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	q, err := vm.Parse(`Memory > 900 && Memory < 1100`)
	if err != nil {
		t.Fatal(err)
	}
	ex := c.ExplainQuery(q)
	if ex.IndexUsable != 1 {
		t.Errorf("IndexUsable = %d, want 1 (the two bounds coalesce into one probe)", ex.IndexUsable)
	}
	if len(ex.Probes) != 2 {
		t.Fatalf("want both conjuncts explained, got %+v", ex.Probes)
	}
	for _, pe := range ex.Probes {
		if !pe.Coalesced {
			t.Errorf("%s %s: want Coalesced", pe.Attr, pe.Op)
		}
		// ~200 of 2000 values fall in the band; each half alone would be ~half the set.
		if !pe.HasSelectivity || pe.Selectivity > 0.25 {
			t.Errorf("%s %s: selectivity %.3f should reflect the narrow band", pe.Attr, pe.Op, pe.Selectivity)
		}
	}
}

// TestRangeBandTierParity checks that a band probe gives identical candidates on the
// in-RAM segIndex and on the mmap sidecar a sealed segment is read through.
func TestRangeBandTierParity(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1, CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"Memory"}})
	for i := 0; i < 6000; i++ {
		text := fmt.Sprintf(`[ ID=%d; Owner="u%d"; Memory=%d ]`, i, i%37, i%1500)
		if i%67 == 0 {
			text = fmt.Sprintf(`[ ID=%d; Owner="u%d"; Memory="x" ]`, i, i%37)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, text)); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	queries := []string{
		`Memory > 200 && Memory < 800`,
		`Memory >= 0 && Memory <= 1499`,
		`Memory > 1400 && Memory < 200`, // empty band
		`Memory > 200 && Memory < 800 && Owner == "u5"`,
		`Memory >= 700 && Memory <= 700`,
	}
	segs := 0
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			live := seg.idx.Load()
			if live == nil {
				continue
			}
			path := filepath.Join(t.TempDir(), fmt.Sprintf("seg-%d.idx", seg.id))
			if err := writeSidecarIndex(path, live); err != nil {
				t.Fatalf("write sidecar: %v", err)
			}
			data, closer, err := mapFile(path)
			if err != nil {
				t.Fatalf("map sidecar: %v", err)
			}
			mm, err := parseMmapSidecar(data)
			if err != nil {
				t.Fatalf("parse sidecar: %v", err)
			}
			for _, qs := range queries {
				q, err := vm.Parse(qs)
				if err != nil {
					t.Fatalf("parse %q: %v", qs, err)
				}
				u := c.planIndex(q.Probes())
				if live.skipsPrefix(u) != mm.skipsPrefix(u) {
					t.Errorf("seg %d %q: skipsPrefix mismatch", seg.id, qs)
				}
				if !bmEqual(live.candidateOffsets(u), mm.candidateOffsets(u)) {
					t.Errorf("seg %d %q: candidateOffsets mismatch", seg.id, qs)
				}
			}
			_ = closer()
			segs++
		}
	}
	if segs == 0 {
		t.Fatal("no segments compared")
	}
}

// TestUnionPostings checks that the batched union matches a naive running Or across the
// threshold, for empty, nil-holed, and overlapping inputs.
func TestUnionPostings(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 2, 3, 4, 5, 17, 300} {
		for _, holes := range []bool{false, true} {
			bms := make([]*roaring.Bitmap, n)
			for i := range bms {
				if holes && i%3 == 0 {
					continue // a key with no posting
				}
				bm := roaring.New()
				for j := 0; j < 40; j++ {
					bm.Add(uint32(i*7 + j)) // overlapping runs
				}
				bms[i] = bm
			}
			want := roaring.New()
			for _, bm := range bms {
				if bm != nil {
					want.Or(bm)
				}
			}
			got := unionPostings(n, func(i int) *roaring.Bitmap { return bms[i] })
			if !bmEqual(got, want) {
				t.Errorf("n=%d holes=%v: union mismatch (%d vs %d)", n, holes, got.GetCardinality(), want.GetCardinality())
			}
			// The result must be independently mutable: writing to it must not disturb
			// the postings it was built from (they are live index state, or mmap-backed).
			got.Add(1 << 20)
			for i, bm := range bms {
				if bm != nil && bm.Contains(1<<20) {
					t.Fatalf("n=%d: mutating the union leaked into posting %d", n, i)
				}
			}
		}
	}
}

// --- helpers ---

func firstSegIndex(t *testing.T, c *Collection) *segIndex {
	t.Helper()
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if si := seg.idx.Load(); si != nil {
				return si
			}
		}
	}
	t.Fatal("no built segment index")
	return nil
}

func planOf(t *testing.T, c *Collection, constraint string) []usableProbe {
	t.Helper()
	q, err := vm.Parse(constraint)
	if err != nil {
		t.Fatalf("parse %q: %v", constraint, err)
	}
	return c.planIndex(q.Probes())
}

// indexedVsBrute compares the indexed query answer against evaluating the constraint over
// every source ad.
func indexedVsBrute(t *testing.T, c *Collection, src map[int]*classad.ClassAd, constraint string) {
	t.Helper()
	q, err := vm.Parse(constraint)
	if err != nil {
		t.Fatalf("parse %q: %v", constraint, err)
	}
	got, want := queryIDs(t, c, q), bruteIDs(src, q)
	if len(got) != len(want) {
		t.Errorf("%s: indexed returned %d ads, brute force %d", constraint, len(got), len(want))
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s: id mismatch at %d (%d vs %d)", constraint, i, got[i], want[i])
			return
		}
	}
}

// TestEmptyBandSemantics is the safety proof for eliminating an impossible band ahead of
// the exception set. Every other planner skip must re-verify exceptional records because
// the INDEX cannot classify them; this one claims no ClassAd value of ANY shape can satisfy
// both bounds. If that ever stops holding, the elimination becomes a wrong-answer bug, so
// it is pinned here across the whole value space rather than argued from three-valued logic.
func TestEmptyBandSemantics(t *testing.T) {
	t.Parallel()
	values := []string{
		`512`, `1023`, `2049`, `4096`, `0`, `-1`, // numbers either side of both bounds
		`1024.5`, `2048.0`, // reals
		`"lots"`, `"1500"`, `""`, // strings, including one that looks numeric
		`true`, `false`, // booleans
		`{1000, 3000}`, // list spanning both bounds
		`[ a = 1 ]`,    // nested ad
		`Other + 1`,    // expression over a missing attr -> undefined
		`1/0`,          // error
		`undefined`, `error`,
	}
	for _, qs := range []string{
		`Memory < 1024 && Memory > 2048`,
		`Memory > 2048 && Memory < 1024`,
		`Memory >= 1024 && Memory <= 512`,
		`Memory > 700 && Memory <= 700`, // single point, low bound exclusive
		`Memory >= 700 && Memory < 700`, // single point, high bound exclusive
	} {
		q := mustQuery(t, qs)
		for _, v := range values {
			if q.Matches(mustAd(t, fmt.Sprintf(`[ Memory = %s ]`, v))) {
				t.Errorf("%s matched an ad with Memory = %s: the band is not empty after all", qs, v)
			}
		}
		if q.Matches(mustAd(t, `[ Other = 5 ]`)) {
			t.Errorf("%s matched an ad with no Memory at all", qs)
		}
	}
	// Controls, so the test can only pass for the right reason.
	if !mustQuery(t, `Memory > 1024 && Memory < 2048`).Matches(mustAd(t, `[ Memory = 1500 ]`)) {
		t.Error("control: 1500 should match the band (1024, 2048)")
	}
	if !mustQuery(t, `Memory >= 700 && Memory <= 700`).Matches(mustAd(t, `[ Memory = 700 ]`)) {
		t.Error("control: a single-point band with both bounds inclusive should match")
	}
}

// TestEmptyBandDetection pins which coalesced bands are recognized as impossible.
func TestEmptyBandDetection(t *testing.T) {
	t.Parallel()
	band := func(loOp string, lo float64, hiOp string, hi float64) usableProbe {
		return usableProbe{attrID: 1, op: loOp, fvals: []float64{lo}, hiOp: hiOp, hiVal: hi}
	}
	tests := []struct {
		name string
		up   usableProbe
		want bool
	}{
		{"inverted", band(">", 2048, "<", 1024), true},
		{"inverted inclusive", band(">=", 1024, "<=", 512), true},
		{"point, low exclusive", band(">", 700, "<=", 700), true},
		{"point, high exclusive", band(">=", 700, "<", 700), true},
		{"point, both exclusive", band(">", 700, "<", 700), true},
		{"point, both inclusive", band(">=", 700, "<=", 700), false},
		{"ordinary band", band(">", 1024, "<", 4096), false},
		{"one-sided", usableProbe{attrID: 1, op: ">", fvals: []float64{1024}}, false},
		{"NaN low", band(">", math.NaN(), "<", 1024), false},
		{"NaN high", band(">", 1024, "<", math.NaN()), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.up.emptyBand(); got != tc.want {
				t.Errorf("emptyBand() = %v, want %v", got, tc.want)
			}
			if got := unsatisfiable([]usableProbe{tc.up}); got != tc.want {
				t.Errorf("unsatisfiable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEmptyBandEliminated is the behavioural half: an impossible constraint must be planned
// as "empty", admit no candidates at all (not even the exception set), and touch no record.
func TestEmptyBandEliminated(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2, CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"Memory"}})
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 3000; i++ {
		text := fmt.Sprintf(`[ ID=%d; Owner="u%d"; Memory=%d ]`, i, i%10, (i%16+1)*512)
		if i%97 == 0 { // exceptions: the index cannot classify these
			text = fmt.Sprintf(`[ ID=%d; Owner="u%d"; Memory="lots" ]`, i, i%10)
		}
		ad := mustAd(t, text)
		src[i] = ad
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	for _, qs := range []string{
		`Memory < 1024 && Memory > 2048`,
		`Memory > 2048 && Memory < 1024 && Owner == "u3"`,
		`Memory >= 1024 && Memory <= 512`,
	} {
		u := planOf(t, c, qs)
		if !unsatisfiable(u) {
			t.Errorf("%s: plan should be unsatisfiable, got %+v", qs, u)
		}
		si := firstSegIndex(t, c)
		if !si.skipsPrefix(u) {
			t.Errorf("%s: the indexed prefix should be skippable despite the exception set", qs)
		}
		if cand := si.candidateOffsets(u); !cand.IsEmpty() {
			t.Errorf("%s: %d candidates, want 0 (the exception set must not survive)", qs, cand.GetCardinality())
		}
		if ex := c.ExplainQuery(mustQuery(t, qs)); ex.Plan != "empty" {
			t.Errorf("%s: plan = %q, want \"empty\"", qs, ex.Plan)
		}
		indexedVsBrute(t, c, src, qs) // and it still returns exactly nothing
	}

	// A satisfiable band on the same attribute must be unaffected.
	ok := planOf(t, c, `Memory > 1024 && Memory < 4096`)
	if unsatisfiable(ok) {
		t.Error("a satisfiable band must not be eliminated")
	}
	if ex := c.ExplainQuery(mustQuery(t, `Memory > 1024 && Memory < 4096`)); ex.Plan != "indexed" {
		t.Errorf("satisfiable band plan = %q, want \"indexed\"", ex.Plan)
	}
}

// TestEmptyBandDisjunctionKept checks the DNF rule: an impossible disjunct contributes
// nothing to a union, but one satisfiable disjunct keeps the query alive.
func TestEmptyBandDisjunctionKept(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1, ValueAttrs: []string{"Memory", "Cpus"}})
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 2000; i++ {
		ad := mustAd(t, fmt.Sprintf(`[ ID=%d; Memory=%d; Cpus=%d ]`, i, (i%16+1)*512, i%8))
		src[i] = ad
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	// One dead disjunct, one live: results must equal the live one alone.
	live := `(Memory < 1024 && Memory > 2048) || (Cpus >= 2 && Cpus <= 4)`
	indexedVsBrute(t, c, src, live)
	if len(queryIDs(t, c, mustQuery(t, live))) == 0 {
		t.Error("the satisfiable disjunct should still return ads")
	}
	// Both disjuncts dead: nothing, and every group is recognized as impossible.
	dead := `(Memory < 1024 && Memory > 2048) || (Cpus > 4 && Cpus < 2)`
	indexedVsBrute(t, c, src, dead)
	if groups, prunable := c.planIndexGroups(mustQuery(t, dead).ProbePlan()); !prunable || !unsatisfiableGroups(groups) {
		t.Errorf("both disjuncts impossible: prunable=%v unsatisfiable=%v", prunable, unsatisfiableGroups(groups))
	}
}

// TestSameSideMergeQuery is the behavioural half of same-side coalescing: redundant bounds
// on one attribute collapse to the tightest, the query still answers exactly, and .explain
// attributes the merged probe's selectivity to the conjunct that was folded away rather
// than advertising a reach that is never probed.
func TestSameSideMergeQuery(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2, CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"Memory"}})
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 3000; i++ {
		text := fmt.Sprintf(`[ ID=%d; Owner="u%d"; Memory=%d ]`, i, i%10, (i%64+1)*128)
		if i%89 == 0 {
			text = fmt.Sprintf(`[ ID=%d; Owner="u%d"; Memory="lots" ]`, i, i%10)
		}
		ad := mustAd(t, text)
		src[i] = ad
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	// Each of these must plan as ONE range probe carrying the tightest bound(s), and the
	// looser conjunct must not add a probe of its own.
	for _, tc := range []struct {
		q      string
		op     string
		val    float64
		hiOp   string
		hiVal  float64
		probes int // total probes, including any non-range conjunct
	}{
		{q: `Memory > 1024 && Memory > 512`, op: ">", val: 1024, probes: 1},
		{q: `Memory > 512 && Memory > 1024`, op: ">", val: 1024, probes: 1},
		{q: `Memory < 4096 && Memory < 8192`, op: "<", val: 4096, probes: 1},
		{q: `Memory >= 1024 && Memory > 1024`, op: ">", val: 1024, probes: 1},
		// A non-range conjunct in between must not stop the merge.
		{q: `Memory > 1024 && Owner == "u3" && Memory > 512`, op: ">", val: 1024, probes: 2},
		// Both sides tightened, then coalesced into a band.
		{q: `Memory > 512 && Memory > 1024 && Memory < 8192 && Memory < 4096`,
			op: ">", val: 1024, hiOp: "<", hiVal: 4096, probes: 1},
	} {
		u := planOf(t, c, tc.q)
		if len(u) != tc.probes {
			t.Errorf("%s: %d probes, want %d (%+v)", tc.q, len(u), tc.probes, u)
			continue
		}
		var rng *usableProbe
		for i := range u {
			if !u[i].cat && isRangeOp(u[i].op) {
				rng = &u[i]
			}
		}
		if rng == nil {
			t.Errorf("%s: no range probe planned", tc.q)
			continue
		}
		if rng.op != tc.op || rng.fvals[0] != tc.val || rng.hiOp != tc.hiOp || rng.hiVal != tc.hiVal {
			t.Errorf("%s: planned %s %g / %s %g, want %s %g / %s %g", tc.q,
				rng.op, rng.fvals[0], rng.hiOp, rng.hiVal, tc.op, tc.val, tc.hiOp, tc.hiVal)
		}
		indexedVsBrute(t, c, src, tc.q)
	}

	// .explain: the conjunct that survives as the probe is unmarked and carries its own
	// number; the subsumed one is marked and reports the merged probe's number, not the
	// much weaker reach of `> 512`.
	ex := c.ExplainQuery(mustQuery(t, `Memory > 1024 && Memory > 512`))
	if ex.IndexUsable != 1 {
		t.Errorf("IndexUsable = %d, want 1", ex.IndexUsable)
	}
	if len(ex.Probes) != 2 {
		t.Fatalf("want both conjuncts explained, got %+v", ex.Probes)
	}
	if ex.Probes[0].Coalesced {
		t.Errorf("`> 1024` is the probe itself: want unmarked, got %+v", ex.Probes[0])
	}
	if !ex.Probes[1].Coalesced {
		t.Errorf("`> 512` was folded away: want marked, got %+v", ex.Probes[1])
	}
	if ex.Probes[0].EstCandidates != ex.Probes[1].EstCandidates {
		t.Errorf("both conjuncts should report the merged probe's estimate, got %d and %d",
			ex.Probes[0].EstCandidates, ex.Probes[1].EstCandidates)
	}
}

// TestEqualityFoldSemantics is the safety proof for eliminating an equality whose values a
// range (or another equality) on the same attribute excludes. Like TestEmptyBandSemantics it
// pins the claim empirically across the whole value space, because the elimination skips the
// exception set that every data-driven skip must re-verify.
func TestEqualityFoldSemantics(t *testing.T) {
	t.Parallel()
	values := []string{
		`2048`, `2048.0`, `1024`, `4096`, `8192`, `0`, `-1`,
		`"2048"`, `"lots"`, `""`,
		`true`, `false`,
		`{1024, 2048}`,
		`[ a = 1 ]`,
		`Other + 1`, `1/0`, `undefined`, `error`,
	}
	for _, qs := range []string{
		`Memory == 2048 && Memory > 4096`,
		`Memory == 2048 && Memory < 1024`,
		`Memory == 2048 && Memory >= 2049`,
		`Memory == 2048 && Memory > 1024 && Memory < 2000`,
		`(Memory == 1024 || Memory == 2048) && Memory > 4096`,
		`Memory == 1024 && Memory == 2048`,
		`!Memory && Memory > 1024`,
	} {
		q := mustQuery(t, qs)
		for _, v := range values {
			if q.Matches(mustAd(t, fmt.Sprintf(`[ Memory = %s ]`, v))) {
				t.Errorf("%s matched an ad with Memory = %s: the fold is not sound", qs, v)
			}
		}
		if q.Matches(mustAd(t, `[ Other = 5 ]`)) {
			t.Errorf("%s matched an ad with no Memory at all", qs)
		}
	}
	// Controls: folds that leave a value standing must still match it.
	for _, tc := range []struct{ q, ad string }{
		{`Memory == 2048 && Memory > 1024`, `[ Memory = 2048 ]`},
		{`(Memory == 1024 || Memory == 2048) && Memory > 1500`, `[ Memory = 2048 ]`},
		{`Memory == 2048 && Memory > 1024 && Memory < 4096`, `[ Memory = 2048 ]`},
	} {
		if !mustQuery(t, tc.q).Matches(mustAd(t, tc.ad)) {
			t.Errorf("control: %s should match %s", tc.q, tc.ad)
		}
	}
}

// TestEqualityFoldsRange checks the plan: an equality absorbs the range probe on its
// attribute, so the wide posting union the range would have built never happens, and the
// answer is unchanged.
func TestEqualityFoldsRange(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2, CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"Memory", "Cpus"}})
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 3000; i++ {
		text := fmt.Sprintf(`[ ID=%d; Owner="u%d"; Memory=%d; Cpus=%d ]`, i, i%10, (i%64+1)*128, i%16)
		if i%89 == 0 {
			text = fmt.Sprintf(`[ ID=%d; Owner="u%d"; Memory="lots"; Cpus=%d ]`, i, i%10, i%16)
		}
		ad := mustAd(t, text)
		src[i] = ad
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	for _, tc := range []struct {
		q      string
		probes int
		fvals  []float64 // the surviving equality's value set
	}{
		{q: `Memory == 2048 && Memory > 1024`, probes: 1, fvals: []float64{2048}},
		{q: `Memory > 1024 && Memory == 2048`, probes: 1, fvals: []float64{2048}},
		{q: `Memory == 2048 && Memory > 1024 && Memory < 4096`, probes: 1, fvals: []float64{2048}},
		// Membership narrows to the members the band admits.
		{q: `(Memory == 1024 || Memory == 2048 || Memory == 8192) && Memory > 1500 && Memory < 4096`,
			probes: 1, fvals: []float64{2048}},
		// Two equalities intersect.
		{q: `Memory == 2048 && (Memory == 1024 || Memory == 2048)`, probes: 1, fvals: []float64{2048}},
		// A range on a DIFFERENT attribute is untouched.
		{q: `Memory == 2048 && Cpus > 4`, probes: 2, fvals: []float64{2048}},
		// A categorical conjunct is untouched.
		{q: `Memory == 2048 && Memory > 1024 && Owner == "u3"`, probes: 2, fvals: []float64{2048}},
	} {
		u := planOf(t, c, tc.q)
		if len(u) != tc.probes {
			t.Errorf("%s: %d probes, want %d (%+v)", tc.q, len(u), tc.probes, u)
			continue
		}
		var eq *usableProbe
		for i := range u {
			if !u[i].cat && isEqOp(u[i].op) && u[i].attrID == u[0].attrID {
				eq = &u[i]
				break
			}
		}
		if eq == nil {
			t.Errorf("%s: no equality probe survived", tc.q)
			continue
		}
		if len(eq.fvals) != len(tc.fvals) {
			t.Errorf("%s: equality values %v, want %v", tc.q, eq.fvals, tc.fvals)
		} else {
			for i := range eq.fvals {
				if eq.fvals[i] != tc.fvals[i] {
					t.Errorf("%s: equality values %v, want %v", tc.q, eq.fvals, tc.fvals)
					break
				}
			}
		}
		indexedVsBrute(t, c, src, tc.q)
	}

	// The folded plan must admit no more than the equality alone ever did.
	si := firstSegIndex(t, c)
	eqOnly := si.candidateOffsets(planOf(t, c, `Memory == 2048`))
	folded := si.candidateOffsets(planOf(t, c, `Memory == 2048 && Memory > 1024`))
	if !bmEqual(eqOnly, folded) {
		t.Errorf("folded candidates (%d) should equal the equality's own (%d)",
			folded.GetCardinality(), eqOnly.GetCardinality())
	}
}

// TestEqualityFoldUnsatisfiable checks the elimination half: an equality a range excludes
// leaves nothing to scan at all, exception set included.
func TestEqualityFoldUnsatisfiable(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2, ValueAttrs: []string{"Memory", "Cpus"}})
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 3000; i++ {
		text := fmt.Sprintf(`[ ID=%d; Memory=%d; Cpus=%d ]`, i, (i%64+1)*128, i%16)
		if i%89 == 0 {
			text = fmt.Sprintf(`[ ID=%d; Memory="lots"; Cpus=%d ]`, i, i%16)
		}
		ad := mustAd(t, text)
		src[i] = ad
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	si := firstSegIndex(t, c)
	for _, qs := range []string{
		`Memory == 2048 && Memory > 4096`,
		`Memory == 2048 && Memory < 1024`,
		`Memory == 2048 && Memory > 1024 && Memory < 2000`,
		`(Memory == 1024 || Memory == 2048) && Memory > 4096`,
		`Memory == 1024 && Memory == 2048`,
	} {
		u := planOf(t, c, qs)
		if !unsatisfiable(u) {
			t.Errorf("%s: plan should be unsatisfiable, got %+v", qs, u)
		}
		if !si.skipsPrefix(u) {
			t.Errorf("%s: the indexed prefix should be skippable", qs)
		}
		if cand := si.candidateOffsets(u); !cand.IsEmpty() {
			t.Errorf("%s: %d candidates, want 0", qs, cand.GetCardinality())
		}
		if ex := c.ExplainQuery(mustQuery(t, qs)); ex.Plan != "empty" {
			t.Errorf("%s: plan = %q, want \"empty\"", qs, ex.Plan)
		}
		indexedVsBrute(t, c, src, qs)
	}

	// A satisfiable fold, and a contradiction split across attributes, must survive.
	if unsatisfiable(planOf(t, c, `Memory == 2048 && Memory > 1024`)) {
		t.Error("a satisfiable fold must not be eliminated")
	}
	if unsatisfiable(planOf(t, c, `Memory == 2048 && Cpus > 99`)) {
		t.Error("bounds on different attributes must not fold together")
	}
}

// TestEqualityFoldExplain checks that the absorbed range conjunct reports the equality's
// selectivity rather than the sweeping reach it would have had on its own.
func TestEqualityFoldExplain(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1, ValueAttrs: []string{"Memory"}})
	for i := 0; i < 2000; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[ ID=%d; Memory=%d ]`, i, (i%64+1)*128))); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	ex := c.ExplainQuery(mustQuery(t, `Memory == 2048 && Memory > 1024`))
	if ex.IndexUsable != 1 {
		t.Errorf("IndexUsable = %d, want 1 (the range is absorbed)", ex.IndexUsable)
	}
	if len(ex.Probes) != 2 {
		t.Fatalf("want both conjuncts explained, got %+v", ex.Probes)
	}
	if ex.Probes[0].Coalesced {
		t.Errorf("`== 2048` is the probe itself: want unmarked, got %+v", ex.Probes[0])
	}
	if !ex.Probes[1].Coalesced {
		t.Errorf("`> 1024` was absorbed: want marked, got %+v", ex.Probes[1])
	}
	if ex.Probes[0].EstCandidates != ex.Probes[1].EstCandidates {
		t.Errorf("the absorbed conjunct should report the equality's estimate, got %d and %d",
			ex.Probes[0].EstCandidates, ex.Probes[1].EstCandidates)
	}
	// ~1/64 of the values are 2048; the `> 1024` line must not advertise ~87%.
	if ex.Probes[1].Selectivity > 0.1 {
		t.Errorf("absorbed conjunct selectivity %.3f should be the equality's, not the range's",
			ex.Probes[1].Selectivity)
	}
}
