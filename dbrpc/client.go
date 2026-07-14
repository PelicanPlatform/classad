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
	pending  map[uint64]chan []byte
	closeErr error
}

// NewClient runs the mux over conn. Close (or a transport error) fails all in-flight
// and future calls.
func NewClient(conn MsgConn) *Client {
	c := &Client{conn: conn, pending: make(map[uint64]chan []byte)}
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
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- frame
		}
	}
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeErr == nil {
		c.closeErr = err
	}
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

// Close closes the underlying connection; pending and future calls fail.
func (c *Client) Close() error { return c.conn.Close() }

// call sends one request (built by build with the assigned id) and waits for its
// response, returning the status and a reader positioned at the response payload.
func (c *Client) call(build func(reqID uint64) []byte) (int32, *reader, error) {
	id := c.nextReq.Add(1)
	ch := make(chan []byte, 1)
	c.mu.Lock()
	if c.closeErr != nil {
		err := c.closeErr
		c.mu.Unlock()
		return 0, nil, err
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.conn.WriteMsg(build(id)); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
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
