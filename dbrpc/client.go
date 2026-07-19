package dbrpc

import (
	"context"
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
		// Wrap the transport cause as ErrConnClosed so callers classify with
		// errors.Is (reconnect-and-replay) while retaining the specific I/O error.
		c.closeErr = fmt.Errorf("%w: %v", ErrConnClosed, err)
	}
	for id, p := range c.pending {
		close(p.ch)
		delete(c.pending, id)
	}
}

// Close closes the underlying connection; pending and future calls fail.
func (c *Client) Close() error { return c.conn.Close() }

// callCtx sends one request (built by build with the assigned id) and waits for its
// response under ctx, returning the status and a reader positioned at the response
// payload. If ctx ends first it unregisters the pending call (so the eventual
// response is discarded, not delivered to a since-departed caller) and returns
// ctx.Err(); the shared read loop keeps serving every other in-flight call. There
// is no implicit timeout -- the deadline is exactly whatever ctx carries.
func (c *Client) callCtx(ctx context.Context, build func(reqID uint64) []byte) (int32, *reader, error) {
	id := c.nextReq.Add(1)
	ch, err := c.sendID(id, build, false)
	if err != nil {
		return 0, nil, err
	}
	select {
	case <-ctx.Done():
		c.cancelPending(id)
		return 0, nil, ctx.Err()
	case frame, ok := <-ch:
		if !ok {
			return 0, nil, c.closeErr
		}
		_, status, body, ok := respHeader(frame)
		if !ok {
			return 0, nil, errShort
		}
		return status, body, nil
	}
}

// cancelPending unregisters a unary call's pending entry so a late response frame is
// discarded by the read loop. The response channel is buffered (size 1), so the read
// loop never blocks delivering to an abandoned call.
func (c *Client) cancelPending(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
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
		return &ServerError{Msg: body.str()}
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

// Begin starts a new independent transaction on the default table ("ads").
func (c *Client) Begin(ctx context.Context) (*Tx, error) { return c.BeginTable(ctx, DefaultTable) }

// BeginTable starts a new independent transaction on the named table.
func (c *Client) BeginTable(ctx context.Context, table string) (*Tx, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte { return putStr(req(id, opBegin), table) })
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
func (t *Tx) Commit(ctx context.Context) error {
	status, body, err := t.c.callCtx(ctx, func(id uint64) []byte { return putU64(req(id, opCommit), t.id) })
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

// CommitIdempotent is Commit with exactly-once semantics across retries: the server
// records a durable marker under idemKey, committed atomically with this
// transaction's writes, so replaying the SAME unit of work (same idemKey) after an
// ambiguous failure -- a reconnect where the original commit's fate is unknown --
// applies it at most once. The caller must generate idemKey ONCE per unit of work
// and reuse it verbatim on every replay. Use this for non-idempotent transactions
// (e.g. relative updates, appends); an already-idempotent-by-key workload can use
// the cheaper plain Commit. Returns the same results as Commit (nil, or
// *db.ConflictError on a genuine write-write conflict).
func (t *Tx) CommitIdempotent(ctx context.Context, idemKey string) error {
	status, body, err := t.c.callCtx(ctx, func(id uint64) []byte {
		return putStr(putU64(req(id, opCommitIdem), t.id), idemKey)
	})
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
func (t *Tx) Abort(ctx context.Context) error {
	_, _, err := t.c.callCtx(ctx, func(id uint64) []byte { return putU64(req(id, opAbort), t.id) })
	return err
}

func (t *Tx) simple(ctx context.Context, o op, fields ...string) error {
	status, body, err := t.c.callCtx(ctx, func(id uint64) []byte {
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
func (t *Tx) NewClassAd(ctx context.Context, key, adText string) error {
	return t.simple(ctx, opNewAd, key, adText)
}

// DestroyClassAd removes key.
func (t *Tx) DestroyClassAd(ctx context.Context, key string) error {
	return t.simple(ctx, opDestroyAd, key)
}

// SetAttribute sets key's attribute name to expr.
func (t *Tx) SetAttribute(ctx context.Context, key, name, expr string) error {
	return t.simple(ctx, opSetAttr, key, name, expr)
}

// DeleteAttribute removes key's attribute name.
func (t *Tx) DeleteAttribute(ctx context.Context, key, name string) error {
	return t.simple(ctx, opDeleteAttr, key, name)
}

// LookupAttr returns key's attribute name (unparsed expression) as the transaction
// sees it, or ("", false).
func (t *Tx) LookupAttr(ctx context.Context, key, name string) (string, bool, error) {
	status, body, err := t.c.callCtx(ctx, func(id uint64) []byte {
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
func (t *Tx) LookupClassAd(ctx context.Context, key string) (string, bool, error) {
	status, body, err := t.c.callCtx(ctx, func(id uint64) []byte {
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

// streamCtx collects a finite streamed read into a slice of old-ClassAd texts,
// honoring ctx. If ctx ends first it returns ctx.Err() and hands the response
// channel to a background drain: the shared read loop must never block delivering a
// stream frame, and the pending entry is cleaned only when the server's stream
// terminator arrives, so we keep reading (discarding) rather than orphaning it.
func (c *Client) streamCtx(ctx context.Context, build func(id uint64) []byte) ([]string, error) {
	_, ch, err := c.callStream(build)
	if err != nil {
		return nil, err
	}
	var out []string
	for {
		select {
		case <-ctx.Done():
			drain(ch) // backgrounds itself; keeps the read loop unblocked
			return out, ctx.Err()
		case frame, ok := <-ch:
			if !ok {
				return out, nil
			}
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
	}
}

// Query returns the committed ads (old-ClassAd texts) in the default table ("ads")
// matching a constraint. The server streams results; a slow scan does not block
// other calls.
func (c *Client) Query(ctx context.Context, constraint string) ([]string, error) {
	return c.QueryTable(ctx, DefaultTable, constraint, 0)
}

// QueryLimit is Query with a row cap (<= 0 = all) on the default table.
func (c *Client) QueryLimit(ctx context.Context, constraint string, limit int) ([]string, error) {
	return c.QueryTable(ctx, DefaultTable, constraint, limit)
}

// QueryTable returns the committed ads in the named table matching constraint,
// stopping after at most limit matches (<= 0 = all). The limit is pushed to the
// server, which stops the scan early.
func (c *Client) QueryTable(ctx context.Context, table, constraint string, limit int) ([]string, error) {
	return c.streamCtx(ctx, func(id uint64) []byte {
		return putStr(putI32(putStr(req(id, opQuery), table), int32(limit)), constraint)
	})
}

// MatchSorted returns job's matches in the default table, ranked best-first, at
// most limit (<=0 = all).
func (c *Client) MatchSorted(ctx context.Context, jobText string, limit int) ([]string, error) {
	return c.MatchSortedTable(ctx, DefaultTable, jobText, limit)
}

// MatchSortedTable returns job's ranked matches in the named table.
func (c *Client) MatchSortedTable(ctx context.Context, table, jobText string, limit int) ([]string, error) {
	return c.streamCtx(ctx, func(id uint64) []byte {
		return putStr(putI32(putStr(req(id, opMatchSorted), table), int32(limit)), jobText)
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
func (c *Client) Ordered(ctx context.Context, index int, partition string) ([]OrderedRow, error) {
	return c.OrderedTable(ctx, DefaultTable, index, partition)
}

// OrderedTable streams one partition of an ordered index in the named table.
func (c *Client) OrderedTable(ctx context.Context, table string, index int, partition string) ([]OrderedRow, error) {
	_, frames, err := c.callStream(func(id uint64) []byte {
		return putStr(putI32(putStr(req(id, opOrdered), table), int32(index)), partition)
	})
	if err != nil {
		return nil, err
	}
	var out []OrderedRow
	for {
		select {
		case <-ctx.Done():
			drain(frames)
			return out, ctx.Err()
		case frame, ok := <-frames:
			if !ok {
				return out, nil
			}
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
	}
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
func (c *Client) Watch(ctx context.Context, cursor []byte) (<-chan WatchEvent, func(), error) {
	return c.WatchTable(ctx, DefaultTable, cursor)
}

// WatchHead returns an opaque cursor at the current head of table's change log, so a
// following WatchTable(table, cursor) streams only subsequent changes (no replay of
// current contents) -- the "tail from now" path.
func (c *Client) WatchHead(ctx context.Context, table string) ([]byte, error) {
	status, body, err := c.callCtx(ctx, func(rid uint64) []byte {
		return putStr(req(rid, opWatchHead), table)
	})
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	return append([]byte(nil), body.bytesRef()...), nil
}

// WatchTable streams changes to the named table.
func (c *Client) WatchTable(ctx context.Context, table string, cursor []byte) (<-chan WatchEvent, func(), error) {
	id, frames, err := c.callStream(func(rid uint64) []byte {
		return putBytes(putStr(req(rid, opWatch), table), cursor)
	})
	if err != nil {
		return nil, nil, err
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_, _, _ = c.callCtx(context.Background(), func(rid uint64) []byte { return putU64(req(rid, opWatchStop), id) })
		})
	}
	events := make(chan WatchEvent, 64)
	go func() {
		defer close(events)
		for {
			select {
			case <-ctx.Done():
				stop() // tell the server to end the stream; frames is drained below
				drain(frames)
				return
			case frame, ok := <-frames:
				if !ok {
					return
				}
				_, status, body, ok := respHeader(frame)
				if !ok || status != stStream {
					continue
				}
				ev := WatchEvent{Kind: body.u8()}
				ev.Key = body.str()
				ev.AdText = body.str()
				ev.Cursor = append([]byte(nil), body.bytesRef()...)
				select {
				case events <- ev:
				case <-ctx.Done():
					stop()
					drain(frames)
					return
				}
			}
		}
	}()
	return events, stop, nil
}
