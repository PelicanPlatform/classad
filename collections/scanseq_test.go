package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// rawIDOf pulls the Id attribute out of a raw ad's rendered expressions.
func rawIDOf(t *testing.T, raw RawAd) int {
	t.Helper()
	for _, e := range raw.Exprs {
		name, val, ok := strings.Cut(string(e), " = ")
		if ok && name == "Id" {
			var id int
			if _, err := fmt.Sscanf(val, "%d", &id); err != nil {
				t.Fatalf("Id %q does not parse: %v", val, err)
			}
			return id
		}
	}
	t.Fatalf("no Id in %v", raw.Exprs)
	return -1
}

// pageAll walks every page of a sequence cursor and returns the ids in the order
// they were yielded, plus how many pages it took.
func pageAll(t *testing.T, c *Collection, q *vm.Query, limit int, mutate func(page int)) ([]int, int) {
	return pageAllProjected(t, c, q, nil, limit, mutate)
}

// pageAllProjected is pageAll with a projection applied to every page.
func pageAllProjected(t *testing.T, c *Collection, q *vm.Query, projection []string, limit int, mutate func(page int)) ([]int, int) {
	t.Helper()
	var (
		ids    []int
		cursor SeqCursor
		pages  int
	)
	for {
		seq, page := c.QueryRawFromSeq(q, projection, cursor, limit)
		for raw := range seq {
			ids = append(ids, rawIDOf(t, raw))
		}
		pages++
		if !page.More {
			return ids, pages
		}
		cursor = page.Next
		if mutate != nil {
			mutate(pages)
		}
		if pages > 1000 {
			t.Fatal("pagination did not terminate")
		}
	}
}

// TestSeqCursorPagesCoverEverythingExactlyOnce is the contract: walking the
// pages visits every record once, in a stable order, regardless of page size.
func TestSeqCursorPagesCoverEverythingExactlyOnce(t *testing.T) {
	t.Parallel()
	const n = 500
	for _, limit := range []int{1, 7, 64, n, n * 2} {
		// A small arena so the records span segments: with one active segment
		// per shard there is nothing to merge and the ordering is trivially
		// whatever the arena walk produced.
		c := New(Options{Shards: 2, SegmentSize: 4096})
		for i := 0; i < n; i++ {
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
				t.Fatal(err)
			}
		}

		if segs := c.segmentCountForTest(); segs <= 2 {
			t.Fatalf("limit %d: expected the records to span several segments, got %d", limit, segs)
		}

		ids, pages := pageAll(t, c, nil, limit, nil)
		if len(ids) != n {
			t.Fatalf("limit %d: got %d records over %d pages, want %d", limit, len(ids), pages, n)
		}
		seen := make([]int, n)
		for _, id := range ids {
			if id < 0 || id >= n {
				t.Fatalf("limit %d: id %d out of range", limit, id)
			}
			seen[id]++
		}
		for id, count := range seen {
			if count != 1 {
				t.Fatalf("limit %d: id %d seen %d times, want 1", limit, id, count)
			}
		}
	}
}

// TestSeqCursorOrderIsStable checks the property pagination rests on: the
// order is a total order over the collection, the same on every walk, so a
// cursor means the same thing to the next page as it did to the last.
//
// It is (shard, seq, key), NOT insertion order — records committed together
// share a sequence and are then ordered by key, and sequences from different
// shards are unrelated numbers.
func TestSeqCursorOrderIsStable(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4})
	const n = 200
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}

	whole, _ := pageAll(t, c, nil, 0, nil)     // one unlimited page
	paged, pages := pageAll(t, c, nil, 7, nil) // many small pages
	if pages < 2 {
		t.Fatalf("expected several pages, got %d", pages)
	}
	if len(whole) != n || len(paged) != n {
		t.Fatalf("whole=%d paged=%d, want %d each", len(whole), len(paged), n)
	}
	for i := range whole {
		if whole[i] != paged[i] {
			t.Fatalf("page boundaries changed the order at %d: %d vs %d", i, whole[i], paged[i])
		}
	}
}

// TestSeqCursorIgnoresWritesAfterTheSnapshot is why the cursor carries a
// snapshot: once a shard's pagination has begun, a record written to it must
// not appear in a later page, and must not shift the records that follow.
//
// One shard, so every write lands in the shard already being paginated. With
// several shards a write to a shard not yet started IS visible, which is the
// per-shard guarantee Scan makes and QueryRawFromSeq inherits — the multi-shard
// case is covered below by checking coverage rather than exclusion.
func TestSeqCursorIgnoresWritesAfterTheSnapshot(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1})
	const n = 100
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}

	added := 0
	ids, _ := pageAll(t, c, nil, 10, func(page int) {
		if err := c.Put([]byte(fmt.Sprintf("late%d", page)), mustAd(t, fmt.Sprintf(`[Id=%d]`, 10_000+page))); err != nil {
			t.Fatal(err)
		}
		added++
	})

	if added == 0 {
		t.Fatal("the test wrote nothing mid-pagination; it proves nothing")
	}
	if len(ids) != n {
		t.Fatalf("got %d records, want the %d present when the shard was frozen", len(ids), n)
	}
	for _, id := range ids {
		if id >= 10_000 {
			t.Errorf("a record written after the shard's snapshot appeared: Id %d", id)
		}
	}
}

// TestSeqCursorCoversEverythingUnderConcurrentWrites is the multi-shard
// counterpart: writes during pagination may or may not be seen depending on
// whether their shard had started, but nothing present at the start may be
// missed or duplicated.
func TestSeqCursorCoversEverythingUnderConcurrentWrites(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 8})
	const n = 300
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}

	ids, _ := pageAll(t, c, nil, 20, func(page int) {
		if err := c.Put([]byte(fmt.Sprintf("late%d", page)), mustAd(t, fmt.Sprintf(`[Id=%d]`, 10_000+page))); err != nil {
			t.Fatal(err)
		}
	})

	seen := make([]int, n)
	for _, id := range ids {
		if id >= 10_000 {
			continue // a late write in a shard that had not started yet
		}
		if id < 0 || id >= n {
			t.Fatalf("id %d out of range", id)
		}
		seen[id]++
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("id %d seen %d times under concurrent writes, want 1", id, count)
		}
	}
}

// TestSeqCursorSurvivesCompaction is the property that ruled out a physical
// cursor: compaction renumbers segment ids and moves records, so a
// (segment, offset) cursor would be silently wrong. A sequence is carried onto
// the destination record, so pagination is unaffected.
func TestSeqCursorSurvivesCompaction(t *testing.T) {
	t.Parallel()
	// A small arena so the records span many segments: the cursor's merge
	// across segment runs is only exercised when there is more than one, and a
	// rewrite only moves records that live in a sealed segment.
	c := New(Options{Shards: 2, SegmentSize: 1 << 16})
	const n = 400
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	// Churn every key repeatedly, the way TestCompactionReclaimsAndPreservesData
	// does: the dead/used ratio has to clear the compaction threshold for the
	// rewrite path to run at all.
	for round := 1; round <= 20; round++ {
		for i := 0; i < n; i++ {
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; Rev=%d]`, i, round))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if segs := c.segmentCountForTest(); segs < 4 {
		t.Fatalf("expected the records to span several segments, got %d", segs)
	}

	compacted := 0
	ids, _ := pageAll(t, c, nil, 25, func(int) { compacted += c.Compact() })

	if compacted == 0 {
		t.Fatal("compaction reclaimed nothing, so this test would pass without exercising a rewrite")
	}
	seen := map[int]int{}
	for _, id := range ids {
		seen[id]++
	}
	if len(seen) != n {
		t.Fatalf("saw %d distinct records across compaction, want %d", len(seen), n)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("id %d seen %d times across compaction, want 1", id, count)
		}
	}
}

// TestSeqCursorAppliesTheQuery checks the constraint is applied, so pagination
// is over the matching set rather than the whole collection.
func TestSeqCursorAppliesTheQuery(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4})
	const n = 200
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; Owner=%q]`, i, []string{"alice", "bob"}[i%2]))); err != nil {
			t.Fatal(err)
		}
	}
	q, err := vm.Parse(`Owner == "alice"`)
	if err != nil {
		t.Fatal(err)
	}

	ids, _ := pageAll(t, c, q, 13, nil)
	if len(ids) != n/2 {
		t.Fatalf("got %d matching records, want %d", len(ids), n/2)
	}
	for _, id := range ids {
		if id%2 != 0 {
			t.Errorf("id %d does not match the constraint", id)
		}
	}
}

// segmentCountForTest reports how many segments the collection currently holds,
// so a test can assert it is exercising the multi-segment path rather than one
// active arena.
func (c *Collection) segmentCountForTest() int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		n += len(sh.segs)
		sh.mu.RUnlock()
	}
	return n
}

// TestSeqCursorMaxSeqAfterReopen covers what a restart does to the pruning half
// of the cursor.
//
// A clean Close leaves a directory snapshot and the next Open restores from it
// without walking records, so neither minSeq nor maxSeq is rebuilt — both stay
// zero and are treated conservatively (never skip). That is pre-existing
// behavior for minSeq, and correctness does not depend on it: a zero maxSeq
// costs a walk, it does not skip a segment it should have read.
//
// Where the metadata IS rebuilt — recovery, when there is no snapshot to trust
// — maxSeq must come back with minSeq, which is the half this test pins.
func TestSeqCursorMaxSeqAfterReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	c, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	const n = 400
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if segs := c.segmentCountForTest(); segs < 3 {
		t.Fatalf("expected several segments, got %d", segs)
	}
	before := segMaxSeqsForTest(c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Drop the directory snapshots so Open has to walk the records, which is
	// the path that rebuilds the pruning metadata.
	snaps, globErr := filepath.Glob(filepath.Join(dir, "*", "dir.snap"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(snaps) == 0 {
		t.Fatal("no directory snapshot was written, so this test would not reach the recovery path")
	}
	for _, p := range snaps {
		if rmErr := os.Remove(p); rmErr != nil {
			t.Fatal(rmErr)
		}
	}

	c2, openErr := Open(Options{Shards: 1, Dir: dir, SegmentSize: 4096})
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer c2.Close()

	after := segMaxSeqsForTest(c2)
	if len(after) != len(before) {
		t.Fatalf("segment count changed across reopen: %d then %d", len(before), len(after))
	}
	for i, v := range after {
		if v == 0 {
			t.Errorf("segment %d has no maxSeq after recovery; resume pruning would be disabled", i)
		}
		if v != before[i] {
			t.Errorf("segment %d maxSeq %d before reopen, %d after", i, before[i], v)
		}
	}

	// And pagination still covers everything after the reopen.
	ids, _ := pageAll(t, c2, nil, 32, nil)
	if len(ids) != n {
		t.Fatalf("after reopen got %d records, want %d", len(ids), n)
	}
}

// segMaxSeqsForTest reports each segment's maxSeq, in shard then segment order.
func segMaxSeqsForTest(c *Collection) []uint64 {
	var out []uint64
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil {
				out = append(out, seg.maxSeq)
			}
		}
		sh.mu.RUnlock()
	}
	return out
}

// TestSeqCursorProjects checks the projection is applied in the walk, like
// QueryRawProjected: a caller asking for two attributes gets two, not a whole
// ad it then has to trim.
func TestSeqCursorProjects(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2, SegmentSize: 4096})
	const n = 120
	for i := 0; i < n; i++ {
		src := fmt.Sprintf(`[Id=%d; Owner="alice"; Cmd="/bin/true"; Args="-x"]`, i)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, src)); err != nil {
			t.Fatal(err)
		}
	}

	seq, page := c.QueryRawFromSeq(nil, []string{"Id", "Owner"}, SeqCursor{}, 10)
	rows := 0
	for raw := range seq {
		rows++
		names := map[string]bool{}
		for _, e := range raw.Exprs {
			name, _, _ := strings.Cut(string(e), " = ")
			names[name] = true
		}
		if !names["Id"] || !names["Owner"] {
			t.Fatalf("projection dropped a requested attribute: %v", raw.Exprs)
		}
		if names["Cmd"] || names["Args"] {
			t.Fatalf("projection kept an attribute that was not asked for: %v", raw.Exprs)
		}
	}
	if rows != 10 {
		t.Fatalf("got %d rows, want the page limit of 10", rows)
	}
	if !page.More {
		t.Error("expected more pages after the first 10 of 120")
	}

	// And the projected walk still paginates over everything.
	ids, _ := pageAllProjected(t, c, nil, []string{"Id"}, 16, nil)
	if len(ids) != n {
		t.Fatalf("projected pagination covered %d records, want %d", len(ids), n)
	}
}

// TestSeqCursorEqualSequences is the case the single-writer tests miss.
// Records committed TOGETHER share a sequence, and the arena holds them in
// insertion order, not key order — so within one commit a record belonging
// after the cursor can sit physically before one belonging before it. A merge
// that assumed each segment was sorted by (seq, key) skipped records at every
// page boundary landing inside a commit.
//
// A transaction gives that shape deterministically. Concurrent Puts also
// coalesce into shared commits, but only when they happen to queue up together,
// which under load they stop doing — a test built on that passes or fails with
// the machine's scheduling rather than with the code.
func TestSeqCursorEqualSequences(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, SegmentSize: 4096})
	const n = 200

	tx := c.Begin()
	for i := 0; i < n; i++ {
		tx.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i)))
	}
	if res := tx.Commit(); res.Conflicted() || res.Committed != n {
		t.Fatalf("commit wrote %d of %d records (conflicts: %d)", res.Committed, n, len(res.Conflicts))
	}
	if !hasSharedSequenceForTest(c) {
		t.Fatal("no two records share a sequence, so this test would not exercise the group path")
	}

	ids, _ := pageAll(t, c, nil, 15, nil)
	if len(ids) != n {
		t.Fatalf("paged %d records, want %d", len(ids), n)
	}
	seen := make([]int, n)
	for _, id := range ids {
		if id < 0 || id >= n {
			t.Fatalf("id %d out of range", id)
		}
		seen[id]++
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("id %d seen %d times, want 1", id, count)
		}
	}
}

// hasSharedSequenceForTest reports whether any two live records share a
// sequence, so a test can assert it is exercising the commit-group path.
func hasSharedSequenceForTest(c *Collection) bool {
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		counts := map[uint64]int{}
		for _, w := range wins {
			forEachVisibleWindowRef(s0, w, func(r recRef) bool {
				counts[recSeq(r.w.data, r.off)]++
				return true
			})
		}
		releaseWindows(wins)
		for _, n := range counts {
			if n > 1 {
				return true
			}
		}
	}
	return false
}
