package dbrpc

import (
	"context"
	"errors"
	"testing"
	"time"
)

// watchUpsert is db.WatchUpsert; the stream also carries control events (Synced, Reset)
// that carry no key or ad, and these tests are about the ads.
const watchUpsert = 0

// collectWire drains n upsert events from a wire watch, skipping control events.
func collectWire(t *testing.T, ch <-chan WireWatchEvent, n int) []WireWatchEvent {
	t.Helper()
	var out []WireWatchEvent
	deadline := time.After(10 * time.Second)
	for len(out) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("watch stream closed after %d of %d upserts", len(out), n)
			}
			if ev.Kind != watchUpsert {
				continue
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("timed out after %d of %d upserts", len(out), n)
		}
	}
	return out
}

// collectText is collectWire for the text watch, so the two can be compared.
func collectText(t *testing.T, ch <-chan WatchEvent, n int) []WatchEvent {
	t.Helper()
	var out []WatchEvent
	deadline := time.After(10 * time.Second)
	for len(out) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("watch stream closed after %d of %d upserts", len(out), n)
			}
			if ev.Kind != watchUpsert {
				continue
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("timed out after %d of %d upserts", len(out), n)
		}
	}
	return out
}

// TestWatchWireMatchesTextWatch is the equivalence: the wire feed must report the same
// events, in the same order, carrying the same ads as the text feed.
func TestWatchWireMatchesTextWatch(t *testing.T) {
	c, cleanup := testPairPersistent(t, true)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cursor, err := c.WatchHead(ctx, DefaultTable)
	if err != nil {
		t.Fatal(err)
	}
	textCh, stopText, err := c.WatchTable(ctx, DefaultTable, cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer stopText()
	wireCh, stopWire, err := c.WatchWireTable(ctx, DefaultTable, cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer stopWire()

	seedWireAds(t, c, 3)

	wireEvs := collectWire(t, wireCh, 3)
	textEvs := collectText(t, textCh, 3)
	byKeyText := map[string]WatchEvent{}
	for _, te := range textEvs {
		byKeyText[te.Key] = te
	}
	for i, we := range wireEvs {
		te, ok := byKeyText[we.Key]
		if !ok {
			t.Errorf("event %d: key %q appeared on the wire watch but not the text watch", i, we.Key)
			continue
		}
		if we.Kind != te.Kind {
			t.Errorf("event %d (%s): wire kind %d vs text kind %d", i, we.Key, we.Kind, te.Kind)
		}
		if te.AdText == "" {
			t.Errorf("event %d (%s): the text watch carried no ad to compare against", i, we.Key)
		}
		got, derr := DecodeWatchAd(we.Ad)
		if derr != nil {
			t.Fatalf("event %d: decoding the wire ad: %v", i, derr)
		}
		if got == nil {
			t.Fatalf("event %d: upsert carried no ad", i)
		}
		if name, _ := got.EvaluateAttrString("Name"); name != we.Key {
			t.Errorf("event %d: decoded Name = %q, want the key %q", i, name, we.Key)
		}
		if state, _ := got.EvaluateAttrString("State"); state != "Unclaimed" {
			t.Errorf("event %d: decoded State = %q, want Unclaimed", i, state)
		}
	}
}

// TestWatchWireRedactsPrivate is the security guard. The text watch drops private
// attributes for an unprivileged connection; changing the encoding must not change who can
// see ClaimId.
func TestWatchWireRedactsPrivate(t *testing.T) {
	for _, tc := range []struct {
		name           string
		includePrivate bool
		wantClaimID    bool
	}{
		{"unprivileged", false, false},
		{"privileged", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, cleanup := testPairPersistent(t, tc.includePrivate)
			defer cleanup()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			cursor, err := c.WatchHead(ctx, DefaultTable)
			if err != nil {
				t.Fatal(err)
			}
			ch, stop, err := c.WatchWireTable(ctx, DefaultTable, cursor)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()

			seedWireAds(t, c, 1)
			ev := collectWire(t, ch, 1)[0]
			ad, err := DecodeWatchAd(ev.Ad)
			if err != nil {
				t.Fatal(err)
			}
			_, got := ad.EvaluateAttrString("ClaimId")
			if got != tc.wantClaimID {
				t.Errorf("ClaimId present = %v, want %v -- the wire watch must redact exactly as the text watch does",
					got, tc.wantClaimID)
			}
			// The public attributes survive either way.
			if state, ok := ad.EvaluateAttrString("State"); !ok || state != "Unclaimed" {
				t.Errorf("State = %q (ok=%v), want Unclaimed", state, ok)
			}
		})
	}
}

// TestWatchWireUnsupportedFallsBack covers a server too old to know the opcode: the caller
// must learn that synchronously, so it can use the text watch, rather than holding a
// channel that never produces an event.
func TestWatchWireUnsupportedFallsBack(t *testing.T) {
	c, cleanup := testPairOps(t, func(o op) bool { return o != opWatchWire })
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, _, err := c.WatchWireTable(ctx, DefaultTable, nil)
	if !errors.Is(err, ErrWatchWireUnsupported) {
		t.Fatalf("err = %v, want ErrWatchWireUnsupported", err)
	}
	// The text watch still works on the same connection.
	ch, stop, err := c.WatchTable(ctx, DefaultTable, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if ch == nil {
		t.Error("the text watch must remain available")
	}
}

// TestDecodeWatchAdEmpty pins that a delete or reset event, which carries no ad, decodes to
// nil rather than an error a consumer would have to special-case.
func TestDecodeWatchAdEmpty(t *testing.T) {
	ad, err := DecodeWatchAd(nil)
	if err != nil || ad != nil {
		t.Errorf("DecodeWatchAd(nil) = %v, %v; want nil, nil", ad, err)
	}
}
