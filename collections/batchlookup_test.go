package collections

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// batchRow identifies one record of the lookup fixture exactly, so a comparison between the
// batched path and the one-at-a-time path is over records and not over a count. A boundary that
// drops or duplicates one record disappears in a count and does not disappear here.
type batchRow struct{ cluster, proc, run int64 }

func batchRowOf(tb testing.TB, ad *classad.ClassAd) batchRow {
	tb.Helper()
	get := func(name string) int64 {
		v, ok := ad.EvaluateAttrInt(name)
		if !ok {
			tb.Fatalf("a row carried no integer %s", name)
		}
		return v
	}
	return batchRow{get("ClusterId"), get("ProcId"), get("RunInstanceId")}
}

// batchFixture is ONE archive of the epoch shape, shared by every arm. Rebuilt per arm it has
// silently come back empty and benchmarked beautifully, so it is built once and every arm
// asserts what it matched.
type batchFixture struct {
	a        *Archive
	segments int
}

func buildBatchFixture(tb testing.TB) *batchFixture {
	tb.Helper()
	a := buildTileArchive(tb, tb.TempDir(), "epoch", tileSorted, 0x33)
	segs := tileSegments(tb, a, "ClusterId")
	if len(segs) < 60 {
		tb.Fatalf("fixture has %d segments; the per-key floor this measures is O(segments)", len(segs))
	}
	return &batchFixture{a: a, segments: len(segs)}
}

// batchKeysAt returns n keys starting at cluster start, which is the shape a join batch has:
// consecutive rows of an insertion-ordered outer side carry a contiguous run of keys.
func batchKeysAt(start, n int) []int64 {
	keys := make([]int64, n)
	for i := range keys {
		keys[i] = int64((start + i) % tileClusters)
	}
	return keys
}

func collectBatch(tb testing.TB, seq func(func(*classad.ClassAd) bool)) []batchRow {
	tb.Helper()
	var out []batchRow
	for ad := range seq {
		out = append(out, batchRowOf(tb, ad))
	}
	return out
}

func sortRows(rs []batchRow) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].cluster != rs[j].cluster {
			return rs[i].cluster < rs[j].cluster
		}
		if rs[i].proc != rs[j].proc {
			return rs[i].proc < rs[j].proc
		}
		return rs[i].run < rs[j].run
	})
}

// TestLookupKeysEqualsOneAtATime is the correctness bar: a batched lookup must return exactly
// what the same keys looked up one at a time return. It is checked over EVERY record of the
// fixture, in batches that tile the whole key space, so no record is left untested and no
// boundary between batches is skipped.
func TestLookupKeysEqualsOneAtATime(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-hundred-segment archive")
	}
	f := buildBatchFixture(t)
	seen := map[batchRow]int{}
	total := 0
	for start := 0; start < tileClusters; start += 64 {
		keys := batchKeysAt(start, min(64, tileClusters-start))
		batched := collectBatch(t, f.a.LookupKeys("ClusterId", keys))

		var single []batchRow
		for _, k := range keys {
			single = append(single, collectBatch(t, f.a.LookupKeys("ClusterId", []int64{k}))...)
		}
		if len(batched) != len(single) {
			t.Fatalf("keys [%d..%d]: batched returned %d rows, one-at-a-time %d",
				keys[0], keys[len(keys)-1], len(batched), len(single))
		}
		if len(batched) == 0 {
			t.Fatalf("keys [%d..%d] matched nothing; the arm is measuring nothing", keys[0], keys[len(keys)-1])
		}
		b, s := append([]batchRow(nil), batched...), append([]batchRow(nil), single...)
		sortRows(b)
		sortRows(s)
		for i := range b {
			if b[i] != s[i] {
				t.Fatalf("keys [%d..%d] row %d: batched %v, one-at-a-time %v",
					keys[0], keys[len(keys)-1], i, b[i], s[i])
			}
		}
		// The batch's own order must be the archive's read order restricted to the batch,
		// which is newest-first: the join streams it and a reordering would surface there.
		for i := 1; i < len(batched); i++ {
			if batched[i-1].cluster < batched[i].cluster {
				t.Fatalf("keys [%d..%d]: cluster %d came before %d; the batch is not in read order",
					keys[0], keys[len(keys)-1], batched[i-1].cluster, batched[i].cluster)
			}
		}
		for _, r := range batched {
			seen[r]++
		}
		total += len(batched)
	}
	// Every record of the archive was reached exactly once across the tiling batches.
	all, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	scan := map[batchRow]int{}
	for ad := range f.a.Query(all) {
		scan[batchRowOf(t, ad)]++
	}
	if len(scan) == 0 {
		t.Fatal("the full scan returned nothing")
	}
	if len(seen) != len(scan) {
		t.Fatalf("the batches reached %d distinct records, the full scan %d", len(seen), len(scan))
	}
	for r, n := range scan {
		if seen[r] != n {
			t.Fatalf("record %v: batches saw it %d times, the full scan %d", r, seen[r], n)
		}
	}
	t.Logf("%d records reached by %d-key batches over %d segments", total, 64, f.segments)
}

// TestLookupKeysAgreesWithTheConstraintPath checks the batched lookup against the ordinary
// query path for the same constraint -- the path that is correct by construction and that the
// lookup deliberately plans differently. They must return the same records whether or not the
// constraint path reaches an index for that batch size.
func TestLookupKeysAgreesWithTheConstraintPath(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-hundred-segment archive")
	}
	f := buildBatchFixture(t)
	for _, n := range []int{1, 2, 8, 33, 64, 256, 1024} {
		keys := batchKeysAt(4096, n)
		q := BatchKeyQuery("ClusterId", keys)
		want := collectBatch(t, f.a.Query(q))
		got := collectBatch(t, f.a.LookupKeys("ClusterId", keys))
		if len(want) == 0 {
			t.Fatalf("n=%d: the constraint path matched nothing", n)
		}
		if len(got) != len(want) {
			t.Fatalf("n=%d: lookup returned %d rows, the constraint path %d", n, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("n=%d row %d: lookup %v, constraint path %v", n, i, got[i], want[i])
			}
		}
	}
}

// TestLookupKeysSegmentsAreHoisted measures what the hoist is worth as a count rather than as a
// time: the segments a batch's span leaves, against the segments the ordinary constraint path's
// plan reaches for the same keys. It is also where the pushdown gate's blindness shows up --
// past some batch size the constraint path stops pruning at all, and the span-restricted lookup
// does not.
func TestLookupKeysSegmentsAreHoisted(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-hundred-segment archive")
	}
	f := buildBatchFixture(t)
	t.Logf("archive: %d segments", f.segments)
	gateFlipped := 0
	for _, n := range []int{1, 2, 4, 8, 16, 32, 48, 64, 128, 256, 512, 1024} {
		keys := batchKeysAt(4096, n)
		surviving, pruned := f.a.LookupKeysSegments("ClusterId", keys)
		if surviving+pruned != f.segments {
			t.Fatalf("n=%d: %d surviving + %d pruned != %d segments", n, surviving, pruned, f.segments)
		}
		ex := f.a.ExplainQuery(BatchKeyQuery("ClusterId", keys))
		planned := ex.IndexSegments + ex.IndexSkippedSegments + ex.UnindexedSegments
		if ex.Plan != "indexed" && ex.Plan != "indexed-union" && ex.Plan != "composite" {
			gateFlipped++
		}
		t.Logf("n=%4d: span-hoisted lookup reads %3d of %d segments; constraint path plan=%q reads %3d (zone-pruned %d)",
			n, surviving, f.segments, ex.Plan, planned, ex.ZonePrunedSegments)
	}
	if gateFlipped == 0 {
		t.Log("the constraint path reached an index at every batch size measured")
	}
}

// TestLookupKeysPrunesSomething guards the measurement above against a fixture with no usable
// zone maps, where every arm would read everything and the hoist would look free.
func TestLookupKeysPrunesSomething(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-hundred-segment archive")
	}
	f := buildBatchFixture(t)
	surviving, pruned := f.a.LookupKeysSegments("ClusterId", batchKeysAt(4096, 8))
	if pruned == 0 {
		t.Fatalf("an 8-key batch pruned no segment of %d; the fixture has no usable zone maps", f.segments)
	}
	if surviving == 0 {
		t.Fatal("an 8-key batch pruned every segment; it would match nothing")
	}
	// And the READ must actually skip them. LookupKeysSegments predicts what the lookup will
	// touch; the stats report what it did. A prediction checked against a second copy of the
	// rule rather than against the read would hold while the read pruned nothing.
	var st ScanStats
	rows := 0
	for range f.a.LookupKeysStats("ClusterId", batchKeysAt(4096, 8), &st) {
		rows++
	}
	if rows == 0 {
		t.Fatal("the 8-key batch matched no rows")
	}
	if st.SegmentsScanned != surviving || st.SegmentsPruned != pruned {
		t.Fatalf("the lookup walked %d segments and pruned %d; LookupKeysSegments predicted %d and %d",
			st.SegmentsScanned, st.SegmentsPruned, surviving, pruned)
	}
	if st.SegmentsPruned == 0 {
		t.Fatalf("the lookup pruned no segment of %d; the hoisted zone check did nothing", f.segments)
	}
	t.Logf("8 keys: %d of %d segments walked, %d pruned, %d rows",
		st.SegmentsScanned, f.segments, st.SegmentsPruned, rows)
}

// TestLookupKeysBatchSweep is the price measurement: nanoseconds PER KEY against the batch
// size, for the three ways the inner side of a nested loop could be read.
//
// Arms are run in both orders each round and the median of the rounds is reported, because an
// A/B that only ever runs A-then-B measures ordering bias. Every arm asserts the rows it
// matched: an arm that matched nothing is not a fast arm.
func TestLookupKeysBatchSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("times reads over a multi-hundred-segment archive")
	}
	f := buildBatchFixture(t)
	sizes := []int{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048}
	const rounds = 5

	run := func(arm string, keys []int64) (time.Duration, int) {
		rows := 0
		start := time.Now()
		switch arm {
		case "single":
			for _, k := range keys {
				for range f.a.LookupKeys("ClusterId", []int64{k}) {
					rows++
				}
			}
		case "constraint":
			for range f.a.Query(BatchKeyQuery("ClusterId", keys)) {
				rows++
			}
		case "hoisted":
			for range f.a.LookupKeys("ClusterId", keys) {
				rows++
			}
		}
		return time.Since(start), rows
	}

	type key struct {
		arm string
		n   int
	}
	samples := map[key][]time.Duration{}
	matched := map[key]int{}
	arms := []string{"single", "constraint", "hoisted"}
	for r := 0; r < rounds; r++ {
		order := arms
		if r%2 == 1 { // flip the arm order every other round
			order = []string{"hoisted", "constraint", "single"}
		}
		for _, n := range sizes {
			keys := batchKeysAt(4096+r*997, n)
			for _, arm := range order {
				d, rows := run(arm, keys)
				k := key{arm, n}
				samples[k] = append(samples[k], d)
				if prev, ok := matched[k]; ok && prev != rows {
					t.Fatalf("%s n=%d matched %d rows this round and %d last; the arms are not comparable",
						arm, n, rows, prev)
				}
				matched[k] = rows
			}
		}
	}
	for _, n := range sizes {
		line := fmt.Sprintf("n=%4d rows=%6d", n, matched[key{"hoisted", n}])
		for _, arm := range arms {
			k := key{arm, n}
			if matched[k] == 0 {
				t.Fatalf("%s n=%d matched 0 rows; the arm is measuring nothing", arm, n)
			}
			if matched[k] != matched[key{"hoisted", n}] {
				t.Fatalf("n=%d: %s matched %d rows, hoisted %d", n, arm, matched[k], matched[key{"hoisted", n}])
			}
			med := medianOf(samples[k])
			line += fmt.Sprintf("  %-10s %8.1f us (%7.1f us/key)", arm,
				float64(med.Microseconds()), float64(med.Nanoseconds())/float64(n)/1000)
		}
		t.Log(line)
	}
}

func medianOf(ds []time.Duration) time.Duration {
	c := append([]time.Duration(nil), ds...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

// TestBatchGateOverestimatesByTheSegmentCount is the pushdown gate's behaviour on a key
// batch, measured rather than argued, because it decides whether a batch can be expressed as
// an ordinary constraint.
//
// The gate was expected to be blind to zone pruning. It is not: estimateCandidates asks each
// segment index, and a segment whose min/max rules the probe out already contributes nothing
// (indexCanSkip). What the estimate cannot express is that a key lives in ONE of the segments
// its span leaves, not in all of them -- a value index keeps no per-segment membership filter,
// so every surviving segment is priced as though it held every key of the batch. The
// over-estimate is therefore about the surviving segment count, and it grows with the batch
// until the gate declines the index.
//
// Hoisting the zone check does not move that: the segment set the estimate runs over is
// already the pruned one. What a batched lookup can do is decline the gate knowingly, having
// established the restriction itself -- which is what LookupKeys does, and what this measures
// the ceiling of.
func TestBatchGateOverestimatesByTheSegmentCount(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-hundred-segment archive")
	}
	f := buildBatchFixture(t)
	c := f.a.c
	for _, n := range []int{8, 64, 512, 1024} {
		keys := batchKeysAt(4096, n)
		q := BatchKeyQuery("ClusterId", keys)
		usable := c.planIndex(q.Probes())
		if len(usable) != 1 {
			t.Fatalf("n=%d: the batch planned %d usable probes, not one", n, len(usable))
		}
		frac, estimable := c.planFrac([][]usableProbe{usable})
		if !estimable {
			t.Fatalf("n=%d: the batch's candidate fraction is not estimable; the gate would not fire either way", n)
		}
		rows := 0
		for range f.a.LookupKeys("ClusterId", keys) {
			rows++
		}
		if rows == 0 {
			t.Fatalf("n=%d matched nothing", n)
		}
		surviving, _ := f.a.LookupKeysSegments("ClusterId", keys)
		truth := float64(rows) / float64(c.Len())
		t.Logf("n=%4d: estimated %.4f of the collection, truly %.4f (%d rows of %d); %d segments survive; over by %.1fx; gate=%.2f declines=%v",
			n, frac, truth, rows, c.Len(), surviving, frac/truth, maxPushdownFrac, frac > maxPushdownFrac)
		// The estimate is a bound the gate declines on, so it must never fall materially
		// below the truth -- that would decline too late rather than too early, and is a
		// different failure from the one measured here.
		if frac < 0.9*truth {
			t.Errorf("n=%d: the estimate (%.4f) is below the truth (%.4f)", n, frac, truth)
		}
		if n == 1024 {
			if frac <= maxPushdownFrac {
				t.Errorf("n=1024: the gate estimated %.4f and did not decline; the measurement below it is moot", frac)
			}
			if truth > maxPushdownFrac {
				t.Errorf("n=1024: the batch truly covers %.4f of the collection, so declining the index is correct and there is nothing to report", truth)
			}
			if frac/truth < 2 {
				t.Errorf("n=1024: the estimate is only %.1fx the truth; the over-count this records is gone", frac/truth)
			}
		}
	}
}

// TestLookupKeysWithoutAnIndex covers the fallback the batched path must have: an attribute
// with no index and no zone map. Nothing can be ruled out, so every segment is walked and the
// matcher decides every record -- slower than a lookup, but the same answer as the ordinary
// query path, which is the part that matters.
func TestLookupKeysWithoutAnIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-hundred-segment archive")
	}
	f := buildBatchFixture(t)
	// RunInstanceId is written on every record but is neither a ValueAttr nor a ZoneAttr of
	// the fixture, so it has neither postings nor a span.
	const attr = "RunInstanceId"
	keys := []int64{0, 2}
	surviving, pruned := f.a.LookupKeysSegments(attr, keys)
	if pruned != 0 {
		t.Fatalf("%s has no zone map, yet the batch pruned %d segments", attr, pruned)
	}
	if surviving != f.segments {
		t.Fatalf("%s: %d of %d segments survive; an unzoned key must leave all of them", attr, surviving, f.segments)
	}
	want := collectBatch(t, f.a.Query(BatchKeyQuery(attr, keys)))
	got := collectBatch(t, f.a.LookupKeys(attr, keys))
	if len(want) == 0 {
		t.Fatalf("the constraint path matched nothing on %s", attr)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: lookup returned %d rows, the constraint path %d", attr, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s row %d: lookup %v, constraint path %v", attr, i, got[i], want[i])
		}
	}
	t.Logf("%s: %d rows over all %d segments, no pruning", attr, len(got), f.segments)
}
