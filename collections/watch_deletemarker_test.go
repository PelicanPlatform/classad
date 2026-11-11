package collections

import (
	"fmt"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// deleteMarkerKeys walks every segment of every shard and returns the key each surviving
// delete marker names, by sequence. It reads the arena the way recovery does, so it sees
// exactly what a resuming watcher could be told after a restart.
func deleteMarkerKeys(t *testing.T, c *Collection) map[uint64]string {
	t.Helper()
	out := map[uint64]string{}
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 {
				continue
			}
			for off := 0; off < seg.used; {
				o := uint32(off)
				total := recTotalLen(seg.data, o)
				if total == 0 {
					break
				}
				if k, ok := recDeleteMarkerKey(seg.data, o); ok {
					out[recSeq(seg.data, o)] = string(k)
				}
				off += int(total)
			}
		}
		sh.mu.RUnlock()
	}
	return out
}

// TestCompactionKeepsDeleteMarkerKeys pins the one fact a restart-surviving watch resume
// needs from storage: the key a delete marker names must still be there after the segment
// holding it has been rewritten. Compaction carries markers above the retain floor forward,
// so the marker's sequence survives; this is about its body.
func TestCompactionKeepsDeleteMarkerKeys(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{
		Dir:          dir,
		Shards:       1,
		SegmentSize:  8 << 10,
		WatchHistory: 1024,
		TimeTravel:   &TimeTravelOptions{MaxDistance: time.Hour, CheckpointInterval: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const deleted = "key-000003"
	put := func(i, gen int) {
		t.Helper()
		if err := c.Put([]byte(fmt.Sprintf("key-%06d", i)), padAd(t, gen)); err != nil {
			t.Fatal(err)
		}
	}
	// The marker must land in a segment that keeps a current record, so the segment stays
	// in the working set and is rewritten rather than left alone as pure history. Keys 0-9
	// share the first segment with it and are never written again.
	for i := range 10 {
		put(i, i)
	}
	if !c.Delete([]byte(deleted)) {
		t.Fatalf("Delete(%q) found nothing to remove", deleted)
	}
	for i := 10; i < 40; i++ {
		put(i, i)
	}
	before := deleteMarkerKeys(t, c)
	if len(before) != 1 || before[firstKey(before)] != deleted {
		t.Fatalf("before compaction, delete markers = %v, want one naming %q", before, deleted)
	}

	// Churn only the later keys, so the marker's segment accumulates dead bytes without
	// becoming pure history. compactShard is called directly rather than through Compact so
	// the rewrite does not depend on a byte threshold the fixture would have to be tuned to.
	for round := range 12 {
		for i := 20; i < 40; i++ {
			put(i, i+100*round)
		}
	}
	segsBefore := liveSegmentCount(c)
	c.compactShard(c.shards[0], c.currentCodec())
	if segsBefore == liveSegmentCount(c) {
		t.Fatalf("compactShard rewrote nothing (%d segments before and after); the fixture is not exercising the carry path", segsBefore)
	}

	after := deleteMarkerKeys(t, c)
	if len(after) == 0 {
		t.Fatalf("compaction dropped the delete marker entirely; before = %v", before)
	}
	for seq, key := range after {
		if key != deleted {
			t.Errorf("delete marker at seq %d names %q, want %q", seq, key, deleted)
		}
	}
}

// padAd is an ad big enough that a handful fill a small segment.
func padAd(t *testing.T, i int) *classad.ClassAd {
	t.Helper()
	return mkAd(t, fmt.Sprintf("[N = %d; Pad = \"%0200d\"]", i, i))
}

// liveSegmentCount is how many segments the shard holds, so a test can tell a rewrite
// that ran from one that silently did nothing.
func liveSegmentCount(c *Collection) int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg.used > 0 {
				n++
			}
		}
		sh.mu.RUnlock()
	}
	return n
}

func firstKey(m map[uint64]string) uint64 {
	for k := range m {
		return k
	}
	return 0
}

// TestColumnarizeKeepsDeleteMarkerKeys is the twin of
// TestCompactionKeepsDeleteMarkerKeys. Columnarizing a sealed segment rewrites it
// record by record; a marker is not a data record, so it is dropped unless the rewrite
// carries it, and dropping it loses the only durable evidence of the delete.
func TestColumnarizeKeepsDeleteMarkerKeys(t *testing.T) {
	const deleted = "7.0"
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 64 << 10, WatchHistory: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	put := func(i int) {
		t.Helper()
		ad := mkAd(t, fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep" ]`,
			i, i%10, i%7, i%6, (i%16)*1024))
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 20 {
		put(i)
	}
	if !c.Delete([]byte(deleted)) {
		t.Fatalf("Delete(%q) found nothing to remove", deleted)
	}
	for i := 20; i < 2000; i++ {
		put(i)
	}
	if got := deleteMarkerKeys(t, c); len(got) != 1 || got[firstKey(got)] != deleted {
		t.Fatalf("before columnarizing, delete markers = %v, want one naming %q", got, deleted)
	}

	s, hot, ok := c.deriveSchema(4096, 8)
	if !ok {
		t.Fatal("no schema could be derived from the fixture")
	}
	if !c.installSchemaScan(s, hot) {
		t.Fatal("schema scan did not enable")
	}

	sh := c.shards[0]
	sh.mu.Lock()
	var src *segment
	for _, seg := range sh.segs {
		if seg == nil || seg == sh.act || seg.used == 0 {
			continue
		}
		if segmentHasDeleteMarker(seg) {
			src = seg
			break
		}
	}
	if src == nil {
		sh.mu.Unlock()
		t.Fatal("no sealed segment holds the delete marker; the fixture did not seal it")
	}
	dst, _, _ := c.columnarizeSegment(sh, src, s, hot, c.planDeltasLocked(sh, src))
	sh.mu.Unlock()
	if dst == nil {
		t.Fatal("columnarizeSegment returned nil")
	}
	defer func() {
		dst.retire()
		dst.reapAndHook()
	}()
	if !segmentHasDeleteMarker(dst) {
		t.Error("columnarizing dropped the delete marker; a watcher resuming from before it would never be told the key went away")
	}
}

// segmentHasDeleteMarker reports whether seg holds any delete marker.
func segmentHasDeleteMarker(seg *segment) bool {
	for off := 0; off < seg.used; {
		o := uint32(off)
		total := recTotalLen(seg.data, o)
		if total == 0 {
			break
		}
		if _, ok := recDeleteMarkerKey(seg.data, o); ok {
			return true
		}
		off += int(total)
	}
	return false
}

// TestKeyReaddedAfterColumnarizeStillReads guards the hazard that carrying delete markers
// across a rewrite introduces: the key index files a carried marker as a tombstone under
// the key it deletes, so a rewritten segment now claims that key was removed. A key put
// back afterwards must still read, or the carry has turned a preserved delete into a lost
// record.
func TestKeyReaddedAfterColumnarizeStillReads(t *testing.T) {
	const key = "7.0"
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 64 << 10, WatchHistory: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	put := func(i int, status int) {
		t.Helper()
		ad := mkAd(t, fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep" ]`,
			i, i%10, i%7, status, (i%16)*1024))
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 20 {
		put(i, 1)
	}
	if !c.Delete([]byte(key)) {
		t.Fatalf("Delete(%q) found nothing to remove", key)
	}
	for i := 20; i < 2000; i++ {
		put(i, 1)
	}
	c.BuildAndEnableSchemaScan(4096, 8) // columnarizes every sealed segment
	if columnarSegmentsForTest(c) == 0 {
		t.Fatal("fixture: nothing columnarized, so no marker was carried")
	}
	if got, ok := c.Get([]byte(key)); ok {
		t.Fatalf("Get(%q) = %v after the delete, want absent", key, got)
	}

	// Put it back. The tombstone the rewrite carried is older than this record and must not
	// shadow it.
	put(7, 5)
	ad, ok := c.Get([]byte(key))
	if !ok {
		t.Fatalf("Get(%q) found nothing after it was put back; a carried tombstone is shadowing a live record", key)
	}
	v, ok := ad.EvaluateAttrInt("JobStatus")
	if !ok || v != 5 {
		t.Errorf("JobStatus = %v (ok %v), want 5: the read did not come from the record just written", v, ok)
	}
}
