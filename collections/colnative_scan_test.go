package collections

import (
	"context"
	"fmt"
	"testing"

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
	c.colNativeEnabled = true
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
	if !c.BuildAndEnableSchemaScan(4096, 8) {
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
