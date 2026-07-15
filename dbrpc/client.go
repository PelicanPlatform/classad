package dbrpc

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/db"
)

// Client is one multiplexed connection to a dbrpc Server. It issues requests with
// monotonic ids and a single reader goroutine demuxes responses by id, so many calls
// (and independent transactions) proceed concurrently over the one connection with no
// head-of-line blocking. Safe for concurrent use. For many connections, see Pool.
type Client struct {
	conn    MsgConn
	nextReq atomic.Uint64

	mu       sync.Mutex
	pending  map[uint64]*pending
	closeErr error
}

// pending is a waiting call: a unary call's channel receives exactly one frame; a
// stream's channel receives each result frame and is closed on the stream terminator.
type pending struct {
	ch     chan []byte
	stream bool
}

// NewClient runs the mux over conn. Close (or a transport error) fails all in-flight
// and future calls.
func NewClient(conn MsgConn) *Client {
	c := &Client{conn: conn, pending: make(map[uint64]*pending)}
	go c.readLoop()
	return c
}

func (c *Client) readLoop() {
	for {
		frame, err := c.conn.ReadMsg()
		if err != nil {
			c.failAll(err)
			return
		}
		id, ok := frameReqID(frame)
		if !ok {
			continue
		}
		c.mu.Lock()
		p := c.pending[id]
		if p != nil && (!p.stream || frameStatus(frame) == stStreamEnd) {
			delete(c.pending, id) // unary call, or the stream's terminator
		}
		c.mu.Unlock()
		if p == nil {
			continue
		}
		if p.stream && frameStatus(frame) == stStreamEnd {
			close(p.ch)
			continue
		}
		p.ch <- frame // buffered; a slow stream consumer is the only backpressure
	}
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeErr == nil {
		c.closeErr = err
	}
	for id, p := range c.pending {
		close(p.ch)
		delete(c.pending, id)
	}
}

// Close closes the underlying connection; pending and future calls fail.
func (c *Client) Close() error { return c.conn.Close() }

// call sends one request (built by build with the assigned id) and waits for its
// response, returning the status and a reader positioned at the response payload.
func (c *Client) call(build func(reqID uint64) []byte) (int32, *reader, error) {
	ch, err := c.send(build, false)
	if err != nil {
		return 0, nil, err
	}
	frame, ok := <-ch
	if !ok {
		return 0, nil, c.closeErr
	}
	_, status, body, ok := respHeader(frame)
	if !ok {
		return 0, nil, errShort
	}
	return status, body, nil
}

// callStream issues a streaming request; the returned channel yields each result
// frame and is closed at the stream's end (or on connection failure). The reqID is
// returned so the caller can cancel a long-lived stream (opWatchStop).
func (c *Client) callStream(build func(reqID uint64) []byte) (uint64, <-chan []byte, error) {
	id := c.nextReq.Add(1)
	ch, err := c.sendID(id, build, true)
	if err != nil {
		return 0, nil, err
	}
	return id, ch, nil
}

// send assigns a request id, registers the pending call, and writes the request.
func (c *Client) send(build func(reqID uint64) []byte, stream bool) (chan []byte, error) {
	return c.sendID(c.nextReq.Add(1), build, stream)
}

func (c *Client) sendID(id uint64, build func(reqID uint64) []byte, stream bool) (chan []byte, error) {
	bufSize := 1
	if stream {
		bufSize = 256
	}
	ch := make(chan []byte, bufSize)
	c.mu.Lock()
	if c.closeErr != nil {
		err := c.closeErr
		c.mu.Unlock()
		return nil, err
	}
	c.pending[id] = &pending{ch: ch, stream: stream}
	c.mu.Unlock()
	if err := c.conn.WriteMsg(build(id)); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	return ch, nil
}

func statusErr(status int32, body *reader) error {
	if status == stErr {
		return fmt.Errorf("dbrpc: %s", body.str())
	}
	return fmt.Errorf("dbrpc: status %d", status)
}

// Tx is a client-side handle to a server transaction. Its operations are issued in
// order over the client (the transaction is server-side state); different Tx (even on
// one Client) proceed concurrently.
type Tx struct {
	c  *Client
	id uint64
}

// Begin starts a new independent transaction.
func (c *Client) Begin() (*Tx, error) {
	status, body, err := c.call(func(id uint64) []byte { return req(id, opBegin) })
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	return &Tx{c: c, id: body.u64()}, nil
}

// Commit applies the transaction, returning *db.ConflictError with the conflicted
// keys if any lost a write-write race (the rest committed), or nil.
func (t *Tx) Commit() error {
	status, body, err := t.c.call(func(id uint64) []byte { return putU64(req(id, opCommit), t.id) })
	if err != nil {
		return err
	}
	switch status {
	case stOK:
		return nil
	case stConflict:
		var keys []string
		for body.err == nil && len(body.b) > 0 {
			keys = append(keys, body.str())
		}
		return &db.ConflictError{Keys: keys}
	default:
		return statusErr(status, body)
	}
}

// Abort discards the transaction.
func (t *Tx) Abort() error {
	_, _, err := t.c.call(func(id uint64) []byte { return putU64(req(id, opAbort), t.id) })
	return err
}

func (t *Tx) simple(o op, fields ...string) error {
	status, body, err := t.c.call(func(id uint64) []byte {
		b := putU64(req(id, o), t.id)
		for _, f := range fields {
			b = putStr(b, f)
		}
		return b
	})
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// NewClassAd stores the ad (old-ClassAd text) under key.
func (t *Tx) NewClassAd(key, adText string) error { return t.simple(opNewAd, key, adText) }

// DestroyClassAd removes key.
func (t *Tx) DestroyClassAd(key string) error { return t.simple(opDestroyAd, key) }

// SetAttribute sets key's attribute name to expr.
func (t *Tx) SetAttribute(key, name, expr string) error { return t.simple(opSetAttr, key, name, expr) }

// DeleteAttribute removes key's attribute name.
func (t *Tx) DeleteAttribute(key, name string) error { return t.simple(opDeleteAttr, key, name) }

// LookupAttr returns key's attribute name (unparsed expression) as the transaction
// sees it, or ("", false).
func (t *Tx) LookupAttr(key, name string) (string, bool, error) {
	status, body, err := t.c.call(func(id uint64) []byte {
		return putStr(putStr(putU64(req(id, opLookupAttr), t.id), key), name)
	})
	if err != nil {
		return "", false, err
	}
	switch status {
	case stOK:
		return body.str(), true, nil
	case stMissing:
		return "", false, nil
	default:
		return "", false, statusErr(status, body)
	}
}

// LookupClassAd returns key's ad (old-ClassAd text) as the transaction sees it.
func (t *Tx) LookupClassAd(key string) (string, bool, error) {
	status, body, err := t.c.call(func(id uint64) []byte {
		return putStr(putU64(req(id, opLookupAd), t.id), key)
	})
	if err != nil {
		return "", false, err
	}
	switch status {
	case stOK:
		return body.str(), true, nil
	case stMissing:
		return "", false, nil
	default:
		return "", false, statusErr(status, body)
	}
}

// --- streaming reads over the committed store (no transaction) ---

// stream collects a finite streamed read into a slice of old-ClassAd texts.
func (c *Client) stream(build func(id uint64) []byte) ([]string, error) {
	_, ch, err := c.callStream(build)
	if err != nil {
		return nil, err
	}
	var out []string
	for frame := range ch {
		_, status, body, ok := respHeader(frame)
		if !ok {
			return out, errShort
		}
		switch status {
		case stStream:
			out = append(out, body.str())
		case stErr:
			return out, statusErr(status, body)
		}
	}
	return out, nil
}

// Query returns the committed ads (old-ClassAd texts) matching a constraint
// expression. The server streams results; a slow scan does not block other calls.
func (c *Client) Query(constraint string) ([]string, error) {
	return c.QueryLimit(constraint, 0)
}

// QueryLimit is Query that stops after at most limit matching ads (limit <= 0 =
// all). The limit is pushed to the server, which stops the scan early -- so
// LIMIT does less work, not the same work truncated on the client.
func (c *Client) QueryLimit(constraint string, limit int) ([]string, error) {
	return c.stream(func(id uint64) []byte {
		return putStr(putI32(req(id, opQuery), int32(limit)), constraint)
	})
}

// MatchSorted returns job's matches (old-ClassAd texts) ranked best-first, at most
// limit (<=0 = all).
func (c *Client) MatchSorted(jobText string, limit int) ([]string, error) {
	return c.stream(func(id uint64) []byte {
		return putStr(putI32(req(id, opMatchSorted), int32(limit)), jobText)
	})
}

// OrderedRow is one ad from an ordered scan (old-ClassAd text) with its cluster
// signature -- run-length-fold equal signatures into a resource-request list.
type OrderedRow struct {
	Signature uint64
	AdText    string
}

// Ordered streams one partition of the index-th configured ordered index in sort
// order (the negotiator resource-request path). One-shot: the whole partition is
// returned (the server-side resume cursor is not carried over the wire).
func (c *Client) Ordered(index int, partition string) ([]OrderedRow, error) {
	_, frames, err := c.callStream(func(id uint64) []byte {
		return putStr(putI32(req(id, opOrdered), int32(index)), partition)
	})
	if err != nil {
		return nil, err
	}
	var out []OrderedRow
	for frame := range frames {
		_, status, body, ok := respHeader(frame)
		if !ok {
			return out, errShort
		}
		switch status {
		case stStream:
			row := OrderedRow{Signature: body.u64()}
			row.AdText = body.str()
			out = append(out, row)
		case stErr:
			return out, statusErr(status, body)
		}
	}
	return out, nil
}

// WatchEvent is one change delivered over a Watch. AdText is the ad's old-ClassAd text
// (empty for a delete/reset). Cursor resumes the watch just after this event.
type WatchEvent struct {
	Kind   uint8 // 0 upsert, 1 delete, 2 reset (see db.WatchKind)
	Key    string
	AdText string
	Cursor []byte
}

// Watch streams changes committed after cursor (nil = from now) on the channel, which
// closes when the returned stop is called, the connection fails, or the server ends
// the stream. A full replay leads with a reset event.
func (c *Client) Watch(cursor []byte) (<-chan WatchEvent, func(), error) {
	id, frames, err := c.callStream(func(rid uint64) []byte {
		return putBytes(req(rid, opWatch), cursor)
	})
	if err != nil {
		return nil, nil, err
	}
	events := make(chan WatchEvent, 64)
	go func() {
		defer close(events)
		for frame := range frames {
			_, status, body, ok := respHeader(frame)
			if !ok || status != stStream {
				continue
			}
			ev := WatchEvent{Kind: body.u8()}
			ev.Key = body.str()
			ev.AdText = body.str()
			ev.Cursor = append([]byte(nil), body.bytesRef()...)
			events <- ev
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_, _, _ = c.call(func(rid uint64) []byte { return putU64(req(rid, opWatchStop), id) })
		})
	}
	return events, stop, nil
}
