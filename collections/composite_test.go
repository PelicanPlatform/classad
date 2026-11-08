package collections

import (
	"fmt"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// compositeTestAds is a job-queue shape with the awkward cases mixed in: a string component,
// records missing a component, a component holding an expression rather than a literal, and
// mixed-case strings.
func compositeTestAds() []string {
	var out []string
	for cl := 1; cl <= 40; cl++ {
		for p := range 5 {
			out = append(out, fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"User%d\"\nJobStatus = %d\n",
				cl, p, cl%7, 1+(cl+p)%5))
		}
	}
	return append(out,
		// No ProcId at all: `ProcId == 0` is UNDEFINED for it, so it must never come back,
		// composite or not.
		"ClusterId = 7\nOwner = \"User0\"\nJobStatus = 2\n",
		// A ProcId that is an expression, not a literal: the index cannot classify it, so
		// it must stay a candidate and be re-verified.
		"ClusterId = 7\nProcId = 1 + 1\nOwner = \"User0\"\nJobStatus = 2\n",
		// An exceptional record whose evaluated value is a tuple NOTHING posts literally:
		// the composite's key lookup misses, and only its exception set keeps the record a
		// candidate.
		"ClusterId = 7\nProcId = 10 + 1\nOwner = \"User0\"\nJobStatus = 2\n",
		// Negative and zero components, and a differently-cased owner for the same bucket.
		"ClusterId = -3\nProcId = 0\nOwner = \"USER1\"\nJobStatus = 3\n",
		"ClusterId = 0\nProcId = -1\nOwner = \"user1\"\nJobStatus = 3\n",
		// A CATEGORICAL component the index cannot key on: an expression, not a string
		// literal. Nothing posts the tuple ("ab","zz"), so only the composite's exception
		// set can keep this record a candidate.
		"ClusterId = 201\nProcId = 0\nOwner = strcat(\"a\", \"b\")\nCmd = \"zz\"\nJobStatus = 6\n",
		// A negative zero: the same value as +0 and the same posting, which the raw float64
		// bit pattern would not be.
		"ClusterId = 55\nProcId = -0.0\nOwner = \"User2\"\nJobStatus = 5\n",
		// Two string components whose concatenations collide if the key encoding does not
		// delimit them: ("ab","c") and ("a","bc").
		"ClusterId = 101\nProcId = 0\nOwner = \"ab\"\nCmd = \"c\"\nJobStatus = 4\n",
		"ClusterId = 102\nProcId = 0\nOwner = \"a\"\nCmd = \"bc\"\nJobStatus = 4\n")
}

func loadComposite(t *testing.T, composite bool) *Collection {
	t.Helper()
	opts := Options{Shards: 4,
		ValueAttrs:       []string{"ClusterId", "ProcId", "JobStatus"},
		CategoricalAttrs: []string{"Owner", "Cmd"}}
	if composite {
		opts.CompositeAttrs = [][]string{{"ClusterId", "ProcId"}, {"Owner", "JobStatus"}, {"Owner", "Cmd"}}
	}
	c := New(opts)
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

func mustCompositeQuery(t *testing.T, constraint string) *vm.Query {
	t.Helper()
	q, err := vm.Parse(constraint)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func queryKeys(t *testing.T, c *Collection, constraint string) []string {
	t.Helper()
	q := mustCompositeQuery(t, constraint)
	var out []string
	for ad := range c.Query(q) {
		cl, _ := ad.EvaluateAttrString("ClusterId")
		p, _ := ad.EvaluateAttrString("ProcId")
		o, _ := ad.EvaluateAttrString("Owner")
		s, _ := ad.EvaluateAttrString("JobStatus")
		cmd, _ := ad.EvaluateAttrString("Cmd")
		out = append(out, cl+"."+p+"/"+o+"/"+s+"/"+cmd)
	}
	sort.Strings(out)
	return out
}

// compositeCase is one conjunction both configurations must answer identically, with the
// row count it is expected to produce. The count is there so a case cannot quietly go
// vacuous: two configurations that both return nothing agree about nothing.
type compositeCase struct {
	constraint string
	want       int
}

func compositeConstraints() []compositeCase {
	return []compositeCase{
		{`ClusterId == 7 && ProcId == 3`, 1},
		{`ClusterId == 7 && ProcId == 0`, 1},
		{`ClusterId == 7 && ProcId == 2`, 2}, // the `1 + 1` record evaluates to 2 as well
		{`ClusterId == -3 && ProcId == 0`, 1},
		{`ClusterId == 0 && ProcId == -1`, 1},
		{`ClusterId == 999 && ProcId == 0`, 0}, // no such tuple
		{`ClusterId == 7 && ProcId == 99`, 0},  // cluster present, proc absent
		{`ProcId == 3 && ClusterId == 7`, 1},   // order of the conjuncts must not matter
		{`ClusterId == 7 && ProcId == 3 && JobStatus == 2`, 0},
		{`ClusterId == 7 && ProcId >= 2`, 5}, // not a full tuple: no composite applies
		{`ClusterId == 7`, 8},
		{`Owner == "user1" && JobStatus == 3`, 8}, // categorical component, folded
		{`Owner == "USER1" && JobStatus == 3`, 8},
		{`Owner == "user3" && JobStatus == 1`, 6},
		{`(ClusterId == 7 && ProcId == 1) || (ClusterId == 8 && ProcId == 2)`, 2},
		{`ClusterId == 7 && ProcId != 1`, 6},
		{`ClusterId == 7 && ProcId == 1 && ProcId == 2`, 0}, // contradictory
		{`ClusterId == 7 && ProcId == 11`, 1},               // only the exceptional record can match
		{`Owner == "ab" && Cmd == "c"`, 1},                  // key-encoding delimiting
		{`Owner == "a" && Cmd == "bc"`, 1},
		{`Owner == "ab" && Cmd == "bc"`, 0},   // no such pair
		{`ClusterId == 55 && ProcId == 0`, 1}, // -0 is 0
		{`Owner == "ab" && Cmd == "zz"`, 1},   // only the exceptional categorical record matches
		{`Owner == "AB" && Cmd == "ZZ"`, 1},   // ... and string equality is case-insensitive
	}
}

// A wrong-answer index is far worse than a slow one: every constraint must return exactly
// the rows it returns without the composite.
func TestCompositeIndexReturnsSameRows(t *testing.T) {
	plain, comp := loadComposite(t, false), loadComposite(t, true)
	for _, cc := range compositeConstraints() {
		want := queryKeys(t, plain, cc.constraint)
		got := queryKeys(t, comp, cc.constraint)
		if len(want) != cc.want {
			t.Fatalf("%s: %d rows without the composite, want %d -- the fixture no longer exercises this case", cc.constraint, len(want), cc.want)
		}
		if fmt.Sprint(want) != fmt.Sprint(got) {
			t.Fatalf("%s: composite returned %v, want %v", cc.constraint, got, want)
		}
	}
}

// The EFFECT, not the plan label: a composite lookup hands the scan fewer records to
// re-verify than intersecting the two attributes' postings does.
func TestCompositeIndexVisitsFewerRecords(t *testing.T) {
	visited := func(c *Collection, constraint string) (int, QueryExplain) {
		q, err := vm.Parse(constraint)
		if err != nil {
			t.Fatal(err)
		}
		var st ScanStats
		for range c.QueryProjectStats(q, []string{"ClusterId"}, &st) { //nolint:revive // draining the iterator is the point
		}
		return st.RecordsVisited, c.ExplainQuery(q)
	}
	// Ten procs per cluster over two thousand clusters: ProcId's posting spans every
	// cluster, so the intersection either merges it or (worthProbing) drops it and
	// re-verifies the whole cluster. No exceptional record here -- the exception set is a
	// candidate for every probe, composite or not, and would mask the difference.
	load := func(composite bool) *Collection {
		opts := Options{Shards: 4, ValueAttrs: []string{"ClusterId", "ProcId"}}
		if composite {
			opts.CompositeAttrs = [][]string{{"ClusterId", "ProcId"}}
		}
		c := New(opts)
		for cl := range 2000 {
			for p := range 10 {
				ad, err := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\n", cl, p, cl%7))
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
	plain, comp := load(false), load(true)
	const constraint = `ClusterId == 1234 && ProcId == 3`
	base, _ := visited(plain, constraint)
	with, ex := visited(comp, constraint)
	if with >= base {
		t.Fatalf("composite visited %d records, intersection visited %d: the composite bought nothing", with, base)
	}
	if with != 1 {
		t.Fatalf("composite visited %d records for a unique tuple, want 1", with)
	}
	if ex.CompositeSegments == 0 {
		t.Fatalf("explain reports no segment answered from a composite: %+v", ex)
	}
	if ex.CompositeIndex != "ClusterId+ProcId" {
		t.Fatalf("explain names the composite %q, want ClusterId+ProcId", ex.CompositeIndex)
	}
}

// The composite key must delimit its components: ("ab","c") and ("a","bc") are two tuples,
// and an encoding that ran them together would answer both with both records. That is still
// a CORRECT answer -- candidates are re-verified -- so it has to be asserted as an effect:
// the lookup must hand the scan one record, not two.
func TestCompositeKeyComponentsAreDelimited(t *testing.T) {
	c := New(Options{Shards: 1,
		CategoricalAttrs: []string{"Owner", "Cmd"},
		CompositeAttrs:   [][]string{{"Owner", "Cmd"}}})
	for i, pair := range [][2]string{{"ab", "c"}, {"a", "bc"}, {"ab", "bc"}, {"a", "c"}} {
		ad, perr := classad.ParseOld(fmt.Sprintf("Owner = %q\nCmd = %q\nSeq = %d\n", pair[0], pair[1], i))
		if perr != nil {
			t.Fatal(perr)
		}
		if perr = c.Put([]byte(fmt.Sprintf("k%d", i)), ad); perr != nil {
			t.Fatal(perr)
		}
	}
	c.Reindex()
	q, err := vm.Parse(`Owner == "ab" && Cmd == "c"`)
	if err != nil {
		t.Fatal(err)
	}
	var st ScanStats
	n := 0
	for range c.QueryProjectStats(q, []string{"Owner"}, &st) {
		n++
	}
	if n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
	if st.RecordsVisited != 1 {
		t.Fatalf("visited %d records for one tuple, want 1: the key encoding runs its components together", st.RecordsVisited)
	}
}

// A composite proves a segment empty where neither attribute alone can: the segment holds
// cluster 7 and holds procs numbered 3, just never together.
func TestCompositeSkipsSegmentNeitherAttributeCan(t *testing.T) {
	c := New(Options{Shards: 1,
		ValueAttrs:     []string{"ClusterId", "ProcId"},
		CompositeAttrs: [][]string{{"ClusterId", "ProcId"}}})
	for i, src := range []string{
		"ClusterId = 7\nProcId = 0\n",
		"ClusterId = 8\nProcId = 3\n",
	} {
		ad, err := classad.ParseOld(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	q, err := vm.Parse(`ClusterId == 7 && ProcId == 3`)
	if err != nil {
		t.Fatal(err)
	}
	ex := c.ExplainQuery(q)
	if ex.IndexSkippedSegments == 0 {
		t.Fatalf("the composite did not rule the segment out: %+v", ex)
	}
	var st ScanStats
	n := 0
	for range c.QueryProjectStats(q, []string{"ClusterId"}, &st) {
		n++
	}
	if n != 0 || st.RecordsVisited != 0 {
		t.Fatalf("visited %d records for %d rows, want 0 and 0", st.RecordsVisited, n)
	}
}

// A tuple naming an attribute this spec gives no kind to -- neither indexed nor declared a
// composite component -- is refused: the record side and the probe side have to fold every
// component the same way, and with no kind there is no way to fold it at all.
func TestCompositeRequiresIndexedComponents(t *testing.T) {
	c := New(Options{Shards: 1,
		ValueAttrs:     []string{"ClusterId"},
		CompositeAttrs: [][]string{{"ClusterId", "ProcId"}, {"ClusterId", "ClusterId"}, {"ClusterId"}}})
	if got := len(c.spec.Load().comps); got != 0 {
		t.Fatalf("%d composites configured, want 0", got)
	}
}

// Dropping an index drops every composite that used it.
func TestCompositeDroppedWithItsComponent(t *testing.T) {
	c := New(Options{Shards: 1,
		ValueAttrs:     []string{"ClusterId", "ProcId"},
		CompositeAttrs: [][]string{{"ClusterId", "ProcId"}}})
	if got := len(c.spec.Load().comps); got != 1 {
		t.Fatalf("%d composites, want 1", got)
	}
	if !c.DropIndex("ProcId") {
		t.Fatal("DropIndex reported no change")
	}
	if got := len(c.spec.Load().comps); got != 0 {
		t.Fatalf("%d composites after dropping a component, want 0", got)
	}
}

// Re-kinding a component (a value index added again as categorical) invalidates the
// composite: it would fold the attribute the wrong way and post every record as exceptional.
func TestCompositeDroppedWhenComponentChangesKind(t *testing.T) {
	c := New(Options{Shards: 1,
		ValueAttrs:     []string{"ClusterId", "ProcId"},
		CompositeAttrs: [][]string{{"ClusterId", "ProcId"}}})
	if got := len(c.spec.Load().comps); got != 1 {
		t.Fatalf("%d composites, want 1", got)
	}
	if !c.AddIndex([]string{"ProcId"}, nil) { // ProcId becomes categorical
		t.Fatal("AddIndex reported no change")
	}
	if got := len(c.spec.Load().comps); got != 0 {
		t.Fatalf("%d composites after a component changed kind, want 0", got)
	}
}

// A composite lives in the sealed sidecar, not only in RAM: it must still be there after the
// segments seal and the collection is reopened from disk.
func TestCompositeSurvivesSealAndReopen(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Dir: dir, Shards: 1, SegmentSize: 1 << 13,
		ValueAttrs:       []string{"ClusterId", "ProcId"},
		CategoricalAttrs: []string{"Owner", "Cmd"},
		CompositeAttrs:   [][]string{{"ClusterId", "ProcId"}, {"Owner", "Cmd"}}}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	for cl := 1; cl <= 300; cl++ {
		for p := range 5 {
			ad, perr := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\nCmd = \"/bin/job%d\"\nArgs = \"%d %d\"\n", cl, p, cl%97, cl%89, cl, p))
			if perr != nil {
				t.Fatal(perr)
			}
			if perr = c.Put([]byte(fmt.Sprintf("%d.%d", cl, p)), ad); perr != nil {
				t.Fatal(perr)
			}
		}
	}
	c.Reindex()
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck // test cleanup
	c.Reindex()
	q, err := vm.Parse(`ClusterId == 150 && ProcId == 3`)
	if err != nil {
		t.Fatal(err)
	}
	ex := c.ExplainQuery(q)
	if ex.UnindexedSegments > 0 {
		t.Fatalf("reopened collection has %d unindexed segments: %+v", ex.UnindexedSegments, ex)
	}
	if ex.CompositeSegments == 0 {
		t.Fatalf("no segment answered from the composite after a reopen -- a RAM-only index: %+v", ex)
	}
	var st ScanStats
	n := 0
	for range c.QueryProjectStats(q, []string{"ClusterId"}, &st) {
		n++
	}
	if n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
	if st.RecordsVisited != 1 {
		t.Fatalf("visited %d records after a reopen, want 1: the sealed sidecar is not serving the composite", st.RecordsVisited)
	}
	// And a CATEGORICAL tuple, so the sidecar carries attribute blocks between its meta
	// directory and its composite blocks. A composite directory written in the wrong place
	// parses as garbage only once something else sits where it should be.
	exCat := c.ExplainQuery(mustCompositeQuery(t, `Owner == "u11" && Cmd == "/bin/job11"`))
	if exCat.UnindexedSegments > 0 {
		t.Fatalf("a sealed segment has no index after a reopen -- the sidecar did not parse: %+v", exCat)
	}
	if exCat.CompositeSegments == 0 {
		t.Fatalf("no sealed segment answered the categorical tuple from its sidecar composite: %+v", exCat)
	}
}

// The sealed tier must answer exactly what the in-RAM one does, including for the records
// the index cannot classify.
func TestCompositeSealedSidecarSameRows(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Dir: dir, Shards: 2, SegmentSize: 1 << 12,
		ValueAttrs:       []string{"ClusterId", "ProcId", "JobStatus"},
		CategoricalAttrs: []string{"Owner", "Cmd"},
		CompositeAttrs:   [][]string{{"ClusterId", "ProcId"}, {"Owner", "JobStatus"}, {"Owner", "Cmd"}}}
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
	if c, err = Open(opts); err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck // test cleanup
	c.Reindex()
	plain := loadComposite(t, false)
	for _, cc := range compositeConstraints() {
		want := queryKeys(t, plain, cc.constraint)
		if len(want) != cc.want {
			t.Fatalf("%s: %d rows without the composite, want %d -- the fixture no longer exercises this case", cc.constraint, len(want), cc.want)
		}
		got := queryKeys(t, c, cc.constraint)
		if fmt.Sprint(want) != fmt.Sprint(got) {
			t.Fatalf("%s: sealed composite returned %v, want %v", cc.constraint, got, want)
		}
	}
}

// The sidecar layout itself: a composite section written in the wrong place parses as
// garbage, and the only visible symptom is that the sidecar is rejected and the index
// rebuilt in RAM -- which answers correctly, so no query-level test can see it. This one
// reads the bytes back directly.
func TestCompositeSidecarRoundTrip(t *testing.T) {
	c := New(Options{Shards: 1,
		ValueAttrs:       []string{"ClusterId", "ProcId"},
		CategoricalAttrs: []string{"Owner", "Cmd"},
		CompositeAttrs:   [][]string{{"ClusterId", "ProcId"}, {"Owner", "Cmd"}}})
	for i := range 50 {
		ad, err := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\nCmd = \"/bin/job%d\"\n", i/5, i%5, i%7, i%11))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	sh := c.shards[0]
	var si *segIndex
	sh.mu.RLock()
	for _, seg := range sh.segs {
		if seg == nil {
			continue
		}
		if x := seg.idx.Load(); x != nil && len(x.comp) > 0 {
			si = x
		}
	}
	sh.mu.RUnlock()
	if si == nil {
		t.Fatal("no in-RAM segment index with composites to serialize")
	}
	blob, err := buildSidecarIndex(si)
	if err != nil {
		t.Fatal(err)
	}
	mm, err := parseMmapSidecar(blob)
	if err != nil {
		t.Fatalf("the sidecar this build wrote does not parse: %v", err)
	}
	if len(mm.comps) != len(si.comp) {
		t.Fatalf("sidecar carries %d composites, the index has %d", len(mm.comps), len(si.comp))
	}
	for _, mc := range mm.comps {
		cp, ok := si.comp[mc.spec.key]
		if !ok {
			t.Fatalf("sidecar carries a composite the index does not: %x", mc.spec.key)
		}
		if fmt.Sprint(mc.spec.cats) != fmt.Sprint(cp.spec.cats) {
			t.Fatalf("composite %x: component kinds %v on disk, %v in RAM", mc.spec.key, mc.spec.cats, cp.spec.cats)
		}
		// Every tuple the in-RAM index posts must be findable in the sidecar, with the
		// same posting.
		for key, bm := range cp.post {
			off, hit := mm.compFind(mc.off, key)
			if !hit {
				t.Fatalf("composite %x: key %q posted in RAM, absent from the sidecar", mc.spec.key, key)
			}
			if got := mm.bitmapAt(off).GetCardinality(); got != bm.GetCardinality() {
				t.Fatalf("composite %x key %q: %d records on disk, %d in RAM", mc.spec.key, key, got, bm.GetCardinality())
			}
		}
	}
}
