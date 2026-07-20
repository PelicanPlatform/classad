package collections

import (
	"fmt"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// fakeClock installs a controllable millisecond clock for deterministic time-travel
// tests and returns a restore func plus a setter.
func fakeClock(t *testing.T, start uint64) (set func(uint64)) {
	t.Helper()
	var cur = start
	prev := nowMillisFn
	nowMillisFn = func() uint64 { return cur }
	t.Cleanup(func() { nowMillisFn = prev })
	return func(ms uint64) { cur = ms }
}

// countAsOf runs q against the AS OF snapshot at t and returns how many ads match.
func countAsOf(t *testing.T, c *Collection, qs string, at uint64) int {
	t.Helper()
	q, err := vm.Parse(qs)
	if err != nil {
		t.Fatalf("parse %q: %v", qs, err)
	}
	seq, err := c.QueryAsOf(q, time.UnixMilli(int64(at)))
	if err != nil {
		t.Fatalf("QueryAsOf(%q, %d): %v", qs, at, err)
	}
	n := 0
	for range seq {
		n++
	}
	return n
}

// TestTimeTravelRoundTrip: an AS OF query returns the value that was current at that
// instant, across an update and a delete; a disabled collection refuses.
func TestTimeTravelRoundTrip(t *testing.T) {
	set := fakeClock(t, 10_000)
	c := New(Options{Shards: 2, TimeTravel: &TimeTravelOptions{
		MaxDistance: time.Hour, CheckpointInterval: time.Second,
	}})

	set(10_000)
	if err := c.Put([]byte("k"), mustAd(t, `[Owner="alice"; JobStatus=1]`)); err != nil {
		t.Fatal(err)
	}
	set(20_000) // +10s -> a new checkpoint is due on this commit
	if err := c.Put([]byte("k"), mustAd(t, `[Owner="alice"; JobStatus=2]`)); err != nil {
		t.Fatal(err)
	}
	set(30_000)
	if !c.Delete([]byte("k")) {
		t.Fatal("delete reported nothing removed")
	}
	set(40_000) // advance so the delete's checkpoint is distinguishable
	_ = c.Put([]byte("other"), mustAd(t, `[Owner="bob"]`))

	if n := countAsOf(t, c, `JobStatus == 1`, 15_000); n != 1 {
		t.Errorf("count(JobStatus==1) AS OF 15s = %d, want 1", n)
	}
	if n := countAsOf(t, c, `JobStatus == 2`, 25_000); n != 1 {
		t.Errorf("count(JobStatus==2) AS OF 25s = %d, want 1 (post-update)", n)
	}
	if n := countAsOf(t, c, `JobStatus == 1`, 25_000); n != 0 {
		t.Errorf("count(JobStatus==1) AS OF 25s = %d, want 0 (superseded by then)", n)
	}
	// After the delete, k is gone (only "other" remains).
	if n := countAsOf(t, c, `JobStatus == 2`, 35_000); n != 0 {
		t.Errorf("count(JobStatus==2) AS OF 35s = %d, want 0 (k deleted)", n)
	}

	// Disabled collection refuses AS OF.
	off := New(Options{Shards: 2})
	q, _ := vm.Parse(`true`)
	if _, err := off.QueryAsOf(q, time.UnixMilli(1000)); err != ErrTimeTravelDisabled {
		t.Errorf("QueryAsOf on a disabled collection = %v, want ErrTimeTravelDisabled", err)
	}
}

// TestTimeTravelCompactionRetainsThenReclaims: an in-window superseded version
// survives compaction (still AS OF-queryable), and once it ages past MaxDistance a
// later compaction reclaims it (AS OF that far back is refused).
func TestTimeTravelCompactionRetainsThenReclaims(t *testing.T) {
	set := fakeClock(t, 1_000_000)
	c := New(Options{Shards: 1, SegmentSize: 1 << 14, TimeTravel: &TimeTravelOptions{
		MaxDistance: 10 * time.Minute, CheckpointInterval: time.Second,
	}})

	// Churn one key across many versions at 1s steps so each gets its own checkpoint.
	base := uint64(1_000_000)
	for i := 0; i < 40; i++ {
		set(base + uint64(i)*1000)
		if err := c.Put([]byte("k"), mustAd(t, fmt.Sprintf(`[V=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	// Also churn filler keys to build dead bytes so compaction triggers.
	for i := 0; i < 300; i++ {
		set(base + 40_000 + uint64(i)*10)
		_ = c.Put([]byte(fmt.Sprintf("f%d", i%20)), mustAd(t, fmt.Sprintf(`[F=%d]`, i)))
	}

	set(base + 60_000) // still within the 10-minute window
	c.Compact()

	// The version current at base+5s (V=5) must survive compaction.
	if n := countAsOf(t, c, `V == 5`, base+5_000); n != 1 {
		t.Errorf("count(V==5) AS OF +5s after compaction = %d, want 1 (in-window history retained)", n)
	}
	if n := countAsOf(t, c, `V == 5`, base+38_000); n != 0 {
		t.Errorf("count(V==5) AS OF +38s = %d, want 0 (superseded long before)", n)
	}
	// Current value is the last write (V=39), visible at a recent AS OF.
	if n := countAsOf(t, c, `V == 39`, base+55_000); n != 1 {
		t.Errorf("count(V==39) AS OF +55s = %d, want 1 (current)", n)
	}

	// Advance well past the window and compact: the old versions age out and are
	// reclaimed, so an AS OF that far back is refused.
	set(base + 60_000 + uint64((11 * time.Minute).Milliseconds()))
	// A write moves the working set forward so compaction has something to do.
	_ = c.Put([]byte("k"), mustAd(t, `[V=99]`))
	c.Compact()
	q, _ := vm.Parse(`true`)
	if _, err := c.QueryAsOf(q, time.UnixMilli(int64(base+5_000))); err == nil {
		t.Error("QueryAsOf +5s after the window elapsed should be refused (data reclaimed)")
	}
}

// TestTimeTravelRecovery: markers survive a persistent close/reopen -- the time index
// is rebuilt from the segment markers, so AS OF still resolves after restart.
func TestTimeTravelRecovery(t *testing.T) {
	set := fakeClock(t, 5_000)
	dir := t.TempDir()
	opts := Options{Dir: dir, Shards: 2, TimeTravel: &TimeTravelOptions{
		MaxDistance: time.Hour, CheckpointInterval: time.Second,
	}}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	set(5_000)
	_ = c.Put([]byte("k"), mustAd(t, `[V=1]`))
	set(15_000)
	_ = c.Put([]byte("k"), mustAd(t, `[V=2]`))
	set(25_000)
	_ = c.Put([]byte("k"), mustAd(t, `[V=3]`))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if n := countAsOf(t, c2, `V == 1`, 10_000); n != 1 {
		t.Errorf("after reopen, count(V==1) AS OF 10s = %d, want 1 (time index rebuilt from markers)", n)
	}
	if n := countAsOf(t, c2, `V == 2`, 20_000); n != 1 {
		t.Errorf("after reopen, count(V==2) AS OF 20s = %d, want 1", n)
	}
}

// TestTimeTravelDisabledNoMarkers: with time travel off, no marker entries are written
// (zero write-path overhead) -- a scan of the raw arena finds none.
func TestTimeTravelDisabledNoMarkers(t *testing.T) {
	c := New(Options{Shards: 1})
	for i := 0; i < 50; i++ {
		_ = c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[V=%d]`, i)))
	}
	sh := c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	for _, seg := range sh.segs {
		if seg == nil {
			continue
		}
		for off := 0; off < seg.used; {
			o := uint32(off)
			total := recTotalLen(seg.data, o)
			if total == 0 {
				break
			}
			if recIsMarker(seg.data, o) {
				t.Fatal("found a time-checkpoint marker with time travel disabled")
			}
			off += int(total)
		}
	}
}
