package dbrpc

import (
	"context"
	"encoding/json"
	"fmt"
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
	cat    Catalog
	txns   sync.Map // txnID(uint64) -> *serverTxn
	nextID atomic.Uint64
	stopBG []func()

	// propose, when set, routes a committing transaction's writes through an external
	// consensus layer (raft) instead of committing them to the local store: on opCommit
	// the accumulated ops are handed to propose, which is the store's sole writer. Set by
	// the consistent-HA daemon on the leader; nil for a standalone store. See propose.go.
	propose ProposeFunc
}

// Catalog is the set of named tables the server serves. *db.Catalog implements
// it; a single-DB server wraps its DB as a one-table catalog.
type Catalog interface {
	// Table returns the named table's DB.
	Table(name string) (*db.DB, bool)
	// CreateTable creates (or returns the existing) named table.
	CreateTable(name string) (*db.DB, error)
	// CreateTableInMemory creates (or returns the existing) named table as RAM-only.
	CreateTableInMemory(name string) (*db.DB, error)
	// ConvertTableToMemory drops an existing table's on-disk backing, keeping its data in
	// RAM only. DAEMON-gated by the server (it changes a table's durability).
	ConvertTableToMemory(name string) error
	// DropTable removes the named table and its data.
	DropTable(name string) error
	// Tables lists the table names.
	Tables() []string
	// ArchiveTable / CreateArchiveTable / ArchiveTables manage append-only history
	// tables. A single-table server does not support them.
	ArchiveTable(name string) (*db.ArchiveTable, bool)
	CreateArchiveTable(name string, cfg db.ArchiveConfig) (*db.ArchiveTable, error)
	ArchiveTables() []string
}

// DefaultTable is the table name a single-DB server serves and the client
// targets when a table is not named -- the historical, single-collection view.
const DefaultTable = "ads"

// serverTxn is a live server-side transaction. Its mutex serializes operations on the
// (non-concurrent) *db.Txn even if a client pipelines them.
type serverTxn struct {
	tx    *db.Txn
	mu    sync.Mutex
	table string    // the transaction's table (from opBegin), for the propose hook
	batch []WriteOp // ops accumulated for the propose hook (nil unless propose is set)

	// conn owns this transaction; when that connection closes, its still-open
	// transactions are aborted (a client that drops mid-transaction -- e.g. a
	// transient network reset -- must not leak the server-side txn + its write-set).
	conn *serverConn
	// lastTouch is the unix-nano time of the last op on this txn, read by the idle
	// reaper to abort transactions abandoned on a still-open (half-open) connection.
	lastTouch atomic.Int64
}

// NewServer returns a single-table server over d, served as table "ads". The
// caller owns d's lifetime. Table create/drop are unsupported on this server.
func NewServer(d *db.DB) *Server { return &Server{cat: singleCatalog{name: DefaultTable, d: d}} }

// NewServerCatalog returns a multi-table server over cat.
func NewServerCatalog(cat Catalog) *Server { return &Server{cat: cat} }

// singleCatalog adapts one *db.DB to the Catalog interface as table "ads".
type singleCatalog struct {
	name string
	d    *db.DB
}

func (s singleCatalog) Table(name string) (*db.DB, bool) {
	if name == s.name {
		return s.d, true
	}
	return nil, false
}
func (s singleCatalog) CreateTable(name string) (*db.DB, error) {
	if name == s.name {
		return s.d, nil
	}
	return nil, fmt.Errorf("single-table server: cannot create table %q", name)
}
func (s singleCatalog) CreateTableInMemory(name string) (*db.DB, error) {
	return nil, fmt.Errorf("single-table server: cannot create in-memory tables")
}
func (s singleCatalog) ConvertTableToMemory(name string) error {
	return fmt.Errorf("single-table server: cannot convert tables")
}
func (s singleCatalog) DropTable(name string) error {
	return fmt.Errorf("single-table server: cannot drop tables")
}
func (s singleCatalog) Tables() []string { return []string{s.name} }

func (s singleCatalog) ArchiveTable(string) (*db.ArchiveTable, bool) { return nil, false }
func (s singleCatalog) CreateArchiveTable(string, db.ArchiveConfig) (*db.ArchiveTable, error) {
	return nil, fmt.Errorf("single-table server: archive tables unsupported")
}
func (s singleCatalog) ArchiveTables() []string { return nil }

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

	// Privileged admits the DAEMON-level administrative actions -- ALL of the admin
	// table (index/hot/compact/rewrite/codec.retrain, plus the security/durability ones
	// like the encryption toggle and truncate). These retune or restructure the store,
	// so an ordinary WRITE-level session (which may read and write ads) is refused them;
	// only a DAEMON peer sets this true. Read-only diagnostics are a separate opcode and
	// are not gated here.
	Privileged bool

	// QueryLog, if set, is called once per streamed query with a summary of what
	// the client asked for and what it cost. It is an opt-in query log for
	// operators: it makes visible, for example, a client that fetches every
	// attribute of every ad instead of projecting server-side (a slow, chatty
	// query pattern). Called from the connection's read-loop goroutine, so it must
	// not block.
	QueryLog func(QueryLog)
}

// QueryLog is one observed query, passed to ServeOptions.QueryLog.
type QueryLog struct {
	Op         string        // the query opcode: "Query", "QueryRaw", ...
	Table      string        // the queried table
	Constraint string        // the constraint expression ("" / "true" = match-all)
	Limit      int           // the client's LIMIT (0 = unlimited)
	Rows       int           // rows streamed back
	Duration   time.Duration // wall-clock time to run + stream the query
}

// isMutating reports whether o writes to a transaction (and so is refused on a
// read-only connection).
func (o op) isMutating() bool {
	switch o {
	case opNewAd, opDestroyAd, opSetAttr, opDeleteAttr, opAdmin, opCreateTable, opDropTable,
		opCreateTableMem, opTableToMemory,
		opArchiveCreate, opArchiveAppend, opArchiveRotate, opDeleteWhere, opCommitIdem:
		return true
	}
	return false
}

// defaultMaintenanceInterval is the floor cadence when StartMaintenance is given
// interval <= 0. The effective interval is raised above this whenever needed to keep the
// duty cycle under maintenanceMaxDutyCycle.
const defaultMaintenanceInterval = 15 * time.Minute

// maintenanceMaxDutyCycle caps the fraction of wall-clock time spent in maintenance: the
// interval before the next pass is at least (pass duration / this), so a pass that takes
// D never runs more often than every D/dutyCycle. At 1% a 30s pass paces to >=50 minutes.
const maintenanceMaxDutyCycle = 0.01

// StartMaintenance starts server-managed background maintenance: a single goroutine that,
// each tick, re-enumerates the catalog's tables and runs one Maintain pass (index
// auto-tune, hot-set refresh, and -- if opts.Retrain -- dictionary retrain) on each.
// Re-enumerating each tick means tables created after startup are maintained too (the old
// per-table start missed them).
//
// The cadence is adaptive: interval is a floor, but the wait before the next pass is
// raised to keep maintenance under maintenanceMaxDutyCycle of wall-clock time -- so on a
// large store where a pass (dominated by dictionary retrain's recompaction) takes minutes,
// passes automatically space out rather than consuming the daemon. Stopped by Close.
func (s *Server) StartMaintenance(interval time.Duration, opts db.MaintainOptions) {
	if interval <= 0 {
		interval = defaultMaintenanceInterval
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTimer(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			start := time.Now()
			for _, name := range s.cat.Tables() {
				if d, ok := s.cat.Table(name); ok {
					d.Maintain(opts)
				}
			}
			// Keep the duty cycle under the cap: wait at least pass/dutyCycle before the
			// next pass, never less than the configured floor.
			next := interval
			if paced := time.Duration(float64(time.Since(start)) / maintenanceMaxDutyCycle); paced > next {
				next = paced
			}
			t.Reset(next)
		}
	}()
	s.stopBG = append(s.stopBG, func() { close(done) })
}

// tableOr writes a "no such table" error under reqID and returns ok=false if the
// named table does not exist.
func (s *Server) tableOr(reqID uint64, name string, write func([]byte)) (*db.DB, bool) {
	d, ok := s.cat.Table(name)
	if !ok {
		write(respErr(reqID, "no such table: "+name))
	}
	return d, ok
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
	sc := &serverConn{s: s, ctx: ctx, opts: opts, watches: make(map[uint64]context.CancelFunc), txns: make(map[uint64]struct{})}
	// Abort any transactions still open when the connection ends, so a client that
	// drops mid-transaction (a transient reset, a crash, a cancelled context) does
	// not leak the server-side txn and its buffered write-set.
	defer sc.abortTxns()
	var wmu sync.Mutex
	sc.write = func(b []byte) {
		wmu.Lock()
		_ = conn.WriteMsg(b)
		wmu.Unlock()
	}
	for {
		frame, err := conn.ReadMsg()
		if err != nil {
			sc.abortRestore()
			return err
		}
		// Restore-upload frames are handled inline (synchronously) so their chunks are
		// spooled in receive order; every other op dispatches concurrently.
		if reqID, o, body, ok := reqHeader(frame); ok && isRestoreOp(o) {
			sc.handleRestore(reqID, o, body)
			continue
		}
		go sc.dispatch(frame)
	}
}

func isRestoreOp(o op) bool {
	return o == opRestore || o == opRestoreChunk || o == opRestoreEnd
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

	// tmu guards txns: the ids of transactions this connection opened and has not yet
	// committed or aborted, so they can be aborted when the connection closes.
	tmu  sync.Mutex
	txns map[uint64]struct{}

	// restore is the in-progress restore upload for this connection (at most one), spooled
	// to a temp file. Only touched from the single read-loop goroutine (restore frames are
	// handled inline), so it needs no lock. See snapshot.go.
	restore *restoreUpload
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
		sc.s.streamQuery(sc.ctx, reqID, body, priv, sc.write, sc.opts.QueryLog)
	case opQueryRaw:
		sc.s.streamQueryRaw(sc.ctx, reqID, body, priv, sc.write, sc.opts.QueryLog)
	case opQueryRawProj:
		sc.s.streamQueryRawProject(sc.ctx, reqID, body, priv, sc.write, sc.opts.QueryLog)
	case opMatchSorted:
		sc.s.streamMatchSorted(sc.ctx, reqID, body, priv, sc.write)
	case opOrdered:
		sc.s.streamOrdered(sc.ctx, reqID, body, priv, sc.write)
	case opAggregate:
		sc.s.streamAggregate(sc.ctx, reqID, body, priv, sc.write)
	case opMatchTables:
		sc.s.streamMatchTables(sc.ctx, reqID, body, sc.write)
	case opWatch:
		sc.streamWatch(reqID, body)
	case opSnapshot:
		sc.streamSnapshot(reqID, body)
	case opArchiveQuery:
		sc.streamArchiveQuery(reqID, body)
	case opWatchStop:
		sc.stopWatch(body.u64())
		sc.write(resp(reqID, stOK))
	default:
		sc.write(sc.s.handle(sc, reqID, o, body, priv, sc.opts.Privileged))
	}
}

// cancelled reports whether ctx is done -- checked in streaming loops so a long
// scan/match stops promptly when the client disconnects (the connection's context
// is cancelled when ServeConn returns).
func cancelled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
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
	table := r.str()
	cursor := append([]byte(nil), r.bytesRef()...)
	d, ok := sc.s.tableOr(reqID, table, sc.write)
	if !ok {
		return
	}
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

	seq, err := d.Watch(ctx, cursor)
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
func (s *Server) streamQuery(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	start := time.Now()
	table := r.str()
	limit := int(r.i32())
	constraint := r.str()
	n := 0
	if qlog != nil {
		defer func() {
			qlog(QueryLog{Op: "Query", Table: table, Constraint: constraint, Limit: limit, Rows: n, Duration: time.Since(start)})
		}()
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	d, ok := s.tableOr(reqID, table, write)
	if !ok {
		return
	}
	seq, err := d.Query(constraint)
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	// Push LIMIT down: stopping the range stops the underlying scan, so a small
	// LIMIT does proportionally less work instead of scanning everything.
	for ad := range seq {
		if cancelled(ctx) {
			return // client gone: stop the scan
		}
		write(putStr(respHead(reqID, stStream), adString(ad, includePrivate)))
		n++
		if limit > 0 && n >= limit {
			break
		}
	}
	write(respHead(reqID, stStreamEnd))
}

// streamMatchSorted streams job's ranked matches (best first, up to limit).
func (s *Server) streamMatchSorted(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte)) {
	_ = ctx
	table := r.str()
	limit := r.i32()
	jobText := r.str()
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	d, ok := s.tableOr(reqID, table, write)
	if !ok {
		return
	}
	job, err := classad.ParseOld(jobText)
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	for _, ad := range d.MatchSorted(job, int(limit)) {
		write(putStr(respHead(reqID, stStream), adString(ad, includePrivate)))
	}
	write(respHead(reqID, stStreamEnd))
}

// streamOrdered streams one partition of an ordered index in sort order, each ad with
// its cluster signature (for resource-request-list folding). One-shot: the in-memory
// resume cursor is not carried over the wire, so a full partition is streamed.
func (s *Server) streamOrdered(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte)) {
	_ = ctx
	table := r.str()
	index := r.i32()
	partition := r.str()
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	d, ok := s.tableOr(reqID, table, write)
	if !ok {
		return
	}
	for oa := range d.Ordered(int(index), partition, db.OrderCursor{}) {
		b := putU64(respHead(reqID, stStream), oa.Signature)
		b = putStr(b, adString(oa.Ad, includePrivate))
		write(b)
	}
	write(respHead(reqID, stStreamEnd))
}

// handle executes one request and returns its response frame. includePrivate
// controls whether ads returned by lookups carry their private attributes.
func (s *Server) handle(sc *serverConn, reqID uint64, o op, r *reader, includePrivate, privileged bool) []byte {
	switch o {
	case opBegin:
		table := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		d, ok := s.cat.Table(table)
		if !ok {
			return respErr(reqID, "no such table: "+table)
		}
		id := s.nextID.Add(1)
		st := &serverTxn{tx: d.Begin(), table: table, conn: sc}
		st.lastTouch.Store(nowNano())
		s.txns.Store(id, st)
		sc.addTxn(id)
		return putU64(resp(reqID, stOK), id)

	case opCreateTable:
		name := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		if _, err := s.cat.CreateTable(name); err != nil {
			return respErr(reqID, err.Error())
		}
		return resp(reqID, stOK)

	case opCreateTableMem:
		name := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		if _, err := s.cat.CreateTableInMemory(name); err != nil {
			return respErr(reqID, err.Error())
		}
		return resp(reqID, stOK)

	case opTableToMemory:
		name := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		// Converting a table's durability is a DAEMON-level administrative action, like the
		// admin() maintenance ops -- refused to an ordinary writer.
		if !privileged {
			return respErr(reqID, "ConvertTableToMemory requires DAEMON authorization")
		}
		if err := s.cat.ConvertTableToMemory(name); err != nil {
			return respErr(reqID, err.Error())
		}
		return resp(reqID, stOK)

	case opDropTable:
		name := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		if err := s.cat.DropTable(name); err != nil {
			return respErr(reqID, err.Error())
		}
		return resp(reqID, stOK)

	case opListTables:
		names := s.cat.Tables()
		b := putI32(resp(reqID, stOK), int32(len(names)))
		for _, n := range names {
			b = putStr(b, n)
		}
		return b

	case opWatchHead:
		table := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		d, ok := s.cat.Table(table)
		if !ok {
			return respErr(reqID, "no such table: "+table)
		}
		cursor, err := d.WatchCursor()
		if err != nil {
			return respErr(reqID, err.Error())
		}
		return putBytes(resp(reqID, stOK), cursor)

	case opCommit:
		id := r.u64()
		st, ok := s.take(id)
		if !ok {
			return respErr(reqID, "no such transaction")
		}
		sc.removeTxn(id)
		// Consistent-HA routing: propose the accumulated writes through consensus (raft)
		// instead of committing locally. The local transaction was only for read-your-
		// writes during the session; the propose hook (via the FSM) is the real writer.
		if s.propose != nil {
			st.tx.Abort()
			if err := s.propose(st.table, st.batch); err != nil {
				return respErr(reqID, err.Error())
			}
			return resp(reqID, stOK)
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

	case opCommitIdem:
		id := r.u64()
		idemKey := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		st, ok := s.take(id)
		if !ok {
			return respErr(reqID, "no such transaction")
		}
		sc.removeTxn(id)
		// Consensus routing has its own write path (the local txn is not the writer),
		// so the in-txn marker does not apply; fall back to a plain proposed commit.
		if s.propose != nil {
			st.tx.Abort()
			if err := s.propose(st.table, st.batch); err != nil {
				return respErr(reqID, err.Error())
			}
			return resp(reqID, stOK)
		}
		d, ok := s.cat.Table(st.table)
		if !ok {
			st.tx.Abort()
			return respErr(reqID, "no such table: "+st.table)
		}
		markerKey := idemMarkerKey(idemKey)
		// Replay of an already-committed unit of work: the marker is present, so the
		// prior attempt landed. Do not re-apply; report success.
		if _, exists := d.LookupClassAd(markerKey); exists {
			st.tx.Abort()
			return resp(reqID, stOK)
		}
		// Add the marker to this same transaction so it commits atomically with the
		// data (durable, survives restart) and doubles as the OCC guard: a concurrent
		// replay of the same unit of work conflicts on the marker key.
		marker := classad.New()
		marker.InsertAttr(idemMarkerAttr, time.Now().Unix())
		st.tx.NewClassAd(markerKey, marker)
		cerr := st.tx.Commit()
		if cerr == nil {
			return resp(reqID, stOK)
		}
		if ce, isConf := cerr.(*db.ConflictError); isConf {
			// A conflict on the marker key means a concurrent replay of the same unit
			// of work already committed -> exactly-once success, not a data conflict.
			for _, k := range ce.Keys {
				if k == markerKey {
					return resp(reqID, stOK)
				}
			}
			b := respHead(reqID, stConflict)
			for _, k := range ce.Keys {
				b = putStr(b, k)
			}
			return b
		}
		return respErr(reqID, cerr.Error())

	case opAbort:
		id := r.u64()
		if st, ok := s.take(id); ok {
			sc.removeTxn(id)
			st.tx.Abort()
		}
		return resp(reqID, stOK)

	case opNewAd:
		return s.withTxn(reqID, r, func(st *serverTxn) []byte {
			key, adText := r.str(), r.str()
			if r.err != nil {
				return respBad(reqID)
			}
			ad, err := classad.ParseOld(adText)
			if err != nil {
				return respErr(reqID, err.Error())
			}
			st.tx.NewClassAd(key, ad)
			st.record(s, WriteOp{Kind: WriteNewClassAd, Key: key, Value: adText})
			return resp(reqID, stOK)
		})

	case opDestroyAd:
		return s.withTxn(reqID, r, func(st *serverTxn) []byte {
			key := r.str()
			st.tx.DestroyClassAd(key)
			st.record(s, WriteOp{Kind: WriteDestroyClassAd, Key: key})
			return resp(reqID, stOK)
		})

	case opSetAttr:
		return s.withTxn(reqID, r, func(st *serverTxn) []byte {
			key, name, expr := r.str(), r.str(), r.str()
			if r.err != nil {
				return respBad(reqID)
			}
			if err := st.tx.SetAttribute(key, name, expr); err != nil {
				return respErr(reqID, err.Error())
			}
			st.record(s, WriteOp{Kind: WriteSetAttribute, Key: key, Name: name, Value: expr})
			return resp(reqID, stOK)
		})

	case opDeleteAttr:
		return s.withTxn(reqID, r, func(st *serverTxn) []byte {
			key, name := r.str(), r.str()
			if r.err != nil {
				return respBad(reqID)
			}
			st.tx.DeleteAttribute(key, name)
			st.record(s, WriteOp{Kind: WriteDeleteAttribute, Key: key, Name: name})
			return resp(reqID, stOK)
		})

	case opLookupAttr:
		return s.withTxn(reqID, r, func(st *serverTxn) []byte {
			key, name := r.str(), r.str()
			if r.err != nil {
				return respBad(reqID)
			}
			v, ok := st.tx.LookupAttr(key, name)
			if !ok {
				return resp(reqID, stMissing)
			}
			return putStr(resp(reqID, stOK), v)
		})

	case opLookupAd:
		return s.withTxn(reqID, r, func(st *serverTxn) []byte {
			ad, ok := st.tx.LookupClassAd(r.str())
			if !ok {
				return resp(reqID, stMissing)
			}
			return putStr(resp(reqID, stOK), adString(ad, includePrivate))
		})

	case opDiag:
		table := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		d, ok := s.cat.Table(table)
		if !ok {
			return respErr(reqID, "no such table: "+table)
		}
		data, err := s.diagJSON(d)
		if err != nil {
			return respErr(reqID, err.Error())
		}
		return putStr(resp(reqID, stOK), string(data))

	case opDeleteWhere:
		table := r.str()
		constraint := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		d, ok := s.cat.Table(table)
		if !ok {
			return respErr(reqID, "no such table: "+table)
		}
		removed, err := d.DeleteWhere(constraint)
		if err != nil {
			return respErr(reqID, err.Error())
		}
		return putI32(resp(reqID, stOK), int32(removed))

	case opExplain:
		table := r.str()
		constraint := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		d, ok := s.cat.Table(table)
		if !ok {
			return respErr(reqID, "no such table: "+table)
		}
		ex, err := d.Explain(constraint)
		if err != nil {
			return respErr(reqID, err.Error())
		}
		data, err := json.Marshal(ex)
		if err != nil {
			return respErr(reqID, err.Error())
		}
		return putStr(resp(reqID, stOK), string(data))

	case opMatchExplain:
		reqTable := r.str()
		selector := r.str()
		resTable := r.str()
		targetWhere := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		reqDB, ok := s.cat.Table(reqTable)
		if !ok {
			return respErr(reqID, "no such table: "+reqTable)
		}
		resDB, ok := s.cat.Table(resTable)
		if !ok {
			return respErr(reqID, "no such table: "+resTable)
		}
		seq, err := reqDB.Query(orTrue(selector))
		if err != nil {
			return respErr(reqID, err.Error())
		}
		var job *classad.ClassAd
		for ad := range seq {
			job = ad
			break // explain plans one specific request
		}
		if job == nil {
			return respErr(reqID, "no request ad matches "+selector)
		}
		data, err := json.Marshal(resDB.ExplainMatch(job, targetWhere))
		if err != nil {
			return respErr(reqID, err.Error())
		}
		return putStr(resp(reqID, stOK), string(data))

	case opAdmin:
		table := r.str()
		action := r.str()
		n := int(r.i32())
		if r.err != nil || n < 0 || n > 1024 {
			return respBad(reqID)
		}
		args := make([]string, n)
		for i := range args {
			args[i] = r.str()
		}
		if r.err != nil {
			return respBad(reqID)
		}
		d, ok := s.cat.Table(table)
		if !ok {
			return respErr(reqID, "no such table: "+table)
		}
		msg, err := s.admin(d, action, args, privileged)
		if err != nil {
			return respErr(reqID, err.Error())
		}
		return putStr(resp(reqID, stOK), msg)

	case opArchiveCreate:
		name := r.str()
		cfgJSON := r.bytesRef()
		if r.err != nil {
			return respBad(reqID)
		}
		var cfg db.ArchiveConfig
		if len(cfgJSON) > 0 {
			if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
				return respErr(reqID, "archive config: "+err.Error())
			}
		}
		if _, err := s.cat.CreateArchiveTable(name, cfg); err != nil {
			return respErr(reqID, err.Error())
		}
		return resp(reqID, stOK)

	case opArchiveAppend:
		name := r.str()
		adText := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		a, ok := s.cat.ArchiveTable(name)
		if !ok {
			return respErr(reqID, "no such archive: "+name)
		}
		if err := a.AppendOld(adText); err != nil {
			return respErr(reqID, err.Error())
		}
		return resp(reqID, stOK)

	case opArchiveList:
		names := s.cat.ArchiveTables()
		b := putI32(resp(reqID, stOK), int32(len(names)))
		for _, n := range names {
			b = putStr(b, n)
		}
		return b

	case opArchiveRotate:
		name := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		a, ok := s.cat.ArchiveTable(name)
		if !ok {
			return respErr(reqID, "no such archive: "+name)
		}
		dropped, err := a.Rotate(float64(time.Now().Unix()))
		if err != nil {
			return respErr(reqID, err.Error())
		}
		return putI32(resp(reqID, stOK), int32(dropped))
	}
	return respBad(reqID)
}

// withTxn resolves the leading txnID field, locks the transaction (serializing
// pipelined ops on it), and runs fn.
func (s *Server) withTxn(reqID uint64, r *reader, fn func(*serverTxn) []byte) []byte {
	id := r.u64()
	if r.err != nil {
		return respBad(reqID)
	}
	v, ok := s.txns.Load(id)
	if !ok {
		return respErr(reqID, "no such transaction")
	}
	st := v.(*serverTxn)
	st.lastTouch.Store(nowNano())
	st.mu.Lock()
	defer st.mu.Unlock()
	return fn(st)
}

// nowNano is the current time in unix nanoseconds, stamped on a transaction at each
// op so the idle reaper can measure inactivity.
func nowNano() int64 { return time.Now().UnixNano() }

// addTxn / removeTxn track the transactions this connection has open so they can be
// aborted if the connection closes with them still uncommitted.
func (sc *serverConn) addTxn(id uint64) {
	sc.tmu.Lock()
	sc.txns[id] = struct{}{}
	sc.tmu.Unlock()
}

func (sc *serverConn) removeTxn(id uint64) {
	sc.tmu.Lock()
	delete(sc.txns, id)
	sc.tmu.Unlock()
}

// abortTxns aborts every transaction still open on this connection. Called from the
// serve loop's defer, so a client that disconnects mid-transaction releases its
// server-side txn and write-set instead of leaking it. take() deduplicates against a
// concurrently-arriving commit/abort, so exactly one side wins.
func (sc *serverConn) abortTxns() {
	sc.tmu.Lock()
	ids := make([]uint64, 0, len(sc.txns))
	for id := range sc.txns {
		ids = append(ids, id)
	}
	sc.txns = make(map[uint64]struct{})
	sc.tmu.Unlock()
	for _, id := range ids {
		if st, ok := sc.s.take(id); ok {
			st.mu.Lock()
			st.tx.Abort()
			st.mu.Unlock()
		}
	}
}

// reapIdleTxns aborts transactions with no activity for at least maxIdle -- a
// backstop for a transaction abandoned on a connection that stays open (half-open
// TCP, a wedged client). Disconnect handles the common case; this handles the rest.
func (s *Server) reapIdleTxns(maxIdle time.Duration) int {
	cutoff := nowNano() - int64(maxIdle)
	reaped := 0
	s.txns.Range(func(k, v any) bool {
		st := v.(*serverTxn)
		if st.lastTouch.Load() > cutoff {
			return true
		}
		id := k.(uint64)
		if taken, ok := s.take(id); ok {
			taken.mu.Lock()
			taken.tx.Abort()
			taken.mu.Unlock()
			if taken.conn != nil {
				taken.conn.removeTxn(id)
			}
			reaped++
		}
		return true
	})
	return reaped
}

// StartTxnReaper runs reapIdleTxns every interval, aborting transactions idle longer
// than maxIdle, until the returned stop is called. Optional: disconnect cleanup is
// automatic; a server that wants to bound transactions abandoned on still-open
// connections starts this.
func (s *Server) StartTxnReaper(interval, maxIdle time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.reapIdleTxns(maxIdle)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// record appends a write op to the transaction's batch when the server routes commits
// through consensus (propose set); a no-op otherwise.
func (st *serverTxn) record(s *Server, op WriteOp) {
	if s.propose != nil {
		st.batch = append(st.batch, op)
	}
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
