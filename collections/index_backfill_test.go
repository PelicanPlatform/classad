package collections

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestIndexBackfillHorizonLeavesOlderSegments is the payoff for identifying index
// configurations by content and using whatever a segment's index covers: with backfill
// bounded, a configuration change reaches only recent segments, older ones keep the index
// they already had, and that mixture SURVIVES a restart -- which is what makes it worth
// having, since recovery used to re-index everything and erase it.
//
// The bound exists because rebuilding decompresses every record it covers, while the index
// it would replace still answers for the attributes it holds. On an archive read newest-first
// with a limit, the oldest segments may never be reached at all.
func TestIndexBackfillHorizonLeavesOlderSegments(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	open := func(cat []string) *Collection {
		t.Helper()
		c, err := Open(Options{
			Dir: dir, Shards: 1, SegmentSize: 1 << 13,
			CategoricalAttrs: cat,
			// Small enough that most segments fall outside it.
			IndexBackfillBytes: 24 << 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	c := open([]string{"Owner"})
	const n = 2400
	for i := 0; i < n; i++ {
		ad := mustAdOld(t, fmt.Sprintf("Owner = %q\nGroup = %q\nPad = %q",
			fmt.Sprintf("u%d", i%4), fmt.Sprintf("g%d", (i/4)%8), strings.Repeat("s", 60)))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	// Add a second index. Only segments inside the horizon should pick it up.
	c.AddIndex([]string{"Group"}, nil)
	c.Reindex()
	c.Close()

	c2 := open([]string{"Owner", "Group"})
	defer c2.Close()

	ownerID := inlineAttrID("owner")
	groupID := inlineAttrID("group")
	oldConfig, newConfig := 0, 0
	sh := c2.shards[0]
	sh.mu.RLock()
	for _, seg := range sh.segs {
		if seg == nil || seg == sh.act {
			continue
		}
		mm := seg.msidx.Load()
		if mm == nil {
			continue
		}
		_, hasOwner := mm.catDir[ownerID]
		_, hasGroup := mm.catDir[groupID]
		switch {
		case hasOwner && hasGroup:
			newConfig++
		case hasOwner:
			oldConfig++
		}
	}
	sh.mu.RUnlock()

	if oldConfig == 0 {
		t.Error("every segment was backfilled: the horizon did not bound the rebuild, or " +
			"recovery re-indexed everything anyway")
	}
	if newConfig == 0 {
		t.Error("no segment carries the new configuration: the horizon bounded too much")
	}
	t.Logf("%d segments on the older configuration, %d on the newer, across a restart",
		oldConfig, newConfig)

	// Whatever the mixture, results must be complete: segments holding Group answer from
	// their index, the rest are scanned.
	for _, tc := range []struct {
		expr string
		want int
	}{
		{`Owner == "u1"`, n / 4},
		{`Group == "g3"`, n / 8},
		{`Owner == "u1" && Group == "g3"`, n / 32},
	} {
		if got := countWhere(t, c2, tc.expr); got != tc.want {
			t.Errorf("%s: %d rows, want %d", tc.expr, got, tc.want)
		}
	}
}

// TestStaleCountsSeparatePolicyFromFailure pins the distinction an alert depends on. With a
// backfill horizon, segments outside it stay on an older index configuration forever by
// design; counting them as "stale" would leave the number permanently non-zero and make it
// useless as a signal that a reindex is stuck.
func TestStaleCountsSeparatePolicyFromFailure(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{
		Dir: dir, Shards: 1, SegmentSize: 1 << 13,
		CategoricalAttrs: []string{"Owner"}, IndexBackfillBytes: 24 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 2400; i++ {
		ad := mustAdOld(t, fmt.Sprintf("Owner = %q\nGroup = %q\nPad = %q",
			fmt.Sprintf("u%d", i%4), fmt.Sprintf("g%d", (i/4)%8), strings.Repeat("s", 60)))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	c.AddIndex([]string{"Group"}, nil)
	c.Reindex() // carries the change as far as the horizon allows, and no further

	pending, sealed := c.StaleIndexSegments()
	byPolicy := c.StaleIndexSegmentsByPolicy()
	if sealed == 0 {
		t.Fatal("no sealed segments")
	}
	if pending != 0 {
		t.Errorf("%d segments still pending a rebuild after a completed reindex -- an alert "+
			"on this would fire forever", pending)
	}
	if byPolicy == 0 {
		t.Errorf("no segments reported as left behind by policy, though the horizon is far "+
			"smaller than the store (%d sealed)", sealed)
	}
}

// backfillArchive opens (or creates) a persistent archive whose Reindex carries a
// configuration change only to its newest few segments, the way db configures history.
func backfillArchive(t *testing.T, dir string, create bool, value ...string) *Archive {
	t.Helper()
	opts := ArchiveOptions{Dir: dir, SegmentSize: 1 << 12, ValueAttrs: value, IndexBackfillBytes: 16 << 10}
	var a *Archive
	var err error
	if create {
		a, err = CreateArchive(opts)
	} else {
		a, err = OpenArchive(opts)
	}
	if err != nil {
		t.Fatal(err)
	}
	return a
}

const backfillRecords = 2000

func fillBackfillArchive(t *testing.T, a *Archive) {
	t.Helper()
	for i := 0; i < backfillRecords; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; EnteredHistoryTime=%d; Pad=%q ]`,
			i, 1000+i, strings.Repeat("p", 24)))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
}

// sealedCoverage reports, newest sealed segment first, whether each covers every indexed
// attribute in a sidecar.
func sealedCoverage(a *Archive) []bool {
	spec := a.c.spec.Load()
	sh := a.c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	var out []bool
	for i := len(sh.segs) - 1; i >= 0; i-- {
		seg := sh.segs[i]
		if seg == nil || seg == sh.act || seg.used == 0 {
			continue
		}
		mm := seg.msidx.Load()
		out = append(out, mm != nil && mm.coversSpec(spec))
	}
	return out
}

func unindexedSegments(t *testing.T, a *Archive, expr string) QueryExplain {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	return a.ExplainQuery(q)
}

func archiveCount(t *testing.T, a *Archive, expr string) int {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range a.Query(q) {
		n++
	}
	return n
}

// TestReindexAllCoversSegmentsOutsideHorizon is the operator's reindex on an archive whose
// index was added by the auto-tuner. Reindex carries a configuration change only within the
// backfill horizon, so the segments beyond it never gained postings for the new attribute and
// an explicit reindex finished at once having done nothing for them. ReindexAll must leave
// every sealed segment covering the attribute, and that coverage must survive a reopen
// without being rebuilt.
func TestReindexAllCoversSegmentsOutsideHorizon(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	a := backfillArchive(t, dir, true, "ClusterId")
	fillBackfillArchive(t, a)
	if !a.c.AddAutoIndex(nil, []string{"EnteredHistoryTime"}) { // the auto-tuner's add
		t.Fatal("index not added")
	}
	a.Reindex()
	const expr = `EnteredHistoryTime == 1500`
	if ex := unindexedSegments(t, a, expr); ex.UnindexedSegments == 0 {
		t.Fatalf("fixture: the horizon left nothing unindexed (%s)", ex.Reason)
	}

	res := a.ReindexAll()
	if res.Rebuilt == 0 || res.PendingSegments != 0 || res.Failed != 0 {
		t.Fatalf("ReindexAll = %+v, want segments rebuilt and none pending", res)
	}
	if ex := unindexedSegments(t, a, expr); ex.UnindexedSegments != 0 {
		t.Errorf("after ReindexAll: %s", ex.Reason)
	}
	for i, ok := range sealedCoverage(a) {
		if !ok {
			t.Errorf("sealed segment %d (from newest) lacks the new index after ReindexAll", i)
		}
	}
	if got := archiveCount(t, a, expr); got != 1 {
		t.Errorf("%s: %d rows, want 1", expr, got)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	a2 := backfillArchive(t, dir, false, "ClusterId", "EnteredHistoryTime")
	defer a2.Close()
	if n := a2.c.openIdxDiag.Reasons["attr-section-missing-or-stale"]; n != 0 {
		t.Errorf("reopen rejected %d attribute sections", n)
	}
	if ex := unindexedSegments(t, a2, expr); ex.UnindexedSegments != 0 {
		t.Errorf("after reopen: %s", ex.Reason)
	}
	if segs, _ := a2.IndexBackfillPending(); segs != 0 {
		t.Errorf("after reopen %d segments pending backfill, want 0", segs)
	}
	if got := archiveCount(t, a2, `EnteredHistoryTime >= 2900`); got != 100 {
		t.Errorf("range after reopen: %d rows, want 100", got)
	}
}

// TestBackfillIndexesBudgetNewestFirst pins the maintenance contract: each pass rebuilds about
// its byte budget's worth of segments, always the newest uncovered ones, and repeated passes
// cover the whole archive.
func TestBackfillIndexesBudgetNewestFirst(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	a := backfillArchive(t, t.TempDir(), true, "ClusterId")
	defer a.Close()
	fillBackfillArchive(t, a)
	a.c.AddAutoIndex(nil, []string{"EnteredHistoryTime"})
	a.Reindex()

	pending, pendingBytes := a.IndexBackfillPending()
	if pending < 12 {
		t.Fatalf("fixture: only %d segments pending", pending)
	}
	segBytes := pendingBytes / int64(pending)
	budget := 3 * segBytes
	passes := 0
	for {
		before, _ := a.IndexBackfillPending()
		if before == 0 {
			break
		}
		res := a.BackfillIndexes(budget)
		passes++
		if res.Rebuilt == 0 {
			t.Fatalf("pass %d rebuilt nothing with %d pending", passes, before)
		}
		// The budget is a stop condition checked before each segment, so a pass overshoots by
		// at most one segment.
		if res.RebuiltBytes-budget >= 2*segBytes {
			t.Errorf("pass %d re-read %d bytes against a budget of %d", passes, res.RebuiltBytes, budget)
		}
		if res.PendingSegments != before-res.Rebuilt {
			t.Errorf("pass %d: pending %d -> %d after %d rebuilt", passes, before, res.PendingSegments, res.Rebuilt)
		}
		// Newest first: the covered segments form an unbroken run from the newest end.
		cov := sealedCoverage(a)
		seenGap := false
		for i, ok := range cov {
			if !ok {
				seenGap = true
			} else if seenGap {
				t.Fatalf("pass %d: segment %d from newest is covered behind an uncovered newer one: %v", passes, i, cov)
			}
		}
		if passes > pending {
			t.Fatal("backfill is not converging")
		}
	}
	if passes < pending/4 {
		t.Errorf("%d segments covered in %d passes: the budget did not bound a pass", pending, passes)
	}
	if got := archiveCount(t, a, `EnteredHistoryTime < 1100`); got != 100 {
		t.Errorf("oldest range: %d rows, want 100", got)
	}
	if st := a.c.OpStats().IndexBackfill; st.Count != int64(pending) {
		t.Errorf("IndexBackfill count = %d, want %d", st.Count, pending)
	}
}

// TestBackfillIndexesResumesAfterRestart stops a backfill midway: the segments it finished
// must stay finished across a reopen, so the next pass starts where the last one stopped.
func TestBackfillIndexesResumesAfterRestart(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	a := backfillArchive(t, dir, true, "ClusterId")
	fillBackfillArchive(t, a)
	a.c.AddAutoIndex(nil, []string{"EnteredHistoryTime"})
	a.Reindex()
	total, totalBytes := a.IndexBackfillPending()
	res := a.BackfillIndexes(totalBytes / 2)
	if res.Rebuilt == 0 || res.PendingSegments == 0 {
		t.Fatalf("fixture: pass did not stop midway: %+v of %d", res, total)
	}
	covered := sealedCoverage(a)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	a2 := backfillArchive(t, dir, false, "ClusterId", "EnteredHistoryTime")
	defer a2.Close()
	if pending, _ := a2.IndexBackfillPending(); pending != res.PendingSegments {
		t.Errorf("after reopen %d segments pending, want the %d left by the interrupted pass", pending, res.PendingSegments)
	}
	if got := sealedCoverage(a2); !slices.Equal(got, covered) {
		t.Errorf("coverage changed across reopen:\n before %v\n after  %v", covered, got)
	}
	res2 := a2.BackfillIndexes(0)
	if res2.Rebuilt != res.PendingSegments || res2.PendingSegments != 0 {
		t.Errorf("resumed pass = %+v, want exactly the %d left over rebuilt", res2, res.PendingSegments)
	}
	if got := archiveCount(t, a2, `EnteredHistoryTime == 1001`); got != 1 {
		t.Errorf("oldest record: %d rows, want 1", got)
	}
}

// TestBackfillIndexesConcurrentQueriesAndAppends runs a backfill while readers query the
// attribute being backfilled and a writer keeps appending (sealing segments, so the append
// path's own Reindex contends for the same lock). Every read must see exactly the records
// that match, whichever sidecar each segment carries at the moment it is read.
func TestBackfillIndexesConcurrentQueriesAndAppends(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	a := backfillArchive(t, t.TempDir(), true, "ClusterId")
	defer a.Close()
	fillBackfillArchive(t, a)
	a.c.AddAutoIndex(nil, []string{"EnteredHistoryTime"})
	a.Reindex()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Only original records carry EnteredHistoryTime below 3000.
				for _, tc := range []struct {
					expr string
					want int
				}{
					{`EnteredHistoryTime == 1234`, 1},
					{`EnteredHistoryTime >= 1000 && EnteredHistoryTime < 1300`, 300},
				} {
					q, _ := vm.Parse(tc.expr)
					n := 0
					for range a.Query(q) {
						n++
					}
					if n != tc.want {
						select {
						case errs <- fmt.Sprintf("%s: %d rows, want %d", tc.expr, n, tc.want):
						default:
						}
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 600; i++ {
			ad, _ := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; EnteredHistoryTime=%d; Pad=%q ]`,
				100000+i, 100000+i, strings.Repeat("q", 24)))
			if err := a.Append(ad); err != nil {
				errs <- err.Error()
				return
			}
		}
	}()
	for {
		res := a.BackfillIndexes(8 << 10)
		if res.PendingSegments == 0 {
			break
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if segs, _ := a.IndexBackfillPending(); segs != 0 {
		t.Errorf("%d segments still pending", segs)
	}
}

// TestReindexPersistsIndexOverKeyOnlySidecar covers a segment sealed while nothing was
// indexed: its sidecar holds only the key index. A later AddIndex + Reindex built that
// segment's postings on the heap and then tried to persist them through sealSegmentIndex,
// which does nothing once a key index exists -- so the index stayed on the heap for the life
// of the process and was rebuilt from every record on every open.
func TestReindexPersistsIndexOverKeyOnlySidecar(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	open := func(cat []string) *Collection {
		t.Helper()
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 13, CategoricalAttrs: cat})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open(nil)
	const n = 1200
	for i := 0; i < n; i++ {
		ad := mustAdOld(t, fmt.Sprintf("Owner = %q\nPad = %q", fmt.Sprintf("u%d", i%4), strings.Repeat("s", 60)))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex() // seals key-only sidecars
	c.AddIndex([]string{"Owner"}, nil)
	c.Reindex()
	sh := c.shards[0]
	sh.mu.RLock()
	sealed, withAttr := 0, 0
	for _, seg := range sh.segs {
		if seg == nil || seg == sh.act || seg.used == 0 {
			continue
		}
		sealed++
		if seg.msidx.Load() != nil {
			withAttr++
		}
	}
	sh.mu.RUnlock()
	if sealed < 3 {
		t.Fatalf("fixture: %d sealed segments", sealed)
	}
	if withAttr != sealed {
		t.Errorf("%d of %d sealed segments carry the attribute index in their sidecar", withAttr, sealed)
	}
	c.Close()

	c2 := open([]string{"Owner"})
	defer c2.Close()
	if n := c2.openIdxDiag.Reasons["attr-section-missing-or-stale"]; n != 0 {
		t.Errorf("reopen found %d sidecars without an attribute section; their index is rebuilt every open", n)
	}
	if got := countWhere(t, c2, `Owner == "u1"`); got != n/4 {
		t.Errorf(`Owner == "u1": %d rows, want %d`, got, n/4)
	}
}
