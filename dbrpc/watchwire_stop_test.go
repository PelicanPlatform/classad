package dbrpc

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedWatchRows writes n ads to the default table in one transaction, enough of them that a
// catch-up overruns both the stream's frame buffer and a watch's event buffer.
func seedWatchRows(t *testing.T, c *Client, n int) {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		key := fmt.Sprintf("slot%d.0", i)
		if addErr := tx.NewClassAd(ctx, key, fmt.Sprintf("Name = %q\nState = \"Unclaimed\"", key)); addErr != nil {
			t.Fatal(addErr)
		}
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		t.Fatal(commitErr)
	}
}

// TestWatchWireStopDoesNotDeadlockTheReadLoop: the wire watch holds a second copy of the same
// stop, so fixing the text one alone would have left this deadlock in place for every caller
// that uses the wire form -- which, on the hub this was found on, is all of them.
// TestWatchStopDoesNotDeadlockTheReadLoop describes the cycle.
func TestWatchWireStopDoesNotDeadlockTheReadLoop(t *testing.T) {
	c, cleanup := testPairPersistent(t, true)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seedWatchRows(t, c, 600)

	// Deliberately never read events.
	_, stop, err := c.WatchWireTable(ctx, DefaultTable, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // let the catch-up fill both buffers and park the read loop

	done := make(chan struct{})
	go func() { defer close(done); stop() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("stop() did not return on a wire watch: the read loop is parked delivering to " +
			"it, and the stop is waiting for a response only that loop can deliver")
	}
	if _, tablesErr := c.Tables(ctx); tablesErr != nil {
		t.Fatalf("connection unusable after stopping an abandoned wire watch: %v", tablesErr)
	}
}

// TestWatchWireCancelReleasesTheWatcher: cancelling a watch's context must end its goroutine
// and close its channel. This is the path a daemon shutdown actually takes -- the watcher is
// parked handing an event to a consumer that has gone, and the cancel has to unwind the
// watcher and the read loop together, not one waiting on the other.
func TestWatchWireCancelReleasesTheWatcher(t *testing.T) {
	c, cleanup := testPairPersistent(t, true)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seedWatchRows(t, c, 600)

	events, _, err := c.WatchWireTable(ctx, DefaultTable, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // park the watcher on its event send, and the read loop behind it
	cancel()

	deadline := time.After(20 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return // the watcher unwound and closed its channel, which is the whole point
			}
		case <-deadline:
			t.Fatal("the watch channel never closed after its context was cancelled")
		}
	}
}

// TestWatchWireDeadContextLeavesNoStream: asking for a watch with an already-cancelled context
// must not leave the request's stream running on the server with nobody draining it on this
// side. Which branch the open takes is a race against the server's first frame -- either it
// refuses with the context's error, or it hands back a watch that the cancel then unwinds --
// so the assertion is the invariant both branches owe: no hang, and a connection still usable.
func TestWatchWireDeadContextLeavesNoStream(t *testing.T) {
	c, cleanup := testPairPersistent(t, true)
	defer cleanup()
	seedWatchRows(t, c, 600)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		events, stop, err := c.WatchWireTable(ctx, DefaultTable, nil)
		if err != nil {
			return // refused with the dead context, which is the other legal outcome
		}
		stop()
		for range events { //nolint:revive // drain to the close the cancel causes
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("opening a watch with a dead context neither returned nor unwound")
	}
	if _, err := c.Tables(context.Background()); err != nil {
		t.Fatalf("connection unusable after a watch opened with a dead context: %v", err)
	}
}
