package dbrpc

import (
	"context"
	"sync/atomic"
)

// Pool is a set of multiplexed connections to one Server, load-balanced round-robin.
// It borrows the database/sql idea of a connection pool, but adapts it: an SQL driver
// needs a connection per in-flight query (connections do not mux), so its pool
// serializes checkout; here each Client already multiplexes many concurrent calls, so
// the pool exists for throughput across connections and fault isolation, not mutual
// exclusion. A transaction begun through the pool pins the connection it started on
// (its ops go over that one Client, in order), while that connection keeps serving
// everyone else.
type Pool struct {
	clients []*Client
	next    atomic.Uint64
}

// NewPool dials n connections with dial and multiplexes each. On any dial error it
// closes the connections already opened and returns the error.
func NewPool(dial func() (MsgConn, error), n int) (*Pool, error) {
	if n < 1 {
		n = 1
	}
	p := &Pool{}
	for i := 0; i < n; i++ {
		conn, err := dial()
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		p.clients = append(p.clients, NewClient(conn))
	}
	return p, nil
}

// pick returns the next connection round-robin.
func (p *Pool) pick() *Client {
	return p.clients[int(p.next.Add(1)-1)%len(p.clients)]
}

// Begin starts a transaction on a pool connection; the returned Tx's operations all
// run over that connection (server-side transaction state + op ordering).
func (p *Pool) Begin(ctx context.Context) (*Tx, error) { return p.pick().Begin(ctx) }

// Query runs a constraint query on a pool connection, streaming the results.
func (p *Pool) Query(ctx context.Context, constraint string) ([]string, error) {
	return p.pick().Query(ctx, constraint)
}

// MatchSorted runs a ranked match on a pool connection.
func (p *Pool) MatchSorted(ctx context.Context, jobText string, limit int) ([]string, error) {
	return p.pick().MatchSorted(ctx, jobText, limit)
}

// Watch starts a watch on a pool connection.
func (p *Pool) Watch(ctx context.Context, cursor []byte) (<-chan WatchEvent, func(), error) {
	return p.pick().Watch(ctx, cursor)
}

// Ordered streams a partition of an ordered index on a pool connection.
func (p *Pool) Ordered(ctx context.Context, index int, partition string) ([]OrderedRow, error) {
	return p.pick().Ordered(ctx, index, partition)
}

// Close closes every connection. The first close error is returned.
func (p *Pool) Close() error {
	var first error
	for _, c := range p.clients {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
