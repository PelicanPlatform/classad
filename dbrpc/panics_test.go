package dbrpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// syncBuffer is a bytes.Buffer safe for the server's goroutines to log into.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// panicServer is a server over a few committed ads with its log captured, and a way to open more
// connections to it.
func panicServer(t *testing.T) (dial func() *Client, log *syncBuffer) {
	t.Helper()
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	log = &syncBuffer{}
	s.Logger = slog.New(slog.NewTextHandler(log, nil))
	var clients []*Client
	dial = func() *Client {
		cconn, sconn := netPipe()
		go func() { _ = s.ServeConn(sconn) }()
		c := NewClient(cconn)
		clients = append(clients, c)
		return c
	}
	t.Cleanup(func() {
		for _, c := range clients {
			c.Close()
		}
		s.Close()
		d.Close()
	})
	c := dial()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", i), fmt.Sprintf("Cpus = %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return dial, log
}

// setHook installs a test hook for the rest of the test.
func setHook[T any](t *testing.T, p *atomic.Pointer[T], h T) {
	t.Helper()
	p.Store(&h)
	t.Cleanup(func() { p.Store(nil) })
}

// assertServing checks that the server still answers on c and on a fresh connection.
func assertServing(t *testing.T, dial func() *Client, c *Client) {
	t.Helper()
	ctx := context.Background()
	for name, cl := range map[string]*Client{"same connection": c, "new connection": dial()} {
		rows, err := cl.Query(ctx, "Cpus >= 0")
		if err != nil || len(rows) != 5 {
			t.Fatalf("%s after the panic: Query = %d rows, %v; want 5", name, len(rows), err)
		}
		if tables, err := cl.Tables(ctx); err != nil || len(tables) == 0 {
			t.Fatalf("%s after the panic: Tables = %v, %v", name, tables, err)
		}
	}
}

// assertInternal checks err is the contained-panic error and the log holds the panic and its stack.
func assertInternal(t *testing.T, err error, log *syncBuffer, wantInLog ...string) {
	t.Helper()
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("err = %v, want ErrInternal", err)
	}
	var se *ServerError
	if !errors.As(err, &se) {
		t.Fatalf("err = %T, want a *ServerError", err)
	}
	if errors.Is(err, ErrBadRequest) {
		t.Fatal("a contained panic must not read as a bad request")
	}
	out := log.String()
	for _, w := range append([]string{"level=ERROR", "dbrpc: request panicked", "goroutine "}, wantInLog...) {
		if !strings.Contains(out, w) {
			t.Fatalf("log lacks %q:\n%s", w, out)
		}
	}
}

// TestUnaryReadPanicFailsOnlyThatRequest: a read that panics before answering fails with an internal
// error, is logged and counted, and the connection and the server go on serving.
func TestUnaryReadPanicFailsOnlyThatRequest(t *testing.T) {
	dial, log := panicServer(t)
	c := dial()
	setHook(t, &dispatchHook, func(o op) {
		if o == opListTables {
			panic("injected: list tables")
		}
	})
	before := RequestPanics()
	_, err := c.Tables(context.Background())
	assertInternal(t, err, log, "injected: list tables", "op=ListTables")
	if got := RequestPanics() - before; got != 1 {
		t.Fatalf("RequestPanics grew by %d, want 1", got)
	}
	dispatchHook.Store(nil)
	assertServing(t, dial, c)
}

// TestStreamPanicMidStreamFailsOnlyThatRequest: a streaming read that panics after sending rows ends
// in an internal error the client sees -- not a short stream read as complete -- and the server goes
// on serving.
func TestStreamPanicMidStreamFailsOnlyThatRequest(t *testing.T) {
	dial, log := panicServer(t)
	c := dial()
	var rows atomic.Int32
	setHook(t, &writeHook, func(frame []byte) {
		if frameStatus(frame) == stStream && rows.Add(1) == 2 {
			panic(fmt.Errorf("injected: second row"))
		}
	})
	before := RequestPanics()
	var got []string
	// Bounded, so a server that never answers fails here by name instead of hanging the run.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.QueryTableStream(ctx, DefaultTable, "Cpus >= 0", 0, func(row string) bool {
		got = append(got, row)
		return true
	})
	assertInternal(t, err, log, "injected: second row", "op=Query")
	if len(got) != 1 {
		t.Fatalf("yielded %d rows before the error, want 1", len(got))
	}
	if n := RequestPanics() - before; n != 1 {
		t.Fatalf("RequestPanics grew by %d, want 1", n)
	}
	// The failed stream is also ENDED, so the client does not keep its pending entry forever.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.pending)
		c.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d pending calls left on the client after the failed stream", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	writeHook.Store(nil)
	assertServing(t, dial, c)
}

// TestCollectedStreamPanicIsAnError: the collecting stream form returns the error too, so a caller
// that only looks at the slice cannot take a truncated result for the whole one.
func TestCollectedStreamPanicIsAnError(t *testing.T) {
	dial, log := panicServer(t)
	c := dial()
	var rows atomic.Int32
	setHook(t, &writeHook, func(frame []byte) {
		if frameStatus(frame) == stStream && rows.Add(1) == 3 {
			panic("injected: third row")
		}
	})
	got, err := c.Query(context.Background(), "Cpus >= 0")
	assertInternal(t, err, log, "injected: third row")
	if len(got) != 2 {
		t.Fatalf("returned %d rows with the error, want the 2 sent before it", len(got))
	}
	writeHook.Store(nil)
	assertServing(t, dial, c)
}

// stackedPanic stands in for collections.WorkerPanic: a panic value carrying another goroutine's stack.
type stackedPanic struct{ msg, stack string }

func (p stackedPanic) String() string     { return p.msg }
func (p stackedPanic) PanicStack() []byte { return []byte(p.stack) }

// TestWorkerPanicLogsTheWorkerStack: a panic carried back from a scan worker is logged with the
// worker's own stack, since the stack at the re-raise does not show the frames that failed.
func TestWorkerPanicLogsTheWorkerStack(t *testing.T) {
	dial, log := panicServer(t)
	c := dial()
	setHook(t, &dispatchHook, func(o op) {
		if o == opQuery {
			panic(stackedPanic{msg: "injected: worker", stack: "worker frames here"})
		}
	})
	_, err := c.Query(context.Background(), "Cpus >= 0")
	assertInternal(t, err, log, "injected: worker", "worker_stack=", "worker frames here")
	dispatchHook.Store(nil)
	assertServing(t, dial, c)
}

// TestOnlyReadsAreContained pins the classification: every op that writes durable or shared state is
// left to crash the process, and the reads are contained.
func TestOnlyReadsAreContained(t *testing.T) {
	writes := []op{opBegin, opCommit, opAbort, opNewAd, opNewAdBatch, opDestroyAd, opSetAttr, opDeleteAttr,
		opCommitIdem, opAdmin, opCreateTable, opDropTable, opCreateTableMem, opTableToMemory,
		opCreateView, opDropView, opCreateExporter, opDropExporter, opPutExporterState,
		opArchiveCreate, opArchiveAppend, opArchiveRotate, opDeleteWhere,
		opRestore, opRestoreChunk, opRestoreEnd}
	for _, o := range writes {
		if o.recoverable() {
			t.Errorf("%s is contained; a write must not be", o)
		}
	}
	for _, o := range []op{opQuery, opQueryRawWire, opArchiveAggregate, opArchiveAggregateStats, opAggregate,
		opTopK, opWatch, opLookupAd, opTxnQuery, opListTables, opQueryValues, opAttrTypes} {
		if !o.recoverable() {
			t.Errorf("%s is not contained; a read should be", o)
		}
	}
}
