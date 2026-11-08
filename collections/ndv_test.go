package collections

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// ndvStore builds a collection indexing Owner categorically and Memory by value, and
// writes the ads parsed from each of texts. segSize sized small enough makes several
// segments, which is what moves the answer off the exact path.
func ndvStore(t *testing.T, segSize int, texts []string) *Collection {
	t.Helper()
	c := New(Options{
		Shards: 1, SegmentSize: segSize,
		CategoricalAttrs: []string{"Owner", "Tags"},
		ValueAttrs:       []string{"Memory"},
	})
	for i, txt := range texts {
		ad, err := classad.Parse(txt)
		if err != nil {
			t.Fatalf("parse %q: %v", txt, err)
		}
		if err := c.Update([]AdUpdate{{Key: []byte("k" + strconv.Itoa(i)), Ad: ad}}); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	return c
}

// TestNDVAbsentIsNotOneDistinct is the distinction the whole return signature exists for.
// An attribute nothing indexes yields no estimate at all; an attribute every record agrees
// on yields exactly one. A planner that cannot tell them apart treats the unknown key as
// the degenerate one, or the degenerate one as unknown, and both pick the wrong plan.
func TestNDVAbsentIsNotOneDistinct(t *testing.T) {
	t.Parallel()
	c := ndvStore(t, 64<<20, []string{
		`[ Owner="alice"; Memory=1024; Project="atlas" ]`,
		`[ Owner="alice"; Memory=2048; Project="atlas" ]`,
	})

	one, ok := c.AttrNDV("Owner")
	if !ok {
		t.Fatal("Owner is indexed; it must have an estimate")
	}
	if one.Distinct != 1 {
		t.Errorf("Owner distinct = %d, want 1", one.Distinct)
	}
	if one.Accuracy != NDVExact {
		t.Errorf("Owner accuracy = %q, want %q", one.Accuracy, NDVExact)
	}

	// Project is in every record and indexed by nothing. "No statistic" is the answer,
	// and it must not arrive as a zero or a one.
	if est, ok := c.AttrNDV("Project"); ok {
		t.Errorf("Project is unindexed; got an estimate %+v", est)
	}
	// An attribute no record carries at all is the same answer for the same reason.
	if est, ok := c.AttrNDV("NoSuchAttribute"); ok {
		t.Errorf("an unknown attribute got an estimate %+v", est)
	}
}

// TestNDVExactOnOneSegment pins the exact source: when one segment's index holds every
// distinct value, its own posting count is the answer and nothing is sketched.
func TestNDVExactOnOneSegment(t *testing.T) {
	t.Parallel()
	var texts []string
	for i := 0; i < 200; i++ {
		texts = append(texts, fmt.Sprintf(`[ Owner="user%d"; Memory=%d ]`, i%37, (i%11)*512))
	}
	c := ndvStore(t, 64<<20, texts)

	est, ok := c.AttrNDV("Owner")
	if !ok {
		t.Fatal("no estimate")
	}
	if est.Accuracy != NDVExact || est.Err != NDVErrNone {
		t.Fatalf("accuracy/err = %q/%q, want exact/none (segments=%d uncovered=%d)",
			est.Accuracy, est.Err, est.Segments, est.Uncovered)
	}
	if est.Distinct != 37 {
		t.Errorf("Owner distinct = %d, want 37", est.Distinct)
	}
	if est.RelErr != 0 {
		t.Errorf("an exact answer carries RelErr %v, want 0", est.RelErr)
	}
	if est.Records != 200 {
		t.Errorf("records = %d, want 200", est.Records)
	}
	if mem, _ := c.AttrNDV("Memory"); mem.Distinct != 11 || mem.Accuracy != NDVExact {
		t.Errorf("Memory = %d/%q, want 11/exact", mem.Distinct, mem.Accuracy)
	}
}

// TestNDVSketchAcrossSegments pins the sketch source: several segments cannot have their
// distinct counts added (they share values), so the merged HyperLogLog answers, the class
// says so, and the answer stays inside the bounds the exact per-segment counts give.
func TestNDVSketchAcrossSegments(t *testing.T) {
	t.Parallel()
	var texts []string
	for i := 0; i < 4000; i++ {
		texts = append(texts, fmt.Sprintf(`[ Owner="user%04d"; Memory=%d ]`, i%500, (i%11)*512))
	}
	c := ndvStore(t, 32<<10, texts)
	if segs := c.Stats().Segments; segs < 2 {
		t.Fatalf("want several segments, got %d", segs)
	}

	est, ok := c.AttrNDV("Owner")
	if !ok {
		t.Fatal("no estimate")
	}
	if est.Accuracy != NDVEstimated || est.Err != NDVErrEither {
		t.Fatalf("accuracy/err = %q/%q, want estimate/either", est.Accuracy, est.Err)
	}
	if est.RelErr <= 0 {
		t.Errorf("a sketched answer must quantify its error; RelErr = %v", est.RelErr)
	}
	// 500 distinct owners spread over many segments: well inside the sketch's accuracy.
	if est.Distinct < 450 || est.Distinct > 550 {
		t.Errorf("Owner distinct = %d, want ~500", est.Distinct)
	}
	// A value every segment holds must not be counted once per segment.
	mem, _ := c.AttrNDV("Memory")
	if mem.Distinct > 20 {
		t.Errorf("Memory distinct = %d: 11 values shared by every segment were double counted", mem.Distinct)
	}
}

// TestNDVLowerBoundWithoutSketch pins the bound source. A segment summary written before
// distinct-count sketches were kept has an exact count and no registers, so the union of
// several cannot be estimated -- only floored by the largest. The answer must say that
// rather than adding counts that overlap.
func TestNDVLowerBoundWithoutSketch(t *testing.T) {
	t.Parallel()
	var texts []string
	for i := 0; i < 4000; i++ {
		texts = append(texts, fmt.Sprintf(`[ Owner="user%04d"; Memory=%d ]`, i, (i%11)*512))
	}
	c := ndvStore(t, 32<<10, texts)

	// Drop one segment's registers, as an older sidecar would have had none, and note
	// the largest exact count among the segments that hold Owner.
	up, ok := c.ndvProbe("Owner")
	if !ok {
		t.Fatal("Owner is indexed")
	}
	dropped := false
	var largest uint64
	for _, sh := range c.shards {
		sh.mu.Lock()
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 {
				continue
			}
			ip, _ := seg.readIdx().(indexPrimitives)
			if ip == nil {
				continue
			}
			st := ip.statsFor(up)
			if st == nil || st.ndv == 0 {
				continue
			}
			largest = max(largest, st.ndv)
			if !dropped {
				st.hll = nil
				dropped = true
			}
		}
		sh.mu.Unlock()
	}
	if !dropped {
		t.Fatal("no categorical segment summary to strip")
	}

	est, ok := c.AttrNDV("Owner")
	if !ok {
		t.Fatal("no estimate")
	}
	if est.Accuracy != NDVBound || est.Err != NDVErrLow {
		t.Fatalf("accuracy/err = %q/%q, want bound/low", est.Accuracy, est.Err)
	}
	if est.Distinct != largest {
		t.Errorf("distinct = %d, want the largest segment's own count %d", est.Distinct, largest)
	}
	if int(est.Distinct) > 4000 {
		t.Errorf("a lower bound of %d exceeds the 4000 distinct owners written", est.Distinct)
	}
}

// TestNDVUncoveredRecordsMakeItALowerBound: records the index has not reached hold values
// nothing has counted, so an otherwise exact answer can only be low.
func TestNDVUncoveredRecordsMakeItALowerBound(t *testing.T) {
	t.Parallel()
	c := ndvStore(t, 64<<20, []string{
		`[ Owner="alice"; Memory=1024 ]`,
		`[ Owner="bob"; Memory=2048 ]`,
	})
	before, _ := c.AttrNDV("Owner")
	if before.Accuracy != NDVExact {
		t.Fatalf("accuracy before the late write = %q, want exact", before.Accuracy)
	}

	// Written after the index was built, and deliberately NOT re-indexed.
	ad, err := classad.Parse(`[ Owner="carol"; Memory=4096 ]`)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Update([]AdUpdate{{Key: []byte("late"), Ad: ad}}); err != nil {
		t.Fatal(err)
	}

	after, ok := c.AttrNDV("Owner")
	if !ok {
		t.Fatal("no estimate")
	}
	if after.Uncovered == 0 {
		t.Fatal("the late write left no segment with records beyond its index")
	}
	if after.Accuracy != NDVBound || after.Err != NDVErrLow {
		t.Errorf("accuracy/err = %q/%q, want bound/low", after.Accuracy, after.Err)
	}
	if int(after.Distinct) > 3 {
		t.Errorf("a lower bound of %d exceeds the 3 distinct owners written", after.Distinct)
	}
}

// TestNDVSegmentsWithoutTheIndexAreCounted: an index added to a store whose older
// segments the backfill has not reached yet leaves whole segments it has never seen. Those
// records hold values nothing has counted, so the answer is a floor and must say how many
// segments it skipped -- a planner told "500 distinct" over a tenth of the table, with no
// note that it is a tenth, prices the other nine tenths at nothing.
func TestNDVSegmentsWithoutTheIndexAreCounted(t *testing.T) {
	t.Parallel()
	// Owner is indexed from the start, so every segment carries an index; Group is added
	// later with a backfill horizon too small to reach the older segments, so those
	// segments carry an index that does not cover Group.
	c := New(Options{
		Shards: 1, SegmentSize: 32 << 10,
		CategoricalAttrs:   []string{"Owner"},
		IndexBackfillBytes: 64 << 10,
	})
	for i := 0; i < 4000; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ Owner="user%04d"; Group="g%03d" ]`, i, i%250))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Update([]AdUpdate{{Key: []byte("k" + strconv.Itoa(i)), Ad: ad}}); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	if !c.AddIndex([]string{"Group"}, nil) {
		t.Fatal("AddIndex")
	}
	c.Reindex()

	est, ok := c.AttrNDV("Group")
	if !ok {
		t.Fatal("no estimate: the segments inside the horizon do carry the index")
	}
	if est.Uncovered == 0 {
		t.Fatalf("every segment carries the index; the backfill horizon did not hold (%d segments)", est.Segments)
	}
	// The covered segments are counted by a sketch, which already errs both ways, so the
	// unseen segments cannot narrow the class further -- but they must not be allowed to
	// leave it reading as a count.
	if est.Err == NDVErrNone || est.Accuracy == NDVExact {
		t.Errorf("accuracy/err = %q/%q over a partially indexed store: it reads as a count", est.Accuracy, est.Err)
	}
	if est.Accuracy != accuracyFor(est.Err) {
		t.Errorf("accuracy %q does not follow from err %q", est.Accuracy, est.Err)
	}
	if int(est.Distinct) > 250 {
		t.Errorf("a lower bound of %d exceeds the 250 distinct groups written", est.Distinct)
	}
	if est.Segments+est.Uncovered < 2 {
		t.Errorf("segments %d + uncovered %d describes fewer than two segments", est.Segments, est.Uncovered)
	}
}

// TestNDVSupersededVersionsMakeItAnUpperBound: on a mutable table the postings still carry
// overwritten versions, so a value only an overwritten version held is counted. That can
// only make the answer high, and it must be labelled that way rather than as a count.
func TestNDVSupersededVersionsMakeItAnUpperBound(t *testing.T) {
	t.Parallel()
	c := ndvStore(t, 64<<20, []string{`[ Owner="alice"; Memory=1024 ]`})
	ad, err := classad.Parse(`[ Owner="bob"; Memory=2048 ]`)
	if err != nil {
		t.Fatal(err)
	}
	// Same key: alice's record becomes dead, bob's is live.
	if err := c.Update([]AdUpdate{{Key: []byte("k0"), Ad: ad}}); err != nil {
		t.Fatal(err)
	}
	c.Reindex()

	est, ok := c.AttrNDV("Owner")
	if !ok {
		t.Fatal("no estimate")
	}
	if est.Err != NDVErrHigh || est.Accuracy != NDVBound {
		t.Fatalf("accuracy/err = %q/%q, want bound/high", est.Accuracy, est.Err)
	}
	if c.Len() != 1 {
		t.Fatalf("live records = %d, want 1", c.Len())
	}
	if est.Distinct < 1 {
		t.Errorf("an upper bound of %d is below the 1 live distinct owner", est.Distinct)
	}
}

// TestNDVEmptyAndSingleRow covers the two smallest tables. Neither is an error, and
// neither may be answered with a number the table cannot support.
func TestNDVEmptyAndSingleRow(t *testing.T) {
	t.Parallel()
	empty := ndvStore(t, 64<<20, nil)
	if est, ok := empty.AttrNDV("Owner"); ok {
		t.Errorf("an empty table has no segment to summarize; got %+v", est)
	}

	one := ndvStore(t, 64<<20, []string{`[ Owner="alice"; Memory=1024 ]`})
	est, ok := one.AttrNDV("Owner")
	if !ok {
		t.Fatal("a one-row table has a summary")
	}
	if est.Distinct != 1 || est.Accuracy != NDVExact || est.Records != 1 {
		t.Errorf("one row = %d distinct/%q/%d records, want 1/exact/1", est.Distinct, est.Accuracy, est.Records)
	}
}

// TestNDVUnkeyableValues covers an attribute whose values the index cannot key: undefined,
// a list, a nested ad, an unevaluated expression. They are not distinct values of anything
// an equi-join compares, so they are reported as exceptions and left out of the count --
// and the count is then an exact zero, which is a different statement from "no statistic".
// A planner joining on such an attribute is joining on values nothing here has seen, and
// Exceptions is how it finds that out.
func TestNDVUnkeyableValues(t *testing.T) {
	t.Parallel()
	c := ndvStore(t, 64<<20, []string{
		`[ Owner=undefined; Tags={"a","b"}; Memory=1024 ]`,
		`[ Owner=undefined; Tags={"c"}; Memory=2048 ]`,
		`[ Owner=undefined; Tags=[ x=1 ]; Memory=4096 ]`,
	})

	owner, ok := c.AttrNDV("Owner")
	if !ok {
		t.Fatal("Owner is indexed, so it has a summary even with no keyable value")
	}
	if owner.Distinct != 0 {
		t.Errorf("an all-undefined attribute has %d distinct keyable values, want 0", owner.Distinct)
	}
	if owner.Records != 0 {
		t.Errorf("records with a keyable value = %d, want 0", owner.Records)
	}

	tags, ok := c.AttrNDV("Tags")
	if !ok {
		t.Fatal("Tags is indexed")
	}
	if tags.Distinct != 0 {
		t.Errorf("lists and nested ads are not keyed; distinct = %d, want 0", tags.Distinct)
	}
	if tags.Exceptions != 3 {
		t.Errorf("exceptions = %d, want 3 -- the three unkeyable values must be visible", tags.Exceptions)
	}
}

// TestNDVMemoDoesNotServeAStaleAnswer: the memo is keyed on the exact segment indexes the
// answer came from, so new data that changes them must change the answer. A memo that
// outlives its inputs would hand a planner a number for a table that no longer exists.
func TestNDVMemoDoesNotServeAStaleAnswer(t *testing.T) {
	t.Parallel()
	c := ndvStore(t, 64<<20, []string{`[ Owner="alice"; Memory=1024 ]`})
	first, _ := c.AttrNDV("Owner")
	if first.Distinct != 1 {
		t.Fatalf("first = %d, want 1", first.Distinct)
	}
	// Warm it, so a stale read would be served rather than recomputed.
	if again, _ := c.AttrNDV("Owner"); again.Distinct != 1 {
		t.Fatalf("warm read = %d, want 1", again.Distinct)
	}

	for i := 0; i < 50; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ Owner="user%d"; Memory=%d ]`, i, i*16))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Update([]AdUpdate{{Key: []byte("n" + strconv.Itoa(i)), Ad: ad}}); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	after, ok := c.AttrNDV("Owner")
	if !ok {
		t.Fatal("no estimate")
	}
	if after.Distinct != 51 {
		t.Errorf("after 50 new owners the estimate is %d, want 51 -- the memo outlived its inputs", after.Distinct)
	}
}

// TestNDVMemoNoticesSupersessionWithoutAnIndexChange: an overwrite on a store whose index
// is already behind leaves every segment's index object untouched -- nothing is rebuilt --
// while making the postings hold a version no longer live. The memo is keyed on the index
// objects, so this is the one change it cannot see from them, and it is exactly the change
// that turns a count into a ceiling.
func TestNDVMemoNoticesSupersessionWithoutAnIndexChange(t *testing.T) {
	t.Parallel()
	c := ndvStore(t, 64<<20, []string{`[ Owner="alice"; Memory=1024 ]`})
	// A late write leaves records past what the index covers, so the answer is already a
	// floor and stays one through everything below.
	late, err := classad.Parse(`[ Owner="bob"; Memory=2048 ]`)
	if err != nil {
		t.Fatal(err)
	}
	if uerr := c.Update([]AdUpdate{{Key: []byte("late"), Ad: late}}); uerr != nil {
		t.Fatal(uerr)
	}
	before, ok := c.AttrNDV("Owner")
	if !ok {
		t.Fatal("no estimate")
	}
	if before.Err != NDVErrLow {
		t.Fatalf("err before the overwrite = %q, want low", before.Err)
	}

	// Overwrite the late key, WITHOUT re-indexing: the segment's index object is the same
	// one, and the records beyond it are still beyond it.
	over, err := classad.Parse(`[ Owner="carol"; Memory=4096 ]`)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Update([]AdUpdate{{Key: []byte("late"), Ad: over}}); err != nil {
		t.Fatal(err)
	}

	after, ok := c.AttrNDV("Owner")
	if !ok {
		t.Fatal("no estimate after the overwrite")
	}
	if after.Err != NDVErrEither {
		t.Errorf("err after the overwrite = %q, want either: the table now holds a superseded version", after.Err)
	}
}

// TestNDVEmptySummaryDoesNotForceASketch: a segment that indexes the attribute but posted
// no keyable value holds no distinct value, so it must not make a one-segment answer look
// like a several-segment one. Replacing an exact count with a sketched one is a 3% error
// introduced for nothing.
func TestNDVEmptySummaryDoesNotForceASketch(t *testing.T) {
	t.Parallel()
	pad := ""
	for i := 0; i < 2000; i++ {
		pad += "x"
	}
	var texts []string
	// Segment 0, written into an empty arena, holds both keyable records.
	texts = append(texts,
		`[ Owner="alice"; Memory=1024 ]`,
		`[ Owner="bob"; Memory=2048 ]`)
	// Everything after carries the attribute as undefined, which the index cannot key, and
	// is padded enough to fill several more segments.
	for i := 0; i < 60; i++ {
		texts = append(texts, fmt.Sprintf(`[ Owner=undefined; Memory=%d; Pad=%q ]`, i, pad))
	}
	c := ndvStore(t, 8<<10, texts)
	if segs := c.Stats().Segments; segs < 3 {
		t.Fatalf("want several segments, got %d", segs)
	}

	est, ok := c.AttrNDV("Owner")
	if !ok {
		t.Fatal("no estimate")
	}
	if est.Distinct != 2 {
		t.Errorf("distinct = %d, want 2", est.Distinct)
	}
	if est.Accuracy != NDVExact || est.Err != NDVErrNone {
		t.Errorf("accuracy/err = %q/%q, want exact/none: the empty segments hold no distinct value",
			est.Accuracy, est.Err)
	}
	if est.Segments < 3 {
		t.Errorf("segments summarized = %d: the empty ones index the attribute and must be counted", est.Segments)
	}
}

// TestNDVErrLattice pins how error directions compose. Two one-sided errors in opposite
// directions bound nothing, and saying they do is the failure this file exists to prevent.
func TestNDVErrLattice(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b, want NDVErr
	}{
		{NDVErrNone, NDVErrNone, NDVErrNone},
		{NDVErrNone, NDVErrLow, NDVErrLow},
		{NDVErrHigh, NDVErrNone, NDVErrHigh},
		{NDVErrLow, NDVErrLow, NDVErrLow},
		{NDVErrLow, NDVErrHigh, NDVErrEither},
		{NDVErrHigh, NDVErrLow, NDVErrEither},
		{NDVErrEither, NDVErrNone, NDVErrEither},
		{NDVErrLow, NDVErrEither, NDVErrEither},
	}
	for _, tc := range cases {
		if got := combineErr(tc.a, tc.b); got != tc.want {
			t.Errorf("combineErr(%q,%q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
	for err, want := range map[NDVErr]NDVAccuracy{
		NDVErrNone:   NDVExact,
		NDVErrLow:    NDVBound,
		NDVErrHigh:   NDVBound,
		NDVErrEither: NDVEstimated,
	} {
		if got := accuracyFor(err); got != want {
			t.Errorf("accuracyFor(%q) = %q, want %q", err, got, want)
		}
	}
}

// TestNDVArchiveReadsSealedSidecars: an archive's summaries live in its sidecars and are
// parsed back on open, so the estimate survives a reopen with no index rebuild. That is
// what makes this a metadata read rather than a thing the process happened to remember.
func TestNDVArchiveReadsSealedSidecars(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := buildArchive(t, dir, 400, ArchiveOptions{
		CategoricalAttrs: []string{"Owner", "JobStatus"},
		ValueAttrs:       []string{"Memory"},
	})
	before, ok := a.AttrNDV("Owner")
	if !ok {
		t.Fatal("no estimate")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	re, err := OpenArchive(ArchiveOptions{
		Dir: dir, SegmentSize: 8 << 10,
		CategoricalAttrs: []string{"Owner", "JobStatus"},
		ValueAttrs:       []string{"Memory"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()

	after, ok := re.AttrNDV("Owner")
	if !ok {
		t.Fatal("no estimate after reopen")
	}
	// buildArchive writes five owners.
	if after.Distinct != before.Distinct {
		t.Errorf("after reopen distinct = %d, want the %d read before", after.Distinct, before.Distinct)
	}
	if after.Distinct < 5 {
		t.Errorf("distinct = %d, want at least the 5 owners written", after.Distinct)
	}
}
