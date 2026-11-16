package collections

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// collectCatchUpN consumes at most n catch-up events, returning them, the cursor of the last
// one that carried one, and whether the stream reached WatchSynced within n.
func collectCatchUpN(t *testing.T, c *Collection, cursor []byte, n int) (evs []WatchEvent, last []byte, synced bool) {
	t.Helper()
	seq, err := c.Watch(context.Background(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	for ev := range seq {
		if ev.Kind == WatchSynced {
			synced = true
			if len(ev.Cursor) > 0 {
				last = ev.Cursor
			}
			break
		}
		evs = append(evs, ev)
		if len(ev.Cursor) > 0 {
			last = ev.Cursor
		}
		if len(evs) >= n {
			break
		}
	}
	return evs, last, synced
}

// TestCatchupResumesPartwayWithoutReReading is the property a long catch-up lives or dies by: a
// consumer that is cut off partway and comes back with the last cursor it was handed reads the
// REST of the backlog, not the whole of it again.
//
// Without a cursor on catch-up events there is nothing to come back with -- the first resume
// point is the one WatchSynced carries, at the very end -- so a catch-up that keeps being
// interrupted before it finishes makes no progress at all, however many times it runs. That is
// not a slow catch-up; it is a stalled one, and it is invisible: every session ends "cleanly"
// and applies real records, it just never commits.
func TestCatchupResumesPartwayWithoutReReading(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, WatchHistory: 8192})
	const base = 100
	for i := 0; i < base; i++ {
		if err := c.Put(wkey(i), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	_, cursor := collectCatchUp(t, c, nil) // a resume point, as a consumer that kept up would hold

	// The backlog that builds while the consumer is away.
	const n = 400
	for i := 1000; i < 1000+n; i++ {
		if err := c.Put(wkey(i), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}

	// Cut the catch-up off a quarter of the way in and come back with what it handed us.
	const cut = n / 4
	first, resume, synced := collectCatchUpN(t, c, cursor, cut)
	if synced {
		t.Fatalf("the stream synced within %d events; it cannot show a partial resume", cut)
	}
	if len(resume) == 0 {
		t.Fatal("no catch-up event carried a cursor, so an interrupted catch-up has nothing to " +
			"resume from and must re-read the whole backlog on every reconnect")
	}

	seen := map[string]int{}
	for _, ev := range first {
		if ev.Kind == WatchUpsert {
			seen[string(ev.Key)]++
		}
	}
	rest, _ := collectCatchUp(t, c, resume)
	restUpserts := 0
	for _, ev := range rest {
		if ev.Kind == WatchReset {
			t.Fatal("resuming from a catch-up cursor forced a full replay; the cursor is not a " +
				"valid resume point")
		}
		if ev.Kind == WatchUpsert {
			seen[string(ev.Key)]++
			restUpserts++
		}
	}

	// Nothing lost across the seam.
	for i := 1000; i < 1000+n; i++ {
		if seen[skey(i)] == 0 {
			t.Fatalf("key %s was delivered by neither the interrupted catch-up nor the resume; "+
				"the cursor skipped past records it had not handed over", skey(i))
		}
	}
	// And real progress: the resume reads the remainder, not the whole backlog over again.
	if restUpserts > n-len(first) {
		t.Errorf("the resume re-read %d upserts after %d were already delivered, of %d in the "+
			"backlog; the catch-up is not resuming where it left off", restUpserts, len(first), n)
	}
	for i := 1000; i < 1000+n; i++ {
		if seen[skey(i)] > 1 {
			t.Errorf("key %s was delivered %d times across the seam; the resume cursor is not "+
				"exclusive of the record it names", skey(i), seen[skey(i)])
		}
	}
}

// TestArchiveFullReplayResumesPartway covers the longest catch-up there is: the full replay a
// Reset opens on an archive. An archive discards nothing on a Reset -- there is no sweep, and a
// record redelivered is one its consumer deduplicates -- so its replay is resumable, which
// matters most here because it is the one that can run for hours.
func TestArchiveFullReplayResumesPartway(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	t.Parallel()
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 13})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const n = 1500
	for i := 0; i < n; i++ {
		ad, _ := classad.ParseOld(fmt.Sprintf("Id = %d", i))
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}

	// A nil cursor is the full replay: Reset, then everything retained.
	const cut = 200
	first, resume, synced := collectCatchUpN(t, a.c, nil, cut)
	if synced {
		t.Fatalf("the replay synced within %d events; it cannot show a partial resume", cut)
	}
	if first[0].Kind != WatchReset {
		t.Fatalf("a replay from nil opened with %v, want WatchReset", first[0].Kind)
	}
	if len(resume) == 0 {
		t.Fatal("no event of an archive's full replay carried a cursor, so a replay interrupted " +
			"partway restarts from the beginning every time")
	}
	delivered := 0
	for _, ev := range first {
		if ev.Kind == WatchUpsert {
			delivered++
		}
	}

	rest, _ := collectCatchUp(t, a.c, resume)
	restUpserts := 0
	for _, ev := range rest {
		if ev.Kind == WatchReset {
			t.Fatal("resuming an archive replay from its own cursor started the replay over")
		}
		if ev.Kind == WatchUpsert {
			restUpserts++
		}
	}
	if got := delivered + restUpserts; got != n {
		t.Errorf("the replay and its resume delivered %d records in all (%d then %d), want %d: "+
			"the seam lost or duplicated records", got, delivered, restUpserts, n)
	}
}

// TestMutableFullReplayCarriesNoCursor guards the one case that must NOT be resumable. A Reset
// on a mutable collection tells the consumer to discard what it holds and rebuild from the
// replay, and the reconcile that decides which of its old keys are gone runs only once the
// replay is complete. A cursor committed partway would let the consumer come back as an
// incremental resume over a half-built table whose reconcile never runs -- keys deleted at the
// source would stay in it forever.
func TestMutableFullReplayCarriesNoCursor(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, WatchHistory: 4096})
	for i := 0; i < 200; i++ {
		if err := c.Put(wkey(i), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	events, synced := collectCatchUp(t, c, nil)
	if len(events) == 0 || events[0].Kind != WatchReset {
		t.Fatalf("a replay from nil did not open with WatchReset")
	}
	for _, ev := range events {
		if len(ev.Cursor) > 0 {
			t.Errorf("a %v in a mutable collection's full replay carried a resume cursor; a "+
				"consumer that commits one resumes over a rebuild whose reconcile never ran",
				ev.Kind)
			break
		}
	}
	if len(synced) == 0 {
		t.Error("WatchSynced carried no cursor, so the completed replay cannot be resumed from")
	}
}
