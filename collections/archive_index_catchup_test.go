package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// activeSegState returns the archive's active segment, the bytes written to it, and how many of
// them its index covers (-1 when it has none).
func activeSegState(tb testing.TB, c *Collection) (seg *segment, used, upto int) {
	tb.Helper()
	sh := c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	seg = sh.act
	if seg == nil {
		tb.Fatal("archive has no active segment")
	}
	used = seg.used
	upto = -1
	if si := seg.idx.Load(); si != nil {
		upto = int(si.upto)
	}
	return
}

// archiveSealedCount returns how many of the archive's segments are sealed.
func archiveSealedCount(c *Collection) (sealed int) {
	sh := c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	for _, s := range sh.segs {
		if s != nil && s != sh.act {
			sealed++
		}
	}
	return
}

// catchUpArchive builds an archive whose open segment carries a long uncovered tail: enough
// records to seal several segments and then to fill most of another.
func catchUpArchive(tb testing.TB, opts ArchiveOptions, n int) (*Archive, []int64) {
	tb.Helper()
	if opts.Dir == "" {
		opts.Dir = tb.TempDir()
	}
	a, err := CreateArchive(opts)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { a.Close() })
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%8))
		ad.InsertAttrString("Pad", "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
		if aerr := a.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
		ids = append(ids, int64(i))
	}
	return a, ids
}

// clusterIDs runs q and returns the ClusterId of every row, in the order the read produced them.
func clusterIDs(tb testing.TB, a *Archive, q *vm.Query) []int64 {
	tb.Helper()
	var out []int64
	for ad := range a.Query(q) {
		v, ok := ad.EvaluateAttrInt("ClusterId")
		if !ok {
			tb.Fatal("row has no integer ClusterId")
		}
		out = append(out, v)
	}
	return out
}

// mustProbeQuery parses a constraint and resolves its probes, as a planned read does.
func mustProbeQuery(tb testing.TB, s string) *vm.Query {
	tb.Helper()
	q, err := vm.Parse(s)
	if err != nil {
		tb.Fatal(err)
	}
	q.Probes()
	return q
}

// TestArchiveCatchUpIdenticalResults is the correctness bar for extending an archive's active
// segment index on a read: the rows a read returns, and their order, do not depend on how much of
// the open segment the index happens to cover.
//
// The uncaught-up arm is produced by holding reindexMu, which is exactly what a read concurrent
// with a Reindex meets: catchUpIndexes declines the lock and the tail is walked.
func TestArchiveCatchUpIdenticalResults(t *testing.T) {
	const n = 6000
	a, _ := catchUpArchive(t, ArchiveOptions{
		SegmentSize:      1 << 16,
		ValueAttrs:       []string{"ClusterId"},
		CategoricalAttrs: []string{"Owner"},
	}, n)

	// Preconditions. Without a long uncovered tail this test proves nothing.
	if sealed := archiveSealedCount(a.c); sealed < 3 {
		t.Fatalf("fixture sealed %d segments, want several", sealed)
	}
	_, used0, upto0 := activeSegState(t, a.c)
	if used0 <= indexCatchUpBytes {
		t.Fatalf("open segment holds %d bytes, not enough to be worth catching up", used0)
	}
	if upto0 < 0 {
		t.Fatal("open segment has no index at all; the fixture is not the shape under test")
	}
	if used0-upto0 <= indexCatchUpBytes {
		t.Fatalf("open segment's uncovered tail is %d bytes; the fixture does not exercise catch-up",
			used0-upto0)
	}

	queries := []string{
		`ClusterId >= 0`,
		`Owner == "user3"`,
		`ClusterId >= 5000`,
		`ClusterId == 5999`,
		`Owner == "user0" && ClusterId >= 4096`,
	}

	// Arm 1: no catch-up. reindexMu is held, so every read walks the tail.
	a.c.reindexMu.Lock()
	before := IndexCatchUps()
	walked := make([][]int64, len(queries))
	for i, s := range queries {
		walked[i] = clusterIDs(t, a, mustProbeQuery(t, s))
	}
	if got := IndexCatchUps(); got != before {
		a.c.reindexMu.Unlock()
		t.Fatalf("a read that found reindexMu busy extended %d indexes; it must walk the tail", got-before)
	}
	_, _, uptoBusy := activeSegState(t, a.c)
	if uptoBusy != upto0 {
		a.c.reindexMu.Unlock()
		t.Fatalf("covered prefix moved from %d to %d while reindexMu was held", upto0, uptoBusy)
	}
	a.c.reindexMu.Unlock()

	// Arm 2: the same reads, now free to extend the index.
	indexed := make([][]int64, len(queries))
	for i, s := range queries {
		indexed[i] = clusterIDs(t, a, mustProbeQuery(t, s))
	}
	if got := IndexCatchUps(); got <= before {
		t.Fatalf("no index was extended across %d indexed reads of an archive", len(queries))
	}
	_, used1, upto1 := activeSegState(t, a.c)
	if upto1 <= upto0 {
		t.Fatalf("covered prefix did not grow: %d -> %d (used %d)", upto0, upto1, used1)
	}

	// The rows themselves.
	for i, s := range queries {
		if len(walked[i]) == 0 {
			t.Fatalf("query %q matched nothing in either arm; it tests nothing", s)
		}
		if len(walked[i]) != len(indexed[i]) {
			t.Fatalf("query %q: %d rows walking the tail, %d rows with the index caught up",
				s, len(walked[i]), len(indexed[i]))
		}
		for j := range walked[i] {
			if walked[i][j] != indexed[i][j] {
				t.Fatalf("query %q row %d: ClusterId %d walking the tail, %d with the index caught up",
					s, j, walked[i][j], indexed[i][j])
			}
		}
	}
	if len(walked[0]) != n {
		t.Fatalf("the match-everything query returned %d of %d rows", len(walked[0]), n)
	}

	// Every record individually, so a prefix boundary that drops or duplicates ONE record is
	// caught rather than averaged away.
	for i := 0; i < n; i++ {
		got := clusterIDs(t, a, mustProbeQuery(t, fmt.Sprintf("ClusterId == %d", i)))
		if len(got) != 1 || got[0] != int64(i) {
			t.Fatalf("ClusterId == %d returned %v, want exactly [%d]", i, got, i)
		}
	}
}

// TestArchiveCatchUpThresholdHonored checks that a short uncovered tail is walked rather than
// indexed: extending re-summarizes the whole segment, so a read must not pay that for a handful
// of records.
func TestArchiveCatchUpThresholdHonored(t *testing.T) {
	a, _ := catchUpArchive(t, ArchiveOptions{
		SegmentSize: 1 << 16,
		ValueAttrs:  []string{"ClusterId"},
	}, 3000)

	// Bring the open segment's index up to date, so the tail below starts at zero.
	q := mustProbeQuery(t, `ClusterId >= 0`)
	for range a.Query(q) { //nolint:revive // the read is what extends the index
	}
	_, used0, upto0 := activeSegState(t, a.c)
	if upto0 < 0 {
		t.Fatal("open segment has no index after an indexed read")
	}
	if used0-upto0 > indexCatchUpBytes {
		t.Fatalf("the open segment still has a %d-byte tail after a catch-up read", used0-upto0)
	}

	// Append a tail strictly shorter than the threshold.
	before := IndexCatchUps()
	for {
		_, used, _ := activeSegState(t, a.c)
		if used-upto0 > indexCatchUpBytes/2 {
			break
		}
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(100000+used))
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	_, usedShort, uptoShort := activeSegState(t, a.c)
	if uptoShort != upto0 {
		t.Fatalf("appending moved the covered prefix from %d to %d", upto0, uptoShort)
	}
	tail := usedShort - uptoShort
	if tail == 0 || tail > indexCatchUpBytes {
		t.Fatalf("the tail under test is %d bytes; it must be non-empty and under %d",
			tail, indexCatchUpBytes)
	}

	n := 0
	for range a.Query(q) {
		n++
	}
	if n == 0 {
		t.Fatal("the read matched nothing")
	}
	if got := IndexCatchUps(); got != before {
		t.Errorf("a %d-byte tail (threshold %d) was indexed: %d catch-ups",
			tail, indexCatchUpBytes, got-before)
	}
	_, _, uptoAfter := activeSegState(t, a.c)
	if uptoAfter != upto0 {
		t.Errorf("covered prefix moved from %d to %d over a %d-byte tail", upto0, uptoAfter, tail)
	}
}

// TestArchiveCatchUpBeforeFirstSeal covers a young archive: its one segment has sealed nothing,
// so nothing has reindexed it, and before this it was walked in full by every read for its whole
// first segment. An append log indexes every segment as it seals, so its active segment is a
// catch-up target from the first record.
func TestArchiveCatchUpBeforeFirstSeal(t *testing.T) {
	a, _ := catchUpArchive(t, ArchiveOptions{
		SegmentSize: 1 << 20,
		ValueAttrs:  []string{"ClusterId"},
	}, 2000)
	if sealed := archiveSealedCount(a.c); sealed != 0 {
		t.Fatalf("fixture sealed %d segments; this test is about an archive that has sealed none", sealed)
	}
	_, used0, upto0 := activeSegState(t, a.c)
	if used0-upto0 <= indexCatchUpBytes {
		t.Fatalf("the one segment's uncovered tail is %d bytes; nothing to catch up", used0-upto0)
	}

	before := IndexCatchUps()
	q := mustProbeQuery(t, `ClusterId == 1999`)
	got := clusterIDs(t, a, q)
	if len(got) != 1 || got[0] != 1999 {
		t.Fatalf("ClusterId == 1999 returned %v", got)
	}
	if IndexCatchUps() <= before {
		t.Error("an archive that has sealed nothing did not index its only segment on a read")
	}
	_, _, upto1 := activeSegState(t, a.c)
	if upto1 <= upto0 {
		t.Errorf("covered prefix did not grow: %d -> %d", upto0, upto1)
	}
	if n := len(clusterIDs(t, a, mustProbeQuery(t, `ClusterId >= 0`))); n != 2000 {
		t.Errorf("the match-everything query returned %d of 2000 rows", n)
	}
}

// TestArchiveCatchUpNoDeltaRecords pins the reason an archive's candidate set needs no delta
// augmentation: an append log stores no delta record, whatever DeltaMax asks for, because there
// is no earlier version of a key to merge with. This is the tripwire for that assumption -- a
// partially covered segment whose uncovered tail held deltas would need them in every candidate
// set, and nothing below would notice on its own.
func TestArchiveCatchUpNoDeltaRecords(t *testing.T) {
	c, err := Open(Options{
		Dir:         t.TempDir(),
		AppendOnly:  true,
		Shards:      1,
		SegmentSize: 1 << 16,
		DeltaMax:    16,
		ValueAttrs:  []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.deltaMax != 0 || c.deltaRead {
		t.Fatalf("an append log took DeltaMax: deltaMax=%d deltaRead=%v", c.deltaMax, c.deltaRead)
	}

	const n = 4000
	for i := 0; i < n; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttrString("Pad", "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
		if err := c.Put(nil, ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	// No stored record carries the delta flag, in any segment.
	sh := c.shards[0]
	sh.mu.RLock()
	recs, deltas := 0, 0
	for _, seg := range sh.segs {
		if seg == nil {
			continue
		}
		for off := uint32(0); off < uint32(seg.used); {
			total := recTotalLen(seg.data, off)
			if total == 0 {
				break
			}
			if !recIsMarker(seg.data, off) {
				recs++
				if recIsDelta(seg.data, off) {
					deltas++
				}
			}
			off += total
		}
	}
	sh.mu.RUnlock()
	if recs < n {
		t.Fatalf("walked %d records, expected at least %d; the walk is not covering the store", recs, n)
	}
	if deltas != 0 {
		t.Fatalf("an append log stored %d delta records", deltas)
	}

	// And the reads agree with a full scan once the index is caught up.
	for i := 0; i < n; i += 97 {
		q := mustProbeQuery(t, fmt.Sprintf("ClusterId == %d", i))
		got := 0
		for range c.Query(q) {
			got++
		}
		if got != 1 {
			t.Fatalf("ClusterId == %d returned %d rows, want 1", i, got)
		}
	}
	_, used, upto := activeSegState(t, c)
	if upto <= 0 || used-upto > indexCatchUpBytes {
		t.Errorf("open segment ended at used=%d upto=%d; the reads did not catch it up", used, upto)
	}
}

// TestArchiveCatchUpSkipsAnUnindexedStore pins the guard that keeps a store nobody asked to
// index unindexed. A read cannot reach it: planReadQuery calls catch-up only once it has an
// index-usable probe, and a store with no index configured never has one. So the guard answers
// for the direct callers, where building an index nobody configured would walk and decode every
// record of the store on the first call.
func TestArchiveCatchUpSkipsAnUnindexedStore(t *testing.T) {
	a, _ := catchUpArchive(t, ArchiveOptions{SegmentSize: 1 << 20}, 2000)
	if a.c.spec.Load().any() {
		t.Fatal("fixture configured an index; this test is about a store with none")
	}
	_, used, upto := activeSegState(t, a.c)
	if used <= indexCatchUpBytes {
		t.Fatalf("the active segment holds %d bytes, under the %d-byte threshold, so it would be "+
			"left alone whatever the guard did", used, indexCatchUpBytes)
	}
	if upto != -1 {
		t.Fatalf("an unindexed store's active segment already carries an index covering %d bytes", upto)
	}

	before := IndexCatchUps()
	a.c.catchUpIndexes()
	if got := IndexCatchUps(); got != before {
		t.Errorf("catch-up indexed %d segments of a store with no index configured", got-before)
	}
	if _, _, after := activeSegState(t, a.c); after != -1 {
		t.Errorf("catch-up built an index covering %d bytes on a store with no index configured", after)
	}
}
