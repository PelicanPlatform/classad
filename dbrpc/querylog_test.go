package dbrpc

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestQueryLog verifies the opt-in per-query log: ServeOptions.QueryLog is called
// once per streamed query with the op, table, constraint, and the row count.
func TestQueryLog(t *testing.T) {
	var mu sync.Mutex
	var logs []QueryLog
	c, cleanup := serveOptsPair(t, ServeOptions{
		QueryLog: func(q QueryLog) {
			mu.Lock()
			logs = append(logs, q)
			mu.Unlock()
		},
	})
	defer cleanup()

	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.NewClassAd(ctx, "a", `Name = "a"`+"\n"+`State = "Idle"`)
	_ = tx.NewClassAd(ctx, "b", `Name = "b"`+"\n"+`State = "Claimed"`)
	_ = tx.NewClassAd(ctx, "c", `Name = "c"`+"\n"+`State = "Idle"`)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if rows, err := c.Query(ctx, `State == "Idle"`); err != nil || len(rows) != 2 {
		t.Fatalf("Query: rows=%d err=%v, want 2 rows", len(rows), err)
	}

	// The log fires on the server after the response is streamed, so wait for it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := len(logs)
		mu.Unlock()
		if got >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("QueryLog was not called within 2s")
		}
		time.Sleep(2 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 1 {
		t.Fatalf("QueryLog called %d times, want 1", len(logs))
	}
	e := logs[0]
	if e.Op != "Query" {
		t.Errorf("Op = %q, want Query", e.Op)
	}
	if e.Table != DefaultTable {
		t.Errorf("Table = %q, want %q", e.Table, DefaultTable)
	}
	if e.Constraint != `State == "Idle"` {
		t.Errorf("Constraint = %q", e.Constraint)
	}
	if e.Rows != 2 {
		t.Errorf("Rows = %d, want 2", e.Rows)
	}
	if e.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", e.Duration)
	}
}
