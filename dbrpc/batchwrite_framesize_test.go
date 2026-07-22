package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// sizeCappedConn rejects an outgoing frame larger than limit, exactly as the CEDAR stream
// does at MaxMessageSize -- so this test reproduces the "message too large" write failure
// that the byte-aware batch chunker must avoid. (The test pipe conn has no such cap.)
type sizeCappedConn struct {
	MsgConn
	limit int
	over  atomic.Int64 // frames written over the limit
}

func (c *sizeCappedConn) WriteMsg(b []byte) error {
	if len(b) > c.limit {
		c.over.Add(1)
		return fmt.Errorf("message too large: %d bytes (max %d)", len(b), c.limit)
	}
	return c.MsgConn.WriteMsg(b)
}

// TestNewClassAdBatchPipelinedFrameSize is the regression for the "message too large"
// write failures: a batch of large ads whose count-based chunk (64) would build a frame
// over the 1 MiB message limit must instead be chunked by BYTES, so every frame fits and
// all ads commit.
func TestNewClassAdBatchPipelinedFrameSize(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	capped := &sizeCappedConn{MsgConn: cconn, limit: 1 << 20} // 1 MiB, matching the CEDAR stream
	c := NewClient(capped)
	defer func() { c.Close(); s.Close(); d.Close() }()
	ctx := context.Background()

	// 128 ads of ~30 KiB each. A count-64 chunk would be ~1.9 MiB -- well over the cap;
	// byte-aware chunking must cut it into smaller frames.
	const n = 128
	big := strings.Repeat("x", 30000)
	items := make([]AdKV, n)
	for i := range items {
		items[i] = AdKV{Key: fmt.Sprintf("k%d", i), Ad: fmt.Sprintf("N = %d\nBig = %q", i, big)}
	}

	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rejects, err := tx.NewClassAdBatchPipelined(ctx, items, 64)
	if err != nil {
		t.Fatalf("pipelined batch of large ads failed (over-size frame?): %v", err)
	}
	if len(rejects) != 0 {
		t.Fatalf("unexpected rejects: %v", rejects)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if over := capped.over.Load(); over != 0 {
		t.Fatalf("%d frame(s) exceeded the %d-byte cap; byte-aware chunking failed", over, capped.limit)
	}

	// Every ad landed.
	tx2, _ := c.Begin(ctx)
	defer func() { _ = tx2.Abort(ctx) }()
	for _, i := range []int{0, n / 2, n - 1} {
		v, ok, err := tx2.LookupAttr(ctx, fmt.Sprintf("k%d", i), "N")
		if err != nil || !ok || v != fmt.Sprintf("%d", i) {
			t.Fatalf("k%d N = %q,%v,%v want %d", i, v, ok, err, i)
		}
	}
}
