package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// A stream's channel is closed both by the server's terminator and by the
// connection dying under it. These tests pin that the two are told apart: a
// consumer that cannot tell forwards a truncated result as a complete one.

func truncationPair(t *testing.T, rows int) (*Client, net.Conn, func()) {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if err := d.UpdateOld(fmt.Sprintf("%d", i),
			fmt.Sprintf("Owner = \"u%d\"\nCpus = %d\nPad = \"%0512d\"", i%7, i%16, i)); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServer(d)
	ca, cb := net.Pipe()
	go func() { _ = s.ServeConn(NewStreamConn(cb)) }()
	c := NewClient(NewStreamConn(ca))
	return c, ca, func() { c.Close(); s.Close(); d.Close() }
}

// QueryRawTableStream is what a relay uses to forward ads one at a time. A
// connection that dies partway must be reported, not read as the end of a short
// result set.
func TestStreamRawTruncationIsAnError(t *testing.T) {
	c, raw, cleanup := truncationPair(t, 4000)
	defer cleanup()

	n := 0
	err := c.QueryRawTableStream(context.Background(), DefaultTable, "true", 0, func(row string) bool {
		n++
		if n == 5 {
			_ = raw.Close() // the database goes away mid-answer
		}
		return true
	})
	if err == nil {
		t.Fatalf("the connection died after %d of 4000 rows but the stream reported success; "+
			"a relay would forward a truncated result as a complete one", n)
	}
	if !errors.Is(err, ErrConnClosed) {
		t.Fatalf("error = %v, want one that wraps ErrConnClosed so a caller can classify it", err)
	}
	if n >= 4000 {
		t.Fatalf("yielded every row (%d); the connection cannot have died mid-stream", n)
	}
}

// The collecting form has the same hazard: it returns a slice, and a short slice
// with a nil error is indistinguishable from a small table.
func TestStreamCollectTruncationIsAnError(t *testing.T) {
	c, raw, cleanup := truncationPair(t, 4000)
	defer cleanup()

	done := make(chan struct{})
	go func() { defer close(done); _ = raw.Close() }()
	<-done

	out, err := c.QueryTable(context.Background(), DefaultTable, "true", 0)
	if err == nil {
		t.Fatalf("a query on a dead connection returned %d rows and no error", len(out))
	}
}

// A stream that really does end normally must still report success -- the fix
// must not turn every completed stream into an error.
func TestStreamCompletesWithoutError(t *testing.T) {
	c, _, cleanup := truncationPair(t, 200)
	defer cleanup()

	n := 0
	err := c.QueryRawTableStream(context.Background(), DefaultTable, "true", 0, func(row string) bool {
		n++
		return true
	})
	if err != nil {
		t.Fatalf("a complete stream reported %v", err)
	}
	if n != 200 {
		t.Fatalf("got %d rows, want 200", n)
	}
}

// Stopping early is the caller's choice, not a failure.
func TestStreamEarlyStopIsNotAnError(t *testing.T) {
	c, _, cleanup := truncationPair(t, 500)
	defer cleanup()

	n := 0
	err := c.QueryRawTableStream(context.Background(), DefaultTable, "true", 0, func(row string) bool {
		n++
		return n < 10
	})
	if err != nil {
		t.Fatalf("stopping early reported %v", err)
	}
	if n != 10 {
		t.Fatalf("yielded %d rows after stopping at 10", n)
	}
}
