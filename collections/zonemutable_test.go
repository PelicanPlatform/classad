package collections

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// ZONE MAPS ON A MUTABLE COLLECTION.
//
// A zone prunes a whole segment, so getting it wrong does not make a query slow, it makes
// it return too few rows -- and a query that silently loses records looks exactly like a
// query whose records were never there. So the bar here is a differential one: the SAME
// data and the SAME queries against a collection that prunes and one that cannot, with the
// unzoned answer as the oracle.
//
// The cases that matter are the ones where a mutable collection differs from an append
// log: a value UPDATED out of its old segment's span, a key deleted, and an attribute that
// is sometimes absent, sometimes a string, and sometimes an expression.

// mutZoneFixture writes the same records into a collection with zone maps and one without,
// and returns both. zoned gets ValueAttrs (which zone automatically); plain gets neither,
// so it reads every record and is the oracle.
func mutZoneFixture(t *testing.T, write func(put func(key, ad string))) (zoned, plain *Collection) {
	t.Helper()
	mk := func(opts Options) *Collection {
		opts.Shards, opts.SegmentSize, opts.Dir = 1, 1<<13, t.TempDir()
		c, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	zoned = mk(Options{ValueAttrs: []string{"QDate"}, ZoneAttrs: []string{"QDate"}})
	plain = mk(Options{})
	write(func(key, ad string) {
		for _, c := range []*Collection{zoned, plain} {
			if err := c.Put([]byte(key), mustAd(t, ad)); err != nil {
				t.Fatal(err)
			}
		}
	})
	zoned.Reindex()
	plain.Reindex()
	return zoned, plain
}

// zoneRows runs q and returns the matching records' Id attributes, sorted. Id rather than
// the storage key because Query yields ads; it identifies a record just as well and a
// failure then names the record that went missing.
func zoneRows(t *testing.T, c *Collection, q string) []string {
	t.Helper()
	ex, err := classad.ParseExpr("Id")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for ad := range c.Query(mustQuery(t, q)) {
		n, verr := ex.Eval(ad).IntValue()
		if verr != nil {
			t.Fatalf("a matched record has no Id: %v", verr)
		}
		out = append(out, fmt.Sprintf("%06d", n))
	}
	sort.Strings(out)
	return out
}

// A pruned collection must answer exactly what an unpruned one answers, through updates
// that move a value out of its segment's span and through deletes.
func TestZoneMutableAnswersWhatAnUnzonedCollectionAnswers(t *testing.T) {
	zoned, plain := mutZoneFixture(t, func(put func(key, ad string)) {
		for i := 0; i < 4000; i++ {
			put(fmt.Sprintf("k%d", i), fmt.Sprintf(`[ Id=%d; QDate=%d ]`, i, 1000000+i))
		}
		// Updates that move a key's QDate FAR outside the span of the segment its first
		// version sealed into. The old segment's zone still covers the old value, which is
		// what makes this the case a wrong zone would get wrong: the key must be found
		// under its NEW value and must not be found under its old one.
		for i := 0; i < 400; i++ {
			put(fmt.Sprintf("k%d", i*10), fmt.Sprintf(`[ Id=%d; QDate=%d ]`, i*10, 9000000+i))
		}
		// And a few moved the other way, below everything.
		for i := 0; i < 50; i++ {
			put(fmt.Sprintf("k%d", 3000+i), fmt.Sprintf(`[ Id=%d; QDate=%d ]`, 3000+i, 1000+i))
		}
	})
	for i := 0; i < 200; i++ {
		key := []byte(fmt.Sprintf("k%d", 2000+i))
		if !zoned.Delete(key) || !plain.Delete(key) {
			t.Fatalf("delete of %s did not remove a record", key)
		}
	}

	for _, q := range []string{
		"QDate > 9000000",  // only the updated keys, none in their original segment
		"QDate >= 9000000", //
		"QDate < 2000",     // only the keys moved below everything
		"QDate > 1003900",  // a tail range, the shape the pruning is for
		"QDate >= 1000000 && QDate <= 1000500",
		"QDate == 1000777", // equality prunes on a zone too
		"QDate != 1000777", // and a negated one must prune nothing
		"QDate > 1000000 && QDate < 9000000",
		"QDate >= 0", // everything
	} {
		got, want := zoneRows(t, zoned, q), zoneRows(t, plain, q)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: zoned returned %d keys, unzoned %d\n  missing/extra around: %s",
				q, len(got), len(want), zoneDiff(got, want))
		}
	}
}

// zoneDiff names the first key the two answers disagree on, so a failure says WHICH
// record was lost rather than only how many.
func zoneDiff(got, want []string) string {
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			return fmt.Sprintf("got %q, want %q", got[i], want[i])
		}
	}
	if len(got) < len(want) {
		return fmt.Sprintf("missing %q", want[len(got)])
	}
	if len(want) < len(got) {
		return fmt.Sprintf("extra %q", got[len(want)])
	}
	return "(none)"
}

// A zone attribute that is not always a number must not prune on the records it cannot
// describe. An expression is the sharp case: its value is not known until the record is
// evaluated, so a segment holding one can bound nothing.
func TestZoneMutableHandlesNonNumericValues(t *testing.T) {
	zoned, plain := mutZoneFixture(t, func(put func(key, ad string)) {
		for i := 0; i < 3000; i++ {
			switch i % 10 {
			case 7: // absent
				put(fmt.Sprintf("k%d", i), fmt.Sprintf(`[ Id=%d ]`, i))
			case 8: // a string, which no numeric comparison is true of
				put(fmt.Sprintf("k%d", i), fmt.Sprintf(`[ Id=%d; QDate="nope" ]`, i))
			default:
				put(fmt.Sprintf("k%d", i), fmt.Sprintf(`[ Id=%d; QDate=%d ]`, i, 1000000+i))
			}
		}
	})
	// The negated orderings are the ones that need `sparse`: a segment where some record
	// holds no number cannot answer `!(QDate < x)` from its [min,max] alone, because the
	// range cannot show whether it also holds a value no comparison is true of. Plain
	// ranges never consult that, so a test without these does not pin it at all.
	for _, q := range []string{
		"QDate > 1002900", "QDate < 1000100", "QDate == 1001234",
		"QDate is undefined", "QDate isnt undefined",
		"!(QDate < 1002900)", "!(QDate >= 1000100)",
		"!(QDate > 1002900)", "!(QDate <= 1000100)",
	} {
		got, want := zoneRows(t, zoned, q), zoneRows(t, plain, q)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: zoned %d keys, unzoned %d (%s)", q, len(got), len(want), zoneDiff(got, want))
		}
	}
}

// A segment holding an EXPRESSION for the zone attribute gets no zone at all, so nothing
// in it is pruned. Separate from the case above because it is the one where a zone would
// be unsound rather than merely incomplete: the value is not known until evaluation.
func TestZoneMutableRefusesToBoundAnExpression(t *testing.T) {
	zoned, plain := mutZoneFixture(t, func(put func(key, ad string)) {
		for i := 0; i < 2000; i++ {
			if i == 1500 {
				// Evaluates to a number well outside every literal span below.
				put(fmt.Sprintf("k%d", i), fmt.Sprintf(`[ Id=%d; QDate = 9000000 + 1 ]`, i))
				continue
			}
			put(fmt.Sprintf("k%d", i), fmt.Sprintf(`[ Id=%d; QDate=%d ]`, i, 1000000+i))
		}
	})
	got, want := zoneRows(t, zoned, "QDate > 8000000"), zoneRows(t, plain, "QDate > 8000000")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("an expression-valued record was pruned away: zoned %v, unzoned %v", got, want)
	}
	if len(want) == 0 {
		t.Fatal("the fixture's expression record matched nothing, so this proves nothing")
	}
}

// The pruning has to actually happen, or every test above passes against a collection that
// simply never prunes.
func TestZoneMutableActuallyPrunes(t *testing.T) {
	zoned, _ := mutZoneFixture(t, func(put func(key, ad string)) {
		for i := 0; i < 5000; i++ {
			put(fmt.Sprintf("k%d", i), fmt.Sprintf(`[ Id=%d; QDate=%d ]`, i, 1000000+i))
		}
	})
	ex := zoned.ExplainQuery(mustQuery(t, "QDate > 1004900"))
	segs := zoned.Stats().Segments
	if segs < 10 {
		t.Fatalf("the fixture made %d segments, too few for pruning to mean anything", segs)
	}
	if ex.ZonePrunedSegments == 0 {
		t.Fatalf("a mutable collection pruned no segments of %d; zone maps are not running", segs)
	}
	if ex.ZonePrunedSegments < segs/2 {
		t.Errorf("pruned only %d of %d segments on a tail range", ex.ZonePrunedSegments, segs)
	}
}

// Compaction is the one path that fills a new segment that is sealed from birth. Its
// output must be zoned, or pruning works until the first maintenance pass and then stops.
func TestZonePruningSurvivesCompaction(t *testing.T) {
	zoned, plain := mutZoneFixture(t, func(put func(key, ad string)) {
		for i := 0; i < 5000; i++ {
			put(fmt.Sprintf("k%d", i), fmt.Sprintf(`[ Id=%d; QDate=%d ]`, i, 1000000+i))
		}
		// Rewrite EVERY key, twice. That leaves the original segments entirely dead, so
		// compaction's sources are nearly the whole collection and its output is nearly
		// every live record -- which is what makes this able to tell a zoned output from
		// an unzoned one. A light churn leaves most segments untouched and still pruning,
		// and the test then passes whether or not compaction zones anything.
		for round := 0; round < 2; round++ {
			for i := 0; i < 5000; i++ {
				put(fmt.Sprintf("k%d", i), fmt.Sprintf(`[ Id=%d; QDate=%d ]`, i, 1000000+i))
			}
		}
	})
	const q = "QDate > 1004900"
	want := zoneRows(t, plain, q)
	before := zoned.ExplainQuery(mustQuery(t, q)).ZonePrunedSegments
	segsBefore := zoned.Stats().Segments
	if zoned.Compact() == 0 {
		t.Fatal("nothing compacted, so this does not test the compacted path")
	}
	zoned.Reindex()
	segsAfter := zoned.Stats().Segments

	if got := zoneRows(t, zoned, q); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("after compaction: %d keys, want %d (%s)", len(got), len(want), zoneDiff(got, want))
	}
	after := zoned.ExplainQuery(mustQuery(t, q)).ZonePrunedSegments
	t.Logf("segments %d -> %d, pruned %d -> %d", segsBefore, segsAfter, before, after)
	// Proportional, not absolute: compaction reduces the segment count, so the number
	// pruned falls even when every output segment is zoned. What must not happen is the
	// FRACTION collapsing, which is what an unzoned output looks like.
	if segsAfter == 0 {
		t.Fatal("no segments left after compaction")
	}
	if frac := float64(after) / float64(segsAfter); frac < 0.5 {
		t.Errorf("after compaction only %d of %d segments prune (%.0f%%); compaction did not "+
			"zone its output", after, segsAfter, 100*frac)
	}
}

// Zones are recomputed on reopen for every non-active segment, which must hold for a
// mutable collection too -- otherwise pruning works for one process lifetime.
func TestZonePruningSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Shards: 1, SegmentSize: 1 << 13, Dir: dir,
		ValueAttrs: []string{"QDate"}, ZoneAttrs: []string{"QDate"}}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		if perr := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[ Id=%d; QDate=%d ]`, i, 1000000+i))); perr != nil {
			t.Fatal(perr)
		}
	}
	c.Reindex()
	wantRows := zoneRows(t, c, "QDate > 1004900")
	c.Close()

	c2, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if got := zoneRows(t, c2, "QDate > 1004900"); strings.Join(got, ",") != strings.Join(wantRows, ",") {
		t.Errorf("after reopen: %d keys, want %d (%s)", len(got), len(wantRows), zoneDiff(got, wantRows))
	}
	if ex := c2.ExplainQuery(mustQuery(t, "QDate > 1004900")); ex.ZonePrunedSegments == 0 {
		t.Error("a reopened mutable collection prunes nothing; loadShard did not zone its segments")
	}
}

// AddIndex at runtime starts zoning a mutable collection's LATER segments.
//
// Not its earlier ones: those are already sealed, and zoning them would mean re-walking
// the whole collection. That is the same partial-coverage rule the postings follow -- an
// index covers a prefix and the rest is scanned -- so the answer is unchanged either way
// and only the pruning differs. The assertion is therefore that segments sealed AFTER the
// call prune, not that every segment does.
func TestZoneMutableAddIndexZonesLaterSegments(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 13})
	put := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
				mustAd(t, fmt.Sprintf(`[ Id=%d; QDate=%d ]`, i, 1000000+i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	put(0, 5000)
	const q = "QDate > 1008900"
	if pruned := c.ExplainQuery(mustQuery(t, q)).ZonePrunedSegments; pruned != 0 {
		t.Fatalf("an unindexed collection pruned %d segments before AddIndex", pruned)
	}

	if !c.AddIndex(nil, []string{"QDate"}) {
		t.Fatal("AddIndex reported no change")
	}
	put(5000, 10000) // these seal with the zone attributes configured
	c.Reindex()

	got := zoneRows(t, c, q)
	var want []string
	for i := 1008901 - 1000000; i < 10000; i++ {
		want = append(want, fmt.Sprintf("%06d", i))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("AddIndex changed the answer: %d keys, want %d (%s)",
			len(got), len(want), zoneDiff(got, want))
	}
	if ex := c.ExplainQuery(mustQuery(t, q)); ex.ZonePrunedSegments == 0 {
		t.Errorf("no segment prunes after AddIndex, so the later ones were sealed without "+
			"zone maps (%d segments)", c.Stats().Segments)
	}
}
