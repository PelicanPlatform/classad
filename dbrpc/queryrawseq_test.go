package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// TestQueryRawProjectedFromSeqPaginates is the round trip: pages cross the wire
// with a cursor, and walking them covers the matching set exactly once with the
// projection and constraint applied.
func TestQueryRawProjectedFromSeqPaginates(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()

	const n = 200
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		owner := "alice"
		if i%2 == 1 {
			owner = "bob"
		}
		ad := fmt.Sprintf("MyType = \"Job\"\nClusterId = %d\nOwner = %q\nCmd = \"/bin/true\"", i, owner)
		if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", i), ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var (
		cursor SeqCursor
		seen   = map[int]int{}
		pages  int
	)
	for {
		rows, page, err := c.QueryRawProjectedFromSeq(ctx, DefaultTable, `Owner == "alice"`,
			[]string{"ClusterId", "Owner"}, cursor, 15)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if strings.Contains(row, "Cmd") {
				t.Fatalf("projection kept an attribute that was not requested: %q", row)
			}
			if !strings.Contains(row, `Owner = "alice"`) {
				t.Fatalf("constraint let through: %q", row)
			}
			var cluster int
			for _, line := range strings.Split(row, "\n") {
				if name, val, ok := strings.Cut(line, " = "); ok && name == "ClusterId" {
					if _, err := fmt.Sscanf(val, "%d", &cluster); err != nil {
						t.Fatalf("ClusterId %q does not parse: %v", val, err)
					}
				}
			}
			seen[cluster]++
		}
		pages++
		if !page.More {
			break
		}
		cursor = page.Next
		if pages > 100 {
			t.Fatal("pagination did not terminate")
		}
	}

	if pages < 2 {
		t.Fatalf("expected several pages, got %d", pages)
	}
	if len(seen) != n/2 {
		t.Fatalf("paged over %d distinct jobs, want %d", len(seen), n/2)
	}
	for cluster, count := range seen {
		if count != 1 {
			t.Errorf("cluster %d seen %d times across pages, want 1", cluster, count)
		}
	}
}

// TestQueryRawProjectedFromSeqRejectsArchive checks the op says so rather than
// pretending: an archive is already newest-first with its limit pushed down, so
// a cursor there would be a worse version of what it already does.
func TestQueryRawProjectedFromSeqRejectsArchive(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	ctx := context.Background()

	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{ValueAttrs: []string{"ClusterId"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.ArchiveAppend(ctx, "history", "MyType = \"Job\"\nClusterId = 1"); err != nil {
		t.Fatal(err)
	}

	_, _, err := c.QueryRawProjectedFromSeq(ctx, "history", "true", nil, SeqCursor{}, 10)
	if err == nil {
		t.Fatal("expected an archive to refuse cursor pagination")
	}
	if !strings.Contains(err.Error(), "archive") {
		t.Errorf("expected the error to explain why, got: %v", err)
	}
}

// TestQueryRawProjectedFromSeqCursorExpired: a cursor whose snapshot the server can no longer read
// -- here one issued before a restart, with writes after it -- is refused with ErrCursorExpired
// before any row, never answered with a page that ends the listing early. A cursor the reopened
// server issues itself still pages.
func TestQueryRawProjectedFromSeqCursorExpired(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	serve := func() (*Client, func()) {
		d, err := db.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		s := NewServer(d)
		cconn, sconn := netPipe()
		go func() { _ = s.ServeConn(sconn) }()
		c := NewClient(cconn)
		return c, func() { c.Close(); s.Close(); d.Close() }
	}
	write := func(c *Client, gen int) {
		tx, err := c.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 50; i++ {
			if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", i), fmt.Sprintf("ClusterId = %d\nGen = %d", i, gen)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	c, stop := serve()
	write(c, 0)
	_, page, err := c.QueryRawProjectedFromSeq(ctx, DefaultTable, "true", nil, SeqCursor{}, 5)
	if err != nil || !page.More {
		t.Fatalf("first page: more %v, err %v", page != nil && page.More, err)
	}
	write(c, 1)
	stop()

	c, stop = serve()
	defer stop()
	rows := 0
	_, err = c.QueryRawProjectedFromSeqStream(ctx, DefaultTable, "true", nil, page.Next, 5, func(string) bool {
		rows++
		return true
	})
	if !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("err %v, want ErrCursorExpired", err)
	}
	if rows != 0 {
		t.Fatalf("an expired cursor streamed %d rows", rows)
	}

	_, fresh, err := c.QueryRawProjectedFromSeq(ctx, DefaultTable, "true", nil, SeqCursor{}, 5)
	if err != nil || !fresh.More {
		t.Fatalf("fresh first page: err %v", err)
	}
	if _, _, err = c.QueryRawProjectedFromSeq(ctx, DefaultTable, "true", nil, fresh.Next, 5); err != nil {
		t.Fatalf("a cursor issued after the restart: %v", err)
	}
}
