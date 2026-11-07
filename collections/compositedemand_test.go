package collections

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// --- helpers -------------------------------------------------------------------------

// probesOf parses a constraint and returns the probes one conjunction of it yields, which is
// what every demand call site hands record.
func probesOf(t *testing.T, qs string) []vm.Probe {
	t.Helper()
	q, err := vm.Parse(qs)
	if err != nil {
		t.Fatalf("parse %q: %v", qs, err)
	}
	return q.Probes()
}

// tupleDemandOf reports the demand recorded for the tuple of these attributes, in any order.
func tupleDemandOf(d *demandTracker, names ...string) int64 {
	v, ok := d.tuples.Load(strings.Join(foldedSorted(names), tupleKeySep))
	if !ok {
		return 0
	}
	return v.(*tupleCounts).eq.Load()
}

// compositeProvenance maps each configured composite's label to whether it is auto-created.
func compositeProvenance(c *Collection) map[string]bool {
	out := map[string]bool{}
	for _, cs := range c.spec.Load().comps {
		out[cs.label()] = cs.auto
	}
	return out
}

// compositeCoverage reports how many non-empty live segments carry postings for the
// composite with this label, out of the total -- the difference between a composite that is
// configured and one that is built.
func compositeCoverage(c *Collection, label string) (segsWith, total int) {
	var key string
	for _, cs := range c.spec.Load().comps {
		if cs.label() == label {
			key = cs.key
		}
	}
	if key == "" {
		return 0, 0
	}
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 {
				continue
			}
			total++
			if si := seg.idx.Load(); si != nil && si.comp[key] != nil {
				segsWith++
			}
		}
		sh.mu.RUnlock()
	}
	return segsWith, total
}

// putTupleAds fills a collection with a job-queue shape whose conjunctions are worth a
// composite: ClusterId/ProcId as values, Owner/Arch as strings.
func putTupleAds(t *testing.T, c *Collection, n int) map[int]*classad.ClassAd {
	t.Helper()
	owners := []string{"alice", "bob", "carol"}
	arches := []string{"X86_64", "aarch64"}
	src := map[int]*classad.ClassAd{}
	for i := range n {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ID=%d; ClusterId=%d; ProcId=%d; Owner=%q; Arch=%q ]`,
			i, i/5, i%5, owners[i%len(owners)], arches[i%len(arches)]))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
		src[i] = ad
	}
	return src
}

// --- counting ------------------------------------------------------------------------

// TestTupleDemandCountsConjunctions is the signal the per-attribute counters cannot carry:
// two equalities in ONE conjunction, which is what a composite answers, as opposed to the
// same two equalities issued separately, which it does not.
func TestTupleDemandCountsConjunctions(t *testing.T) {
	t.Parallel()
	d := newDemandTracker()
	d.record(probesOf(t, `ClusterId == 7 && ProcId == 3`))
	if got := tupleDemandOf(d, "ClusterId", "ProcId"); got != 1 {
		t.Fatalf("tuple demand after one conjunction = %d, want 1", got)
	}
	// The tuple counter is in ADDITION to the per-attribute ones, not instead of them.
	v, ok := d.m.Load("clusterid")
	if !ok || v.(*demandCounts).eq.Load() != 1 {
		t.Errorf("per-attribute demand for ClusterId not recorded alongside the tuple")
	}

	d.record(probesOf(t, `ClusterId == 8 && ProcId == 1`))
	if got := tupleDemandOf(d, "ClusterId", "ProcId"); got != 2 {
		t.Errorf("tuple demand after two conjunctions = %d, want 2", got)
	}

	// The same two equalities as separate queries are two attributes being queried, not a
	// tuple: nothing here would be faster for a composite over the pair.
	d.record(probesOf(t, `ClusterId == 9`))
	d.record(probesOf(t, `ProcId == 4`))
	if got := tupleDemandOf(d, "ClusterId", "ProcId"); got != 2 {
		t.Errorf("tuple demand after two SEPARATE queries = %d, want it unchanged at 2", got)
	}
}

// TestTupleDemandCountsOnlySingleValueEqualities covers the rule a composite imposes: it
// answers an equality on every component and nothing else, so only a probe pinning its
// attribute to exactly one value is demand for a tuple.
func TestTupleDemandCountsOnlySingleValueEqualities(t *testing.T) {
	t.Parallel()
	// A pinned pair first, so a later zero reads as "this shape does not count" rather than
	// "nothing was recorded at all".
	d := newDemandTracker()
	d.record(probesOf(t, `ClusterId == 7 && ProcId == 3`))
	if tupleDemandOf(d, "ClusterId", "ProcId") != 1 {
		t.Fatal("the control conjunction recorded no tuple; the rest of this test would be vacuous")
	}

	for _, qs := range []string{
		`ClusterId == 7 && ProcId > 3`,                   // a range pins nothing
		`ClusterId == 7 && ProcId != 3`,                  // nor does an inequality
		`ClusterId == 7 && ProcId =?= 3`,                 // =?= is not the equality an index keys on
		`ClusterId == 7 && (ProcId == 3 || ProcId == 4)`, // an `in` over two values is not one value
		`ClusterId == 7 && isUndefined(ProcId)`,          // a presence test pins no value
	} {
		before := tupleDemandOf(d, "ClusterId", "ProcId")
		d.record(probesOf(t, qs))
		if got := tupleDemandOf(d, "ClusterId", "ProcId"); got != before {
			t.Errorf("%q moved the tuple counter %d -> %d; it pins no tuple", qs, before, got)
		}
	}

	// A membership narrowed to a single value IS an equality, and does count.
	d.record(probesOf(t, `ClusterId == 7 && member(ProcId, {3})`))
	if got := tupleDemandOf(d, "ClusterId", "ProcId"); got != 2 {
		t.Errorf("single-value membership: tuple demand = %d, want 2", got)
	}
}

// TestTupleDemandCanonicalizesOrder: a conjunction is a set, so the tuple is keyed on the
// sorted components. Two queries spelling the same pair in either order are demand for one
// composite, and must not split their evidence between two counters.
func TestTupleDemandCanonicalizesOrder(t *testing.T) {
	t.Parallel()
	d := newDemandTracker()
	d.record(probesOf(t, `ClusterId == 7 && ProcId == 3`))
	d.record(probesOf(t, `ProcId == 3 && ClusterId == 7`))
	if got := tupleDemandOf(d, "ProcId", "ClusterId"); got != 2 {
		t.Errorf("tuple demand = %d, want 2 (both orders are one tuple)", got)
	}
	if n := d.tupleN.Load(); n != 1 {
		t.Errorf("%d tuple keys, want 1", n)
	}
	// Attribute names are case-insensitive, so the casing a query happens to use is not a
	// second tuple either.
	d.record(probesOf(t, `CLUSTERID == 7 && procid == 3`))
	if n := d.tupleN.Load(); n != 1 {
		t.Errorf("%d tuple keys after a differently-cased query, want 1", n)
	}
}

// TestTupleDemandRespectsWidthLimit covers maxCompositeAttrs and its awkward consequence: a
// conjunction one attribute too wide contributes nothing, not a truncated tuple, because
// choosing which components to drop would be choosing a tuple for the query.
func TestTupleDemandRespectsWidthLimit(t *testing.T) {
	t.Parallel()
	conj := func(n int) string {
		var parts []string
		for i := range n {
			parts = append(parts, fmt.Sprintf("A%d == %d", i, i))
		}
		return strings.Join(parts, " && ")
	}
	d := newDemandTracker()
	d.record(probesOf(t, conj(maxCompositeAttrs)))
	if n := d.tupleN.Load(); n != 1 {
		t.Fatalf("a conjunction of exactly maxCompositeAttrs recorded %d tuples, want 1", n)
	}
	d.record(probesOf(t, conj(maxCompositeAttrs+1)))
	if n := d.tupleN.Load(); n != 1 {
		t.Errorf("a conjunction of maxCompositeAttrs+1 recorded a tuple: %d keys, want 1", n)
	}
}

// TestTupleDemandCapsKeys covers the bound the per-attribute map does not need. Distinct
// conjunction shapes are not bounded by the schema, so the map is bounded explicitly: past
// the cap the known tuples keep counting and new ones are ignored.
func TestTupleDemandCapsKeys(t *testing.T) {
	t.Parallel()
	d := newDemandTracker()
	for i := range maxTupleKeys {
		d.record(probesOf(t, fmt.Sprintf(`A%d == 1 && B%d == 2`, i, i)))
	}
	if n := d.tupleN.Load(); n != maxTupleKeys {
		t.Fatalf("%d tuple keys after filling to the cap, want %d", n, maxTupleKeys)
	}
	d.record(probesOf(t, `Fresh == 1 && Shape == 2`))
	if got := tupleDemandOf(d, "Fresh", "Shape"); got != 0 {
		t.Errorf("a new tuple was admitted past the cap (demand %d)", got)
	}
	if n := d.tupleN.Load(); n != maxTupleKeys {
		t.Errorf("%d tuple keys, want the cap %d", n, maxTupleKeys)
	}
	// A tuple already known still counts, so a stable workload is not frozen by the cap.
	d.record(probesOf(t, `A0 == 1 && B0 == 2`))
	if got := tupleDemandOf(d, "A0", "B0"); got != 2 {
		t.Errorf("known tuple demand = %d, want 2", got)
	}
}

// TestTupleDemandDecays: the tuple counters are on the per-attribute counters' clock, so a
// tuple a workload has moved on from fades at the same rate its components do, and is
// forgotten at the same floor.
func TestTupleDemandDecays(t *testing.T) {
	t.Parallel()
	d := newDemandTracker()
	for range 10 {
		d.record(probesOf(t, `ClusterId == 7 && ProcId == 3`))
	}
	if got := tupleDemandOf(d, "ClusterId", "ProcId"); got != 10 {
		t.Fatalf("tuple demand = %d, want 10", got)
	}
	d.decay(0.5)
	if got := tupleDemandOf(d, "ClusterId", "ProcId"); got != 5 {
		t.Errorf("tuple demand after a half decay = %d, want 5", got)
	}
	d.decay(0.01) // 0.05, below demandFloor
	if got := tupleDemandOf(d, "ClusterId", "ProcId"); got != 0 {
		t.Errorf("tuple demand below the floor = %d, want it forgotten", got)
	}
	if n := d.tupleN.Load(); n != 0 {
		t.Errorf("tuple key count = %d after the tuple was forgotten, want 0", n)
	}
}

// --- checkpointing -------------------------------------------------------------------

// TestTupleDemandSurvivesReopen: tuple demand is checkpointed with the per-attribute demand,
// so a restart does not erase the evidence for a composite -- and the restored evidence is
// actionable, not merely present.
func TestTupleDemandSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Dir: dir, SegmentSize: 1 << 16, CategoricalAttrs: []string{"Owner", "Arch"}}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	putTupleAds(t, c, 100)
	for range 12 {
		drainQuery(t, c, `Owner == "alice" && Arch == "X86_64"`)
	}
	before := tupleDemandOf(c.demand, "Owner", "Arch")
	if before != 12 {
		t.Fatalf("pre-save tuple demand = %d, want 12", before)
	}
	c.SaveDemand()
	if cerr := c.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	c2, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if got := tupleDemandOf(c2.demand, "Owner", "Arch"); got != before {
		t.Fatalf("tuple demand after reopen = %d, want %d", got, before)
	}
	// The components are configured again by the caller; the tuple is not configured
	// anywhere, so the composite can only come back from the restored demand.
	if len(c2.spec.Load().comps) != 0 {
		t.Fatal("a composite was configured on reopen; this test could not show demand restored it")
	}
	res := c2.AutoTune(AutoTuneOptions{Reindex: true})
	if !res.Changed {
		t.Fatal("AutoTune made no change from restored tuple demand")
	}
	if auto, ok := compositeProvenance(c2)["Arch+Owner"]; !ok || !auto {
		t.Errorf("composite Arch+Owner not auto-created from restored demand: %v", compositeProvenance(c2))
	}
}

// TestDemandCheckpointWithoutTuplesStillLoads: the tuple counters were added to demand.json
// as an optional field rather than a new version, so a checkpoint written before they
// existed is still a checkpoint, and the per-attribute demand in it survives untouched.
func TestDemandCheckpointWithoutTuplesStillLoads(t *testing.T) {
	dir := t.TempDir()
	old := persistedDemand{
		Version: demandVer, SavedUnix: time.Now().UnixNano(), Total: 40,
		Attrs: map[string]persistedCounts{"owner": {Eq: 40}},
	}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "tuples") {
		t.Fatalf("the fixture carries a tuples field, so it is not a pre-tuples checkpoint: %s", data)
	}
	if werr := os.WriteFile(filepath.Join(dir, demandFile), data, 0o600); werr != nil {
		t.Fatal(werr)
	}
	c, err := Open(Options{Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	v, ok := c.demand.m.Load("owner")
	if !ok || v.(*demandCounts).eq.Load() != 40 {
		t.Errorf("per-attribute demand from a pre-tuples checkpoint was not restored")
	}
	if n := c.demand.tupleN.Load(); n != 0 {
		t.Errorf("a pre-tuples checkpoint produced %d tuples", n)
	}
}

// --- suggestions ----------------------------------------------------------------------

// TestSuggestCompositesNeedsIndexedComponents covers the components-first rule: a composite
// answers the full tuple only, and everything else falls back to the components' own
// postings, so there has to be something to fall back to before the tuple is suggested.
func TestSuggestCompositesNeedsIndexedComponents(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4})
	src := putTupleAds(t, c, 150)
	for range 6 {
		assertMatchesBrute(t, c, src, `Owner == "alice" && Arch == "X86_64"`)
	}
	// The demand is there; the suggestion is withheld because the components are not
	// indexed. Without this check the empty result below would prove nothing.
	if got := tupleDemandOf(c.demand, "Owner", "Arch"); got != 6 {
		t.Fatalf("tuple demand = %d, want 6", got)
	}
	if s := c.SuggestComposites(); len(s) != 0 {
		t.Fatalf("composite suggested over unindexed components: %+v", s)
	}
	// One component is not enough either.
	c.AddIndex([]string{"Owner"}, nil)
	if s := c.SuggestComposites(); len(s) != 0 {
		t.Errorf("composite suggested with only one component indexed: %+v", s)
	}
	c.AddIndex([]string{"Arch"}, nil)
	s := c.SuggestComposites()
	if len(s) != 1 {
		t.Fatalf("got %d composite suggestions, want 1: %+v", len(s), s)
	}
	if strings.Join(s[0].Attrs, "+") != "Arch+Owner" {
		t.Errorf("suggested tuple = %v, want [Arch Owner]", s[0].Attrs)
	}
	if s[0].Queries != 6 {
		t.Errorf("suggested tuple demand = %d, want 6", s[0].Queries)
	}
}

// TestSuggestCompositesSkipsConfiguredTuple: order is part of a composite's identity but not
// of what it answers, so a tuple a human configured in the other order must not be created a
// second time in the sorted order the counters canonicalize to.
func TestSuggestCompositesSkipsConfiguredTuple(t *testing.T) {
	t.Parallel()
	// The components are declared Arch-then-Owner and the composite Owner-then-Arch, so the
	// configured tuple's component IDS run descending while the demand counters canonicalize
	// to the sorted spelling. A set comparison that was really a sequence comparison would
	// miss the match and suggest the same tuple a second time.
	base := Options{Shards: 4, CategoricalAttrs: []string{"Arch", "Owner"}}
	withComp := base
	withComp.CompositeAttrs = [][]string{{"Owner", "Arch"}} // the REVERSE of the sorted order

	c := New(withComp)
	src := putTupleAds(t, c, 100)
	for range 5 {
		assertMatchesBrute(t, c, src, `Owner == "alice" && Arch == "X86_64"`)
	}
	if len(c.spec.Load().comps) != 1 {
		t.Fatalf("the configured composite is missing; this test would be vacuous")
	}
	if ids := c.spec.Load().comps[0].ids; len(ids) != 2 || ids[0] < ids[1] {
		t.Fatalf("the configured composite's ids %v are already ascending, so this fixture cannot tell a set comparison from a sequence one", ids)
	}
	if s := c.SuggestComposites(); len(s) != 0 {
		t.Errorf("suggested a tuple already configured in the other order: %+v", s)
	}

	// The control: the same demand against the same indexes, with nothing configured.
	c2 := New(base)
	src2 := putTupleAds(t, c2, 100)
	for range 5 {
		assertMatchesBrute(t, c2, src2, `Owner == "alice" && Arch == "X86_64"`)
	}
	if s := c2.SuggestComposites(); len(s) != 1 {
		t.Errorf("got %d suggestions with nothing configured, want 1: %+v", len(s), s)
	}
}

// --- AutoTune ---------------------------------------------------------------------------

// TestAutoTuneCreatesCompositeAfterComponents is the staging the components-first rule
// produces: the pass that creates the component indexes does not also create the composite,
// because the suggestion is computed from the snapshot before any change. The composite
// lands on the next pass, by which time the demand has survived another interval of decay.
func TestAutoTuneCreatesCompositeAfterComponents(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4})
	src := putTupleAds(t, c, 300)
	for range 5 {
		assertMatchesBrute(t, c, src, `Owner == "alice" && Arch == "X86_64"`)
	}

	first := c.AutoTune(AutoTuneOptions{Reindex: true})
	if !first.Changed {
		t.Fatal("the first AutoTune made no change despite demand")
	}
	for _, ch := range first.Changes {
		if ch.Kind == "composite" {
			t.Errorf("a composite was created on the same pass as its components: %+v", ch)
		}
	}
	cat, _ := c.IndexedAttrs()
	if !contains(cat, "Owner") || !contains(cat, "Arch") {
		t.Fatalf("the components were not indexed by the first pass (%v); the second pass could not create a composite", cat)
	}

	second := c.AutoTune(AutoTuneOptions{Reindex: true})
	if !second.Changed {
		t.Fatal("the second AutoTune did not create the composite")
	}
	var added *AutoTuneChange
	for i, ch := range second.Changes {
		if ch.Kind == "composite" {
			added = &second.Changes[i]
		}
	}
	if added == nil {
		t.Fatalf("no composite change on the second pass: %+v", second.Changes)
	}
	if added.Attr != "Arch+Owner" || added.Action != "add" || added.Reason != "demand" {
		t.Errorf("composite change = %+v, want Arch+Owner/add/demand", *added)
	}
	if auto, ok := compositeProvenance(c)["Arch+Owner"]; !ok || !auto {
		t.Fatalf("composite not configured as auto: %v", compositeProvenance(c))
	}
	// Reindex:true means it is built, not merely configured.
	if w, total := compositeCoverage(c, "Arch+Owner"); total == 0 || w != total {
		t.Errorf("composite not built into every segment: %d/%d", w, total)
	}
	// And the rows are what they were before any of this.
	assertMatchesBrute(t, c, src, `Owner == "alice" && Arch == "X86_64"`)

	// A third pass has nothing left to do: the tuple is configured now, so it is not
	// suggested again.
	if third := c.AutoTune(AutoTuneOptions{Reindex: true}); third.Changed {
		t.Errorf("a third pass changed something: %+v", third.Changes)
	}
}

// TestAutoTuneDropsUnusedComposite mirrors TestAutoTuneDropsUnused for the tuple: an AUTO
// composite no conjunction pins is droppable, a configured one is not, and neither is
// touched without DropUnused.
func TestAutoTuneDropsUnusedComposite(t *testing.T) {
	t.Parallel()
	waived := AutoTuneOptions{
		DropUnused: true, Reindex: true,
		DropUnusedMinWindow: -1, DropUnusedMinQueries: -1,
	}
	base := Options{Shards: 4, CategoricalAttrs: []string{"Owner", "Arch"}}

	c := New(base)
	putTupleAds(t, c, 100)
	if added := c.addCompositesAuto([][]string{{"Owner", "Arch"}}); len(added) != 1 {
		t.Fatalf("the auto composite was not installed; the drop below would be vacuous")
	}
	if !c.AutoTune(waived).Changed {
		t.Fatal("AutoTune should have dropped the unused auto composite")
	}
	if n := len(c.spec.Load().comps); n != 0 {
		t.Errorf("%d composites left after the drop, want 0", n)
	}

	// A human-configured composite is never auto-dropped, however idle.
	withComp := base
	withComp.CompositeAttrs = [][]string{{"Owner", "Arch"}}
	ch := New(withComp)
	putTupleAds(t, ch, 100)
	if len(ch.spec.Load().comps) != 1 {
		t.Fatal("the configured composite is missing; this half of the test would be vacuous")
	}
	if res := ch.AutoTune(waived); res.Changed {
		t.Errorf("AutoTune dropped a human-configured composite: %+v", res.Changes)
	}
	if n := len(ch.spec.Load().comps); n != 1 {
		t.Errorf("%d configured composites left, want 1", n)
	}

	// Without DropUnused an idle auto composite is left alone.
	c2 := New(base)
	putTupleAds(t, c2, 100)
	c2.addCompositesAuto([][]string{{"Owner", "Arch"}})
	if res := c2.AutoTune(AutoTuneOptions{Reindex: true}); res.Changed {
		t.Errorf("AutoTune without DropUnused dropped the composite: %+v", res.Changes)
	}
}

// TestAutoTuneCompositeDropNeedsEvidence: a freshly started collection has watched nothing,
// so every tuple reads as unused. Acting on that would discard the composites on every
// restart and rebuild them -- and a composite's rebuild is every sealed segment's index.
func TestAutoTuneCompositeDropNeedsEvidence(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Owner", "Arch"}})
	putTupleAds(t, c, 100)
	c.addCompositesAuto([][]string{{"Owner", "Arch"}})
	if len(c.spec.Load().comps) != 1 {
		t.Fatal("the auto composite was not installed; this test would be vacuous")
	}

	c.AutoTune(AutoTuneOptions{DropUnused: true, Reindex: true})
	if len(c.spec.Load().comps) == 0 {
		t.Fatal("composite dropped with no observation behind it")
	}
	// Waiving the window but not the query floor must still hold.
	c.AutoTune(AutoTuneOptions{DropUnused: true, Reindex: true, DropUnusedMinWindow: -1})
	if len(c.spec.Load().comps) == 0 {
		t.Error("composite dropped though no queries had been observed")
	}
}

// TestAutoTuneCompositeDropDefersToBudget: with a memory budget configured, unused-drops
// stand down for composites exactly as they do for attributes -- the budget trim reclaims
// the same memory when space is actually short, and only then.
func TestAutoTuneCompositeDropDefersToBudget(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Owner", "Arch"}})
	putTupleAds(t, c, 100)
	c.addCompositesAuto([][]string{{"Owner", "Arch"}})
	if len(c.spec.Load().comps) != 1 {
		t.Fatal("the auto composite was not installed; this test would be vacuous")
	}
	c.AutoTune(AutoTuneOptions{
		DropUnused: true, Reindex: true,
		DropUnusedMinWindow: -1, DropUnusedMinQueries: -1,
		BudgetHighFrac: 1000,
	})
	if len(c.spec.Load().comps) == 0 {
		t.Error("composite dropped as unused while well under the memory budget")
	}
}

// TestCompositeDemandCountsWiderConjunctions: a composite over (Arch, Owner) serves a
// conjunction that pins those two and a third attribute as well -- it covers two of the
// three probes. Counting only the conjunctions that pin exactly its tuple would read a
// composite that is answering every query in the workload as unused.
func TestCompositeDemandCountsWiderConjunctions(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4,
		CategoricalAttrs: []string{"Owner", "Arch"}, ValueAttrs: []string{"ID"}})
	src := putTupleAds(t, c, 150)
	c.addCompositesAuto([][]string{{"Owner", "Arch"}})
	if len(c.spec.Load().comps) != 1 {
		t.Fatal("the auto composite was not installed; this test would be vacuous")
	}
	for range 5 {
		assertMatchesBrute(t, c, src, `Owner == "alice" && Arch == "X86_64" && ID == 30`)
	}
	// Nothing pins exactly the pair ...
	if got := tupleDemandOf(c.demand, "Owner", "Arch"); got != 0 {
		t.Fatalf("exact pair demand = %d, want 0 (only the triple was queried)", got)
	}
	// ... but the composite over the pair is being used by every one of those queries.
	if got := c.demand.compositeDemand([]string{"arch", "owner"}); got != 5 {
		t.Errorf("composite demand = %d, want 5 from the wider conjunction", got)
	}
	if s := c.SuggestCompositeDrops(); len(s) != 0 {
		t.Errorf("a composite serving the workload was suggested for dropping: %+v", s)
	}
	// The same pass creates a composite over the TRIPLE, which is the tuple the queries
	// actually pin -- but it must not take the pair with it.
	c.AutoTune(AutoTuneOptions{
		DropUnused: true, Reindex: true,
		DropUnusedMinWindow: -1, DropUnusedMinQueries: -1,
	})
	if _, ok := compositeProvenance(c)["Owner+Arch"]; !ok {
		t.Errorf("AutoTune dropped a composite the workload is using through a wider conjunction: %v",
			compositeProvenance(c))
	}
}

// TestBudgetTrimDropsAutoComposite: a composite is reported to the memory budget under its
// tuple, not an attribute, so the trim has to drop it as a composite. The components here
// are human-configured, so the composite is the ONLY trim candidate and cannot be removed as
// a side effect of dropping one of them.
func TestBudgetTrimDropsAutoComposite(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Owner", "Arch"}})
	src := putTupleAds(t, c, 300)
	c.addCompositesAuto([][]string{{"Owner", "Arch"}})
	c.Reindex()
	if w, total := compositeCoverage(c, "Owner+Arch"); total == 0 || w != total {
		t.Fatalf("the composite was not built (%d/%d), so it would not reach the budget at all", w, total)
	}
	var sawComposite bool
	for _, s := range c.IndexSizes().PerIndex {
		if s.Kind == "composite" && s.Auto {
			sawComposite = true
		}
	}
	if !sawComposite {
		t.Fatal("the composite is not reported as an auto index; the trim could not select it")
	}

	res := c.AutoTune(AutoTuneOptions{
		BudgetHighFrac: 1e-9, BudgetSlackBytes: -1, Reindex: true,
	})
	var trimmed bool
	for _, ch := range res.Changes {
		if ch.Kind == "composite" && ch.Action == "drop" && ch.Reason == "over-budget" {
			trimmed = true
		}
	}
	if !trimmed {
		t.Fatalf("the composite was not selected by the budget trim: %+v", res.Changes)
	}
	if n := len(c.spec.Load().comps); n != 0 {
		t.Errorf("%d composites survived the trim, want 0", n)
	}
	// The components are human-configured, so the trim left them where they were.
	if cat, _ := c.IndexedAttrs(); !contains(cat, "Owner") || !contains(cat, "Arch") {
		t.Errorf("the trim dropped human-configured component indexes: %v", cat)
	}
	assertMatchesBrute(t, c, src, `Owner == "alice" && Arch == "X86_64"`)
}

// TestBudgetTrimRanksCompositeByTupleDemand: the budget trim drops the least-demanded auto
// index first, and a composite's demand is the tuple counters' -- looking it up by
// attribute name finds nothing and ranks every composite as the least valuable index in the
// collection, i.e. the first thing to be thrown away.
func TestBudgetTrimRanksCompositeByTupleDemand(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Owner", "Arch"}})
	src := putTupleAds(t, c, 300)
	c.addIndexAuto([]string{"Ghost"}, nil) // auto, and no ad has it and no query wants it
	c.addCompositesAuto([][]string{{"Owner", "Arch"}})
	c.Reindex()
	for range 5 {
		assertMatchesBrute(t, c, src, `Owner == "alice" && Arch == "X86_64"`)
	}
	if got := c.compositeDemandByLabel("Owner+Arch"); got != 5 {
		t.Fatalf("composite demand = %d, want 5; the ranking below would compare two zeroes", got)
	}

	// A target of zero, so everything auto is listed and the ORDER is what is being read.
	order := c.budgetTrim(1e-9, 1e-9, -1)
	var names []string
	for _, s := range order {
		names = append(names, s.Attr+"/"+s.Kind)
	}
	if len(order) < 2 {
		t.Fatalf("budgetTrim selected %v, want both auto indexes", names)
	}
	if order[0].Attr != "Ghost" {
		t.Errorf("trim order %v: the demanded composite was ranked above the idle Ghost index", names)
	}
}

// TestAddCompositesAutoRefusesDuplicateTuple: one composite per tuple SET. Order is part of
// a composite's identity, so nothing stops two specs over the same pair in two orders from
// coexisting -- they would index the same records twice under two keys, and the drop and
// trim paths would each see half the picture.
func TestAddCompositesAutoRefusesDuplicateTuple(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Arch", "Owner"}})
	putTupleAds(t, c, 50)
	if added := c.addCompositesAuto([][]string{{"Owner", "Arch"}}); len(added) != 1 {
		t.Fatalf("the first add installed %v, want the tuple; the rest would be vacuous", added)
	}
	if added := c.addCompositesAuto([][]string{{"Arch", "Owner"}}); len(added) != 0 {
		t.Errorf("the same tuple in the other order was installed again: %v", added)
	}
	if n := len(c.spec.Load().comps); n != 1 {
		t.Errorf("%d composites over one tuple, want 1: %v", n, compositeProvenance(c))
	}
	// Two different orders of the same tuple handed over in ONE call are also one composite.
	c2 := New(Options{Shards: 4, CategoricalAttrs: []string{"Arch", "Owner"}})
	putTupleAds(t, c2, 50)
	if added := c2.addCompositesAuto([][]string{{"Owner", "Arch"}, {"Arch", "Owner"}}); len(added) != 1 {
		t.Errorf("one call with both orders installed %v, want one", added)
	}
}
