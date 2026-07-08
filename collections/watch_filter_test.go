package collections

import (
	"fmt"
	"iter"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func seqOf(evs ...WatchEvent) iter.Seq[WatchEvent] {
	return func(yield func(WatchEvent) bool) {
		for _, e := range evs {
			if !yield(e) {
				return
			}
		}
	}
}

// matchM reports whether ad has M == 1.
func matchM(ad *classad.ClassAd) bool {
	v, ok := ad.EvaluateAttrInt("M")
	return ok && v == 1
}

func collectKinds(t *testing.T, seq iter.Seq[WatchEvent]) []string {
	t.Helper()
	var got []string
	for ev := range seq {
		got = append(got, fmt.Sprintf("%d:%s", ev.Kind, ev.Key))
	}
	return got
}

// TestWatchFilterLive covers the live-phase filtering rules: matching upserts are
// delivered, non-matching upserts are dropped, an upsert that stops matching
// becomes a Delete, a Delete is delivered only for a matching key, Reset/Synced
// pass through.
func TestWatchFilterLive(t *testing.T) {
	m := func(v int) *classad.ClassAd { return mustAd(t, fmt.Sprintf("[M=%d]", v)) }
	in := seqOf(
		WatchEvent{Kind: WatchReset},
		WatchEvent{Kind: WatchUpsert, Key: []byte("a"), Ad: m(1)}, // match -> deliver
		WatchEvent{Kind: WatchUpsert, Key: []byte("b"), Ad: m(0)}, // no match -> drop
		WatchEvent{Kind: WatchSynced, Cursor: []byte("c1")},
		WatchEvent{Kind: WatchUpsert, Key: []byte("a"), Ad: m(0)}, // a stops matching -> Delete
		WatchEvent{Kind: WatchDelete, Key: []byte("b")},           // b never matched, live -> drop
		WatchEvent{Kind: WatchUpsert, Key: []byte("d"), Ad: m(1)}, // match -> deliver
		WatchEvent{Kind: WatchDelete, Key: []byte("d")},           // d matched -> Delete
	)
	got := collectKinds(t, WatchFilter(in, matchM))
	want := []string{
		fmt.Sprintf("%d:", WatchReset),
		fmt.Sprintf("%d:a", WatchUpsert),
		fmt.Sprintf("%d:", WatchSynced),
		fmt.Sprintf("%d:a", WatchDelete),
		fmt.Sprintf("%d:d", WatchUpsert),
		fmt.Sprintf("%d:d", WatchDelete),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("filtered stream:\n got  %v\n want %v", got, want)
	}
}

// TestWatchFilterCatchupDelete covers an incremental resume (no Reset): the prior
// match state of a resumed key is unknown, so a Delete arriving before Synced is
// forwarded conservatively (the client no-ops an unknown key). After Synced,
// deletes are filtered precisely.
func TestWatchFilterCatchupDelete(t *testing.T) {
	m := func(v int) *classad.ClassAd { return mustAd(t, fmt.Sprintf("[M=%d]", v)) }
	in := seqOf(
		WatchEvent{Kind: WatchDelete, Key: []byte("x")},           // catch-up delete -> forward
		WatchEvent{Kind: WatchUpsert, Key: []byte("y"), Ad: m(1)}, // match -> deliver
		WatchEvent{Kind: WatchSynced, Cursor: []byte("c1")},
		WatchEvent{Kind: WatchDelete, Key: []byte("z")}, // live, never matched -> drop
	)
	got := collectKinds(t, WatchFilter(in, matchM))
	want := []string{
		fmt.Sprintf("%d:x", WatchDelete),
		fmt.Sprintf("%d:y", WatchUpsert),
		fmt.Sprintf("%d:", WatchSynced),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("filtered stream:\n got  %v\n want %v", got, want)
	}
}

// TestWatchFilterNil passes everything through when match is nil.
func TestWatchFilterNil(t *testing.T) {
	in := seqOf(WatchEvent{Kind: WatchReset}, WatchEvent{Kind: WatchSynced})
	if n := len(collectKinds(t, WatchFilter(in, nil))); n != 2 {
		t.Errorf("nil match: delivered %d events, want 2 (passthrough)", n)
	}
}
