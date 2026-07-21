package dbrpc

import (
	"context"
	"fmt"
	"testing"
)

// TestNewClassAdBatch: a bulk write applies all valid ads in one round-trip and reports
// the unparseable ones by index without losing the rest.
func TestNewClassAdBatch(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()

	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}

	items := []AdKV{
		{Key: "1.0", Ad: "ClusterId = 1\nProcId = 0"},
		{Key: "2.0", Ad: "this is not a valid classad {{{"}, // rejected
		{Key: "3.0", Ad: "ClusterId = 3\nProcId = 0"},
	}
	rejects, err := tx.NewClassAdBatch(ctx, items)
	if err != nil {
		t.Fatalf("NewClassAdBatch: %v", err)
	}
	if len(rejects) != 1 || rejects[0].Index != 1 {
		t.Fatalf("rejects = %+v, want exactly index 1", rejects)
	}
	if rejects[0].Err == "" {
		t.Errorf("reject should carry the server error message")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// The two valid ads are readable; the rejected one is absent.
	tx2, _ := c.Begin(ctx)
	defer func() { _ = tx2.Abort(ctx) }()
	for _, k := range []string{"1.0", "3.0"} {
		if _, ok, err := tx2.LookupAttr(ctx, k, "ClusterId"); err != nil || !ok {
			t.Errorf("key %s should be stored (ok=%v err=%v)", k, ok, err)
		}
	}
	if _, ok, _ := tx2.LookupAttr(ctx, "2.0", "ClusterId"); ok {
		t.Errorf("rejected key 2.0 should not be stored")
	}
}

// TestNewClassAdBatchEmpty: an empty batch is a no-op, no round-trip needed.
func TestNewClassAdBatchEmpty(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()
	tx, _ := c.Begin(ctx)
	defer func() { _ = tx.Abort(ctx) }()
	if rejects, err := tx.NewClassAdBatch(ctx, nil); err != nil || rejects != nil {
		t.Fatalf("empty batch: rejects=%v err=%v", rejects, err)
	}
}

// TestNewClassAdBatchLarge: a batch far larger than a per-ad path exercises the loop and
// confirms all ads land.
func TestNewClassAdBatchLarge(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()
	tx, _ := c.Begin(ctx)
	const n = 500
	items := make([]AdKV, n)
	for i := range items {
		items[i] = AdKV{Key: fmt.Sprintf("k%d", i), Ad: fmt.Sprintf("N = %d", i)}
	}
	rejects, err := tx.NewClassAdBatch(ctx, items)
	if err != nil || len(rejects) != 0 {
		t.Fatalf("large batch: rejects=%v err=%v", rejects, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx2, _ := c.Begin(ctx)
	defer func() { _ = tx2.Abort(ctx) }()
	if v, ok, err := tx2.LookupAttr(ctx, "k499", "N"); err != nil || !ok || v != "499" {
		t.Fatalf("k499 N = %q,%v,%v want 499", v, ok, err)
	}
}

// TestNewClassAdBatchPipelined: a large batch split into many small chunks applies every
// valid ad and reports a bad one at its GLOBAL index -- exercising the send-all-then-
// collect-all path across multiple in-flight chunks.
func TestNewClassAdBatchPipelined(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()
	tx, _ := c.Begin(ctx)

	const n = 400
	items := make([]AdKV, n)
	for i := range items {
		items[i] = AdKV{Key: fmt.Sprintf("k%d", i), Ad: fmt.Sprintf("N = %d", i)}
	}
	// A bad ad at a global index inside a later chunk (chunkSize 32 -> index 100 is chunk 3).
	items[100] = AdKV{Key: "k100", Ad: "definitely not a classad ]["}

	rejects, err := tx.NewClassAdBatchPipelined(ctx, items, 32)
	if err != nil {
		t.Fatalf("pipelined: %v", err)
	}
	if len(rejects) != 1 || rejects[0].Index != 100 {
		t.Fatalf("rejects = %+v, want exactly global index 100", rejects)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	tx2, _ := c.Begin(ctx)
	defer func() { _ = tx2.Abort(ctx) }()
	// A spread of good keys landed; the bad one did not.
	for _, i := range []int{0, 99, 101, 399} {
		if _, ok, err := tx2.LookupAttr(ctx, fmt.Sprintf("k%d", i), "N"); err != nil || !ok {
			t.Errorf("k%d should be stored (ok=%v err=%v)", i, ok, err)
		}
	}
	if _, ok, _ := tx2.LookupAttr(ctx, "k100", "N"); ok {
		t.Errorf("rejected k100 should not be stored")
	}
}

// TestNewClassAdBatchPipelinedEmpty: no items is a no-op.
func TestNewClassAdBatchPipelinedEmpty(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()
	tx, _ := c.Begin(ctx)
	defer func() { _ = tx.Abort(ctx) }()
	if r, err := tx.NewClassAdBatchPipelined(ctx, nil, 32); err != nil || r != nil {
		t.Fatalf("empty pipelined: r=%v err=%v", r, err)
	}
}
