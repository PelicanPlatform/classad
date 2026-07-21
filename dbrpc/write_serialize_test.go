package dbrpc

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// serialCheckConn wraps a MsgConn and flags any two WriteMsg calls that overlap in
// time. It deliberately does NOT serialize writes itself (unlike the test pipeConn,
// whose internal wmu would mask the bug) -- it models the real CEDAR adapter, which is
// single-writer: it reuses one frame buffer and advances one AES-GCM nonce per message,
// so two concurrent writers corrupt the encrypted stream and the peer drops it on an
// authentication failure. The MsgConn contract (see conn.go) requires the mux (Client)
// to serialize writers; this asserts the Client actually does.
type serialCheckConn struct {
	MsgConn
	inflight   atomic.Int32
	violations atomic.Int32
}

func (c *serialCheckConn) WriteMsg(b []byte) error {
	if c.inflight.Add(1) != 1 {
		c.violations.Add(1)
	}
	// Widen the window so a missing lock reliably produces an overlap.
	time.Sleep(50 * time.Microsecond)
	err := c.MsgConn.WriteMsg(b)
	c.inflight.Add(-1)
	return err
}

// TestClientSerializesConcurrentWrites is the regression guard for the AES-GCM
// "message authentication failed" disconnects: independent transactions multiplexed
// over one connection must never call WriteMsg concurrently, or an encrypted CEDAR
// stream corrupts. Fails if the client mux lets two writes overlap.
func TestClientSerializesConcurrentWrites(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	cconn, sconn := netPipe()
	det := &serialCheckConn{MsgConn: cconn}
	s := NewServer(d)
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(det)
	defer func() { c.Close(); s.Close(); d.Close() }()

	const n = 200
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			tx, err := c.Begin(ctx) // independent transaction, concurrent over the one conn
			if err != nil {
				errs[i] = err
				return
			}
			if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", i), "N = 1"); err != nil {
				errs[i] = err
				return
			}
			errs[i] = tx.Commit(ctx)
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("call %d: %v", i, e)
		}
	}
	if v := det.violations.Load(); v != 0 {
		t.Fatalf("client issued %d concurrent WriteMsg calls; the MsgConn contract requires the mux to serialize writers "+
			"(unserialized writes corrupt an encrypted CEDAR stream -> peer drops it on AES-GCM auth failure)", v)
	}
}
