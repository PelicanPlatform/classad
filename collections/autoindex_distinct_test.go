package collections

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// What the reported distinct-value count owes a reader, measured against ground truth it
// is allowed to be short of.
//
// The count comes from a bounded sample, so on an attribute with more distinct values than
// the sample has ads it is necessarily short. That is not the bug; publishing a short count
// as though it were the count is. These tests fix the contract: the number may be a floor,
// and when it is, Capped says so.

// distinctCorpus fills c with n ads whose attributes span the cardinality classes that
// matter, each named for what it is so a failure reads as a statement about that class:
//
//   - Unique: a distinct value per ad, so its true count is n -- far past any sample.
//   - Mid: 600 values, which the first HALF of the sample already exhausts. It separates
//     the half-sample comparison from the cruder "many values relative to ads" test that
//     agrees with it everywhere else in this corpus: Mid's count is exact, and a rule
//     reading only the final totals calls it short.
//   - Machine: 250 values, reachable well within a sample of a few thousand.
//   - State: two values.
//   - Arch: one value, which is the case the low-cardinality drop rule turns on.
//   - Rare: present in a handful of ads only, each with its own value. Short for the same
//     reason as Unique and from a tiny population, which is the case a half-sample
//     comparison has least to work with.
//   - RareConst: present in a small fraction of ads, all agreeing on one value. Single-
//     valued and sparse at once, which is the pair the drop rule has to keep telling apart
//     from a sparse attribute whose values are all different.
//   - Missing: carried by no ad at all, so it exists only as an absence.
func distinctCorpus(t *testing.T, c *Collection, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, `[ Unique="slot%d@host%d.example"; Machine="host%d.example"; Mid="m%d"; `, i, i, i%250, i%600)
		fmt.Fprintf(&b, `State=%q; Arch="X86_64"; Idx=%d; `, []string{"Unclaimed", "Claimed"}[i%2], i)
		if i%997 == 0 {
			fmt.Fprintf(&b, `Rare="r%d"; `, i)
		}
		if i%50 == 0 {
			b.WriteString(`RareConst="const"; `)
		}
		b.WriteString(`]`)
		ad, err := classad.Parse(b.String())
		if err != nil {
			t.Fatalf("ad %d: %v", i, err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
}

// trueDistinct counts an attribute's distinct values over EVERY ad in c, which is the
// ground truth a sampled count is measured against. It scans rather than sampling and
// reads no index, so it is independent of everything under test; the keys it builds match
// the profile's so the two numbers are comparable.
func trueDistinct(t *testing.T, c *Collection, attr string) int {
	t.Helper()
	seen := map[string]struct{}{}
	for w := range c.ScanRawWire(nil, false) {
		wire.Ad(w).ForEachNamed(c.intern, func(name string, node []byte) bool {
			if !strings.EqualFold(name, attr) {
				return true
			}
			if lit, ok := scalarValue(node); ok {
				switch lit.Kind {
				case wire.LitString:
					seen[strings.ToLower(lit.Str)] = struct{}{}
				case wire.LitInt, wire.LitReal, wire.LitBool:
					seen[numKey(lit)] = struct{}{}
				}
			}
			return false
		})
	}
	return len(seen)
}

// TestDistinctCountIsFlaggedWhenShort is the reproduction: a corpus whose true distinct
// count is several times what the sample can hold, asserting the reported pair is not a
// lie. Unique has one value per ad over 20,000 ads, so at the default sample bound the
// count is short by roughly 80% -- the number that has to carry a flag.
func TestDistinctCountIsFlaggedWhenShort(t *testing.T) {
	t.Parallel()
	const n = 20000
	c := New(Options{Shards: 1, SegmentSize: 1 << 20})
	distinctCorpus(t, c, n)

	truth := trueDistinct(t, c, "Unique")
	if truth != n {
		t.Fatalf("corpus: Unique has %d distinct values, want one per ad (%d)", truth, n)
	}

	p := c.profileAttrs(defaultSampleMax)[strings.ToLower("Unique")]
	if p == nil {
		t.Fatal("Unique is in every ad but was not profiled")
	}
	got, capped := len(p.distinct), p.lowerBound()
	t.Logf("Unique: truth=%d sampled=%d of %d ads capped=%v (%.1f%% of the truth)",
		truth, got, p.present, capped, 100*float64(got)/float64(truth))

	if got >= truth {
		t.Fatalf("the sample reported %d of %d distinct values; this test needs a count the sample cannot reach", got, truth)
	}
	if !capped {
		t.Errorf("%d of %d distinct values reported as the count: a floor published unflagged", got, truth)
	}
	// The error is one-sided in both directions it can be read: the count never exceeds
	// the truth, and the flag never claims a count that is in fact complete.
	if got > truth {
		t.Errorf("sampled %d > truth %d: the count is supposed to be a floor", got, truth)
	}
}

// TestDistinctSetCeilingExceedsSampleDefault pins the constant relationship the flag needs
// to be raisable at all. A sample of n ads yields at most n distinct values of one
// attribute, so a distinct-set ceiling equal to the default sample bound can only be
// reached by the very last ad of the sample -- and the overflow that would raise the flag
// is the ad after that, which never comes.
func TestDistinctSetCeilingExceedsSampleDefault(t *testing.T) {
	t.Parallel()
	if maxDistinctSample <= defaultSampleMax {
		t.Fatalf("distinct-set ceiling %d does not exceed the default sample bound %d: at the default bound the ceiling can never overflow",
			maxDistinctSample, defaultSampleMax)
	}
}

// TestDistinctCountByCardinalityClass walks the classes. An attribute the sample cannot
// exhaust has to be flagged; one it does exhaust has to not be, otherwise the flag is
// raised everywhere and says nothing.
//
// What an unflagged count promises is that it is not MATERIALLY short, which is not the
// same as exact and must not be asserted as though it were. Mid is the case that shows the
// difference: 4,096 draws over 600 values leave 600*exp(-4096/600) ~ 0.65 of them unseen on
// average, so its count lands on 599 or 600 most passes and occasionally lower, and no flag
// will tell those apart. maxShort is where each case states how much of that noise it can
// produce -- 6 for Mid, which is deep enough into the tail to be stable and is still a 1%
// shortfall, three orders of magnitude away from the four-in-twenty-thousand this whole
// change is about.
func TestDistinctCountByCardinalityClass(t *testing.T) {
	t.Parallel()
	const n = 20000
	c := New(Options{Shards: 1, SegmentSize: 1 << 20})
	distinctCorpus(t, c, n)

	profiles := c.profileAttrs(defaultSampleMax)
	for _, tc := range []struct {
		attr       string
		wantCapped bool
		maxShort   int // how far below the truth an UNFLAGGED count may land
		why        string
	}{
		{"Unique", true, 0, "one value per ad, five times the sample"},
		{"Mid", false, 6, "600 values, all of them seen in the first half of the sample bar sampling noise"},
		{"Machine", false, 0, "250 values, which a sample of thousands exhausts"},
		{"State", false, 0, "two values"},
		{"Arch", false, 0, "one value"},
		{"Rare", true, 0, "a handful of ads, each with its own value"},
		{"RareConst", false, 0, "one value over a sparse but ample population, repeated throughout"},
	} {
		p := profiles[strings.ToLower(tc.attr)]
		if p == nil {
			t.Errorf("%s: not profiled, but the corpus carries it", tc.attr)
			continue
		}
		truth := trueDistinct(t, c, tc.attr)
		got, capped := len(p.distinct), p.lowerBound()
		t.Logf("%-9s truth=%-6d sampled=%-6d present=%-6d capped=%-5v  (%s)", tc.attr, truth, got, p.present, capped, tc.why)
		if got > truth {
			t.Errorf("%s: sampled %d > truth %d", tc.attr, got, truth)
		}
		if capped != tc.wantCapped {
			t.Errorf("%s: capped=%v, want %v (%s; truth %d, sampled %d of %d ads)",
				tc.attr, capped, tc.wantCapped, tc.why, truth, got, p.present)
		}
		if !capped && truth-got > tc.maxShort {
			t.Errorf("%s: unflagged count %d is %d below the truth %d, past the %d this attribute may produce",
				tc.attr, got, truth-got, truth, tc.maxShort)
		}
	}

	// Absence is not a count of zero. An attribute no ad carries has no profile at all, so
	// there is no DistinctValues == 0 to mistake for a genuinely single-valued attribute --
	// which is the distinction the low-cardinality drop rule turns on.
	if p := profiles[strings.ToLower("Missing")]; p != nil {
		t.Errorf("Missing: profiled with present=%d, but no ad carries it", p.present)
	}
}

// TestDistinctCountAllUndefined pins the attribute that is present everywhere and
// indexable nowhere. An undefined literal is counted under "other" and contributes no
// distinct value, so the count is a true zero over a non-zero population -- readable as
// such because SampledPresent is not zero.
func TestDistinctCountAllUndefined(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1})
	for i := 0; i < 500; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ Owner="alice"; Blank=undefined; Idx=%d ]`, i))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	p := c.profileAttrs(defaultSampleMax)[strings.ToLower("Blank")]
	if p == nil {
		t.Fatal("Blank is in every ad but was not profiled")
	}
	if p.present != 500 {
		t.Errorf("Blank present=%d, want 500", p.present)
	}
	if n := len(p.distinct); n != 0 {
		t.Errorf("Blank distinct=%d, want 0: an undefined literal is not a value", n)
	}
	if p.lowerBound() {
		t.Error("Blank: flagged as a floor, but zero indexable values over 500 ads is the count")
	}
}

// TestLowCardinalityDropSurvivesHonestFlag is the behaviour guard on the one decision that
// ever read the flag. The low-cardinality rule reports an index whose sampled values are
// effectively constant; it used to also require the flag clear, which against the old
// meaning could never discriminate anything and against the new meaning would start
// suppressing the single-valued attributes that are also short. Both kinds have to come
// through: Arch (one value over every ad) and RareConst (one value over four ads).
func TestLowCardinalityDropSurvivesHonestFlag(t *testing.T) {
	t.Parallel()
	const n = 20000
	c := New(Options{Shards: 1, SegmentSize: 1 << 20, CategoricalAttrs: []string{"Arch", "RareConst", "Unique"}})
	distinctCorpus(t, c, n)
	c.Reindex()

	// Before any query these are "unused", the rule that fires first. What this half pins is
	// the rationale travelling with the suggestion: Unique's count is a floor and says so.
	byAttr := func(ds []DropSuggestion, attr string) *DropSuggestion {
		for i := range ds {
			if strings.EqualFold(ds[i].Attr, attr) {
				return &ds[i]
			}
		}
		return nil
	}
	unused := c.SuggestDrops(defaultSampleMax)
	uniq := byAttr(unused, "Unique")
	if uniq == nil {
		t.Fatal("Unique is indexed and no query has used it; it should be reported unused")
	}
	t.Logf("Unique drop suggestion: reason=%q distinct=%d present=%d capped=%v",
		uniq.Reason, uniq.DistinctValues, uniq.SampledPresent, uniq.Capped)
	if !uniq.Capped {
		t.Errorf("Unique: drop suggestion carries %d distinct values over %d ads unflagged, with %d in the corpus",
			uniq.DistinctValues, uniq.SampledPresent, n)
	}

	// With demand recorded, "unused" no longer applies and low-cardinality is what has to
	// carry the suggestion -- past a flag that is now set on a great many attributes.
	for i := 0; i < 5; i++ {
		drainQuery(t, c, `Arch == "X86_64"`)
		drainQuery(t, c, `RareConst == "const"`)
		drainQuery(t, c, `Unique == "slot1@host1.example"`)
	}
	drops := c.SuggestDrops(defaultSampleMax)
	if d := byAttr(drops, "Unique"); d != nil {
		t.Errorf("Unique has a distinct value per ad; it must never be called low-cardinality (%+v)", *d)
	}
	for _, attr := range []string{"Arch", "RareConst"} {
		d := byAttr(drops, attr)
		if d == nil {
			t.Errorf("%s: a single-valued index went unreported once it had demand", attr)
			continue
		}
		t.Logf("%-9s reason=%-16q distinct=%d present=%-5d capped=%v", attr, d.Reason, d.DistinctValues, d.SampledPresent, d.Capped)
		if d.Reason != "low-cardinality" {
			t.Errorf("%s reason=%q, want low-cardinality", attr, d.Reason)
		}
		if d.DistinctValues != 1 {
			t.Errorf("%s distinct=%d, want 1", attr, d.DistinctValues)
		}
	}
}

// TestLowCardinalityDropSurvivesAFlaggedCount is the other half of that guard, and the half
// the corpus above cannot produce: an index whose count is BOTH one value and a floor. Over
// a collection small enough that the sample is a census, an attribute carried by a single ad
// is counted as one distinct value out of one ad -- no repetition at all, so the count is
// flagged -- while still being exactly the constant index the low-cardinality rule exists to
// report. Requiring the flag clear would lose it, which is why the rule does not.
func TestLowCardinalityDropSurvivesAFlaggedCount(t *testing.T) {
	t.Parallel()
	const n = 100 // well under defaultSampleMax, so the profile sees every ad
	c := New(Options{Shards: 1, CategoricalAttrs: []string{"Solo"}})
	for i := 0; i < n; i++ {
		src := fmt.Sprintf(`[ Owner="alice"; Idx=%d ]`, i)
		if i == 0 {
			src = fmt.Sprintf(`[ Owner="alice"; Idx=%d; Solo="only" ]`, i)
		}
		ad, err := classad.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	for i := 0; i < 5; i++ {
		drainQuery(t, c, `Solo == "only"`) // so "unused" does not claim it first
	}

	p := c.profileAttrs(defaultSampleMax)[strings.ToLower("Solo")]
	if p == nil || p.present != 1 || len(p.distinct) != 1 {
		t.Fatalf("Solo profile = %+v; this test needs exactly one ad carrying exactly one value", p)
	}
	if !p.lowerBound() {
		t.Fatal("Solo: one value seen in one ad is not flagged; this test needs the flagged-and-constant case")
	}

	for _, d := range c.SuggestDrops(defaultSampleMax) {
		if strings.EqualFold(d.Attr, "Solo") {
			t.Logf("Solo drop suggestion: reason=%q distinct=%d present=%d capped=%v", d.Reason, d.DistinctValues, d.SampledPresent, d.Capped)
			if d.Reason != "low-cardinality" {
				t.Errorf("Solo reason=%q, want low-cardinality", d.Reason)
			}
			return
		}
	}
	t.Error("Solo: a single-valued index went unreported because its count is a floor")
}

// TestSuggestIndexesUnchangedByHonestFlag pins the other half of that promise: what gets
// SUGGESTED does not move. The add path chooses a Kind from literal-kind dominance and
// observed demand and reads neither DistinctValues nor Capped, so correcting the statistic
// is a reporting change and not an indexing one. The suggested set and its kinds are
// spelled out here so a later change to the add path that starts consulting the count has
// to say so by breaking this.
func TestSuggestIndexesUnchangedByHonestFlag(t *testing.T) {
	t.Parallel()
	const n = 20000
	c := New(Options{Shards: 1, SegmentSize: 1 << 20})
	distinctCorpus(t, c, n)

	for i := 0; i < 3; i++ {
		drainQuery(t, c, `Unique == "slot7@host7.example"`) // near-unique, badly undercounted
		drainQuery(t, c, `Machine == "host3.example"`)      // mid cardinality, counted exactly
		drainQuery(t, c, `State == "Claimed"`)              // two values
		drainQuery(t, c, `Arch == "X86_64"`)                // one value
		drainQuery(t, c, `Idx > 100`)                       // numeric range
	}

	got := c.SuggestIndexes(defaultSampleMax)
	var lines []string
	for _, s := range got {
		lines = append(lines, fmt.Sprintf("%s/%s", s.Attr, s.Kind))
	}
	sort.Strings(lines)
	want := []string{"Arch/categorical", "Idx/value", "Machine/categorical", "State/categorical", "Unique/categorical"}
	if strings.Join(lines, " ") != strings.Join(want, " ") {
		t.Errorf("suggested %v, want %v: the add path is not supposed to read the distinct count", lines, want)
	}

	// Every attribute above is suggested regardless of how its count reads, including the
	// one whose count is a floor five times short.
	for _, s := range got {
		if strings.EqualFold(s.Attr, "Unique") {
			if !s.Capped {
				t.Errorf("Unique: suggested with an unflagged count of %d over %d ads", s.DistinctValues, s.SampledPresent)
			}
			if s.DistinctValues >= n {
				t.Errorf("Unique: distinct=%d, but a sample of %d ads cannot have seen that many", s.DistinctValues, s.SampledPresent)
			}
		}
		if strings.EqualFold(s.Attr, "Arch") && s.Capped {
			t.Errorf("Arch: a single-valued attribute over thousands of ads flagged as a floor (%+v)", s)
		}
	}
}

// TestAutoTuneUnchangedByHonestFlag runs the applying path over the same corpus. AutoTune
// adds from demand and drops only on "unused", so neither the count nor the flag reaches a
// decision; this records the changes it makes so a regression that starts gating adds on
// cardinality is visible as a diff rather than as a quieter pool.
func TestAutoTuneUnchangedByHonestFlag(t *testing.T) {
	t.Parallel()
	const n = 20000
	c := New(Options{Shards: 1, SegmentSize: 1 << 20})
	distinctCorpus(t, c, n)
	for i := 0; i < 3; i++ {
		drainQuery(t, c, `Unique == "slot7@host7.example"`)
		drainQuery(t, c, `Arch == "X86_64"`)
		drainQuery(t, c, `Idx > 100`)
	}

	res := c.AutoTune(AutoTuneOptions{})
	var got []string
	for _, ch := range res.Changes {
		got = append(got, fmt.Sprintf("%s/%s/%s", ch.Action, ch.Attr, ch.Kind))
	}
	sort.Strings(got)
	want := []string{"add/Arch/categorical", "add/Idx/value", "add/Unique/categorical"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("AutoTune applied %v, want %v", got, want)
	}
}

// TestSampleBoundNeedsASecondHalf pins the degenerate inputs the half-sample comparison has
// to survive. With no second half there is no evidence of growth, and claiming a floor on
// no evidence would flag every profile taken over a trivial sample.
func TestSampleBoundNeedsASecondHalf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		p    attrProfile
		want bool
	}{
		{"empty profile", attrProfile{}, false},
		{"whole sample in the first half", attrProfile{present: 10, presentHalf: 10, distinct: map[string]struct{}{}}, false},
		{"second half adds nothing", attrProfile{present: 100, presentHalf: 50, distinctHalf: 3, distinct: map[string]struct{}{"a": {}, "b": {}, "c": {}}}, false},
		{"a value per ad, no halves to compare", attrProfile{present: 3, presentHalf: 3, distinct: map[string]struct{}{"a": {}, "b": {}, "c": {}}}, true},
		{"ceiling reached", attrProfile{present: 2, presentHalf: 2, capped: true, distinct: map[string]struct{}{}}, true},
	} {
		if got := tc.p.lowerBound(); got != tc.want {
			t.Errorf("%s: lowerBound()=%v, want %v", tc.name, got, tc.want)
		}
	}
}
