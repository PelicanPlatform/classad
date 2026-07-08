package collections

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func collectArchiveCatchUp(t *testing.T, a *Archive, cursor []byte) ([]WatchEvent, []byte) {
	t.Helper()
	seq, err := a.Watch(context.Background(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	var events []WatchEvent
	var synced []byte
	for ev := range seq {
		if ev.Kind == WatchSynced {
			synced = ev.Cursor
			break
		}
		events = append(events, ev)
	}
	if synced == nil {
		t.Fatal("archive watch ended without WatchSynced")
	}
	return events, synced
}

// upsertIDs returns the Id attribute of every Upsert event, in order.
func upsertIDs(t *testing.T, events []WatchEvent) []int {
	t.Helper()
	var ids []int
	for _, ev := range events {
		if ev.Kind == WatchUpsert {
			if ev.Ad == nil {
				t.Fatal("upsert with nil Ad")
			}
			id, ok := ev.Ad.EvaluateAttrInt("Id")
			if !ok {
				t.Fatal("upsert ad missing Id")
			}
			ids = append(ids, int(id))
		}
	}
	return ids
}

func TestArchiveWatchFullReplay(t *testing.T) {
	t.Parallel()
	a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const n = 200
	for i := 0; i < n; i++ {
		if err := a.Append(mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	events, cursor := collectArchiveCatchUp(t, a, nil)
	if len(events) == 0 || events[0].Kind != WatchReset {
		t.Fatalf("first event should be Reset, got %v", events)
	}
	ids := upsertIDs(t, events[1:])
	if len(ids) != n {
		t.Fatalf("full replay yielded %d ads, want %d", len(ids), n)
	}
	for i, id := range ids { // archive preserves append (chronological) order
		if id != i {
			t.Fatalf("out-of-order replay at %d: got Id=%d", i, id)
		}
	}
	if cursor == nil {
		t.Fatal("no synced cursor")
	}
}

func TestArchiveWatchIncremental(t *testing.T) {
	t.Parallel()
	a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 50; i++ {
		_ = a.Append(mustAd(t, fmt.Sprintf(`[Id=%d]`, i)))
	}
	_, cursor := collectArchiveCatchUp(t, a, nil)
	for i := 50; i < 80; i++ {
		_ = a.Append(mustAd(t, fmt.Sprintf(`[Id=%d]`, i)))
	}
	events, _ := collectArchiveCatchUp(t, a, cursor)
	for _, ev := range events {
		if ev.Kind == WatchReset {
			t.Fatal("unexpected Reset on an in-window incremental resume")
		}
	}
	ids := upsertIDs(t, events)
	if len(ids) != 30 {
		t.Fatalf("incremental resume yielded %d ads, want 30 (50..79): %v", len(ids), ids)
	}
	for i, id := range ids {
		if id != 50+i {
			t.Fatalf("incremental resume out of order: %v", ids)
		}
	}
}

// TestArchiveWatchResumeAcrossReopen verifies the cursor survives Close/OpenArchive
// (segment ids and the epoch are persisted), so a resume is still incremental.
func TestArchiveWatchResumeAcrossReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	opts := ArchiveOptions{Dir: dir}
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		_ = a.Append(mustAd(t, fmt.Sprintf(`[Id=%d]`, i)))
	}
	_, cursor := collectArchiveCatchUp(t, a, nil)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := OpenArchive(opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer b.Close()
	for i := 60; i < 90; i++ {
		_ = b.Append(mustAd(t, fmt.Sprintf(`[Id=%d]`, i)))
	}
	events, _ := collectArchiveCatchUp(t, b, cursor)
	for _, ev := range events {
		if ev.Kind == WatchReset {
			t.Fatal("resume across reopen should be incremental, not a Reset")
		}
	}
	ids := upsertIDs(t, events)
	// Must include the 30 records appended after reopen; may include none from before
	// (cursor was at the pre-close tail).
	got := map[int]bool{}
	for _, id := range ids {
		got[id] = true
	}
	for i := 60; i < 90; i++ {
		if !got[i] {
			t.Fatalf("resume across reopen missed Id=%d; got %v", i, ids)
		}
	}
}

// TestArchiveWatchResetAfterRotation: a cursor older than what rotation retains gets
// a full replay from the current floor.
func TestArchiveWatchResetAfterRotation(t *testing.T) {
	t.Parallel()
	a, err := CreateArchive(ArchiveOptions{
		Dir: t.TempDir(), SegmentSize: 1 << 12,
		Retention: Retention{MaxSegments: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 20; i++ {
		_ = a.Append(mustAd(t, fmt.Sprintf(`[Id=%d]`, i)))
	}
	_, oldCursor := collectArchiveCatchUp(t, a, nil) // cursor near the start

	// Append enough to seal many segments; retention keeps only the newest, so the
	// oldCursor's segment is dropped.
	for i := 20; i < 900; i++ {
		_ = a.Append(mustAd(t, fmt.Sprintf(`[Id=%d]`, i)))
	}
	if _, err := a.Rotate(0); err != nil {
		t.Fatal(err)
	}
	events, _ := collectArchiveCatchUp(t, a, oldCursor)
	if len(events) == 0 || events[0].Kind != WatchReset {
		t.Fatalf("resume past the retention floor should Reset; first kind = %v",
			func() any {
				if len(events) == 0 {
					return "none"
				}
				return events[0].Kind
			}())
	}
}

func TestArchiveWatchLive(t *testing.T) {
	t.Parallel()
	a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_ = a.Append(mustAd(t, `[Id=0]`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := a.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan WatchEvent, 128)
	go func() {
		for ev := range seq {
			events <- ev
		}
		close(events)
	}()
	for ev := range events {
		if ev.Kind == WatchSynced {
			break
		}
	}
	_ = a.Append(mustAd(t, `[Id=1]`))
	_ = a.Append(mustAd(t, `[Id=2]`))
	got := 0
	deadline := time.After(3 * time.Second)
	for got < 2 {
		select {
		case ev := <-events:
			if ev.Kind == WatchUpsert {
				got++
				if ev.Cursor == nil {
					t.Error("live archive event missing cursor")
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for live appends, got %d", got)
		}
	}
}

// TestArchiveWatchConcurrent streams live while concurrent appenders run. Run -race.
func TestArchiveWatchConcurrent(t *testing.T) {
	t.Parallel()
	a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const n = 400
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := a.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := 0
	resync := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range seq {
			mu.Lock()
			if ev.Kind == WatchResync {
				resync = true
				mu.Unlock()
				return
			}
			if ev.Kind == WatchUpsert {
				seen++
			}
			mu.Unlock()
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < n; i += 4 {
				_ = a.Append(mustAd(t, fmt.Sprintf(`[Id=%d]`, i)))
			}
		}(w)
	}
	wg.Wait()

	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		got, r := seen, resync
		mu.Unlock()
		if r || got >= n {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("watcher saw %d/%d appends and no resync", got, n)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
}
