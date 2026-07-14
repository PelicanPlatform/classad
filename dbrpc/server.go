package dbrpc

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// Server serves an embedded ClassAd log (*db.DB) to remote clients. It is
// embeddable: create it around a DB, optionally start its managed maintenance
// goroutines, and hand it each accepted connection via ServeConn. Concurrent
// connections and concurrent in-flight requests per connection are supported;
// requests are dispatched as they arrive and responses carry the request id, so a
// slow call never head-of-line-blocks others.
type Server struct {
	db     *db.DB
	txns   sync.Map // txnID(uint64) -> *serverTxn
	nextID atomic.Uint64
	stopBG []func()
}

// serverTxn is a live server-side transaction. Its mutex serializes operations on the
// (non-concurrent) *db.Txn even if a client pipelines them.
type serverTxn struct {
	tx *db.Txn
	mu sync.Mutex
}

// NewServer returns a server over d. The caller owns d's lifetime.
func NewServer(d *db.DB) *Server { return &Server{db: d} }

// ServeOptions scopes what a single served connection may do. The zero value is
// full read/write access with private attributes excluded from returned ads
// (the historical ServeConn behavior). A privilege-scoped front end (e.g. an
// HTCondor daemon serving READ vs WRITE peers) sets these per connection.
type ServeOptions struct {
	// ReadOnly rejects the mutating operations (NewClassAd, DestroyClassAd,
	// SetAttribute, DeleteAttribute) with an error; reads, snapshots (Begin),
	// and queries still work. A read-only peer may still Begin/Abort a
	// transaction to get a stable snapshot for reads.
	ReadOnly bool

	// IncludePrivate renders returned ads with their private (secret)
	// attributes intact (classad.StringWithPrivate). When false (the default),
	// private attributes are stripped from every ad this connection sees, so an
	// under-privileged peer never learns claim ids and other secrets.
	IncludePrivate bool
}

// isMutating reports whether o writes to a transaction (and so is refused on a
// read-only connection).
func (o op) isMutating() bool {
	switch o {
	case opNewAd, opDestroyAd, opSetAttr, opDeleteAttr:
		return true
	}
	return false
}

// StartMaintenance starts the server-managed background maintenance (dictionary
// retrain + hot-attribute refresh) on the given interval. Stopped by Close.
func (s *Server) StartMaintenance(interval time.Duration) {
	s.stopBG = append(s.stopBG, s.db.StartMaintenance(interval))
}

// Close stops the server's managed goroutines. It does not close the DB.
func (s *Server) Close() {
	for _, stop := range s.stopBG {
		stop()
	}
	s.stopBG = nil
}

// ServeConn runs the request loop on one connection until it errors or the peer
// closes. Blocking; run one per accepted connection. Watches started on the
// connection are cancelled when it returns. Equivalent to ServeConnOpts with the
// zero ServeOptions (full read/write, private attributes excluded).
func (s *Server) ServeConn(conn MsgConn) error {
	return s.ServeConnOpts(conn, ServeOptions{})
}

// ServeConnOpts is ServeConn scoped by opts: a read-only and/or
// private-stripping view of the same DB. Use it to serve a privilege-scoped
// peer (e.g. an HTCondor READ-level client) from the same Server.
func (s *Server) ServeConnOpts(conn MsgConn, opts ServeOptions) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stop this connection's watches when it closes
	sc := &serverConn{s: s, ctx: ctx, opts: opts, watches: make(map[uint64]context.CancelFunc)}
	var wmu sync.Mutex
	sc.write = func(b []byte) {
		wmu.Lock()
		_ = conn.WriteMsg(b)
		wmu.Unlock()
	}
	for {
		frame, err := conn.ReadMsg()
		if err != nil {
			return err
		}
		go sc.dispatch(frame)
	}
}

// serverConn is per-connection state: the serialized writer, a context cancelled when
// the connection closes, and the live watches (by their request id) so opWatchStop
// and connection close can cancel them.
type serverConn struct {
	s     *Server
	ctx   context.Context
	opts  ServeOptions
	write func([]byte)

	wmu     sync.Mutex
	watches map[uint64]context.CancelFunc
}

func (sc *serverConn) dispatch(frame []byte) {
	reqID, o, body, ok := reqHeader(frame)
	if !ok {
		return // unparseable header: cannot even address a response
	}
	if sc.opts.ReadOnly && o.isMutating() {
		sc.write(respErr(reqID, "read-only connection: "+o.String()+" not permitted"))
		return
	}
	priv := sc.opts.IncludePrivate
	switch o {
	case opQuery:
		sc.s.streamQuery(reqID, body, priv, sc.write)
	case opMatchSorted:
		sc.s.streamMatchSorted(reqID, body, priv, sc.write)
	case opOrdered:
		sc.s.streamOrdered(reqID, body, priv, sc.write)
	case opAggregate:
		sc.s.streamAggregate(reqID, body, priv, sc.write)
	case opWatch:
		sc.streamWatch(reqID, body)
	case opWatchStop:
		sc.stopWatch(body.u64())
		sc.write(resp(reqID, stOK))
	default:
		sc.write(sc.s.handle(reqID, o, body, priv))
	}
}

// adString renders ad for the wire, including private attributes only when the
// connection is privileged to see them.
func adString(ad *classad.ClassAd, includePrivate bool) string {
	if includePrivate {
		return ad.StringWithPrivate()
	}
	return ad.String()
}

// streamWatch runs a watch, streaming each event as a frame [kind u8][key][adText]
// [cursor] under reqID, until the client cancels it (opWatchStop) or the connection
// closes. cursor empty starts from now.
func (sc *serverConn) streamWatch(reqID uint64, r *reader) {
	cursor := append([]byte(nil), r.bytesRef()...)
	ctx, cancel := context.WithCancel(sc.ctx)
	sc.wmu.Lock()
	sc.watches[reqID] = cancel
	sc.wmu.Unlock()
	defer func() {
		sc.wmu.Lock()
		delete(sc.watches, reqID)
		sc.wmu.Unlock()
		cancel()
	}()

	seq, err := sc.s.db.Watch(ctx, cursor)
	if err != nil {
		sc.write(respErr(reqID, err.Error()))
		return
	}
	for ev := range seq {
		b := putU8(respHead(reqID, stStream), byte(ev.Kind))
		b = putStr(b, ev.Key)
		if ev.Ad != nil {
			b = putStr(b, adString(ev.Ad, sc.opts.IncludePrivate))
		} else {
			b = putStr(b, "")
		}
		b = putBytes(b, ev.Cursor)
		sc.write(b)
	}
	sc.write(respHead(reqID, stStreamEnd))
}

func (sc *serverConn) stopWatch(watchReqID uint64) {
	sc.wmu.Lock()
	cancel := sc.watches[watchReqID]
	sc.wmu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// streamQuery streams the committed ads matching a constraint. Each result is its own
// frame under reqID, so its frames interleave with other calls' -- no head-of-line
// blocking -- and end with a terminator.
func (s *Server) streamQuery(reqID uint64, r *reader, includePrivate bool, write func([]byte)) {
	constraint := r.str()
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	seq, err := s.db.Query(constraint)
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	for ad := range seq {
		write(putStr(respHead(reqID, stStream), adString(ad, includePrivate)))
	}
	write(respHead(reqID, stStreamEnd))
}

// streamMatchSorted streams job's ranked matches (best first, up to limit).
func (s *Server) streamMatchSorted(reqID uint64, r *reader, includePrivate bool, write func([]byte)) {
	limit := r.i32()
	jobText := r.str()
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	job, err := classad.ParseOld(jobText)
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	for _, ad := range s.db.MatchSorted(job, int(limit)) {
		write(putStr(respHead(reqID, stStream), adString(ad, includePrivate)))
	}
	write(respHead(reqID, stStreamEnd))
}

// streamOrdered streams one partition of an ordered index in sort order, each ad with
// its cluster signature (for resource-request-list folding). One-shot: the in-memory
// resume cursor is not carried over the wire, so a full partition is streamed.
func (s *Server) streamOrdered(reqID uint64, r *reader, includePrivate bool, write func([]byte)) {
	index := r.i32()
	partition := r.str()
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	for oa := range s.db.Ordered(int(index), partition, db.OrderCursor{}) {
		b := putU64(respHead(reqID, stStream), oa.Signature)
		b = putStr(b, adString(oa.Ad, includePrivate))
		write(b)
	}
	write(respHead(reqID, stStreamEnd))
}

// handle executes one request and returns its response frame. includePrivate
// controls whether ads returned by lookups carry their private attributes.
func (s *Server) handle(reqID uint64, o op, r *reader, includePrivate bool) []byte {
	switch o {
	case opBegin:
		id := s.nextID.Add(1)
		s.txns.Store(id, &serverTxn{tx: s.db.Begin()})
		return putU64(resp(reqID, stOK), id)

	case opCommit:
		st, ok := s.take(r.u64())
		if !ok {
			return respErr(reqID, "no such transaction")
		}
		err := st.tx.Commit()
		if err == nil {
			return resp(reqID, stOK)
		}
		if ce, isConf := err.(*db.ConflictError); isConf {
			b := respHead(reqID, stConflict)
			for _, k := range ce.Keys {
				b = putStr(b, k)
			}
			return b
		}
		return respErr(reqID, err.Error())

	case opAbort:
		if st, ok := s.take(r.u64()); ok {
			st.tx.Abort()
		}
		return resp(reqID, stOK)

	case opNewAd:
		return s.withTxn(reqID, r, func(tx *db.Txn) []byte {
			key, adText := r.str(), r.str()
			if r.err != nil {
				return respBad(reqID)
			}
			ad, err := classad.ParseOld(adText)
			if err != nil {
				return respErr(reqID, err.Error())
			}
			tx.NewClassAd(key, ad)
			return resp(reqID, stOK)
		})

	case opDestroyAd:
		return s.withTxn(reqID, r, func(tx *db.Txn) []byte {
			tx.DestroyClassAd(r.str())
			return resp(reqID, stOK)
		})

	case opSetAttr:
		return s.withTxn(reqID, r, func(tx *db.Txn) []byte {
			key, name, expr := r.str(), r.str(), r.str()
			if r.err != nil {
				return respBad(reqID)
			}
			if err := tx.SetAttribute(key, name, expr); err != nil {
				return respErr(reqID, err.Error())
			}
			return resp(reqID, stOK)
		})

	case opDeleteAttr:
		return s.withTxn(reqID, r, func(tx *db.Txn) []byte {
			key, name := r.str(), r.str()
			if r.err != nil {
				return respBad(reqID)
			}
			tx.DeleteAttribute(key, name)
			return resp(reqID, stOK)
		})

	case opLookupAttr:
		return s.withTxn(reqID, r, func(tx *db.Txn) []byte {
			key, name := r.str(), r.str()
			if r.err != nil {
				return respBad(reqID)
			}
			v, ok := tx.LookupAttr(key, name)
			if !ok {
				return resp(reqID, stMissing)
			}
			return putStr(resp(reqID, stOK), v)
		})

	case opLookupAd:
		return s.withTxn(reqID, r, func(tx *db.Txn) []byte {
			ad, ok := tx.LookupClassAd(r.str())
			if !ok {
				return resp(reqID, stMissing)
			}
			return putStr(resp(reqID, stOK), adString(ad, includePrivate))
		})
	}
	return respBad(reqID)
}

// withTxn resolves the leading txnID field, locks the transaction (serializing
// pipelined ops on it), and runs fn.
func (s *Server) withTxn(reqID uint64, r *reader, fn func(*db.Txn) []byte) []byte {
	id := r.u64()
	if r.err != nil {
		return respBad(reqID)
	}
	v, ok := s.txns.Load(id)
	if !ok {
		return respErr(reqID, "no such transaction")
	}
	st := v.(*serverTxn)
	st.mu.Lock()
	defer st.mu.Unlock()
	return fn(st.tx)
}

// take removes and returns a transaction (for commit/abort).
func (s *Server) take(id uint64) (*serverTxn, bool) {
	v, ok := s.txns.LoadAndDelete(id)
	if !ok {
		return nil, false
	}
	return v.(*serverTxn), true
}

// --- response frame builders ---

func respHead(reqID uint64, status int32) []byte {
	return putI32(putU64(make([]byte, 0, 16), reqID), status)
}
func resp(reqID uint64, status int32) []byte { return respHead(reqID, status) }
func respErr(reqID uint64, msg string) []byte {
	return putStr(respHead(reqID, stErr), msg)
}
func respBad(reqID uint64) []byte { return resp(reqID, stBadReq) }
