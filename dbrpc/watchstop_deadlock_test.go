package dbrpc

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestWatchStopDoesNotDeadlockTheReadLoop: a watcher that gives up while the server is still
// streaming must be able to stop its watch.
//
// One goroutine demuxes every frame on a connection, the stop's own response among them. A
// watcher that has stopped reading leaves that loop parked on the send into its stream, so a
// stop that waits for its response before freeing the loop can never be answered -- and the
// connection is dead for every other caller too, not just this watch. On the CHTC dev hub
// that cycle held a federation hub open through its whole 300s termination grace, which cost
// the store a clean close and the next start a full directory rebuild.
//
// The test reproduces the cycle exactly: nothing ever reads events, so the stream's frame
// buffer (256) and the event buffer (64) both fill and the read loop parks.
func TestWatchStopDoesNotDeadlockTheReadLoop(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := c.CreateTable(ctx, "jobs"); err != nil {
		t.Fatal(err)
	}
	tx, err := c.BeginTable(ctx, "jobs")
	if err != nil {
		t.Fatal(err)
	}
	// Comfortably more than frame buffer + event buffer, so the catch-up alone parks the
	// read loop whatever order the two buffers fill in.
	const rows = 600
	for i := range rows {
		k := fmt.Sprintf("j%d", i)
		if err := tx.NewClassAd(ctx, k, fmt.Sprintf("Name = %q", k)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Deliberately never read events.
	_, stop, err := c.WatchTable(ctx, "jobs", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Let the catch-up fill both buffers and park the read loop. If it has not parked the
	// test still passes, so the guard against this test quietly proving nothing is the
	// mutation check: restore the pre-fix stop and it must hang here.
	time.Sleep(500 * time.Millisecond)

	done := make(chan struct{})
	go func() { defer close(done); stop() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("stop() did not return: the read loop is parked delivering to this watch, " +
			"and the stop is waiting for a response only that loop can deliver")
	}

	// The connection must still work afterwards -- the point is that the whole mux survives,
	// not merely that one stop returned.
	if _, err := c.Tables(ctx); err != nil {
		t.Fatalf("connection unusable after stopping an abandoned watch: %v", err)
	}
}
