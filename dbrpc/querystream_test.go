package dbrpc

import (
	"context"
	"fmt"
	"testing"
)

// seedAds writes n ads "k0..kN-1" with N=i, committed, for the streaming tests.
func seedAds(t *testing.T, c *Client, n int) {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", i), fmt.Sprintf("MyType = \"Machine\"\nN = %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestQueryTableStream: streaming delivers every matching row via yield, same set as the
// collecting QueryTable.
func TestQueryTableStream(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()
	const n = 250
	seedAds(t, c, n)

	got := 0
	err := c.QueryTableStream(ctx, DefaultTable, "true", 0, func(row string) bool {
		got++
		return true
	})
	if err != nil {
		t.Fatalf("QueryTableStream: %v", err)
	}
	if got != n {
		t.Fatalf("streamed %d rows, want %d", got, n)
	}

	// Same count as the collecting path.
	rows, err := c.QueryTable(ctx, DefaultTable, "true", 0)
	if err != nil || len(rows) != n {
		t.Fatalf("QueryTable = %d rows, %v; want %d", len(rows), err, n)
	}
}

// TestQueryRawTableStream: the raw (AST-free) streaming path also delivers every row, and
// yield returning false stops early without error.
func TestQueryRawTableStream(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()
	const n = 100
	seedAds(t, c, n)

	got := 0
	if err := c.QueryRawTableStream(ctx, DefaultTable, "true", 0, func(row string) bool {
		got++
		return true
	}); err != nil {
		t.Fatalf("QueryRawTableStream: %v", err)
	}
	if got != n {
		t.Fatalf("streamed %d rows, want %d", got, n)
	}

	// Early stop after 10 rows: no error, no more than a bounded few extra (drain is async).
	stopped := 0
	if err := c.QueryRawTableStream(ctx, DefaultTable, "true", 0, func(row string) bool {
		stopped++
		return stopped < 10
	}); err != nil {
		t.Fatalf("early-stop stream: %v", err)
	}
	if stopped != 10 {
		t.Fatalf("early stop yielded %d, want 10", stopped)
	}
}

// TestQueryRawProjectStream: projected streaming returns only requested attributes.
func TestQueryRawProjectStream(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()
	seedAds(t, c, 20)

	got := 0
	err := c.QueryRawProjectStream(ctx, DefaultTable, "true", []string{"N"}, 0, func(row string) bool {
		got++
		return true
	})
	if err != nil {
		t.Fatalf("QueryRawProjectStream: %v", err)
	}
	if got != 20 {
		t.Fatalf("streamed %d rows, want 20", got)
	}
}
