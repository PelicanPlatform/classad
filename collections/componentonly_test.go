package collections

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Component-only composite components: an attribute that is a composite component and
// nothing else, carrying no postings of its own.
//
// Every test here is about the same hazard. A component-only attribute reaching a
// single-attribute index path would be looked up in a posting map that has no entry for it,
// and an absent entry reads as an EMPTY posting -- "no record holds that value" -- which
// intersected into a candidate set drops every row the query should have returned. There is
// no crash and no error; the query simply returns fewer rows. So the tests below are
// overwhelmingly differential: the same constraints answered by a store with the
// component-only composite and by a store with no index at all, required to agree row for
// row.

// componentOnlyShape is one of the three index configurations every constraint must be
// answered identically by.
type componentOnlyShape int

const (
	// shapeUnindexed has no index at all: the scan, which is the oracle.
	shapeUnindexed componentOnlyShape = iota
	// shapeFullyIndexed indexes both components in their own right and composites them,
	// which is what a composite was allowed to be before component-only existed.
	shapeFullyIndexed
	// shapeComponentOnly indexes ClusterId and Owner in their own right, declares ProcId
	// and Cmd component-only, and composites them -- the shape under test.
	shapeComponentOnly
	// shapeAllComponentOnly declares EVERY component component-only, so no attribute of
	// the composite has an index of its own anywhere.
	shapeAllComponentOnly
)

func (s componentOnlyShape) String() string {
	switch s {
	case shapeUnindexed:
		return "unindexed"
	case shapeFullyIndexed:
		return "fully-indexed"
	case shapeComponentOnly:
		return "component-only"
	}
	return "all-component-only"
}

func componentOnlyOptions(shape componentOnlyShape) Options {
	opts := Options{Shards: 4}
	switch shape {
	case shapeUnindexed:
		return opts
	case shapeFullyIndexed:
		opts.ValueAttrs = []string{"ClusterId", "ProcId", "JobStatus"}
		opts.CategoricalAttrs = []string{"Owner", "Cmd"}
	case shapeComponentOnly:
		opts.ValueAttrs = []string{"ClusterId", "JobStatus"}
		opts.CategoricalAttrs = []string{"Owner"}
		opts.ValueComponentAttrs = []string{"ProcId"}
		opts.CategoricalComponentAttrs = []string{"Cmd"}
	case shapeAllComponentOnly:
		opts.ValueAttrs = []string{"JobStatus"}
		opts.ValueComponentAttrs = []string{"ClusterId", "ProcId"}
		opts.CategoricalComponentAttrs = []string{"Owner", "Cmd"}
	}
	opts.CompositeAttrs = [][]string{{"ClusterId", "ProcId"}, {"Owner", "JobStatus"}, {"Owner", "Cmd"}}
	return opts
}

func loadComponentOnly(t *testing.T, shape componentOnlyShape) *Collection {
	t.Helper()
	c := New(componentOnlyOptions(shape))
	for i, src := range compositeTestAds() {
		ad, err := classad.ParseOld(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	return c
}

// componentOnlyConstraints extends the composite cases with the ones a component-only
// declaration changes the answer-path for: the component named on its own, in a range, in a
// `!=`, and in a disjunct of its own. Each carries the row count the unindexed oracle must
// produce, so a case cannot quietly go vacuous.
func componentOnlyConstraints() []compositeCase {
	return append(compositeConstraints(),
		compositeCase{`ProcId == 3`, 40},       // the component alone: a scan, and it must still be right
		compositeCase{`ProcId == 0`, 45},       // ... including the -0.0 and the key-encoding records
		compositeCase{`ProcId >= 3`, 81},       // a range on a component-only attribute
		compositeCase{`ProcId != 1`, 168},      // a complement on one
		compositeCase{`ProcId == 11`, 1},       // only the exceptional record evaluates to it
		compositeCase{`Cmd == "c"`, 1},         // the categorical component alone
		compositeCase{`Cmd =?= "c"`, 1},        // ... case-sensitively
		compositeCase{`Cmd is undefined`, 206}, // a presence probe on one
		compositeCase{`ProcId == 3 || Owner == "ab"`, 42},
		compositeCase{`(ClusterId == 7 && ProcId == 3) || ProcId == 4`, 41},
		compositeCase{`ClusterId == 7 && ProcId == 3 && Owner == "user0"`, 1},
		compositeCase{`ClusterId == 7 && (ProcId == 1 || ProcId == 2)`, 3},
	)
}

// The headline guarantee: a component-only composite returns exactly the rows the scan does.
// Measured against the unindexed store, which has no index to be wrong, and against the
// fully-indexed one, which is what a composite was before.
func TestComponentOnlyReturnsSameRowsAsUnindexed(t *testing.T) {
	oracle := loadComponentOnly(t, shapeUnindexed)
	shapes := []componentOnlyShape{shapeFullyIndexed, shapeComponentOnly, shapeAllComponentOnly}
	stores := make(map[componentOnlyShape]*Collection, len(shapes))
	for _, s := range shapes {
		stores[s] = loadComponentOnly(t, s)
	}
	for _, cc := range componentOnlyConstraints() {
		want := queryKeys(t, oracle, cc.constraint)
		if len(want) != cc.want {
			t.Fatalf("%s: the scan returned %d rows, want %d -- the fixture no longer exercises this case",
				cc.constraint, len(want), cc.want)
		}
		for _, s := range shapes {
			got := queryKeys(t, stores[s], cc.constraint)
			if fmt.Sprint(want) != fmt.Sprint(got) {
				t.Fatalf("%s [%s]: returned %v, want %v", cc.constraint, s, got, want)
			}
		}
	}
}

// The same differential over the SEALED tier: once the segments are on disk the composite is
// read from the sidecar's byte-sorted key run and the components' postings are not merely
// unused but absent from the file. The sidecar's composite directory already stores each
// component's id and kind, so a component-only composite round-trips with no format change.
func TestComponentOnlySameRowsAfterSealAndReopen(t *testing.T) {
	load := func(shape componentOnlyShape) *Collection {
		dir := t.TempDir()
		opts := componentOnlyOptions(shape)
		opts.Dir, opts.Shards, opts.SegmentSize = dir, 1, 1<<13
		c, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		for i, src := range compositeTestAds() {
			ad, perr := classad.ParseOld(src)
			if perr != nil {
				t.Fatal(perr)
			}
			if perr = c.Put([]byte(fmt.Sprintf("k%d", i)), ad); perr != nil {
				t.Fatal(perr)
			}
		}
		c.Reindex()
		if err = c.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		reopened.Reindex()
		return reopened
	}
	oracle := load(shapeUnindexed)
	sealed := 0
	for _, sh := range oracle.shards {
		sh.mu.RLock()
		sealed += len(sh.segs)
		sh.mu.RUnlock()
	}
	if sealed < 2 {
		t.Fatalf("the fixture produced %d segments: nothing sealed, so the sidecar tier is untested", sealed)
	}
	comp := load(shapeComponentOnly)
	for _, cc := range componentOnlyConstraints() {
		want := queryKeys(t, oracle, cc.constraint)
		if len(want) != cc.want {
			t.Fatalf("%s: the scan returned %d rows, want %d", cc.constraint, len(want), cc.want)
		}
		if got := queryKeys(t, comp, cc.constraint); fmt.Sprint(want) != fmt.Sprint(got) {
			t.Fatalf("%s: sealed component-only composite returned %v, want %v", cc.constraint, got, want)
		}
	}
}

// Without this the differential tests above are vacuous: two configurations that never reach
// the composite agree about everything. The composite must actually be TAKEN, and it must
// hand the scan fewer records than the indexed component alone does.
func TestComponentOnlyCompositeIsActuallyUsed(t *testing.T) {
	load := func(componentOnly bool) *Collection {
		opts := Options{Shards: 4, ValueAttrs: []string{"ClusterId"}}
		if componentOnly {
			opts.ValueComponentAttrs = []string{"ProcId"}
			opts.CompositeAttrs = [][]string{{"ClusterId", "ProcId"}}
		}
		c := New(opts)
		for cl := range 2000 {
			for p := range 10 {
				ad, err := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nProcId = %d\n", cl, p))
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Put([]byte(fmt.Sprintf("%d.%d", cl, p)), ad); err != nil {
					t.Fatal(err)
				}
			}
		}
		c.Reindex()
		return c
	}
	visited := func(c *Collection, constraint string) (int, int, QueryExplain) {
		q, err := vm.Parse(constraint)
		if err != nil {
			t.Fatal(err)
		}
		var st ScanStats
		rows := 0
		for range c.QueryProjectStats(q, []string{"ClusterId"}, &st) {
			rows++
		}
		return rows, st.RecordsVisited, c.ExplainQuery(q)
	}
	const constraint = `ClusterId == 1234 && ProcId == 3`
	baseRows, base, _ := visited(load(false), constraint)
	comp := load(true)
	rows, with, ex := visited(comp, constraint)
	if baseRows != 1 || rows != 1 {
		t.Fatalf("%d rows without the composite and %d with it, want 1 and 1", baseRows, rows)
	}
	if base != 10 {
		t.Fatalf("ClusterId's posting alone handed the scan %d records, want 10 -- the fixture no longer forces an intersection", base)
	}
	if with != 1 {
		t.Fatalf("the component-only composite handed the scan %d records for a unique tuple, want 1", with)
	}
	if ex.CompositeSegments == 0 {
		t.Fatalf("no segment answered from a composite: %+v", ex)
	}
	if ex.CompositeIndex != "ClusterId+ProcId" {
		t.Fatalf("explain names the composite %q, want ClusterId+ProcId", ex.CompositeIndex)
	}
	// And the component-only attribute buys nothing on its own: the same store, asked for
	// ProcId alone, has no index to narrow with and visits every record.
	aloneRows, alone, aloneEx := visited(comp, `ProcId == 3`)
	if aloneRows != 2000 {
		t.Fatalf("`ProcId == 3` returned %d rows, want 2000", aloneRows)
	}
	if alone != 20000 {
		t.Fatalf("`ProcId == 3` visited %d records, want all 20000: a component-only attribute has no index of its own to narrow with", alone)
	}
	if aloneEx.CompositeSegments != 0 || aloneEx.IndexSegments != 0 {
		t.Fatalf("`ProcId == 3` reported %d composite and %d indexed segments, want 0 and 0",
			aloneEx.CompositeSegments, aloneEx.IndexSegments)
	}
}

// IndexedAttrs decides, for its callers, whether a constraint on an attribute can be
// index-answered. For a component-only attribute it cannot, so the attribute must not appear.
func TestComponentOnlyNotReportedAsIndexed(t *testing.T) {
	c := New(componentOnlyOptions(shapeComponentOnly))
	cat, val := c.IndexedAttrs()
	for _, n := range append(append([]string(nil), cat...), val...) {
		if n == "ProcId" || n == "Cmd" {
			t.Fatalf("IndexedAttrs reported the component-only attribute %q: categorical=%v value=%v", n, cat, val)
		}
	}
	// The precondition: the attributes that ARE indexed are reported, so the assertion
	// above is about the component-only ones and not about an empty list.
	if len(cat) == 0 || len(val) == 0 {
		t.Fatalf("IndexedAttrs reported categorical=%v value=%v: nothing to distinguish", cat, val)
	}
}

// A component-only probe must never become part of the plan when no composite can consume
// it: there are no postings to answer it from, and the planner drops it rather than carry it
// into the single-attribute machinery.
func TestComponentOnlyProbeDroppedWhenNoCompositeIsPinned(t *testing.T) {
	c := loadComponentOnly(t, shapeComponentOnly)
	plan := func(constraint string) []usableProbe {
		q, err := vm.Parse(constraint)
		if err != nil {
			t.Fatal(err)
		}
		return c.planIndex(q.Probes())
	}
	// The precondition: with the tuple pinned, the component-only probe IS planned.
	full := plan(`ClusterId == 7 && ProcId == 3`)
	if len(full) != 2 {
		t.Fatalf("`ClusterId == 7 && ProcId == 3` planned %d probes, want 2", len(full))
	}
	found := false
	for _, up := range full {
		if up.compOnly {
			found = true
		}
	}
	if !found {
		t.Fatalf("no component-only probe in %v: the drop assertion below would be vacuous", full)
	}
	for _, constraint := range []string{`ProcId == 3`, `ProcId >= 3`, `ProcId != 3`, `ProcId == 3 && JobStatus == 2`} {
		for _, up := range plan(constraint) {
			if up.compOnly {
				t.Fatalf("%s: planned a component-only probe with no composite pinned", constraint)
			}
		}
	}
}

// The segment-local index primitives, asked directly. These are the paths that would read an
// absent posting map as "no record matches"; each must instead say it knows nothing.
func TestComponentOnlySingleAttrPathsDecline(t *testing.T) {
	c := loadComponentOnly(t, shapeComponentOnly)
	spec := c.spec.Load()
	cid, ok := c.intern.LookupID("clusterid")
	if !ok {
		t.Fatal("ClusterId is not interned")
	}
	pid, ok := c.intern.LookupID("procid")
	if !ok {
		t.Fatal("ProcId is not interned")
	}
	if _, isCompOnly := spec.compOnlyHas(pid); !isCompOnly {
		t.Fatal("ProcId is not registered component-only")
	}
	indexed := usableProbe{attrID: cid, op: "==", fvals: []float64{7}}
	component := usableProbe{attrID: pid, op: "==", fvals: []float64{3}, compOnly: true}

	var checked, narrowed int
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		for _, w := range wins {
			si := w.seg.idx.Load()
			if si == nil {
				continue
			}
			checked++
			// The precondition for every assertion below: the INDEXED probe is served, so
			// a decline is about the component-only flag and not about an empty segment.
			if !si.coversProbe(indexed) {
				t.Fatalf("the segment does not index ClusterId: nothing below distinguishes anything")
			}
			if si.statsFor(indexed) == nil {
				t.Fatalf("no stats for the indexed probe")
			}
			if si.coversProbe(component) {
				t.Fatalf("coversProbe claims the component-only attribute is indexed")
			}
			if si.statsFor(component) != nil {
				t.Fatalf("statsFor returned a summary for an attribute with no postings")
			}
			if si.canSkip(component) {
				t.Fatalf("canSkip proved a segment empty from postings that do not exist")
			}
			if si.bloomAbsent(component) {
				t.Fatalf("bloomAbsent proved a value absent from a bloom that does not exist")
			}
			if est := si.estCandidates(component); est != math.MaxFloat64 {
				t.Fatalf("estCandidates returned %v for a component-only probe, want MaxFloat64", est)
			}
			// candidateOffsets with nothing but a component-only probe must answer "every
			// covered record", never "no record".
			got := si.candidateOffsets([]usableProbe{component})
			if got == nil || !got.Equals(si.allOffsets()) {
				t.Fatalf("candidateOffsets for a lone component-only probe returned %v, want every covered record", got)
			}
			// A lone component-only probe is never covered: no composite can be pinned by
			// one probe, so there is nothing in this index that could answer it.
			if si.covers([]usableProbe{component}) {
				t.Fatalf("covers() admitted an index that can narrow nothing")
			}
			// The precondition: the conjunction that DOES pin the tuple is covered, so the
			// decline above is about the lone probe and not about component-only as such.
			if !si.covers([]usableProbe{indexed, component}) {
				t.Fatalf("covers() declined a conjunction whose indexed component it holds")
			}
			// With the composite postings taken away -- the state a segment indexed before
			// the composite was configured is in -- the conjunction must come back exactly
			// as ClusterId's postings alone do. Anything narrower would be the component's
			// absent postings being read as an answer.
			stripped := &segIndex{upto: si.upto, specGen: si.specGen, all: si.all, cat: si.cat, val: si.val}
			mixed := stripped.candidateOffsets([]usableProbe{indexed, component})
			only := stripped.candidateOffsets([]usableProbe{indexed})
			if mixed == nil || only == nil {
				t.Fatalf("candidateOffsets returned nil for a conjunction the index covers")
			}
			if !mixed.Equals(only) {
				t.Fatalf("the component-only probe narrowed %d candidates to %d with no postings to do it from",
					only.GetCardinality(), mixed.GetCardinality())
			}
			if only.GetCardinality() > 0 {
				narrowed++
			}
		}
		releaseWindows(wins)
	}
	if checked == 0 {
		t.Fatal("no built segment index was examined")
	}
	if narrowed == 0 {
		t.Fatal("ClusterId's postings matched nothing anywhere: the comparison above is vacuous")
	}
}

// A segment indexed BEFORE the composite was configured holds no composite postings at all,
// so the conjunction has nothing but the composite's absent postings to go on -- and the rows
// must still all come back. This is the state an archive sits in between adding a composite
// and the reindex that builds it, and it is the state in which a component-only probe that
// escaped into the single-attribute paths would empty the candidate set.
//
// Both shapes, because they fail differently: with one component indexed the conjunction
// still has a real posting list to narrow by, and with none it has nothing at all and the
// window has to be declined outright.
func TestComponentOnlyStaleSegmentStillReturnsEveryRow(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"one component indexed", Options{Shards: 1,
			ValueAttrs:          []string{"ClusterId"},
			ValueComponentAttrs: []string{"ProcId"}}},
		{"no component indexed", Options{Shards: 1,
			ValueAttrs:          []string{"JobStatus"},
			ValueComponentAttrs: []string{"ClusterId", "ProcId"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(tc.opts)
			for cl := range 20 {
				for p := range 5 {
					ad, err := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = 4\n", cl, p))
					if err != nil {
						t.Fatal(err)
					}
					if err := c.Put([]byte(fmt.Sprintf("%d.%d", cl, p)), ad); err != nil {
						t.Fatal(err)
					}
				}
			}
			c.Reindex() // built WITHOUT the composite
			if !c.addComposite(t, "ClusterId", "ProcId") {
				t.Fatal("the composite was not configured")
			}
			// Deliberately NOT reindexed. catchUpIndexes leaves an index built under an
			// older generation alone (catchUpDue), so the segments stay stale through the
			// queries below -- asserted here, because a rebuilt index would make every
			// assertion that follows a test of the ordinary path.
			stale := 0
			for _, sh := range c.shards {
				_, wins := sh.snapshot()
				for _, w := range wins {
					if si := w.seg.idx.Load(); si != nil && len(si.comp) == 0 {
						stale++
					}
				}
				releaseWindows(wins)
			}
			if stale == 0 {
				t.Fatal("every segment already holds the composite: the stale path is untested")
			}
			for _, cc := range []compositeCase{
				{`ClusterId == 7 && ProcId == 3`, 1},
				{`ClusterId == 7 && ProcId == 3 && JobStatus == 4`, 1},
				{`ClusterId == 7`, 5},
				{`ProcId == 3`, 20},
			} {
				if got := queryKeys(t, c, cc.constraint); len(got) != cc.want {
					t.Fatalf("%s returned %d rows on a stale index, want %d", cc.constraint, len(got), cc.want)
				}
			}
			after := 0
			for _, sh := range c.shards {
				_, wins := sh.snapshot()
				for _, w := range wins {
					if si := w.seg.idx.Load(); si != nil && len(si.comp) == 0 {
						after++
					}
				}
				releaseWindows(wins)
			}
			if after != stale {
				t.Fatalf("%d stale segments before the queries and %d after: the index was rebuilt under us", stale, after)
			}
			// And after the reindex the composite takes over without changing the answers.
			c.Reindex()
			if got := queryKeys(t, c, `ClusterId == 7 && ProcId == 3`); len(got) != 1 {
				t.Fatalf("after reindex: %d rows, want 1", len(got))
			}
		})
	}
}

// addComposite installs a composite over the named attributes at runtime (the auto path,
// which is the only runtime one), and reports whether it is now configured.
func (c *Collection) addComposite(t *testing.T, names ...string) bool {
	t.Helper()
	c.addCompositesAuto([][]string{names})
	ids := make([]uint32, 0, len(names))
	for _, n := range names {
		id, ok := c.spec.Load().resolveID(c.intern, n)
		if !ok {
			return false
		}
		ids = append(ids, id)
	}
	_, ok := c.spec.Load().compositeFor(compositeKey(ids))
	return ok
}

// A tuple naming an attribute the spec gives no kind to -- neither indexed nor declared a
// component -- is still refused. The relaxation is about declared components, not about
// composites conjuring an index configuration out of a tuple.
func TestComponentOnlyTupleStillRefusedWithoutADeclaration(t *testing.T) {
	refused := New(Options{Shards: 1,
		ValueAttrs:     []string{"ClusterId"},
		CompositeAttrs: [][]string{{"ClusterId", "ProcId"}}})
	if got := len(refused.spec.Load().comps); got != 0 {
		t.Fatalf("%d composites over an undeclared component, want 0", got)
	}
	// The precondition: the identical tuple IS accepted once ProcId is declared, so the
	// refusal above is about the declaration and not about the tuple.
	accepted := New(Options{Shards: 1,
		ValueAttrs:          []string{"ClusterId"},
		ValueComponentAttrs: []string{"ProcId"},
		CompositeAttrs:      [][]string{{"ClusterId", "ProcId"}}})
	if got := len(accepted.spec.Load().comps); got != 1 {
		t.Fatalf("%d composites over a declared component, want 1", got)
	}
	// Duplicates and widths are refused as before.
	bad := New(Options{Shards: 1,
		ValueAttrs:          []string{"ClusterId"},
		ValueComponentAttrs: []string{"ProcId"},
		CompositeAttrs:      [][]string{{"ProcId", "ProcId"}, {"ProcId"}}})
	if got := len(bad.spec.Load().comps); got != 0 {
		t.Fatalf("%d composites over a duplicate/narrow tuple, want 0", got)
	}
}

// The component's KIND comes from which list declares it, and nothing else: the record side
// and the probe side must fold a component identically, so the kind is a property of the
// configuration and never of the values observed.
func TestComponentOnlyKindComesFromTheDeclaration(t *testing.T) {
	kinds := map[string]bool{}
	for _, tc := range []struct {
		name string
		opts Options
		want bool
	}{
		{"value", Options{Shards: 1,
			CategoricalAttrs:    []string{"Owner"},
			ValueComponentAttrs: []string{"ProcId"},
			CompositeAttrs:      [][]string{{"Owner", "ProcId"}}}, false},
		{"categorical", Options{Shards: 1,
			CategoricalAttrs:          []string{"Owner"},
			CategoricalComponentAttrs: []string{"ProcId"},
			CompositeAttrs:            [][]string{{"Owner", "ProcId"}}}, true},
	} {
		c := New(tc.opts)
		spec := c.spec.Load()
		if len(spec.comps) != 1 {
			t.Fatalf("%s: %d composites, want 1", tc.name, len(spec.comps))
		}
		cs := spec.comps[0]
		if got := cs.cats[1]; got != tc.want {
			t.Fatalf("%s: ProcId planned as categorical=%v, want %v", tc.name, got, tc.want)
		}
		kinds[tc.name] = cs.cats[1]
		// The spec generation has to separate the two: a sidecar built under one kind folds
		// every record the other way, and must not read as current under the other.
		if _, dup := kinds[fmt.Sprint(spec.gen)]; dup {
			t.Fatalf("%s: the two kinds share a spec generation", tc.name)
		}
		kinds[fmt.Sprint(spec.gen)] = true
	}
	// Declared as both: categorical wins, matching AddIndex's rule.
	both := New(Options{Shards: 1,
		CategoricalAttrs:          []string{"Owner"},
		CategoricalComponentAttrs: []string{"ProcId"},
		ValueComponentAttrs:       []string{"ProcId"},
		CompositeAttrs:            [][]string{{"Owner", "ProcId"}}})
	spec := both.spec.Load()
	if len(spec.comps) != 1 || !spec.comps[0].cats[1] {
		t.Fatalf("declared as both kinds: %+v, want one composite with a categorical component", spec.comps)
	}
}

// A configuration with no component-only attribute must sign exactly as it did before they
// existed, or every sidecar on disk reads as stale and a whole archive is re-indexed for a
// feature it does not use.
func TestComponentOnlyDoesNotMoveAnExistingSignature(t *testing.T) {
	base := New(Options{Shards: 1,
		ValueAttrs:       []string{"ClusterId", "ProcId"},
		CategoricalAttrs: []string{"Owner"},
		CompositeAttrs:   [][]string{{"ClusterId", "ProcId"}}})
	spec := base.spec.Load()
	bare := &indexSpec{cat: spec.cat, val: spec.val, catIDs: spec.catIDs, valIDs: spec.valIDs, comps: spec.comps}
	if got, want := spec.signature(), bare.signature(); got != want {
		t.Fatalf("a spec with no component-only attribute signs %d, want %d", got, want)
	}
	// And a component-only declaration DOES move it, so the two configurations cannot share
	// a sidecar.
	withComp := New(Options{Shards: 1,
		ValueAttrs:          []string{"ClusterId"},
		CategoricalAttrs:    []string{"Owner"},
		ValueComponentAttrs: []string{"ProcId"},
		CompositeAttrs:      [][]string{{"ClusterId", "ProcId"}}})
	if withComp.spec.Load().gen == spec.gen {
		t.Fatal("a component-only declaration did not move the spec generation")
	}
}

// Dropping the attribute drops the composite with it, exactly as dropping a real component
// index does -- and promoting it to a real index re-kinds the composite, which is the one
// change pruneComposites exists to catch.
func TestComponentOnlyDroppedWithItsDeclaration(t *testing.T) {
	newStore := func() *Collection {
		return New(Options{Shards: 1,
			ValueAttrs:          []string{"ClusterId"},
			ValueComponentAttrs: []string{"ProcId"},
			CompositeAttrs:      [][]string{{"ClusterId", "ProcId"}}})
	}
	dropped := newStore()
	if got := len(dropped.spec.Load().comps); got != 1 {
		t.Fatalf("%d composites, want 1", got)
	}
	if !dropped.DropIndex("ProcId") {
		t.Fatal("DropIndex on a component-only attribute reported no change")
	}
	if got := len(dropped.spec.Load().comps); got != 0 {
		t.Fatalf("%d composites after dropping the component declaration, want 0", got)
	}
	if _, ok := dropped.spec.Load().compOnlyHas(1); ok {
		t.Fatal("a component-only registration survived DropIndex")
	}

	rekinded := newStore()
	if !rekinded.AddIndex([]string{"ProcId"}, nil) { // ProcId becomes a real CATEGORICAL index
		t.Fatal("AddIndex reported no change")
	}
	if got := len(rekinded.spec.Load().comps); got != 0 {
		t.Fatalf("%d composites after a component changed kind, want 0", got)
	}
	cat, _ := rekinded.IndexedAttrs()
	if len(cat) != 1 || cat[0] != "ProcId" {
		t.Fatalf("IndexedAttrs reports categorical=%v, want [ProcId] now that it has an index of its own", cat)
	}
	rekindedSpec := rekinded.spec.Load()
	rid, ok := rekindedSpec.resolveID(rekinded.intern, "ProcId")
	if !ok {
		t.Fatal("ProcId does not resolve")
	}
	if _, stillComponent := rekindedSpec.compOnlyHas(rid); stillComponent {
		t.Fatal("the component-only declaration survived the attribute gaining a categorical index")
	}

	promoted := newStore()
	if !promoted.AddIndex(nil, []string{"ProcId"}) { // same kind: the composite survives
		t.Fatal("AddIndex reported no change")
	}
	promotedSpec := promoted.spec.Load()
	if got := len(promotedSpec.comps); got != 1 {
		t.Fatalf("%d composites after promoting a component to a real index of the same kind, want 1", got)
	}
	_, val := promoted.IndexedAttrs()
	if len(val) != 2 {
		t.Fatalf("IndexedAttrs reports value=%v, want both attributes", val)
	}
	// The declaration is gone: the attribute's own index settles its kind now, and leaving
	// the declaration behind would leave two answers to that question in the spec.
	pid, ok := promotedSpec.resolveID(promoted.intern, "ProcId")
	if !ok {
		t.Fatal("ProcId does not resolve")
	}
	if _, stillComponent := promotedSpec.compOnlyHas(pid); stillComponent {
		t.Fatal("the component-only declaration survived the attribute gaining its own index")
	}
}

// The auto-tuner's components-first rule is unchanged: it suggests a composite only over
// attributes indexed in their own right, so a component-only declaration never arrives by
// accident. Declaring one is a deliberate act and stays one.
func TestComponentOnlyIsNotSuggestedByTheAutoTuner(t *testing.T) {
	c := New(Options{Shards: 1,
		ValueAttrs:          []string{"ClusterId"},
		ValueComponentAttrs: []string{"ProcId"},
		CategoricalAttrs:    []string{"Owner", "Cmd"}})
	for i := range 50 {
		ad, err := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nProcId = 0\nOwner = \"u\"\nCmd = \"c\"\n", i))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprint(i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	for range 30 {
		for _, constraint := range []string{`ClusterId == 1 && ProcId == 0`, `Owner == "u" && Cmd == "c"`} {
			q, err := vm.Parse(constraint)
			if err != nil {
				t.Fatal(err)
			}
			for range c.Query(q) { //nolint:revive // the demand is the point, not the rows
			}
		}
	}
	got := c.SuggestComposites()
	// The precondition: the tuple whose components ARE both indexed is suggested, so the
	// absence below is about the component-only declaration.
	var sawIndexed bool
	for _, s := range got {
		label := fmt.Sprint(s.Attrs)
		if label == "[Cmd Owner]" || label == "[Owner Cmd]" {
			sawIndexed = true
		}
		for _, a := range s.Attrs {
			if a == "ProcId" {
				t.Fatalf("SuggestComposites suggested a tuple over the component-only attribute: %v", got)
			}
		}
	}
	if !sawIndexed {
		t.Fatalf("SuggestComposites suggested %v: the fully-indexed tuple is missing, so the assertion above is vacuous", got)
	}
}

// Deltas are decided against the segment indexes, one probe at a time: a delta record posts
// only what its write changed, so whether it inherits a matching value is settled by reading
// the components' postings for its base. A component-only probe has no postings for that to
// read, so every inheriting delta stays admitted -- a superset, re-verified, never a row
// short. Skipping the probe is what keeps that path from asking for postings that are not
// there.
func TestComponentOnlyWithDeltaRecords(t *testing.T) {
	load := func(componentOnly bool) *Collection {
		opts := Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 14, DeltaMax: 16,
			ValueAttrs: []string{"ClusterId", "JobStatus"}}
		if componentOnly {
			opts.ValueComponentAttrs = []string{"ProcId"}
			opts.CompositeAttrs = [][]string{{"ClusterId", "ProcId"}}
		}
		c, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		for cl := range 40 {
			for p := range 4 {
				ad, perr := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = 1\n", cl, p))
				if perr != nil {
					t.Fatal(perr)
				}
				if perr = c.Put([]byte(fmt.Sprintf("%d.%d", cl, p)), ad); perr != nil {
					t.Fatal(perr)
				}
			}
		}
		// Patch one attribute of every record: each write stores a delta that inherits
		// ClusterId and ProcId from its base. Reindexed after each round, so the deltas sit
		// inside a covered prefix rather than in the tail every walk reads in full.
		for round := 1; round <= 3; round++ {
			c.Reindex()
			for cl := range 40 {
				for p := range 4 {
					patch := classad.New()
					patch.InsertAttr("JobStatus", int64(round+1))
					w := c.Begin()
					w.PatchAttrs([]byte(fmt.Sprintf("%d.%d", cl, p)), patch, nil)
					if r := w.Commit(); r.Conflicted() {
						t.Fatalf("round %d cluster %d proc %d conflicted", round, cl, p)
					}
				}
			}
		}
		c.Reindex()
		if d, _ := c.DeltaStats(); d == 0 {
			t.Fatal("no deltas written: the test is not exercising what it claims")
		}
		return c
	}
	oracle, comp := load(false), load(true)
	for _, constraint := range []string{
		`ClusterId == 7 && ProcId == 2`,
		`ClusterId == 7 && ProcId == 2 && JobStatus == 4`,
		`ClusterId == 7 && ProcId == 99`,
		`ClusterId == 7`,
		`ProcId == 2`,
	} {
		want := queryKeys(t, oracle, constraint)
		if len(want) == 0 && constraint != `ClusterId == 7 && ProcId == 99` {
			t.Fatalf("%s: the oracle returned nothing, so the comparison is vacuous", constraint)
		}
		if got := queryKeys(t, comp, constraint); fmt.Sprint(want) != fmt.Sprint(got) {
			t.Fatalf("%s with deltas: component-only returned %v, want %v", constraint, got, want)
		}
	}
}

// A randomized differential over the tuple space, because the hand-written constraints above
// can only cover the shapes someone thought of. Every conjunction, every component alone, and
// every disjunction of two of them must agree with the scan.
func TestComponentOnlyDifferentialOverTheTupleSpace(t *testing.T) {
	oracle := loadComponentOnly(t, shapeUnindexed)
	comp := loadComponentOnly(t, shapeComponentOnly)
	all := loadComponentOnly(t, shapeAllComponentOnly)
	var constraints []string
	for cl := -3; cl <= 8; cl++ {
		for p := -1; p <= 4; p++ {
			constraints = append(constraints,
				fmt.Sprintf("ClusterId == %d && ProcId == %d", cl, p),
				fmt.Sprintf("ProcId == %d && ClusterId == %d", p, cl),
				fmt.Sprintf("ClusterId == %d && ProcId != %d", cl, p),
				fmt.Sprintf("ProcId == %d", p),
				fmt.Sprintf("(ClusterId == %d && ProcId == %d) || (ClusterId == %d && ProcId == %d)", cl, p, cl+1, p),
				fmt.Sprintf("ClusterId == %d && ProcId == %d && Owner == \"user0\"", cl, p),
			)
		}
	}
	matched := 0
	for _, constraint := range constraints {
		want := queryKeys(t, oracle, constraint)
		matched += len(want)
		for name, c := range map[string]*Collection{"component-only": comp, "all-component-only": all} {
			if got := queryKeys(t, c, constraint); fmt.Sprint(want) != fmt.Sprint(got) {
				t.Fatalf("%s [%s]: returned %v, want %v", constraint, name, got, want)
			}
		}
	}
	if matched == 0 {
		t.Fatalf("%d constraints matched nothing at all: the differential proves nothing", len(constraints))
	}
	t.Logf("%d constraints, %d matching rows in total", len(constraints), matched)
}

// The shared planner logic must never answer "no candidates" for a conjunction it could not
// narrow. Asked with nothing but component-only probes -- which indexCovers keeps out of the
// indexed walk, so this is the layer below that -- it answers with every covered record.
//
// The fixture is deliberately clean (no record whose component the index cannot classify), so
// an empty answer from the built composite is EVIDENCE that nothing posted the tuple, not an
// exception set standing in for one.
func TestComponentOnlyCandidateOffsetsNeverEmptyWithoutEvidence(t *testing.T) {
	c := New(Options{Shards: 1,
		ValueComponentAttrs: []string{"ClusterId", "ProcId"},
		CompositeAttrs:      [][]string{{"ClusterId", "ProcId"}}})
	for cl := range 20 {
		for p := range 5 {
			ad, err := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nProcId = %d\n", cl, p))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Put([]byte(fmt.Sprintf("%d.%d", cl, p)), ad); err != nil {
				t.Fatal(err)
			}
		}
	}
	c.Reindex()
	spec := c.spec.Load()
	cid, _ := c.intern.LookupID("clusterid")
	pid, _ := c.intern.LookupID("procid")
	if _, ok := spec.compOnlyHas(cid); !ok {
		t.Fatal("ClusterId is not component-only in this shape")
	}
	absent := []usableProbe{
		{attrID: cid, op: "==", fvals: []float64{99999}, compOnly: true},
		{attrID: pid, op: "==", fvals: []float64{99999}, compOnly: true},
	}
	present := []usableProbe{
		{attrID: cid, op: "==", fvals: []float64{7}, compOnly: true},
		{attrID: pid, op: "==", fvals: []float64{3}, compOnly: true},
	}
	seen := 0
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		for _, w := range wins {
			si := w.seg.idx.Load()
			if si == nil {
				continue
			}
			seen++
			if len(si.comp) == 0 {
				t.Fatal("the segment holds no composite: the fixture is not exercising the built case")
			}
			// The precondition: a tuple that IS posted comes back, so the empty answer
			// below is the composite deciding and not the composite failing.
			if got := si.candidateOffsets(present); got == nil || got.GetCardinality() != 1 {
				t.Fatalf("a posted tuple returned %v, want exactly one record", got)
			}
			if got := si.candidateOffsets(absent); got == nil || !got.IsEmpty() {
				t.Fatalf("a tuple nothing posted returned %v, want an empty candidate set", got)
			}
			// Strip the composite postings and the same question must answer "everything":
			// without them nothing in this index knows anything about either attribute.
			stripped := &segIndex{upto: si.upto, specGen: si.specGen, all: si.all, cat: si.cat, val: si.val}
			got := stripped.candidateOffsets(absent)
			if got == nil || !got.Equals(si.all) {
				var n uint64
				if got != nil {
					n = got.GetCardinality()
				}
				t.Fatalf("without the composite, %d candidates, want all %d records", n, si.all.GetCardinality())
			}
			if stripped.covers(absent) {
				t.Fatal("covers() admitted an index that can narrow nothing")
			}
		}
		releaseWindows(wins)
	}
	if seen == 0 {
		t.Fatal("no built segment index was examined")
	}
}

// compositeTupleKey is the one place a component's kind is consumed on the probe side. A
// probe planned in the other kind must not pin the tuple -- which is what keeps a sidecar
// written under one kind from being read under the other.
func TestComponentOnlyProbeKindMustMatchTheSpec(t *testing.T) {
	cs := compositeSpec{ids: []uint32{10, 11}, cats: []bool{false, false}}
	cs.key = compositeKey(cs.ids)
	right := []usableProbe{
		{attrID: 10, op: "==", fvals: []float64{1}},
		{attrID: 11, op: "==", fvals: []float64{2}, compOnly: true},
	}
	if _, _, ok := compositeTupleKey(cs, right); !ok {
		t.Fatal("the tuple was not pinned by probes of the configured kinds")
	}
	wrong := []usableProbe{
		{attrID: 10, op: "==", fvals: []float64{1}},
		{attrID: 11, cat: true, op: "==", svals: []string{"2"}, compOnly: true},
	}
	if _, _, ok := compositeTupleKey(cs, wrong); ok {
		t.Fatal("a categorical probe pinned a value component")
	}
}

// probeOffsets is the single-attribute path that would answer a component-only probe from
// postings that do not exist, and the empty bitmap it would otherwise build is the shape of
// the wrong answer: intersected into a candidate set it says "no record matches". Both tiers
// answer with the whole covered prefix instead. Asserted directly, because the planner is
// written never to ask -- so without this the guard would be untested code.
func TestComponentOnlyProbeOffsetsNeverAnswersEmpty(t *testing.T) {
	dir := t.TempDir()
	opts := componentOnlyOptions(shapeComponentOnly)
	opts.Dir, opts.Shards, opts.SegmentSize = dir, 1, 1<<13
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	for i, src := range compositeTestAds() {
		ad, perr := classad.ParseOld(src)
		if perr != nil {
			t.Fatal(perr)
		}
		if perr = c.Put([]byte(fmt.Sprintf("k%d", i)), ad); perr != nil {
			t.Fatal(perr)
		}
	}
	c.Reindex()
	spec := c.spec.Load()
	pid, ok := spec.nameToID["procid"]
	if !ok {
		t.Fatal("ProcId is not in the inline id space")
	}
	cid, ok := spec.nameToID["clusterid"]
	if !ok {
		t.Fatal("ClusterId is not in the inline id space")
	}
	component := usableProbe{attrID: pid, op: "==", fvals: []float64{3}, compOnly: true}
	indexed := usableProbe{attrID: cid, op: "==", fvals: []float64{7}}
	var ram, mmapped int
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		for _, w := range wins {
			var ix indexPrimitives
			if m := w.seg.msidx.Load(); m != nil {
				ix, mmapped = m, mmapped+1
			} else if si := w.seg.idx.Load(); si != nil {
				ix, ram = si, ram+1
			} else {
				continue
			}
			all := ix.allOffsets()
			if all.IsEmpty() {
				continue // an empty segment proves nothing either way
			}
			// The precondition: the INDEXED probe is answered from real postings, so the
			// answer below is about the component-only flag.
			if got := ix.probeOffsets(indexed); got.Equals(all) {
				t.Fatalf("the indexed probe matched every record: nothing here distinguishes a real posting from a missing one")
			}
			if got := ix.probeOffsets(component); !got.Equals(all) {
				t.Fatalf("probeOffsets answered a component-only probe with %d of %d records: postings that do not exist were read as an answer",
					got.GetCardinality(), all.GetCardinality())
			}
		}
		releaseWindows(wins)
	}
	if ram == 0 && mmapped == 0 {
		t.Fatal("no segment index was examined")
	}
	if mmapped == 0 {
		t.Fatal("nothing sealed to a sidecar: the mmap tier is untested")
	}
}

// A composite every one of whose components is component-only has no attribute with an index
// of its own anywhere, so the spec's "is anything indexed" gate has to count the composite
// itself -- otherwise the planner declines the query before the composite is offered it.
func TestComponentOnlyAllComponentsCompositeIsUsed(t *testing.T) {
	c := New(Options{Shards: 1,
		ValueComponentAttrs: []string{"ClusterId", "ProcId"},
		CompositeAttrs:      [][]string{{"ClusterId", "ProcId"}}})
	for cl := range 500 {
		for p := range 10 {
			ad, err := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nProcId = %d\n", cl, p))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Put([]byte(fmt.Sprintf("%d.%d", cl, p)), ad); err != nil {
				t.Fatal(err)
			}
		}
	}
	c.Reindex()
	if !c.spec.Load().any() {
		t.Fatal("the spec reports nothing indexed, so no query will ever reach the composite")
	}
	q, err := vm.Parse(`ClusterId == 123 && ProcId == 4`)
	if err != nil {
		t.Fatal(err)
	}
	var st ScanStats
	rows := 0
	for range c.QueryProjectStats(q, []string{"ClusterId"}, &st) {
		rows++
	}
	if rows != 1 {
		t.Fatalf("%d rows, want 1", rows)
	}
	if st.RecordsVisited != 1 {
		t.Fatalf("visited %d records for a unique tuple, want 1: the composite was not used", st.RecordsVisited)
	}
	if ex := c.ExplainQuery(q); ex.CompositeSegments == 0 {
		t.Fatalf("no segment answered from the composite: %+v", ex)
	}
}

// An index change that touches neither component must leave the composite alone.
// pruneComposites runs on every add and drop, and a rule written only against the
// single-attribute maps would take every component-only composite with it.
func TestComponentOnlySurvivesAnUnrelatedIndexChange(t *testing.T) {
	c := New(Options{Shards: 1,
		ValueAttrs:          []string{"ClusterId"},
		ValueComponentAttrs: []string{"ProcId"},
		CompositeAttrs:      [][]string{{"ClusterId", "ProcId"}}})
	if got := len(c.spec.Load().comps); got != 1 {
		t.Fatalf("%d composites, want 1", got)
	}
	if !c.AddIndex([]string{"Owner"}, nil) {
		t.Fatal("AddIndex reported no change")
	}
	if got := len(c.spec.Load().comps); got != 1 {
		t.Fatalf("%d composites after an unrelated AddIndex, want 1", got)
	}
	if !c.DropIndex("Owner") {
		t.Fatal("DropIndex reported no change")
	}
	if got := len(c.spec.Load().comps); got != 1 {
		t.Fatalf("%d composites after an unrelated DropIndex, want 1", got)
	}
	if _, ok := c.spec.Load().compOnlyHas(mustInlineOrInternID(t, c, "ProcId")); !ok {
		t.Fatal("the component-only registration did not survive an unrelated index change")
	}
}

func mustInlineOrInternID(t *testing.T, c *Collection, name string) uint32 {
	t.Helper()
	id, ok := c.spec.Load().resolveID(c.intern, name)
	if !ok {
		t.Fatalf("%s does not resolve to an id", name)
	}
	return id
}

// The guards that say "not indexed" hold even when the postings are THERE.
//
// Today a component-only attribute is kept out of the single-attribute maps, so most of the
// guards would answer correctly from a missing map entry alone. That is not what they are
// for. They exist against the change that allocates the maps -- a future pass that builds a
// (necessarily empty) posting structure for every attribute the spec mentions -- after which
// a missing entry no longer stands in for "not indexed" and every one of these paths would
// start answering from an empty posting instead. So the state is constructed here and each
// path is asked in it.
func TestComponentOnlyGuardsHoldWithStrayPostings(t *testing.T) {
	c := loadComponentOnly(t, shapeComponentOnly)
	spec := c.spec.Load()
	pid, ok := c.intern.LookupID("procid")
	if !ok {
		t.Fatal("ProcId is not interned")
	}
	cid, ok := c.intern.LookupID("clusterid")
	if !ok {
		t.Fatal("ClusterId is not interned")
	}
	cmd, ok := c.intern.LookupID("cmd")
	if !ok {
		t.Fatal("Cmd is not interned")
	}
	if _, isCompOnly := spec.compOnlyHas(pid); !isCompOnly {
		t.Fatal("ProcId is not registered component-only")
	}
	value := usableProbe{attrID: pid, op: "==", fvals: []float64{3}, compOnly: true}
	categorical := usableProbe{attrID: cmd, cat: true, op: "==", svals: []string{"c"}, compOnly: true}
	indexed := usableProbe{attrID: cid, op: "==", fvals: []float64{7}}
	checked := 0
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		for _, w := range wins {
			si := w.seg.idx.Load()
			if si == nil || si.all.IsEmpty() {
				continue
			}
			checked++
			// The mistake this test models: empty postings allocated for the component-only
			// attributes, finished exactly as a real attribute's would be.
			stray := &segIndex{upto: si.upto, specGen: si.specGen, all: si.all, comp: si.comp,
				cat: map[uint32]*catPostings{}, val: map[uint32]*valPostings{}}
			for id, cp := range si.cat {
				stray.cat[id] = cp
			}
			for id, vp := range si.val {
				stray.val[id] = vp
			}
			stray.val[pid] = &valPostings{post: map[float64]*roaring.Bitmap{}, exc: roaring.New(), posted: roaring.New()}
			stray.cat[cmd] = &catPostings{post: map[string]*roaring.Bitmap{}, exact: map[string]*roaring.Bitmap{},
				exc: roaring.New(), posted: roaring.New()}
			stray.finish()
			// The precondition: WITHOUT the component-only flag those postings are believed,
			// and they say "no record matches". That is the wrong answer each guard below
			// exists to refuse, so it has to be reachable for the refusals to mean anything.
			for _, bare := range []usableProbe{
				{attrID: pid, op: "==", fvals: []float64{3}},
				{attrID: cmd, cat: true, op: "==", svals: []string{"c"}},
			} {
				if !stray.coversProbe(bare) {
					t.Fatalf("the stray postings were not installed: coversProbe says the attribute is not indexed")
				}
				if got := stray.probeOffsets(bare); !got.IsEmpty() {
					t.Fatalf("the stray postings are not empty (%d records): nothing here would be a wrong answer", got.GetCardinality())
				}
				if !stray.canSkip(bare) {
					t.Fatalf("canSkip did not take the bait: the stray postings do not prove the segment empty")
				}
			}
			for _, up := range []usableProbe{value, categorical} {
				if stray.coversProbe(up) {
					t.Fatalf("coversProbe believed stray postings for a component-only attribute")
				}
				if stray.statsFor(up) != nil {
					t.Fatalf("statsFor believed stray postings for a component-only attribute")
				}
				if stray.canSkip(up) {
					t.Fatalf("canSkip proved a segment empty from stray postings")
				}
				if stray.bloomAbsent(up) {
					t.Fatalf("bloomAbsent proved a value absent from a stray bloom")
				}
				if est := stray.estCandidates(up); est != math.MaxFloat64 {
					t.Fatalf("estCandidates returned %v from stray postings, want MaxFloat64", est)
				}
				if got := stray.probeOffsets(up); !got.Equals(stray.all) {
					t.Fatalf("probeOffsets answered from stray postings: %d of %d records",
						got.GetCardinality(), stray.all.GetCardinality())
				}
				if stray.skipsPrefix([]usableProbe{indexed, up}) && !si.skipsPrefix([]usableProbe{indexed, up}) {
					t.Fatalf("skipsPrefix skipped a segment the unaltered index does not")
				}
			}
			// And the candidate set is exactly what the indexed probe alone admits.
			bare := &segIndex{upto: si.upto, specGen: si.specGen, all: si.all, cat: stray.cat, val: stray.val}
			mixed := bare.candidateOffsets([]usableProbe{indexed, value})
			alone := bare.candidateOffsets([]usableProbe{indexed})
			if mixed == nil || alone == nil || !mixed.Equals(alone) {
				t.Fatalf("the component-only probe narrowed the candidate set from stray postings")
			}
		}
		releaseWindows(wins)
	}
	if checked == 0 {
		t.Fatal("no built segment index was examined")
	}
}

// compOnlyUsable plans only a probe pinning its attribute to ONE value: that is the only
// shape a composite can consume, and a component-only attribute has no posting list for any
// other shape to fall back to.
func TestComponentOnlyPlansOnlySingleValueEqualities(t *testing.T) {
	c := loadComponentOnly(t, shapeComponentOnly)
	spec := c.spec.Load()
	pid, ok := c.intern.LookupID("procid")
	if !ok {
		t.Fatal("ProcId is not interned")
	}
	probe := func(constraint string) vm.Probe {
		t.Helper()
		q, err := vm.Parse(constraint)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range q.Probes() {
			if strings.EqualFold(p.Attr, "ProcId") {
				return p
			}
		}
		t.Fatalf("%s: no ProcId probe", constraint)
		return vm.Probe{}
	}
	// The precondition: the shape a composite CAN consume is planned.
	if _, ok := compOnlyUsable(spec, pid, probe(`ProcId == 3`)); !ok {
		t.Fatal("a single-value equality on a component-only attribute was not planned")
	}
	for _, constraint := range []string{
		`ProcId >= 3`, `ProcId != 3`, `ProcId < 3`,
		`ProcId is undefined`, `ProcId isnt undefined`,
		`ProcId == 1 || ProcId == 2`,
	} {
		if up, ok := compOnlyUsable(spec, pid, probe(constraint)); ok {
			t.Fatalf("%s planned a component-only probe (op %q, %d values): a composite cannot answer it",
				constraint, up.op, len(up.fvals))
		}
	}
}

// An attribute declared BOTH indexed and component-only keeps its own index and is not
// registered as a component-only attribute: its index already settles its kind, and two
// answers to that question is the thing a fixed kind exists to prevent.
func TestComponentOnlyDoesNotShadowARealIndex(t *testing.T) {
	c := New(Options{Shards: 1,
		ValueAttrs:                []string{"ClusterId", "ProcId"},
		CategoricalComponentAttrs: []string{"ProcId"},
		CompositeAttrs:            [][]string{{"ClusterId", "ProcId"}}})
	spec := c.spec.Load()
	pid, ok := spec.resolveID(c.intern, "ProcId")
	if !ok {
		t.Fatal("ProcId does not resolve")
	}
	if !spec.valHas(pid) {
		t.Fatal("ProcId lost its own value index")
	}
	if _, isCompOnly := spec.compOnlyHas(pid); isCompOnly {
		t.Fatal("ProcId was also registered component-only, shadowing its own index")
	}
	cat, ok2 := spec.componentKind(pid)
	if !ok2 || cat {
		t.Fatalf("componentKind says categorical=%v ok=%v, want the value kind its own index fixes", cat, ok2)
	}
	if len(spec.comps) != 1 || spec.comps[0].cats[1] {
		t.Fatalf("the composite planned ProcId as categorical: %+v", spec.comps)
	}
	// And it is still reported as indexed, because it still has an index.
	_, val := c.IndexedAttrs()
	if len(val) != 2 {
		t.Fatalf("IndexedAttrs reports value=%v, want both attributes", val)
	}
}
