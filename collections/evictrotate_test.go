package collections

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

const (
	newEra = 2_000_000_000
	oldEra = 1_000_000_000
)

func countMatching(t *testing.T, a *Archive, constraint string) int {
	t.Helper()
	q, err := vm.Parse(constraint)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range a.Query(q) {
		n++
	}
	return n
}

func appendEra(t *testing.T, a *Archive, base, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ T=%d; Pad=%q ]`, base+i, profPad(96)))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
}

// The hub/spoke case end to end. The archive first receives recent history, then a spoke
// replays a year of backfill. Under the positional rule the recent history is in the OLDEST
// segments and is evicted first, leaving the backfill. Ordering by the records' own values
// has to reverse that.
func TestRotateEvictsByValueNotPosition(t *testing.T) {
	a, err := CreateArchive(ArchiveOptions{
		Dir:         t.TempDir(),
		SegmentSize: 1 << 14,
		ZoneAttrs:   []string{"T"},
		Retention:   Retention{EvictByAttr: "T"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	appendEra(t, a, newEra, 500)  // recent history: oldest POSITION
	appendEra(t, a, oldEra, 1500) // a spoke's backfill: older DATA, newer position

	recentBefore := countMatching(t, a, fmt.Sprintf("T >= %d", newEra))
	backfillBefore := countMatching(t, a, fmt.Sprintf("T < %d", newEra))
	if recentBefore != 500 || backfillBefore != 1500 {
		t.Fatalf("before: recent=%d backfill=%d, want 500/1500", recentBefore, backfillBefore)
	}

	st := a.Stats()
	a.SetRetention(Retention{EvictByAttr: "T", MaxBytes: st.UsedBytes / 2})
	if _, err := a.Rotate(0); err != nil {
		t.Fatal(err)
	}

	recentAfter := countMatching(t, a, fmt.Sprintf("T >= %d", newEra))
	backfillAfter := countMatching(t, a, fmt.Sprintf("T < %d", newEra))
	t.Logf("recent %d -> %d, backfill %d -> %d", recentBefore, recentAfter, backfillBefore, backfillAfter)

	if backfillAfter >= backfillBefore {
		t.Errorf("the backfill was not evicted at all (%d -> %d)", backfillBefore, backfillAfter)
	}
	// The point: the recent history, which the positional rule would have taken first,
	// survives in full.
	if recentAfter != recentBefore {
		t.Errorf("recent history lost %d records to a size pass while older data survived",
			recentBefore-recentAfter)
	}
}

// With no eviction axis configured the behaviour must be exactly what it was: drop the
// oldest segments by position. This is the regression guard for every existing deployment.
func TestRotateWithoutEvictAttrIsPositional(t *testing.T) {
	a, err := CreateArchive(ArchiveOptions{
		Dir:         t.TempDir(),
		SegmentSize: 1 << 14,
		ZoneAttrs:   []string{"T"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	appendEra(t, a, newEra, 500)
	appendEra(t, a, oldEra, 1500)

	st := a.Stats()
	a.SetRetention(Retention{MaxBytes: st.UsedBytes / 2}) // no EvictByAttr, no MaxAgeAttr
	if _, err := a.Rotate(0); err != nil {
		t.Fatal(err)
	}

	// Positional: the oldest segments go, which here hold the RECENT era.
	recentAfter := countMatching(t, a, fmt.Sprintf("T >= %d", newEra))
	if recentAfter == 500 {
		t.Error("nothing was evicted from the oldest (recent-era) segments: not the positional rule")
	}
}

// A hole left by value-ordered eviction must move the watch floor past it. Without that a
// resuming cursor walks over the gap and reports the stream as contiguous.
func TestRotateHoleRaisesTheWatchFloor(t *testing.T) {
	a, err := CreateArchive(ArchiveOptions{
		Dir:         t.TempDir(),
		SegmentSize: 1 << 14,
		ZoneAttrs:   []string{"T"},
		Retention:   Retention{EvictByAttr: "T"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	appendEra(t, a, newEra, 500)
	appendEra(t, a, oldEra, 1500)

	// Record every segment's newest seq, so we can tell afterwards which ones went and how
	// far the floor has to have moved to cover them.
	sh := a.c.shards[0]
	sh.mu.RLock()
	maxSeqAt := map[int]uint64{}
	for i, s := range sh.segs {
		if s != nil && s != sh.act {
			maxSeqAt[i] = s.maxSeq
		}
	}
	sh.mu.RUnlock()

	st := a.Stats()
	a.SetRetention(Retention{EvictByAttr: "T", MaxBytes: st.UsedBytes / 2})
	dropped, err := a.Rotate(0)
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 {
		t.Fatal("nothing dropped; the floor case is untested")
	}

	sh.mu.RLock()
	floorAfter := sh.appendFloor
	var highestDropped uint64
	var holeFound bool
	for i, seq := range maxSeqAt {
		if sh.segs[i] != nil {
			continue // survived
		}
		highestDropped = max(highestDropped, seq)
		// A hole is a dropped segment with a SURVIVING one below it -- the shape the prefix
		// rule cannot express.
		for j := range maxSeqAt {
			if j < i && sh.segs[j] != nil {
				holeFound = true
			}
		}
	}
	sh.mu.RUnlock()

	if !holeFound {
		t.Fatal("no out-of-order drop happened; this test would pass under the prefix rule alone")
	}
	// Every cursor below a dropped segment's newest record must be forced to resync, or a
	// catch-up walks straight over the gap and reports the stream as contiguous.
	if floorAfter < highestDropped {
		t.Errorf("append floor %d is below the newest record of a dropped segment (%d): "+
			"a cursor between them would read the stream as contiguous", floorAfter, highestDropped)
	}
}

// Rotate runs on the maintenance tick. Between ticks nothing bounded growth, so a spoke
// replaying a backlog overshot MaxBytes by however much arrived in one interval -- which is
// what the ceiling exists to prevent. The seal-time pass has to hold the line with no
// maintenance call at all.
func TestHighMarkBoundsGrowthBetweenMaintenancePasses(t *testing.T) {
	const ceiling = 400 << 10 // 400 KiB

	a, err := CreateArchive(ArchiveOptions{
		Dir:         t.TempDir(),
		SegmentSize: 1 << 14,
		ZoneAttrs:   []string{"T"},
		Retention:   Retention{EvictByAttr: "T", MaxBytes: ceiling, TargetBytes: ceiling * 3 / 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Far more than the cap, appended without a single Rotate.
	appendEra(t, a, newEra, 6000)

	if st := a.Stats(); st.UsedBytes > ceiling*2 {
		t.Errorf("archive grew to %d bytes against a %d cap with no maintenance pass", st.UsedBytes, ceiling)
	} else {
		t.Logf("held at %d bytes against a %d cap", st.UsedBytes, ceiling)
	}
	// It must still be a working archive, not an empty one.
	if n := countMatching(t, a, fmt.Sprintf("T >= %d", newEra)); n == 0 {
		t.Error("the high-mark pass emptied the archive")
	}
}

// With no byte ceiling the seal path must not evict anything -- and must not start walking
// segments on every seal to find that out.
func TestHighMarkInertWithoutAByteCeiling(t *testing.T) {
	a, err := CreateArchive(ArchiveOptions{
		Dir:         t.TempDir(),
		SegmentSize: 1 << 14,
		ZoneAttrs:   []string{"T"},
		Retention:   Retention{EvictByAttr: "T"}, // axis configured, no ceiling
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	appendEra(t, a, newEra, 2000)
	if n := countMatching(t, a, fmt.Sprintf("T >= %d", newEra)); n != 2000 {
		t.Errorf("records = %d, want all 2000 kept (nothing bounds this archive)", n)
	}
}

// TargetBytes is the hysteresis gap. Without it a pass frees to just under the ceiling and
// the next seal puts the archive straight back over it, so eviction runs continuously --
// the same churn MergeOptions.TriggerSegments/TargetSegments and the index budget's
// high/low fractions both exist to avoid. The symptom is passes, not size.
func TestTargetBytesCutsEvictionChurn(t *testing.T) {
	const ceiling = 400 << 10

	run := func(target int64) int64 {
		t.Helper()
		a, err := CreateArchive(ArchiveOptions{
			Dir:         t.TempDir(),
			SegmentSize: 1 << 14,
			ZoneAttrs:   []string{"T"},
			Retention:   Retention{EvictByAttr: "T", MaxBytes: ceiling, TargetBytes: target},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		appendEra(t, a, newEra, 6000)
		passes, _ := a.c.HighMarkStats()
		return passes
	}

	withGap := run(ceiling / 2)
	withoutGap := run(0) // evict to just under the ceiling
	t.Logf("eviction passes: with a low mark %d, without %d", withGap, withoutGap)

	if withGap >= withoutGap {
		t.Errorf("TargetBytes did not reduce eviction passes (%d with, %d without): "+
			"the archive is evicting on nearly every seal", withGap, withoutGap)
	}
}

// Interleaved data is the shape a hub tailing several spokes actually produces: every
// segment holds both eras, so NO segment is entirely below any useful cutoff. Whole-segment
// eviction can only reach the target by discarding recent records along with old ones; the
// trim is the only step that can free space without doing that.
func TestRotateTrimsStraddlingSegments(t *testing.T) {
	a, err := CreateArchive(ArchiveOptions{
		Dir:         t.TempDir(),
		SegmentSize: 1 << 15,
		// Small segments need a bucket to match, or every segment is all-or-nothing to
		// the cutoff search and the trim has nothing to work on. Stated here rather than
		// left to the default, which is sized for a production segment.
		ProfileBucketBytes: 4 << 10,
		ZoneAttrs:          []string{"T"},
		Retention:          Retention{EvictByAttr: "T"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Alternate the eras so every segment straddles.
	for i := 0; i < 3000; i++ {
		base := newEra
		if i%2 == 0 {
			base = oldEra
		}
		ad, perr := classad.Parse(fmt.Sprintf(`[ T=%d; Pad=%q ]`, base+i, profPad(96)))
		if perr != nil {
			t.Fatal(perr)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}

	recentBefore := countMatching(t, a, fmt.Sprintf("T >= %d", newEra))
	oldBefore := countMatching(t, a, fmt.Sprintf("T < %d", newEra))
	if recentBefore == 0 || oldBefore == 0 {
		t.Fatalf("fixture: recent=%d old=%d", recentBefore, oldBefore)
	}

	st := a.Stats()
	a.SetRetention(Retention{EvictByAttr: "T", MaxBytes: st.UsedBytes * 3 / 4, TargetBytes: st.UsedBytes / 2})
	if _, err := a.Rotate(0); err != nil {
		t.Fatal(err)
	}

	recentAfter := countMatching(t, a, fmt.Sprintf("T >= %d", newEra))
	oldAfter := countMatching(t, a, fmt.Sprintf("T < %d", newEra))
	t.Logf("recent %d -> %d, old %d -> %d, bytes %d -> %d",
		recentBefore, recentAfter, oldBefore, oldAfter, st.UsedBytes, a.Stats().UsedBytes)

	if oldAfter >= oldBefore {
		t.Errorf("no old-era records were evicted (%d -> %d)", oldBefore, oldAfter)
	}
	if got := a.Stats().UsedBytes; got > st.UsedBytes*3/4 {
		t.Errorf("still %d bytes, over the %d ceiling", got, st.UsedBytes*3/4)
	}
	// THE assertion. Every segment held both eras in equal measure, so a rule that can only
	// drop whole segments removes them in equal measure too -- the eras come out of such a
	// pass with the same survivor count. Trimming is what lets a pass take the old records
	// out of a segment and leave the recent ones, so the eras must come out LOPSIDED.
	if recentAfter <= oldAfter*2 {
		t.Errorf("recent %d vs old %d survivors: the eras were evicted at the same rate, "+
			"which is what dropping straddlers whole looks like", recentAfter, oldAfter)
	}
}

// A trim must leave the archive readable and self-consistent: the surviving records are
// exactly those at or above the cutoff, and they still read back.
func TestTrimSegmentKeepsEverythingAboveTheCutoff(t *testing.T) {
	a, err := CreateArchive(ArchiveOptions{
		Dir:         t.TempDir(),
		SegmentSize: 1 << 15,
		// Small segments need a bucket to match, or every segment is all-or-nothing to
		// the cutoff search and the trim has nothing to work on. Stated here rather than
		// left to the default, which is sized for a production segment.
		ProfileBucketBytes: 4 << 10,
		ZoneAttrs:          []string{"T"},
		Retention:          Retention{EvictByAttr: "T"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	for i := 0; i < 400; i++ {
		ad, perr := classad.Parse(fmt.Sprintf(`[ T=%d; N=%d; Pad=%q ]`, 1000+i, i, profPad(96)))
		if perr != nil {
			t.Fatal(perr)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	a.Flush() //nolint:errcheck // the seal below is what matters

	sh := a.c.shards[0]
	sh.mu.RLock()
	var victim *segment
	for _, s := range sh.segs {
		if s != nil && s != sh.act {
			victim = s
			break
		}
	}
	sh.mu.RUnlock()
	if victim == nil {
		t.Skip("no sealed segment to trim")
	}

	const cutoff = 1100
	before := countMatching(t, a, fmt.Sprintf("T >= %d", cutoff))

	a.c.maintMu.Lock()
	dropped, ok := a.c.trimSegmentToCutoff(sh, victim, "T", cutoff)
	a.c.maintMu.Unlock()
	if !ok {
		t.Skipf("segment held nothing below %d", cutoff)
	}
	if dropped == 0 {
		t.Fatal("trim reported success but dropped nothing")
	}

	// Nothing above the cutoff may be lost...
	if after := countMatching(t, a, fmt.Sprintf("T >= %d", cutoff)); after != before {
		t.Errorf("records at or above the cutoff: %d -> %d", before, after)
	}
	// ...and nothing below it may remain in the trimmed segment.
	sh.mu.RLock()
	trimmed := sh.segs[victim.id]
	sh.mu.RUnlock()
	if trimmed != nil && trimmed.profile != nil && trimmed.profile.haveValues {
		if trimmed.profile.valueMin < cutoff {
			t.Errorf("trimmed segment still reports values from %v, below the %d cutoff",
				trimmed.profile.valueMin, cutoff)
		}
	}
}

// A trim must keep every record whose value it cannot bound -- an expression, a non-numeric,
// or the attribute missing. Nothing has established that such a record is below the cutoff,
// and a dropped record cannot be brought back to find out. This is the same rule the profile
// applies when it refuses to offer unknown bytes for eviction; the trim is the other half of
// it, and the half that actually deletes data.
func TestTrimKeepsRecordsItCannotBound(t *testing.T) {
	a, err := CreateArchive(ArchiveOptions{
		Dir:                t.TempDir(),
		SegmentSize:        1 << 15,
		ZoneAttrs:          []string{"T"},
		ProfileBucketBytes: 4 << 10,
		Retention:          Retention{EvictByAttr: "T"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Four shapes, interleaved so they share segments.
	for i := 0; i < 400; i++ {
		var src string
		switch i % 4 {
		case 0:
			src = fmt.Sprintf(`[ Kind="low"; T=%d; Pad=%q ]`, 1000+i, profPad(96))
		case 1:
			src = fmt.Sprintf(`[ Kind="expr"; T = 1000 + %d; Pad=%q ]`, i, profPad(96)) // unevaluated
		case 2:
			src = fmt.Sprintf(`[ Kind="string"; T="not a number"; Pad=%q ]`, profPad(96))
		default:
			src = fmt.Sprintf(`[ Kind="absent"; Pad=%q ]`, profPad(96)) // no T at all
		}
		ad, perr := classad.Parse(src)
		if perr != nil {
			t.Fatal(perr)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}

	unboundedBefore := map[string]int{}
	for _, k := range []string{"expr", "string", "absent"} {
		unboundedBefore[k] = countMatching(t, a, fmt.Sprintf("Kind == %q", k))
		if unboundedBefore[k] == 0 {
			t.Fatalf("fixture holds no %q records", k)
		}
	}

	// A cutoff above every bounded value: the "low" records are all below it.
	sh := a.c.shards[0]
	sh.mu.RLock()
	var victim *segment
	for _, s := range sh.segs {
		if s != nil && s != sh.act {
			victim = s
			break
		}
	}
	sh.mu.RUnlock()
	if victim == nil {
		t.Skip("no sealed segment")
	}

	a.c.maintMu.Lock()
	dropped, ok := a.c.trimSegmentToCutoff(sh, victim, "T", 1e9)
	a.c.maintMu.Unlock()
	if !ok || dropped == 0 {
		t.Fatalf("trim did not run (ok=%v dropped=%d)", ok, dropped)
	}

	for _, k := range []string{"expr", "string", "absent"} {
		if after := countMatching(t, a, fmt.Sprintf("Kind == %q", k)); after != unboundedBefore[k] {
			t.Errorf("%q records %d -> %d: the trim removed records whose value it could not bound",
				k, unboundedBefore[k], after)
		}
	}
}

// Value-ordered eviction composes with the Reset reasons: a cursor stranded by a HOLE must
// be told "retention", the reason whose contract is that the records between the cursor and
// the replay are gone rather than merely re-sent. That is exactly true of a hole and of a
// trim, and it is not something either change established on its own -- the floor is raised
// here, the reason is chosen in the watch, and nothing connects them but this.
func TestEvictionHoleResetsForRetention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := CreateArchive(ArchiveOptions{
		Dir:         t.TempDir(),
		SegmentSize: 1 << 14,
		ZoneAttrs:   []string{"T"},
		Retention:   Retention{EvictByAttr: "T"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Recent history first, so it occupies the OLDEST segments...
	appendEra(t, a, newEra, 500)
	_, _, cursor := drainCatchup(t, mustWatch(t, a.c, ctx, nil))
	if cursor == nil {
		t.Fatal("no cursor from the initial sync")
	}
	// ...then a backfill of older data, which lands in the NEWEST segments. Everything the
	// cursor has not seen is in those.
	appendEra(t, a, oldEra, 1500)

	st := a.Stats()
	a.SetRetention(Retention{EvictByAttr: "T", MaxBytes: st.UsedBytes / 2})
	dropped, err := a.Rotate(0)
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 {
		t.Fatal("nothing evicted, so the cursor is still resumable and this proves nothing")
	}

	// The hole is above the surviving recent-era segments, so the oldest surviving record --
	// the prefix rule's whole answer -- is still below the cursor. Only the hole's own bound
	// strands it.
	sh := a.c.shards[0]
	sh.mu.RLock()
	oldest := sh.oldestRecordSeqLocked()
	floor := sh.appendFloor
	sh.mu.RUnlock()
	if floor <= oldest {
		t.Fatalf("append floor %d is at or below the oldest surviving record %d: "+
			"the drop was a prefix, not a hole, and this is the other test", floor, oldest)
	}

	if got := drainReason(t, mustWatch(t, a.c, ctx, cursor)); got != WatchResetRetention {
		t.Errorf("cursor stranded by an eviction hole: reason = %v, want retention", got)
	}
}
