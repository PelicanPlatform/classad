package dbrpc

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// sealConn wraps a served connection and counts the frames the server writes after the
// test seals it -- which the test does once Close has returned. A frame written then came
// from a request goroutine the caller believes has stopped, i.e. one still reading the
// store the caller is about to close.
type sealConn struct {
	MsgConn
	mu     sync.Mutex
	sealed bool
	after  int
}

func (s *sealConn) seal() {
	s.mu.Lock()
	s.sealed = true
	s.mu.Unlock()
}

func (s *sealConn) writesAfterSeal() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.after
}

func (s *sealConn) WriteMsg(b []byte) error {
	s.mu.Lock()
	if s.sealed {
		s.after++
	}
	s.mu.Unlock()
	return s.MsgConn.WriteMsg(b)
}

func commitAd(t *testing.T, d *db.DB, key, text string) {
	t.Helper()
	tx := d.Begin()
	tx.NewClassAd(key, mustAd(t, text))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestCloseEndsInFlightWatch proves Server.Close does not return while a request is still
// being served. A watch streams until something cancels it; nothing did, so before the fix
// Close returned with the watch goroutine still reading the store -- and the caller's next
// act, closing the catalog, munmapped the segments under it.
//
// The watch is confirmed live (it delivers an event) before Close, and the probe after
// Close is a second commit: served, it reaches the watch and the server writes its event.
func TestCloseEndsInFlightWatch(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := NewServer(d)
	s.CloseGrace = 2 * time.Second

	cconn, sconn := netPipe()
	sealed := &sealConn{MsgConn: sconn}
	served := make(chan error, 1)
	go func() { served <- s.ServeConn(sealed) }()
	c := NewClient(cconn)
	defer c.Close()

	events, stop, err := c.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Wait for the committed row to come back rather than sleeping: the watch is then
	// provably streaming, not merely requested.
	commitAd(t, d, "a1", "N = 1")
	waitEvent := func(key string) (found, ended bool) {
		t.Helper()
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					return false, true
				}
				if ev.Key == key {
					return true, false
				}
			case <-time.After(10 * time.Second):
				return false, false
			}
		}
	}
	if found, ended := waitEvent("a1"); !found {
		t.Fatalf("watch never delivered its first event (stream ended: %v)", ended)
	}

	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("Close hung with a watch in flight")
	}
	sealed.seal()

	commitAd(t, d, "a2", "N = 2")
	// Events already buffered client-side were written before Close returned, so drain
	// past them: what must never arrive is the row committed afterwards.
	found, ended := waitEvent("a2")
	if found {
		t.Fatal("the server served a watch event for a row committed after Close returned: a request goroutine was still reading the store")
	}
	if !ended {
		t.Fatal("the watch was neither ended nor cancelled by Close")
	}
	if n := sealed.writesAfterSeal(); n != 0 {
		t.Fatalf("the server wrote %d frames after Close returned", n)
	}

	// Closing the server dropped the connection, so its read loop ended too.
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("ServeConn did not return after Close")
	}
}

// blockConn is a client that sent one request and then stopped reading: every server write
// blocks. releaseOnClose says whether failing the transport releases the blocked writer,
// which is what a real socket does.
type blockConn struct {
	first          []byte
	sent           bool
	release        chan struct{}
	closed         chan struct{}
	releaseOnClose bool
	once           sync.Once
	entered        chan struct{} // closed when the server first blocks in WriteMsg
	enterOnce      sync.Once
}

func newBlockConn(first []byte, releaseOnClose bool) *blockConn {
	return &blockConn{
		first:          first,
		release:        make(chan struct{}),
		closed:         make(chan struct{}),
		releaseOnClose: releaseOnClose,
		entered:        make(chan struct{}),
	}
}

func (b *blockConn) ReadMsg() ([]byte, error) {
	if !b.sent {
		b.sent = true
		return b.first, nil
	}
	<-b.closed
	return nil, io.EOF
}

func (b *blockConn) WriteMsg([]byte) error {
	b.enterOnce.Do(func() { close(b.entered) })
	if b.releaseOnClose {
		select {
		case <-b.release:
		case <-b.closed:
			return io.ErrClosedPipe
		}
		return nil
	}
	<-b.release
	return nil
}

func (b *blockConn) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func (b *blockConn) unblock() { close(b.release) }

// queryFrame builds a plain query request, whose first act is to write a frame -- so a
// client that is not reading leaves the request blocked in WriteMsg.
func queryFrame(table, constraint string) []byte {
	return putStr(putI32(putStr(req(1, opQuery), table), 0), constraint)
}

// TestCloseReleasesRequestBlockedOnWrite covers the request that cancellation cannot
// reach: one blocked writing to a client that stopped reading. No context reaches a
// blocked WriteMsg, so Close closes the transport and the request unwinds on the write
// error -- within its grace budget, rather than holding shutdown open.
func TestCloseReleasesRequestBlockedOnWrite(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	commitAd(t, d, "k", "N = 1")

	s := NewServer(d)
	s.CloseGrace = 500 * time.Millisecond
	conn := newBlockConn(queryFrame(DefaultTable, "true"), true)
	served := make(chan error, 1)
	go func() { served <- s.ServeConn(conn) }()

	select {
	case <-conn.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached a write")
	}

	closed := make(chan struct{})
	start := time.Now()
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung on a request blocked writing to a client that stopped reading")
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("Close took %v with a 500ms grace", el)
	}
	// The transport was closed, so the connection's read loop ended too.
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("ServeConn did not return after Close")
	}
}

// TestCloseAbandonsUnreleasableRequest is the worst case: a transport whose Close does not
// release the blocked writer. Close cannot wait for that request without hanging forever,
// so it gives up at the end of its grace budget and returns. The request is abandoned --
// documented on Close, and the reason the budget exists at all.
func TestCloseAbandonsUnreleasableRequest(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	commitAd(t, d, "k", "N = 1")

	s := NewServer(d)
	s.CloseGrace = 500 * time.Millisecond
	conn := newBlockConn(queryFrame(DefaultTable, "true"), false)
	served := make(chan error, 1)
	go func() { served <- s.ServeConn(conn) }()

	select {
	case <-conn.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached a write")
	}

	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung on a request that cannot be released")
	}

	// Let the abandoned request finish before the store it reads goes away.
	conn.unblock()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
	}
	d.Close()
}

// TestCloseIsPromptWhenIdle keeps Close honest in the ordinary case: with nothing in
// flight it must not sit out any part of its grace budget, and requests that completed on
// their own are unaffected.
func TestCloseIsPromptWhenIdle(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := NewServer(d)
	s.CloseGrace = 10 * time.Second

	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer c.Close()

	commitAd(t, d, "k", "N = 1")
	rows, err := c.Query(context.Background(), "true")
	if err != nil || len(rows) != 1 {
		t.Fatalf("Query = %v, %v; want one row", rows, err)
	}

	start := time.Now()
	s.Close()
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Close of an idle server took %v", el)
	}
	s.Close() // idempotent
}

// TestClosedServerRefusesConnections proves the set Close waits on cannot grow while it
// waits: a connection handed to a closed server is refused outright.
func TestClosedServerRefusesConnections(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := NewServer(d)
	s.Close()

	_, sconn := netPipe()
	if err := s.ServeConn(sconn); err != ErrServerClosed {
		t.Fatalf("ServeConn on a closed server = %v, want ErrServerClosed", err)
	}
}
