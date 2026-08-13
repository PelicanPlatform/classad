package collections

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// columnarizeShard rewrites every eligible sealed segment of sh in place and returns how many were
// replaced, so a test can exercise the read paths against a live shard holding the columnar shape.
func columnarizeShard(t *testing.T, c *Collection, sh *shard, s *adSchema, hot []int) int {
	t.Helper()
	return c.columnarizeSealed()
}

// readAll returns every ad the collection yields for expr, summarized order-insensitively.
func readAll(t *testing.T, c *Collection, expr string) []string {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for w := range c.QueryRawWire(q, nil, false) {
		out = append(out, adSummary(t, c, w))
	}
	return out
}

// TestColumnarizedShardServesEveryReadPath is the test that makes the format usable rather than
// merely buildable: it swaps columnarized segments into a LIVE shard and requires the ordinary read
// paths to return exactly what they returned before.
//
// This is the assertion the reader migration exists to satisfy. A columnarized record physically
// holds only the attributes the schema does not cover, so before that migration a plain scan
// returned a fraction of the rows (measured: 90 of 1500) and every ad it did return was missing
// half its attributes -- with nothing in the result to indicate it.
func TestColumnarizedShardServesEveryReadPath(t *testing.T) {
	c, s, hot := columnarFixture(t, 3000)
	defer c.Close()
	sh := c.shards[0]

	exprs := []string{
		"true",            // full scan
		"ProcId >= 5",     // one indexed-shape conjunct, columnar-servable
		"Owner == \"u3\"", // string equality
		"ProcId >= 5 && ProcId <= 8",
		"ProcId < 2 || ProcId > 7", // disjunction: never columnar-served
	}
	before := make([][]string, len(exprs))
	for i, e := range exprs {
		before[i] = readAll(t, c, e)
	}
	counts := make([]int, len(exprs))
	for i, e := range exprs {
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := c.CountQuery(q)
		counts[i] = n
	}

	n := columnarizeShard(t, c, sh, s, hot)
	if n == 0 {
		t.Skip("no sealed segment was columnarized")
	}
	t.Logf("columnarized %d segments in a live shard", n)

	for i, e := range exprs {
		after := readAll(t, c, e)
		if len(after) != len(before[i]) {
			t.Errorf("%s: %d rows after columnarizing, want %d", e, len(after), len(before[i]))
			continue
		}
		want := map[string]int{}
		for _, k := range before[i] {
			want[k]++
		}
		bad := 0
		for _, k := range after {
			want[k]--
			if want[k] < 0 {
				bad++
			}
		}
		if bad != 0 {
			t.Errorf("%s: %d ads differ after columnarizing", e, bad)
		}
	}
	// The columnar count path reads the same columns the records no longer carry, so it must agree
	// with itself across the transform too.
	for i, e := range exprs {
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := c.CountQuery(q); got != counts[i] {
			t.Errorf("%s: count %d after columnarizing, want %d", e, got, counts[i])
		}
	}
}

// TestColumnarizedShardServesReverseScan covers the newest-first walk separately: it collects each
// window's offsets in a forward pass and replays them backward, so it reaches records through a
// different path than the forward scan and could regress independently.
func TestColumnarizedShardServesReverseScan(t *testing.T) {
	c, s, hot := columnarFixture(t, 3000)
	defer c.Close()
	c.reverseScan = true
	before := readAll(t, c, "true")
	if n := columnarizeShard(t, c, c.shards[0], s, hot); n == 0 {
		t.Skip("no sealed segment was columnarized")
	}
	after := readAll(t, c, "true")
	if len(after) != len(before) {
		t.Fatalf("reverse scan: %d rows, want %d", len(after), len(before))
	}
	for i := range after {
		if after[i] != before[i] {
			t.Fatalf("reverse scan diverges at %d: order or content changed", i)
		}
	}
}

// TestColumnarizedSegmentSurvivesReopen is the test the format is least able to do without: a
// columnarized segment's columnar payload is DURABLE data, not a cache, because the records were
// written without the attributes it holds.
//
// The failure mode it guards is silent. If a reopen does not publish the payload, the segment looks
// like an ordinary one whose records are whole, and every ad it serves is missing every schema'd
// attribute -- with nothing in the result, and no error, to say so. That is worse than a read
// failure, so the read paths treat an unreadable payload as an error and this test requires a
// readable one to be found.
func TestColumnarizedSegmentSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	c, s, hot := columnarFixtureIn(t, dir, 3000)
	before := readAll(t, c, "true")
	n := columnarizeShard(t, c, c.shards[0], s, hot)
	if n == 0 {
		c.Close()
		t.Skip("no sealed segment was columnarized")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	got := 0
	for _, seg := range c2.shards[0].segs {
		if seg != nil && seg.columnarized() {
			got++
		}
		if seg != nil && seg.colDamaged.Load() {
			t.Errorf("segment %d: columnar payload did not survive the reopen", seg.id)
		}
	}
	if got != n {
		t.Fatalf("%d columnarized segments after reopen, want %d", got, n)
	}
	after := readAll(t, c2, "true")
	if len(after) != len(before) {
		t.Fatalf("after reopen: %d rows, want %d", len(after), len(before))
	}
	want := map[string]int{}
	for _, k := range before {
		want[k]++
	}
	bad := 0
	for _, k := range after {
		want[k]--
		if want[k] < 0 {
			bad++
		}
	}
	if bad != 0 {
		t.Fatalf("%d ads differ after reopen", bad)
	}
	t.Logf("%d columnarized segments reopened; all %d ads intact", got, len(after))
}

// TestColumnarizedShardServesIndexedQueries covers the paths where a wrong answer would be FAST.
//
// An attribute index and a zone map are both DERIVED from the records, so both are built by walking
// a segment. If either walk reads stored bytes rather than whole ads, it sees a columnarized
// segment's remnants: the index posts only the attributes the schema does not cover, and the zone
// map reports a range narrower than the segment really holds. An indexed query then misses matching
// records, and zone pruning SKIPS segments that contain matches -- both silently, and faster than
// the correct answer, which is why they are worth their own test rather than trusting the scan.
func TestColumnarizedShardServesIndexedQueries(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{
		Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1,
		CategoricalAttrs: []string{"Owner", "Cmd"},
		ValueAttrs:       []string{"RequestMemory", "JobStatus"},
		ZoneAttrs:        []string{"RequestMemory"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := range 3000 {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep" ]`,
			i, i%10, i%7, i%6, (i%16)*1024))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.RetrainDict(0)
	// deriveSchema + installSchemaScan rather than BuildAndEnableSchemaScan: the latter is a full
	// schema-review pass, which columnarizes, so it would leave nothing to compare against.
	sch, hot, ok := c.deriveSchema(4096, 8)
	if !ok || !c.installSchemaScan(sch, hot) {
		t.Skip("schema scan did not enable")
	}
	exprs := []string{
		"true",
		"RequestMemory > 4096",             // value index + zone pruning
		"RequestMemory > 4096 && ProcId<3", // indexed conjunct plus a scan conjunct
		"Owner == \"user3\"",               // categorical index
		"JobStatus == 4",                   // value index on an equality
		"Cmd == \"/bin/sleep\"",            // categorical matching every record
	}
	before := make([]int, len(exprs))
	for i, e := range exprs {
		before[i] = len(readAll(t, c, e))
	}
	n := c.columnarizeSealed()
	if n == 0 {
		t.Skip("no sealed segment was columnarized")
	}
	for i, e := range exprs {
		if got := len(readAll(t, c, e)); got != before[i] {
			t.Errorf("%s: %d rows after columnarizing, want %d", e, got, before[i])
		}
	}
	// Get is a different path again: it resolves a key to a segment and offset through the
	// directory, with no scan and no window.
	for _, k := range []string{"7.0", "1500.0", "2999.0"} {
		ad, ok := c.Get([]byte(k))
		if !ok {
			t.Fatalf("Get(%s) after columnarizing: not found", k)
		}
		// RequestMemory is a schema field, so a columnarized record does not physically carry it:
		// Get has to reach the columns to answer at all.
		if _, ok := ad.EvaluateAttrInt("RequestMemory"); !ok {
			t.Errorf("Get(%s): RequestMemory did not survive columnarizing", k)
		}
		if _, ok := ad.EvaluateAttrString("Cmd"); !ok {
			t.Errorf("Get(%s): Cmd did not survive columnarizing", k)
		}
	}
	t.Logf("columnarized %d segments; indexes, zone pruning and Get all agree", n)
}

// TestColumnarizedShardServesWatch covers the watch catch-up path, which walks segments directly to
// replay records a consumer has not seen. An event carrying half an ad is the worst case for a
// consumer: a watch reports what CHANGED, so an ad missing its schema'd attributes reads as those
// attributes having been removed -- a change that never happened, indistinguishable from one that
// did.
func TestColumnarizedShardServesWatch(t *testing.T) {
	c, _, _ := columnarFixture(t, 3000)
	defer c.Close()

	catchUp := func() map[string]int {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seq, err := c.Watch(ctx, nil) // nil cursor: replay everything the collection still holds
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for ev := range seq {
			if ev.Kind == WatchUpsert && ev.Ad != nil {
				out[string(ev.Key)] = len(ev.Ad.AST().Attributes)
			}
			if len(out) >= 3000 {
				break
			}
		}
		return out
	}

	before := catchUp()
	if len(before) == 0 {
		t.Skip("watch catch-up produced no events")
	}
	if got := c.columnarizeSealed(); got == 0 {
		t.Skip("no sealed segment was columnarized")
	}
	after := catchUp()
	missing, shrunk := 0, 0
	for k, n := range before {
		m, ok := after[k]
		if !ok {
			missing++
		} else if m != n {
			shrunk++
		}
	}
	if missing != 0 || shrunk != 0 {
		t.Errorf("after columnarizing: %d of %d watch events missing, %d lost attributes",
			missing, len(before), shrunk)
	}
	t.Logf("%d watch catch-up events carry their full ads across the transform", len(before))
}

// TestColumnarizedShardServesTimeTravel covers the point-in-time path, which resolves records
// through a different visibility test than the current-time one and reaches history versions that
// the current-time scan skips entirely.
func TestColumnarizedShardServesTimeTravel(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{
		Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1,
		TimeTravel: &TimeTravelOptions{MaxDistance: time.Hour, CheckpointInterval: time.Millisecond},
	})
	if err != nil {
		t.Skipf("time travel unavailable: %v", err)
	}
	defer c.Close()
	put := func(i int, owner string, status int) {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="%s"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep" ]`,
			i, i%10, owner, status, (i%16)*1024))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 2000 {
		put(i, fmt.Sprintf("user%d", i%7), i%6)
	}
	c.RetrainDict(0)
	// deriveSchema + installSchemaScan rather than BuildAndEnableSchemaScan: the latter is a full
	// schema-review pass, which columnarizes, so it would leave nothing to compare against.
	sch, hot, ok := c.deriveSchema(4096, 8)
	if !ok || !c.installSchemaScan(sch, hot) {
		t.Skip("schema scan did not enable")
	}
	time.Sleep(5 * time.Millisecond)
	asOf := time.Now()
	time.Sleep(5 * time.Millisecond)
	// Supersede every record, so the versions the AS OF query must find are history.
	for i := range 2000 {
		put(i, "changed", 9)
	}

	q, err := vm.Parse(`Owner != "changed"`)
	if err != nil {
		t.Fatal(err)
	}
	countAsOf := func() int {
		seq, err := c.QueryAsOf(q, asOf)
		if err != nil {
			t.Skipf("AS OF unavailable: %v", err)
		}
		n := 0
		for range seq {
			n++
		}
		return n
	}
	before := countAsOf()
	if before == 0 {
		t.Skip("no history visible at the chosen instant")
	}
	if got := c.columnarizeSealed(); got == 0 {
		t.Skip("no sealed segment was columnarized")
	}
	if after := countAsOf(); after != before {
		t.Errorf("AS OF: %d rows after columnarizing, want %d", after, before)
	}
	t.Logf("point-in-time reads agree across the transform (%d historical rows)", before)
}

// TestSchemaReviewColumnarizesByDefault goes through the production entry point: a routine schema
// review both publishes the schema and moves each sealed segment's schema'd attributes into the
// segment, with no option set.
func TestSchemaReviewColumnarizesByDefault(t *testing.T) {
	dir := t.TempDir()
	c, _, _ := columnarFixtureIn(t, dir, 3000)
	defer c.Close()
	before := readAll(t, c, "true")

	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Skip("schema scan did not enable")
	}
	got := 0
	for _, seg := range c.shards[0].segs {
		if seg != nil && seg.columnarized() {
			got++
		}
	}
	if got == 0 {
		t.Fatal("a schema review left every segment whole-record; the default is not on")
	}
	after := readAll(t, c, "true")
	if len(after) != len(before) {
		t.Fatalf("%d rows after the schema review, want %d", len(after), len(before))
	}
	// And the sidecar is not asked for a second copy of what the segments now hold.
	for _, seg := range c.shards[0].segs {
		if seg != nil && seg.columnarized() {
			if blob := c.colBlobForSeg(seg); blob != nil {
				t.Errorf("segment %d: sidecar would still store %d columnar bytes", seg.id, len(blob))
			}
		}
	}
	segs, saved := ColumnarizedSegments()
	t.Logf("schema review columnarized %d segments by default (%d total, %d bytes saved)", got, segs, saved)
}

// TestColumnarSegmentBudgetDisables checks the off switch, which matters because it is the only way
// back to the whole-record shape for a caller who does not want the rewrite.
func TestColumnarSegmentBudgetDisables(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1,
		ColumnarSegmentBudget: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := range 3000 {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d ]`,
			i, i%10, i%7, i%6, (i%16)*1024))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.RetrainDict(0)
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Skip("schema scan did not enable")
	}
	for _, seg := range c.shards[0].segs {
		if seg != nil && seg.columnarized() {
			t.Fatalf("segment %d was columnarized despite ColumnarSegmentBudget < 0", seg.id)
		}
	}
	if n := c.ColumnarizeSealed(); n != 0 {
		t.Errorf("ColumnarizeSealed rewrote %d segments while disabled", n)
	}
	// And the old shape still works: a sidecar block was built instead.
	blocks := 0
	for _, seg := range c.shards[0].segs {
		if seg != nil && seg.colblk.Load() != nil {
			blocks++
		}
	}
	if blocks == 0 {
		t.Error("disabling the rewrite also lost the sidecar columnar accelerator")
	}
	t.Logf("rewrite disabled; %d sidecar blocks built instead", blocks)
}

// TestColumnarSegmentBudgetBounds checks that one pass rewrites at most the budget, so enabling this
// over a large existing archive converges over several maintenance intervals instead of stalling on
// a single pass that rewrites the whole history.
func TestColumnarSegmentBudgetBounds(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1,
		ColumnarSegmentBudget: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := range 4000 {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d ]`,
			i, i%10, i%7, i%6, (i%16)*1024))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.RetrainDict(0)
	sch, hot, ok := c.deriveSchema(4096, 8)
	if !ok || !c.installSchemaScan(sch, hot) {
		t.Skip("schema scan did not enable")
	}
	sealed := 0
	for _, seg := range c.shards[0].segs {
		if seg != nil && seg != c.shards[0].act && seg.used > 0 {
			sealed++
		}
	}
	if sealed <= 2 {
		t.Skip("not enough sealed segments to exercise a budget")
	}
	first := c.ColumnarizeSealed()
	if first > 2 {
		t.Errorf("first pass rewrote %d segments, budget was 2", first)
	}
	// Successive passes pick up where the last stopped, and stop once there is nothing left.
	total := first
	for range 20 {
		n := c.ColumnarizeSealed()
		if n == 0 {
			break
		}
		if n > 2 {
			t.Errorf("a pass rewrote %d segments, budget was 2", n)
		}
		total += n
	}
	if total < sealed {
		t.Errorf("converged at %d of %d sealed segments", total, sealed)
	}
	t.Logf("%d sealed segments converged in passes of at most 2", total)
}

// TestColumnarizeKeepsMidBuildDeletes covers the one race the rewrite genuinely has, and the reason
// it is safe on a MUTABLE table at all.
//
// A sealed segment stops taking appends, but in a mutable table its records can still be superseded
// in place -- and the rewrite reads the source OFF the shard lock. A delete that lands between the
// read and the swap is recorded on the source and absent from the output, so publishing without
// reconciling would bring the deleted record back to life. Interning refuses mutable tables for
// exactly this reason; this one reconciles instead, so the reconcile has to be tested.
//
// colCommitStallHook makes the window deterministic: the delete happens after the build and before
// the commit, which is precisely where a real one would have to land to be lost.
func TestColumnarizeKeepsMidBuildDeletes(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const n = 3000
	for i := range n {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d ]`,
			i, i%10, i%7, i%6, (i%16)*1024))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.RetrainDict(0)
	sch, hot, ok := c.deriveSchema(4096, 8)
	if !ok || !c.installSchemaScan(sch, hot) {
		t.Skip("schema scan did not enable")
	}

	// Delete one record per segment rewrite, from the sealed range, inside the commit window.
	deleted := map[string]bool{}
	next := 0
	colCommitStallHook = func() {
		for next < n {
			k := fmt.Sprintf("%d.0", next)
			next++
			if _, ok := c.Get([]byte(k)); !ok {
				continue
			}
			if !c.Delete([]byte(k)) {
				continue
			}
			deleted[k] = true
			return
		}
	}
	defer func() { colCommitStallHook = nil }()

	if got := c.ColumnarizeSealed(); got == 0 {
		t.Skip("no sealed segment was columnarized")
	}
	colCommitStallHook = nil
	if len(deleted) == 0 {
		t.Skip("no delete landed in the commit window")
	}

	// Every record deleted in the window must still be gone -- through Get and through the scan,
	// since they resolve visibility by different routes.
	for k := range deleted {
		if _, ok := c.Get([]byte(k)); ok {
			t.Errorf("Get(%s): a record deleted mid-rewrite came back", k)
		}
	}
	live := map[string]bool{}
	q, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	for ad := range c.Query(q) {
		if v, ok := ad.EvaluateAttrInt("ClusterId"); ok {
			live[fmt.Sprintf("%d.0", v)] = true
		}
	}
	for k := range deleted {
		if live[k] {
			t.Errorf("scan: a record deleted mid-rewrite came back (%s)", k)
		}
	}
	if want := n - len(deleted); len(live) != want {
		t.Errorf("scan sees %d records, want %d", len(live), want)
	}
	t.Logf("%d deletes landed in the commit window and all stayed dead", len(deleted))
}

// TestColumnarPrefilterAgreesWithRowPath is the safety net for deciding a match from columns instead
// of from the record.
//
// The prefilter's failure mode is asymmetric and that asymmetry is the whole design: claiming a match
// that is not one costs a reassembly the ordinary match then rejects, while claiming a NON-match that
// is one silently drops a row from a query result. So this compares identical data stored both ways
// across the shapes most likely to disagree -- undefined attributes, attribute-to-attribute
// comparisons, strings, negation, arithmetic, and attributes the schema does not carry -- and requires
// the answers to be equal, not merely close.
// bothStorageForms builds the SAME data twice, once whole-record and once columnarized, so a test can
// require the two forms to answer identically.
func bothStorageForms(t *testing.T) (rows, cols *Collection) {
	build := func(columnarize bool) *Collection {
		budget := 0
		if !columnarize {
			budget = -1
		}
		c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16,
			GroupSchemaCount: -1, ColumnarSegmentBudget: budget})
		if err != nil {
			t.Fatal(err)
		}
		for i := range 3000 {
			// Deliberately uneven: an attribute only some ads carry, one whose value is an
			// EXPRESSION (which the columns cannot evaluate and must defer on), a string, and a
			// negative number.
			text := fmt.Sprintf(
				`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep"; Slack=%d ]`,
				i, i%10, i%7, i%6, (i%16)*1024, i%11-5)
			switch i % 7 {
			case 0:
				text = fmt.Sprintf(
					`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep"; Slack=%d; Sometimes=%d ]`,
					i, i%10, i%7, i%6, (i%16)*1024, i%11-5, i)
			}
			// A stored EXPRESSION in a field the schema DOES carry, so the columnar path has to
			// defer these records to the ordinary evaluator. Deliberately rare: at one in seven the
			// field's fit was poor enough that the schema dropped it altogether, the prefilter then
			// declined every query naming it, and the deferral path this is here to cover never ran
			// once -- 18000 records decided and not a single fallback.
			if i%211 == 1 {
				text = fmt.Sprintf(
					`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=ProcId*512+7; Cmd="/bin/sleep"; Slack=%d ]`,
					i, i%10, i%7, i%6, i%11-5)
			}
			ad, err := classad.Parse(text)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
				t.Fatal(err)
			}
		}
		c.RetrainDict(0)
		if !c.BuildAndEnableSchemaScan(4096, 8) {
			t.Skip("schema scan did not enable")
		}
		return c
	}
	return build(false), build(true)
}

func TestColumnarPrefilterAgreesWithRowPath(t *testing.T) {
	build := func(columnarize bool) *Collection {
		budget := 0
		if !columnarize {
			budget = -1
		}
		c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16,
			GroupSchemaCount: -1, ColumnarSegmentBudget: budget})
		if err != nil {
			t.Fatal(err)
		}
		for i := range 3000 {
			// Deliberately uneven: an attribute only some ads carry, one whose value is an
			// EXPRESSION (which the columns cannot evaluate and must defer on), a string, and a
			// negative number.
			text := fmt.Sprintf(
				`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep"; Slack=%d ]`,
				i, i%10, i%7, i%6, (i%16)*1024, i%11-5)
			switch i % 7 {
			case 0:
				text = fmt.Sprintf(
					`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep"; Slack=%d; Sometimes=%d ]`,
					i, i%10, i%7, i%6, (i%16)*1024, i%11-5, i)
			}
			// A stored EXPRESSION in a field the schema DOES carry, so the columnar path has to
			// defer these records to the ordinary evaluator. Deliberately rare: at one in seven the
			// field's fit was poor enough that the schema dropped it altogether, the prefilter then
			// declined every query naming it, and the deferral path this is here to cover never ran
			// once -- 18000 records decided and not a single fallback.
			if i%211 == 1 {
				text = fmt.Sprintf(
					`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=ProcId*512+7; Cmd="/bin/sleep"; Slack=%d ]`,
					i, i%10, i%7, i%6, i%11-5)
			}
			ad, err := classad.Parse(text)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
				t.Fatal(err)
			}
		}
		c.RetrainDict(0)
		if !c.BuildAndEnableSchemaScan(4096, 8) {
			t.Skip("schema scan did not enable")
		}
		return c
	}
	rows := build(false)
	defer rows.Close()
	cols := build(true)
	defer cols.Close()

	columnarized := 0
	for _, seg := range cols.shards[0].segs {
		if seg != nil && seg.columnarized() {
			columnarized++
		}
	}
	if columnarized == 0 {
		t.Skip("no sealed segment was columnarized")
	}

	for _, expr := range []string{
		"true",
		"RequestMemory > 4096",
		"RequestMemory >= 0 && ProcId < 5",
		"ProcId >= 5 || RequestMemory < 2048",
		"!(ProcId >= 5)",
		"Slack < 0",                     // negative numbers
		"Slack + ProcId > 3",            // arithmetic across two columns
		"ProcId != JobStatus",           // attribute to attribute
		"Owner == \"user3\"",            // string equality
		"Owner > \"user1\"",             // string ordering
		"Sometimes is undefined",        // an attribute only some ads carry
		"Sometimes isnt undefined",      // and its complement
		"Sometimes > 100",               // comparing a frequently-absent attribute
		"RequestMemory > ProcId * 100",  // an attribute whose stored value is sometimes an expression
		"JobStatus == 4 && Slack == -5", // conjunction over two columns
		"Cmd == \"/bin/sleep\"",         // matches every record
		"Cmd == \"/bin/nonexistent\"",   // matches none
	} {
		want := readAll(t, rows, expr)
		got := readAll(t, cols, expr)
		if len(got) != len(want) {
			t.Errorf("%-36s columnar %d rows, row path %d", expr, len(got), len(want))
			continue
		}
		bag := map[string]int{}
		for _, k := range want {
			bag[k]++
		}
		bad := 0
		for _, k := range got {
			bag[k]--
			if bag[k] < 0 {
				bad++
			}
		}
		if bad != 0 {
			t.Errorf("%-36s %d ads differ from the row path", expr, bad)
		}
	}
}

// TestColumnarizedAdsAreByteEqualIncludingExpressions compares every ad in a columnarized table
// against the same data stored whole-record, and is sensitive to what an expression CONTAINS.
//
// That sensitivity is the point. An earlier version of this comparison rendered every non-literal as
// "<expr>", which made two ads holding different expressions compare equal -- and that is precisely
// what went wrong: moving an expression into a column rewrote the attribute it referenced, so
// `RequestMemory = ProcId * 512 + 7` came back as `Slack * 512 + 7`. Structurally perfect ad, every
// literal correct, wrong query results, nothing logged. See storableInColumn.
func TestColumnarizedAdsAreByteEqualIncludingExpressions(t *testing.T) {
	rows, cols := bothStorageForms(t)
	defer rows.Close()
	defer cols.Close()
	q, _ := vm.Parse("true")
	want := map[string]string{}
	for w := range rows.QueryRawWire(q, nil, false) {
		a, _ := rows.decodeAd(w, identityCodec{})
		if a != nil {
			if v, ok := a.EvaluateAttrInt("ClusterId"); ok {
				want[fmt.Sprintf("%d", v)] = adSummary(t, rows, w)
			}
		}
	}
	bad := 0
	for w := range cols.QueryRawWire(q, nil, false) {
		a, _ := cols.decodeAd(w, identityCodec{})
		if a == nil {
			continue
		}
		v, ok := a.EvaluateAttrInt("ClusterId")
		if !ok {
			continue
		}
		k := fmt.Sprintf("%d", v)
		got := adSummary(t, cols, w)
		if wa, ok := want[k]; ok && wa != got {
			bad++
			if bad <= 3 {
				t.Logf("ClusterId=%s\n  row: %s\n  col: %s", k, wa, got)
			}
		}
	}
	if bad != 0 {
		t.Errorf("%d of %d ads differ between the columnar and whole-record forms", bad, len(want))
	}
	t.Logf("%d ads identical across both storage forms, expression content included", len(want))
}

// readProjected returns every ad the collection yields for expr, restricted to proj.
func readProjected(t *testing.T, c *Collection, expr string, proj []string) []string {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for w := range c.QueryRawWire(q, proj, false) {
		out = append(out, adSummary(t, c, w))
	}
	return out
}

// TestColumnarProjectedReadsMatchRowPath covers building a projected ad from columns instead of
// reassembling the record and discarding most of it.
//
// The comparison is against the same data stored whole-record, and it is byte-sensitive (adSummary
// renders node bytes), because the two paths have to be interchangeable rather than merely agree on
// values: a projected read that encoded the same number differently would be a silent format change
// for every consumer decoding the result.
//
// The projections are chosen to cover where the attributes come FROM: schema fields read out of their
// columns, an attribute the schema does not carry (which stays in the record), a field whose value is
// sometimes an expression (never moved into a column), one that only some records define, and a
// projection naming something absent everywhere.
func TestColumnarProjectedReadsMatchRowPath(t *testing.T) {
	rows, cols := bothStorageForms(t)
	defer rows.Close()
	defer cols.Close()

	for _, proj := range [][]string{
		{"ClusterId"},
		{"ClusterId", "ProcId"},
		{"Owner"},                     // a string column
		{"Owner", "Cmd", "JobStatus"}, // strings and a numeric together
		{"RequestMemory"},             // sometimes an expression, so sometimes in the record
		{"Sometimes"},                 // only some records define it
		{"Slack", "Sometimes", "ClusterId"},
		{"NoSuchAttribute"}, // absent everywhere
		{"ClusterId", "NoSuchAttribute"},
	} {
		for _, expr := range []string{
			"true",                 // constant: projected straight from columns
			"ProcId < 5",           // decided from columns, then projected from them
			"Sometimes > 100",      // not a schema field: the match reassembles
			"RequestMemory > 4096", // sometimes an expression: some records reassemble
		} {
			want := readProjected(t, rows, expr, proj)
			got := readProjected(t, cols, expr, proj)
			if len(got) != len(want) {
				t.Errorf("%v / %s: %d rows, want %d", proj, expr, len(got), len(want))
				continue
			}
			bag := map[string]int{}
			for _, k := range want {
				bag[k]++
			}
			bad := 0
			for _, k := range got {
				bag[k]--
				if bag[k] < 0 {
					bad++
				}
			}
			if bad != 0 {
				t.Errorf("%v / %s: %d projected ads differ from the row path", proj, expr, bad)
			}
		}
	}
}

// TestColumnarizedSegmentKeepsGroupColumns guards a FEATURE INTERACTION, not a format detail.
//
// Group schemas (secondary columnar schemas for attributes only some ads carry) and columnar-native
// segments are both on by default, and they meet in the same place: the segment's columnar payload. If
// the rewrite builds that payload without the group schemas, and the sidecar then declines to build a
// block for a segment that already has one, the group accelerator silently disappears for every
// columnarized segment -- correct answers, quietly slower, and nothing anywhere says so.
func TestColumnarizedSegmentKeepsGroupColumns(t *testing.T) {
	dir := t.TempDir()
	// GroupStabilityRuns: 1 so one derivation promotes a group -- the default waits for members to
	// keep recurring across passes, which a single-shot test cannot produce.
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16,
		GroupSchemaCount: 4, GroupStabilityRuns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Two attribute bundles that travel together, which is what a group schema is for: one set on
	// records that "ran", another on records with container metadata.
	for i := range 4000 {
		text := fmt.Sprintf(`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d ]`,
			i, i%10, i%7, i%6, (i%16)*1024)
		switch i % 3 {
		case 0:
			text = fmt.Sprintf(`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; RanStart=%d; RanEnd=%d; RanHost="h%d"; RanExit=%d ]`,
				i, i%10, i%7, i%6, (i%16)*1024, i, i+10, i%50, i%3)
		case 1:
			text = fmt.Sprintf(`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; CtrImage="img%d"; CtrTag="t%d"; CtrDigest="d%d" ]`,
				i, i%10, i%7, i%6, (i%16)*1024, i%20, i%5, i%100)
		}
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.RetrainDict(0)
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Skip("schema scan did not enable")
	}
	st := c.schemaScan.Load()
	if st == nil || len(st.groups) == 0 {
		t.Skip("no group schema was derived")
	}
	t.Logf("%d group schemas derived", len(st.groups))

	withGroups, columnarized := 0, 0
	for _, seg := range c.shards[0].segs {
		if seg == nil || seg == c.shards[0].act {
			continue
		}
		if !seg.columnarized() {
			continue
		}
		columnarized++
		if cs := seg.colblk.Load(); cs != nil && len(cs.groups) > 0 {
			withGroups++
		}
	}
	if columnarized == 0 {
		t.Skip("no sealed segment was columnarized")
	}
	if withGroups != columnarized {
		t.Errorf("%d of %d columnarized segments carry group columns: the rewrite drops the group accelerator",
			withGroups, columnarized)
	}
}

// bundledGroupFixture builds ads with two attribute bundles that travel together, which is the shape a
// group schema exists for, storing them whole-record or columnarized.
//
// The order is deliberate: the groups are derived from a CLEAN sample, and only then are records added
// that carry part of a bundle. That is how partial membership actually arises -- a group is a property
// of the sample it was derived from, and the data drifts afterwards -- and it is the case the design
// turns on, because a partial member is not in the group's column and must keep its values in its own
// record. Adding the deviating records up front instead gives them their own presence vector, so they
// form a separate clean group and the partial path never runs at all (measured: zero exceptions).
func bundledGroupFixture(t *testing.T, columnarize bool) *Collection {
	t.Helper()
	budget := 0
	if !columnarize {
		budget = -1
	}
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16,
		GroupSchemaCount: 4, GroupStabilityRuns: 1, ColumnarSegmentBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	put := func(key string, text string) {
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(key), ad); err != nil {
			t.Fatal(err)
		}
	}
	base := func(i int) string {
		return fmt.Sprintf(`ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d`,
			i, i%10, i%7, i%6, (i%16)*1024)
	}
	for i := range 3000 {
		switch i % 3 {
		case 0:
			// Every so often a group value too wide for the slot its column sizes, so the group's
			// ESCAPE path runs -- the value goes to the block's cold tail rather than the column, and
			// reassembly has to read it from there. Without one of these that branch never executes.
			ranEnd := fmt.Sprintf("%d", i+10)
			if i%303 == 0 {
				ranEnd = "9223372036854775807"
			}
			host := fmt.Sprintf("h%d", i%50)
			if i%501 == 0 {
				host = strings.Repeat("wide-host-name-", 20)
			}
			put(fmt.Sprintf("%d.0", i), fmt.Sprintf(`[ %s; RanStart=%d; RanEnd=%s; RanHost=%q; RanExit=%d ]`,
				base(i), i, ranEnd, host, i%3))
		case 1:
			put(fmt.Sprintf("%d.0", i), fmt.Sprintf(`[ %s; CtrImage="img%d"; CtrTag="t%d"; CtrDigest="d%d" ]`,
				base(i), i%20, i%5, i%100))
		default:
			put(fmt.Sprintf("%d.0", i), fmt.Sprintf(`[ %s ]`, base(i)))
		}
	}
	c.RetrainDict(0)
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Skip("schema scan did not enable")
	}
	// Now the drift: records holding PART of each bundle, against groups already fixed.
	for i := 3000; i < 4200; i++ {
		switch i % 3 {
		case 0:
			put(fmt.Sprintf("%d.0", i), fmt.Sprintf(`[ %s; RanStart=%d; RanHost="h%d" ]`, base(i), i, i%50))
		case 1:
			put(fmt.Sprintf("%d.0", i), fmt.Sprintf(`[ %s; CtrImage="img%d" ]`, base(i), i%20))
		default:
			put(fmt.Sprintf("%d.0", i), fmt.Sprintf(`[ %s; RanEnd=%d; RanExit=%d ]`, base(i), i+10, i%3))
		}
	}
	// A second pass covers the segments that sealed since, using the SAME schema and groups.
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Skip("schema scan did not enable")
	}
	return c
}

// TestColumnarizedGroupAttributesRoundTrip is the equivalence test for group columns inside the
// segment, and it is the one that matters most: a group's attributes are REMOVED from the records of
// every ad that belongs to the group wholly, so if reassembly does not put them back they are gone --
// from a table whose other attributes all look right.
//
// The comparison is byte-sensitive and covers both whole-ad and projected reads, and the fixture
// deliberately includes PARTIAL members, whose values stay in the record and must therefore appear
// exactly once rather than twice or not at all.
func TestColumnarizedGroupAttributesRoundTrip(t *testing.T) {
	rows := bundledGroupFixture(t, false)
	defer rows.Close()
	cols := bundledGroupFixture(t, true)
	defer cols.Close()

	withGroups := 0
	for _, seg := range cols.shards[0].segs {
		if seg != nil && seg.columnarized() {
			if cs := seg.colblk.Load(); cs != nil && len(cs.groups) > 0 {
				withGroups++
			}
		}
	}
	if withGroups == 0 {
		t.Skip("no columnarized segment carries group columns")
	}
	t.Logf("%d columnarized segments carry group columns", withGroups)

	compare := func(what string, want, got []string) {
		if len(got) != len(want) {
			t.Errorf("%s: %d rows, want %d", what, len(got), len(want))
			return
		}
		bag := map[string]int{}
		for _, k := range want {
			bag[k]++
		}
		bad := 0
		for _, k := range got {
			bag[k]--
			if bag[k] < 0 {
				bad++
			}
		}
		if bad != 0 {
			t.Errorf("%s: %d ads differ from the whole-record form", what, bad)
		}
	}
	for _, expr := range []string{
		"true",
		"RanExit == 0",          // a group attribute in the predicate
		"RanHost == \"h7\"",     // a group string attribute
		"CtrTag == \"t3\"",      // the other group
		"RanStart is undefined", // records with none of the group
		"RanEnd is undefined",   // partial members lack this one specifically
		"ProcId < 5",            // a base field, so group values come back via reassembly
	} {
		compare("full/"+expr, readAll(t, rows, expr), readAll(t, cols, expr))
	}
	for _, proj := range [][]string{
		{"RanStart", "RanEnd"},
		{"RanHost", "CtrImage"},
		{"ClusterId", "RanExit"},
		{"CtrImage", "CtrTag", "CtrDigest"},
	} {
		for _, expr := range []string{"true", "ProcId < 5"} {
			compare(fmt.Sprintf("proj%v/%s", proj, expr),
				readProjected(t, rows, expr, proj), readProjected(t, cols, expr, proj))
		}
	}
}
