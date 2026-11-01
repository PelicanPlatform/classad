package dbrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/collections/wire"
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
	// bgWG tracks the background maintenance goroutines StartMaintenance launched, so Close
	// can WAIT for an in-flight Maintain/Compact pass to finish -- not merely signal it to
	// stop -- before the caller closes the catalog. Signalling without waiting let cat.Close
	// munmap segments while a Compact/reindex pass was still reading them (SIGSEGV on restart).
	bgWG sync.WaitGroup

	// CloseGrace bounds how long Close waits for cancelled requests to unwind before it
	// gives up on them. Zero means defaultCloseGrace. Set it before serving connections.
	CloseGrace time.Duration

	// Logger receives what the server reports on its own, such as a request that panicked
	// (see containPanic). Nil uses slog.Default(). Set it before serving connections.
	Logger *slog.Logger

	// Shutdown barrier for the per-request goroutines ServeConnOpts dispatches. Those
	// goroutines read the catalog's segments, which the caller munmaps when it closes the
	// catalog after Close returns, so Close must not return while any of them is still
	// running. closing is set first, so the set waited on cannot grow while we wait on it;
	// inflight counts the requests in flight; conns holds the live connections, so a
	// request blocked writing to a client that stopped reading can be released by closing
	// its transport. shutMu also guards stopBG.
	shutMu   sync.Mutex
	closing  bool
	conns    map[*serverConn]MsgConn
	inflight sync.WaitGroup

	// closeOnce makes Close idempotent: a second call must not re-run the stop functions
	// (they close channels) nor wait a second grace period.
	closeOnce sync.Once
	// rootOnce guards the lazily built server context that every served connection derives
	// its own context from. Cancelling it ends every watch and stops every scan that checks
	// for cancellation -- the cancellation surface Close needs, since a request goroutine
	// is otherwise unreachable from the Server.
	rootOnce sync.Once
	rootCtx  context.Context
	rootStop context.CancelFunc

	// maintainOpts is the self-tuning configuration StartMaintenance was given; the on-demand
	// "analyze" admin action reuses it so a manual optimize matches the scheduled pass.
	// maintainMu serializes any Maintain pass (scheduled or on-demand) on this server so two
	// do not retune the same table at once.
	maintainOpts db.MaintainOptions
	maintainMu   sync.Mutex

	// propose, when set, routes a committing transaction's writes through an external
	// consensus layer (raft) instead of committing them to the local store: on opCommit
	// the accumulated ops are handed to propose, which is the store's sole writer. Set by
	// the consistent-HA daemon on the leader; nil for a standalone store. See propose.go.
	propose ProposeFunc

	// qstats aggregates served queries by shape (EnableQueryStats); nil when the server
	// is not aggregating. Held in an atomic pointer because it is read on every query
	// and may be enabled after connections are already being served.
	qstats atomic.Pointer[queryStats]
}

// EnableQueryStats starts (or reconfigures and restarts) aggregation of the queries this
// server serves: each one is bucketed by its shape -- opcode, table, constraint
// fingerprint and whether a LIMIT was sent -- with the slowest actual instances of each
// shape kept as exemplars. A client reads the result with Client.QueryStats; exemplars
// are returned only on a DAEMON-authorized connection, since they carry real constraint
// text and the requesting identity.
//
// Recording happens on the goroutine that served the query, after its last frame went
// out, and costs one constraint parse plus a map update under a mutex.
func (s *Server) EnableQueryStats(opts QueryStatsOptions) {
	s.qstats.Store(newQueryStats(opts))
}

// DisableQueryStats stops aggregation and discards what was collected.
func (s *Server) DisableQueryStats() { s.qstats.Store(nil) }

// queryStatsOn reports whether aggregation is running. The query ops consult it to decide
// whether to collect a scan breakdown they would not otherwise compute.
func (s *Server) queryStatsOn() bool { return s.qstats.Load() != nil }

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
	// CreateView / DropView / Views manage materialized views; ViewBacking returns a view's
	// in-memory backing so reads resolve a view name to its materialized rows.
	CreateView(name string, spec db.ViewSpec) error
	DropView(name string) error
	Views() []string
	ViewBacking(name string) (*db.DB, bool)
	// Exporter registry: passive storage of external-sink definitions and their opaque
	// resume state. The server never runs exporters; it only persists these for the
	// out-of-process exporter that reads them.
	CreateExporter(def db.ExporterDef) error
	DropExporter(name string) error
	Exporters() []db.ExporterDef
	Exporter(name string) (db.ExporterDef, bool)
	SaveExporterState(name string, state []byte) error
	LoadExporterState(name string) ([]byte, bool, error)
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
	// wrote is set (under mu) once an ad write has been accepted into the transaction,
	// so Commit can refuse to apply writes to a table that stopped being writable after
	// they were buffered (ServeOptions.TableWritable).
	wrote bool

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
func (s singleCatalog) CreateView(name string, spec db.ViewSpec) error {
	return fmt.Errorf("single-table server: materialized views unsupported")
}
func (s singleCatalog) DropView(name string) error {
	return fmt.Errorf("single-table server: materialized views unsupported")
}
func (s singleCatalog) Views() []string                   { return nil }
func (s singleCatalog) ViewBacking(string) (*db.DB, bool) { return nil, false }
func (s singleCatalog) Tables() []string                  { return []string{s.name} }

func (s singleCatalog) ArchiveTable(string) (*db.ArchiveTable, bool) { return nil, false }
func (s singleCatalog) CreateArchiveTable(string, db.ArchiveConfig) (*db.ArchiveTable, error) {
	return nil, fmt.Errorf("single-table server: archive tables unsupported")
}
func (s singleCatalog) ArchiveTables() []string { return nil }

func (s singleCatalog) CreateExporter(db.ExporterDef) error {
	return fmt.Errorf("single-table server: exporters unsupported")
}
func (s singleCatalog) DropExporter(string) error              { return nil }
func (s singleCatalog) Exporters() []db.ExporterDef            { return nil }
func (s singleCatalog) Exporter(string) (db.ExporterDef, bool) { return db.ExporterDef{}, false }
func (s singleCatalog) SaveExporterState(string, []byte) error {
	return fmt.Errorf("single-table server: exporters unsupported")
}
func (s singleCatalog) LoadExporterState(string) ([]byte, bool, error) { return nil, false, nil }

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

	// TableWritable, if non-nil, is consulted for every request that would modify a
	// named table; returning false refuses it as if the connection were read-only for
	// that table alone, with an error the client sees as ErrTableReadOnly. A server
	// whose table is owned by an in-process writer (a mirror of another store) uses it
	// so no remote peer -- Privileged included -- can change that table underneath
	// the writer. Reads, watches, snapshots and read-only transactions on the table
	// are unaffected. Nil means every table is writable (the historical behavior).
	//
	// Gated: ad writes in a transaction on the table and a Commit that carries them,
	// CommitIdempotent (it writes a marker), DeleteWhere, ArchiveAppend, ArchiveRotate,
	// Create/Drop of a table, archive or view by that name, ConvertTableToMemory,
	// Restore, and the admin actions that remove data or change what is kept
	// (truncate, rotate, retention.set). Not gated: layout and tuning admin actions
	// that preserve every row (index.*, hot.*, compact, rewrite, codec.retrain,
	// schema.*, analyze, encrypt.set, timetravel.*, backup.key), and exporter ops,
	// which name no table. See tableWriteTarget.
	//
	// Called from request goroutines concurrently; it must be safe for that and must
	// not block. It may change its answer over time: a Commit re-checks it, so writes
	// buffered while a table was writable do not land after it stops being so.
	TableWritable func(table string) bool

	// QueryLog, if set, is called once per streamed query with a summary of what
	// the client asked for and what it cost. It is an opt-in query log for
	// operators: it makes visible, for example, a client that fetches every
	// attribute of every ad instead of projecting server-side (a slow, chatty
	// query pattern). Called on the goroutine that served the query, once its last
	// frame has been written -- so it delays nothing the client is waiting for, but it
	// does hold a serving goroutine and must not block.
	QueryLog func(QueryLog)

	// Peer names the authenticated identity on the other end of this connection, for
	// diagnostics only: it is stamped into QueryLog.User, so a per-query log line and a
	// slow-query exemplar can say WHO ran the expensive query. Never used for access
	// control -- that is ReadOnly/IncludePrivate/Privileged.
	Peer string
}

// QueryLog is one observed query, passed to ServeOptions.QueryLog.
type QueryLog struct {
	Op         string        // the query opcode: "Query", "QueryRaw", ...
	Table      string        // the queried table
	Constraint string        // the constraint expression ("" / "true" = match-all)
	Limit      int           // the client's LIMIT (0 = unlimited)
	Rows       int           // rows streamed back
	Duration   time.Duration // wall-clock time to run + stream the query

	// Projection is how many attributes the client asked the server to return; 0 means
	// it asked for no projection at all, so every attribute of every matched ad crossed
	// the wire. That is the chatty pattern this log exists to make visible, and without
	// this field it was the one thing the log could not show.
	Projection int

	// Scan is the server-side work breakdown for the query -- notably segments scanned
	// versus pruned, which separates "slow because the window is big" from "slow because
	// nothing pruned". It is filled on the read paths that thread it (the projection,
	// wire and top-K ops); elsewhere it is zero, which means NOT MEASURED rather than
	// no work.
	Scan collections.ScanStats

	// User is the connection's authenticated identity (ServeOptions.Peer), or empty when
	// the front end did not name one.
	User string

	// Groups and Aggs describe an AGGREGATE query: the GROUP BY columns (each with its
	// bucket width, where it has one) and the functions reduced over each group. Both are
	// empty on a plain read.
	//
	// They are recorded because grouping decides the work rather than merely the output.
	// A single raw numeric group column is answered from the columnar blocks; the same
	// WHERE grouped by an expression, or by a column the accelerator does not cover,
	// falls through to rebuilding every matched record. Those are the same constraint and
	// orders of magnitude apart, so a report that logged only the constraint would rank
	// them as one shape and name neither.
	//
	// Note that Rows counts GROUPS for these -- a handful, however many records were read
	// to produce them. Scan carries the cost.
	Groups []GroupCol
	Aggs   []AggSpec
}

// isMutating reports whether o writes to a transaction (and so is refused on a
// read-only connection).
func (o op) isMutating() bool {
	switch o {
	case opNewAd, opNewAdBatch, opDestroyAd, opSetAttr, opDeleteAttr, opAdmin, opCreateTable, opDropTable,
		opCreateTableMem, opTableToMemory, opCreateView, opDropView,
		opCreateExporter, opDropExporter, opPutExporterState,
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

// defaultCompactInterval is the cadence of the standalone compaction pass when
// MaintainOptions.CompactInterval is 0. It is far shorter than the maintenance
// (retrain) cadence on purpose: compaction is cheap and self-limiting, and a
// high-churn table accumulates dead space continuously, so reclaiming it must not
// wait on the throttled retrain-dominated Maintain pass.
const defaultCompactInterval = 2 * time.Minute

// compactPassHook, when non-nil, is invoked at the start of each background compaction pass.
// Test-only (nil in production); a test uses it to hold a pass in flight and prove Close waits.
var compactPassHook func()

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
	s.maintainOpts = opts // reused by the on-demand "analyze" admin action
	done := make(chan struct{})
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
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
				// Between tables: a stop signalled mid-pass should not still have to walk
				// every remaining table before the loop notices it at the top.
				select {
				case <-done:
					return
				default:
				}
				if d, ok := s.cat.Table(name); ok {
					s.maintainMu.Lock()
					d.Maintain(opts)
					s.maintainMu.Unlock()
				}
			}
			select {
			case <-done:
				return
			default:
			}
			s.maintainArchives(opts, done)
			// Keep the duty cycle under the cap: wait at least pass/dutyCycle before the
			// next pass, never less than the configured floor.
			next := interval
			if paced := time.Duration(float64(time.Since(start)) / maintenanceMaxDutyCycle); paced > next {
				next = paced
			}
			t.Reset(next)
		}
	}()
	s.addStopBG(func() { close(done) })

	// Standalone compaction pass, decoupled from the retrain-dominated Maintain
	// pass above. Compaction is cheap and self-limiting (Compact unlinks fully-dead
	// segments for free and only recompacts shards past the dead-byte threshold), so
	// it runs on a short fixed cadence -- a high-churn table reclaims dead space
	// continuously instead of only when the throttled retrain happens to run. A
	// negative CompactInterval disables it; 0 uses defaultCompactInterval.
	if opts.CompactInterval >= 0 {
		compactEvery := opts.CompactInterval
		if compactEvery == 0 {
			compactEvery = defaultCompactInterval
		}
		cdone := make(chan struct{})
		s.bgWG.Add(1)
		go func() {
			defer s.bgWG.Done()
			ct := time.NewTimer(compactEvery)
			defer ct.Stop()
			for {
				select {
				case <-cdone:
					return
				case <-ct.C:
				}
				if compactPassHook != nil {
					compactPassHook()
				}
				for _, name := range s.cat.Tables() {
					if d, ok := s.cat.Table(name); ok {
						d.Compact()
					}
				}
				ct.Reset(compactEvery) // reset after the pass so a slow pass never overlaps
			}
		}()
		s.addStopBG(func() { close(cdone) })
	}
}

// tableOr resolves the table op o names, writing the refusal under reqID and returning
// ok=false when o cannot serve it.
//
// It serves a mutable table and a materialized view (through its in-memory backing). Only
// reads reach tableOr; writes (opBegin) resolve through cat.Table only, so a view stays
// read-only.
//
// An archive gets a refusal that says it is an archive and why o does not read one. It used
// to get "no such table", which is how a history table that plainly existed came to deny
// its own existence to half the read surface -- and why a bug on one of those paths could
// sit unseen, since the path was unreachable for the only table that exercises it.
func (s *Server) tableOr(reqID uint64, o op, name string, write func([]byte)) (*db.DB, bool) {
	ref, ok := s.tableRefOr(reqID, o, name, write)
	return ref.db, ok
}

// tableRefOr is tableOr for a handler that needs more than the backing -- in practice, one
// that must also reach a view's sealed half. A handler taking only ref.db from this is
// taking exactly what tableOr gives, and should use tableOr.
func (s *Server) tableRefOr(reqID uint64, o op, name string, write func([]byte)) (tableRef, bool) {
	switch ref := s.resolveTable(name); ref.kind {
	case kindTable, kindView:
		return ref, true
	case kindArchive:
		write(respArchiveTable(reqID, name, archiveRefusal(o)))
	default:
		write(respErr(reqID, "no such table: "+name))
	}
	return tableRef{}, false
}

// defaultCloseGrace bounds, by default, how long Close waits for cancelled in-flight
// requests before giving up on them. Long enough for a scan to notice cancellation and
// for a client that is still reading to drain a stream's last frames; short enough that a
// shutdown is not visibly stalled by one wedged peer.
const defaultCloseGrace = 5 * time.Second

// root returns the server context every served connection derives from, building it on
// first use so a Server made by either constructor (or, in tests, by a struct literal)
// has one.
func (s *Server) root() context.Context {
	s.rootOnce.Do(func() { s.rootCtx, s.rootStop = context.WithCancel(context.Background()) })
	return s.rootCtx
}

// addStopBG registers a background loop's stop function for Close to call.
func (s *Server) addStopBG(stop func()) {
	s.shutMu.Lock()
	s.stopBG = append(s.stopBG, stop)
	s.shutMu.Unlock()
}

// beginRequest admits one request to the shutdown barrier, reporting false once Close has
// started: after that the server takes no new work, so the set of in-flight requests Close
// waits on cannot grow while it waits.
func (s *Server) beginRequest() bool {
	s.shutMu.Lock()
	defer s.shutMu.Unlock()
	if s.closing {
		return false
	}
	s.inflight.Add(1)
	return true
}

func (s *Server) endRequest() { s.inflight.Done() }

// addConn registers a connection being served, so Close can close its transport as a
// backstop. It reports false once Close has started, so a connection handed to the server
// after that is refused rather than served by a stopped server.
func (s *Server) addConn(sc *serverConn, conn MsgConn) bool {
	s.shutMu.Lock()
	defer s.shutMu.Unlock()
	if s.closing {
		return false
	}
	if s.conns == nil {
		s.conns = make(map[*serverConn]MsgConn)
	}
	s.conns[sc] = conn
	return true
}

func (s *Server) removeConn(sc *serverConn) {
	s.shutMu.Lock()
	delete(s.conns, sc)
	s.shutMu.Unlock()
}

// waitFor waits for wg, reporting false if d elapses first.
func waitFor(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// Close stops the server's managed goroutines and the requests it is serving. It does not
// close the DB; the caller does that afterwards, which is why Close must leave nothing
// running that still reads the catalog.
//
// It takes no new work, stops and waits for background maintenance, cancels every served
// connection (ending its watches and stopping its scans) and then waits up to CloseGrace
// for the in-flight requests to unwind. It closes the connections' transports, which both
// tells the peers the server is gone and releases a request blocked writing to a client
// that stopped reading -- the one case cancellation cannot reach, since no context reaches
// a blocked WriteMsg. A request still running when the budget runs out is abandoned: Close
// returns anyway rather than hang, so closing the catalog can still race it.
//
// Close is idempotent. A request that completes on its own is unaffected.
func (s *Server) Close() { s.closeOnce.Do(s.close) }

func (s *Server) close() {
	// Take no new work first: a request admitted after this point would be one more thing
	// to wait for, arriving while we wait.
	s.shutMu.Lock()
	s.closing = true
	stops := s.stopBG
	s.stopBG = nil
	conns := make([]MsgConn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.shutMu.Unlock()

	for _, stop := range stops {
		stop()
	}
	// Closing the channels above only stops a loop BETWEEN passes. A pass already running
	// keeps going, and a bounded-but-not-short one (the columnar rewrite budget is 64
	// segments) then holds the wait below for tens of seconds on an archive at history
	// scale -- a process that sits there after its logs say it stopped. Ask the stores to
	// stop at their next safe boundary so the pass unwinds instead of running to completion.
	for _, name := range s.cat.Tables() {
		if d, ok := s.cat.Table(name); ok {
			d.StopMaintenance()
		}
	}
	for _, name := range s.cat.ArchiveTables() {
		if a, ok := s.cat.ArchiveTable(name); ok {
			a.StopMaintenance()
		}
	}
	// Wait for the signalled goroutines to actually return -- including finishing whatever
	// unit of work is in flight -- before returning, so the caller can safely close the
	// catalog (munmap segments) without racing a Compact/Maintain still reading them.
	s.bgWG.Wait()

	// The same hazard, from the other side: every request runs on its own goroutine, and a
	// watch streams until it is cancelled. Cancel the served connections, then wait for
	// their requests to unwind.
	s.root()
	s.rootStop()
	grace := s.CloseGrace
	if grace <= 0 {
		grace = defaultCloseGrace
	}
	drained := waitFor(&s.inflight, grace/2)
	// Close the transports either way: the server has stopped serving them, so a peer
	// should see the connection drop rather than wait for an answer that will never come,
	// and the one kind of request cancellation cannot reach -- one blocked writing to a
	// client that stopped reading, where no context reaches a blocked WriteMsg -- is
	// released only by failing its transport. This also ends the connections' read loops.
	for _, c := range conns {
		_ = c.Close()
	}
	if !drained {
		waitFor(&s.inflight, grace-grace/2) // and if it STILL has not unwound, abandon it
	}
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
	// Derived from the server context, not from Background: Close cancels that one, which
	// is how it reaches this connection's watches and scans.
	ctx, cancel := context.WithCancel(s.root())
	defer cancel() // stop this connection's watches when it closes
	sc := &serverConn{s: s, ctx: ctx, opts: opts, watches: make(map[uint64]context.CancelFunc), txns: make(map[uint64]struct{})}
	if !s.addConn(sc, conn) {
		return ErrServerClosed
	}
	defer s.removeConn(sc)
	sc.qlog = s.queryLogger(opts)
	// Abort any transactions still open when the connection ends, so a client that
	// drops mid-transaction (a transient reset, a crash, a cancelled context) does
	// not leak the server-side txn and its buffered write-set.
	defer sc.abortTxns()
	var wmu sync.Mutex
	sc.write = func(b []byte) {
		wmu.Lock()
		// Deferred so a panic in the transport cannot leave the lock held: containPanic writes
		// the request's error through this same function, and every other request on the
		// connection needs it too.
		defer wmu.Unlock()
		if h := writeHook.Load(); h != nil {
			(*h)(b)
		}
		_ = conn.WriteMsg(b)
	}
	for {
		frame, err := conn.ReadMsg()
		if err != nil {
			sc.abortRestore()
			return err
		}
		// Every request, inline or dispatched, joins the shutdown barrier before it starts
		// touching the catalog; once Close has begun, the frame is not served at all.
		if !s.beginRequest() {
			sc.abortRestore()
			return ErrServerClosed
		}
		// Restore-upload frames are handled inline (synchronously) so their chunks are
		// spooled in receive order; every other op dispatches concurrently.
		if reqID, o, body, ok := reqHeader(frame); ok && isRestoreOp(o) {
			sc.handleRestore(reqID, o, body)
			s.endRequest()
			continue
		}
		go func() {
			defer s.endRequest()
			sc.dispatch(frame)
		}()
	}
}

// queryLogger builds the per-connection query observer: it stamps the peer identity onto
// each record, feeds the server's shape aggregation when that is enabled, and then calls
// the caller's hook. It is always non-nil so that enabling aggregation reaches connections
// that are already open.
func (s *Server) queryLogger(opts ServeOptions) func(QueryLog) {
	hook := opts.QueryLog
	peer := opts.Peer
	return func(q QueryLog) {
		q.User = peer
		if qs := s.qstats.Load(); qs != nil {
			qs.record(q)
		}
		if hook != nil {
			hook(q)
		}
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

	// qlog is the per-query observer for this connection: it feeds the server's shape
	// aggregation and then the caller's own ServeOptions.QueryLog hook. Built once per
	// connection so the query ops need not know about either.
	qlog func(QueryLog)

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
	if o.recoverable() {
		defer sc.containPanic(reqID, o)
	}
	if h := dispatchHook.Load(); h != nil {
		(*h)(o)
	}
	if sc.opts.ReadOnly && o.isMutating() {
		sc.write(respErr(reqID, "read-only connection: "+o.String()+" not permitted"))
		return
	}
	if sc.opts.TableWritable != nil {
		if table, what, ok := sc.tableWriteTarget(o, body); ok && !sc.opts.TableWritable(table) {
			sc.write(respTableReadOnly(reqID, table, what))
			return
		}
	}
	priv := sc.opts.IncludePrivate
	switch o {
	case opQuery:
		sc.s.streamQuery(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opQueryAsOf:
		sc.s.streamQueryAsOf(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opQueryRaw:
		sc.s.streamQueryRaw(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opQueryRawProj:
		sc.s.streamQueryRawProject(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opQueryRawProjSeq:
		sc.s.streamQueryRawProjectSeq(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opQueryRawProjRefs:
		sc.s.streamQueryRawProjectRefs(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opQueryRawProjRefsStats:
		sc.s.streamQueryRawProjectRefsStats(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opTopK:
		sc.s.streamTopK(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opTopKStats:
		sc.s.streamTopKStats(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opQueryRawWire:
		sc.s.streamQueryRawWire(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opQueryRawWireOpts:
		sc.s.streamQueryRawWireOpts(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opJoin:
		sc.s.streamJoin(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opMatchSorted:
		sc.s.streamMatchSorted(sc.ctx, reqID, body, priv, sc.write)
	case opOrdered:
		sc.s.streamOrdered(sc.ctx, reqID, body, priv, sc.write)
	case opAggregate:
		sc.s.streamAggregate(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opAggregateFiltered:
		sc.s.streamAggregateFiltered(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opAggregateBucketed:
		sc.s.streamAggregateBucketed(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opMatchTables:
		sc.s.streamMatchTables(sc.ctx, reqID, body, priv, sc.write)
	case opWatch:
		sc.streamWatch(reqID, body, false)
	case opWatchWire:
		sc.streamWatch(reqID, body, true)
	case opSnapshot:
		sc.streamSnapshot(reqID, body)
	case opArchiveQuery:
		sc.streamArchiveQuery(reqID, body)
	case opArchiveAggregate:
		sc.streamArchiveAggregate(reqID, body)
	case opArchiveAggregateFiltered:
		sc.streamArchiveAggregateFiltered(reqID, body)
	case opArchiveAggregateStats:
		sc.streamArchiveAggregateStats(reqID, body)
	case opQueryKeys:
		sc.s.streamQueryKeys(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opTxnQuery:
		sc.s.streamTxnQuery(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
	case opTxnQueryKeys:
		sc.s.streamTxnQueryKeys(sc.ctx, reqID, body, priv, sc.write, sc.qlog)
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

// streamWatch runs a watch, streaming each event as a frame [kind u8][key][ad][cursor]
// under reqID -- the ad as ClassAd text, or as wire bytes when wireForm is set (opWatchWire), until the client cancels it (opWatchStop) or the connection
// closes. cursor empty starts from now.
func (sc *serverConn) streamWatch(reqID uint64, r *reader, wireForm bool) {
	table := r.str()
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

	// One watch op serves all three kinds, each forking on privilege the way a read does: an
	// unprivileged peer's watch decodes with no key, so a sealed value is never opened for it.
	var seq iter.Seq[db.WatchEvent]
	var err error
	priv := sc.opts.IncludePrivate
	switch ref := sc.s.resolveTable(table); ref.kind {
	case kindTable, kindView:
		if priv {
			seq, err = ref.db.Watch(ctx, cursor)
		} else {
			seq, err = ref.db.WatchRedacted(ctx, cursor)
		}
	case kindArchive:
		if priv {
			seq, err = ref.arch.Watch(ctx, cursor)
		} else {
			seq, err = ref.arch.WatchRedacted(ctx, cursor)
		}
	default:
		sc.write(respErr(reqID, "no such table: "+table))
		return
	}
	if err != nil {
		sc.write(respErr(reqID, err.Error()))
		return
	}
	for ev := range seq {
		b := putU8(respHead(reqID, stStream), byte(ev.Kind))
		b = putStr(b, ev.Key)
		if wireForm {
			b = putBytes(b, watchAdWire(ev.Ad, sc.opts.IncludePrivate))
		} else if ev.Ad != nil {
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

// viewSealer is the optional Catalog capability to read a continuous aggregate's sealed
// (archived) history. A catalog that has it (the multi-table db.Catalog) lets a view read
// union sealed buckets with the live backing; catalogs without it (single-table) simply
// don't, so this is additive and non-breaking.
type viewSealer interface {
	ViewSealed(name, constraint string) (iter.Seq[*classad.ClassAd], bool, error)
}

// streamQuery streams the committed ads matching a constraint. Each result is its own
// frame under reqID, so its frames interleave with other calls' -- no head-of-line
// blocking -- and end with a terminator.
// streamQueryKeys streams the storage keys of the rows matching a constraint (not their ads), so a
// caller can address matched rows for UPDATE/DELETE by their real db key regardless of any
// self-reported key attribute. Read-only; no private-attribute exposure (keys are returned, not ad
// bodies). The constraint is still evaluated server-side, so it may reference any attribute.
func (s *Server) streamQueryKeys(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	start := time.Now()
	table := r.str()
	constraint := r.str()
	n := 0
	// Returning keys rather than ads makes this cheap to SEND, not cheap to RUN: the
	// constraint is evaluated over the same rows either way, and a caller that walks a
	// large table by key pays for the scan every time.
	if qlog != nil {
		defer func() {
			qlog(QueryLog{Op: "QueryKeys", Table: table, Constraint: constraint,
				Rows: n, Duration: time.Since(start)})
		}()
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	// Which KEYS match a private-attribute predicate leaks it exactly as the ads would.
	if refusePrivateConstraint(reqID, constraint, includePrivate, write) {
		return
	}
	d, ok := s.tableOr(reqID, opQueryKeys, table, write)
	if !ok {
		return
	}
	keysWhere := d.KeysWhere
	if !includePrivate {
		keysWhere = d.KeysWhereRedacted // matched with no key, like the ads a query would return
	}
	seq, err := keysWhere(constraint)
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	for key := range seq {
		if cancelled(ctx) {
			return // client gone: stop the scan
		}
		write(putStr(respHead(reqID, stStream), key))
		n++
	}
	write(respHead(reqID, stStreamEnd))
}

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
	if refusePrivateConstraint(reqID, constraint, includePrivate, write) {
		return
	}
	d, ok := s.tableOr(reqID, opQuery, table, write)
	if !ok {
		return
	}
	// An unprivileged session reads with NO key, so a sealed attribute arrives undefined instead of
	// being decrypted here and dropped by the serializer afterwards (see db.QueryRedacted).
	var seq iter.Seq[*classad.ClassAd]
	var err error
	if includePrivate {
		seq, err = d.Query(constraint)
	} else {
		seq, err = d.QueryRedacted(constraint)
	}
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	// A continuous aggregate's sealed history lives in its archive; resolve it up front
	// (so a bad constraint errors before any frame) and union it after the live backing, so
	// a read of the view returns the full series, not just the unsealed buckets.
	var sealed iter.Seq[*classad.ClassAd]
	if vs, ok := s.cat.(viewSealer); ok {
		var serr error
		if sq, has, e := vs.ViewSealed(table, constraint); e != nil {
			serr = e
		} else if has {
			sealed = sq
		}
		if serr != nil {
			write(respErr(reqID, serr.Error()))
			return
		}
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
	if sealed != nil && (limit <= 0 || n < limit) {
		for ad := range sealed {
			if cancelled(ctx) {
				return
			}
			write(putStr(respHead(reqID, stStream), adString(ad, includePrivate)))
			n++
			if limit > 0 && n >= limit {
				break
			}
		}
	}
	write(respHead(reqID, stStreamEnd))
}

// streamQueryAsOf is streamQuery for a point-in-time ("AS OF") query: it reads a
// wall-clock instant (unix nanos) and streams the ads that matched the constraint as
// they were then. It errors cleanly if time travel is disabled or the instant is
// outside the retained window.
func (s *Server) streamQueryAsOf(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	start := time.Now()
	table := r.str()
	limit := int(r.i32())
	asOf := time.Unix(0, int64(r.u64()))
	constraint := r.str()
	n := 0
	if qlog != nil {
		defer func() {
			qlog(QueryLog{Op: "QueryAsOf", Table: table, Constraint: constraint, Limit: limit, Rows: n, Duration: time.Since(start)})
		}()
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	if refusePrivateConstraint(reqID, constraint, includePrivate, write) {
		return
	}
	d, ok := s.tableOr(reqID, opQueryAsOf, table, write)
	if !ok {
		return
	}
	queryAsOf := d.QueryAsOf
	if !includePrivate {
		queryAsOf = d.QueryAsOfRedacted // matched, and decoded, with no key
	}
	seq, err := queryAsOf(constraint, asOf)
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	for ad := range seq {
		if cancelled(ctx) {
			return
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
	d, ok := s.tableOr(reqID, opMatchSorted, table, write)
	if !ok {
		return
	}
	job, err := classad.ParseOld(jobText)
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	// An unprivileged session matches against the slots as it is shown them: with no key, a sealed
	// value undefined to both Requirements and the Rank, and absent from the ads returned.
	matchSorted := d.MatchSorted
	if !includePrivate {
		matchSorted = d.MatchSortedRedacted
	}
	for _, ad := range matchSorted(job, int(limit)) {
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
	d, ok := s.tableOr(reqID, opOrdered, table, write)
	if !ok {
		return
	}
	ordered := d.Ordered
	if !includePrivate {
		// An ordered index is maintained on the write path, with the key: membership, partition,
		// order and cluster signature are all computed from the plaintext. Redacting the ads it
		// returns does not undo that -- probing a partition by value, or reading the order, answers
		// for any sealed value the spec reads -- and which values those are depends on each record,
		// not on the spec. A table that seals nothing has nothing to withhold.
		if d.SealsValues() {
			write(respErr(reqID, "ordered index reads require DAEMON authorization on a table that "+
				"seals values: the index is maintained from the sealed plaintext"))
			return
		}
		ordered = d.OrderedRedacted // read with no key rather than decrypt-then-filter
	}
	for oa := range ordered(int(index), partition, db.OrderCursor{}) {
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
		// The transaction carries the session's entitlement, so every read through it -- opLookupAd,
		// opTxnQuery -- reads with no key for an unprivileged session. Its SetAttribute and
		// DeleteAttribute change only the attribute named and never open a sealed value (see
		// db.DB.BeginRedacted).
		begin := d.Begin
		if !sc.opts.IncludePrivate {
			begin = d.BeginRedacted
		}
		st := &serverTxn{tx: begin(), table: table, conn: sc}
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
		// Mutable tables AND archives, merged into one sorted list. They are one name
		// space -- the catalog refuses a name the other kind holds -- so a client that
		// sees a name here can address it without first asking which kind it is, and the
		// merge introduces no duplicates. Listing only the mutable half made every archive
		// invisible to the op whose whole job is to say what exists, so each client grew
		// its own second call and its own concatenation.
		//
		// Views stay out: they have their own op (opListViews) and their own lifecycle.
		names := mergeSorted(s.cat.Tables(), s.cat.ArchiveTables())
		b := putI32(resp(reqID, stOK), int32(len(names)))
		for _, n := range names {
			b = putStr(b, n)
		}
		return b

	case opCreateView:
		name := r.str()
		specJSON := r.bytesRef()
		if r.err != nil {
			return respBad(reqID)
		}
		var spec db.ViewSpec
		if err := json.Unmarshal(specJSON, &spec); err != nil {
			return respErr(reqID, "view spec: "+err.Error())
		}
		if err := s.cat.CreateView(name, spec); err != nil {
			return respErr(reqID, err.Error())
		}
		return resp(reqID, stOK)

	case opDropView:
		name := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		if err := s.cat.DropView(name); err != nil {
			return respErr(reqID, err.Error())
		}
		return resp(reqID, stOK)

	case opListViews:
		names := s.cat.Views()
		b := putI32(resp(reqID, stOK), int32(len(names)))
		for _, n := range names {
			b = putStr(b, n)
		}
		return b

	case opCreateExporter:
		defJSON := r.bytesRef()
		if r.err != nil {
			return respBad(reqID)
		}
		// Registering an exporter is a DAEMON-level administrative action: its config may
		// carry broker credentials and it drives data off-box.
		if !privileged {
			return respErr(reqID, "CreateExporter requires DAEMON authorization")
		}
		var def db.ExporterDef
		if err := json.Unmarshal(defJSON, &def); err != nil {
			return respErr(reqID, "exporter def: "+err.Error())
		}
		if err := s.cat.CreateExporter(def); err != nil {
			return respErr(reqID, err.Error())
		}
		return resp(reqID, stOK)

	case opDropExporter:
		name := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		if !privileged {
			return respErr(reqID, "DropExporter requires DAEMON authorization")
		}
		if err := s.cat.DropExporter(name); err != nil {
			return respErr(reqID, err.Error())
		}
		return resp(reqID, stOK)

	case opListExporters:
		// Names and kinds only -- no config -- so listing is safe for an unprivileged peer.
		defs := s.cat.Exporters()
		b := putI32(resp(reqID, stOK), int32(len(defs)))
		for _, d := range defs {
			b = putStr(putStr(b, d.Name), d.Kind)
		}
		return b

	case opGetExporter:
		name := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		// The full definition includes Config, which may hold credentials -> DAEMON-only.
		if !privileged {
			return respErr(reqID, "GetExporter requires DAEMON authorization")
		}
		def, ok := s.cat.Exporter(name)
		if !ok {
			return putU8(resp(reqID, stOK), 0)
		}
		data, err := json.Marshal(def)
		if err != nil {
			return respErr(reqID, err.Error())
		}
		return putBytes(putU8(resp(reqID, stOK), 1), data)

	case opPutExporterState:
		name := r.str()
		state := r.bytesRef()
		if r.err != nil {
			return respBad(reqID)
		}
		if !privileged {
			return respErr(reqID, "PutExporterState requires DAEMON authorization")
		}
		if err := s.cat.SaveExporterState(name, state); err != nil {
			return respErr(reqID, err.Error())
		}
		return resp(reqID, stOK)

	case opGetExporterState:
		name := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		if !privileged {
			return respErr(reqID, "GetExporterState requires DAEMON authorization")
		}
		state, ok, err := s.cat.LoadExporterState(name)
		if err != nil {
			return respErr(reqID, err.Error())
		}
		if !ok {
			return putU8(resp(reqID, stOK), 0)
		}
		return putBytes(putU8(resp(reqID, stOK), 1), state)

	case opWatchHead:
		table := r.str()
		if r.err != nil {
			return respBad(reqID)
		}
		// Resolved the same way streamWatch resolves a watch: a mutable table, a
		// materialized-view backing, or an append-only archive. Without the last two,
		// a client could watch an archive but not ask where its head is -- so the
		// "tail from now" path was unreachable for exactly the tables whose replay is
		// most expensive, and the failure surfaced as "no such table" for a table the
		// very next op would happily stream.
		var cursor []byte
		var err error
		switch ref := s.resolveTable(table); ref.kind {
		case kindTable, kindView:
			cursor, err = ref.db.WatchCursor()
		case kindArchive:
			cursor, err = ref.arch.WatchCursor()
		default:
			return respErr(reqID, "no such table: "+table)
		}
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
		if refused := sc.refuseCommit(reqID, st); refused != nil {
			return refused
		}
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
		return respErr(reqID, refusalMsg(err))

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
		return respErr(reqID, refusalMsg(cerr))

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
			// Wire-native ingest: the ad arrived as text and is stored as wire bytes, so
			// building an ast.ClassAd in between is pure overhead. NewClassAdOld declines
			// where it cannot be faithful (an encrypted store, an ad shape that defers to
			// the reference parser) and the parse path below answers those.
			if !st.tx.NewClassAdOld(key, adText) {
				ad, err := classad.ParseOld(adText)
				if err != nil {
					return respErr(reqID, err.Error())
				}
				// Refused now, before it is buffered, so the client hears which ad it was -- and
				// skips it, as it skips an unparseable one -- instead of the whole Commit failing.
				// The wire-native path above declines such an ad, so every one lands here.
				if err := wire.CheckDepth(ad.AST()); err != nil {
					return respErr(reqID, refusalMsg(fmt.Errorf("NewClassAd %s: %w", key, err)))
				}
				st.tx.NewClassAd(key, ad)
			}
			st.wrote = true
			st.record(s, WriteOp{Kind: WriteNewClassAd, Key: key, Value: adText})
			return resp(reqID, stOK)
		})

	case opNewAdBatch:
		return s.withTxn(reqID, r, func(st *serverTxn) []byte {
			n := r.i32()
			if r.err != nil || n < 0 {
				return respBad(reqID)
			}
			// Apply each ad under this one lock acquisition; an unparseable ad is recorded
			// as a reject (by index) and skipped, exactly as opNewAd's per-ad ServerError
			// is skipped by the caller, so one bad ad never loses the batch. A truncated
			// frame is a bad request.
			var rejIdx []int32
			var rejMsg []string
			for i := int32(0); i < n; i++ {
				key, adText := r.str(), r.str()
				if r.err != nil {
					return respBad(reqID)
				}
				// As opNewAd: wire-native first, parse only where that declines -- which is
				// also where a malformed ad is rejected, by index, without losing the batch.
				if !st.tx.NewClassAdOld(key, adText) {
					ad, err := classad.ParseOld(adText)
					if err == nil {
						if derr := wire.CheckDepth(ad.AST()); derr != nil {
							err = fmt.Errorf("NewClassAd %s: %w", key, derr)
						}
					}
					if err != nil {
						rejIdx = append(rejIdx, i)
						rejMsg = append(rejMsg, refusalMsg(err))
						continue
					}
					st.tx.NewClassAd(key, ad)
				}
				st.record(s, WriteOp{Kind: WriteNewClassAd, Key: key, Value: adText})
				st.wrote = true
			}
			b := putI32(resp(reqID, stOK), int32(len(rejIdx)))
			for i := range rejIdx {
				b = putStr(putI32(b, rejIdx[i]), rejMsg[i])
			}
			return b
		})

	case opDestroyAd:
		return s.withTxn(reqID, r, func(st *serverTxn) []byte {
			key := r.str()
			st.wrote = true
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
			st.wrote = true
			if err := st.tx.SetAttribute(key, name, expr); err != nil {
				return respErr(reqID, refusalMsg(err))
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
			st.wrote = true
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
		// An archive's diagnostics mirror the mutable stat surface plus retention. A VIEW is
		// not answered here, though its backing is an ordinary *db.DB that diagJSON could
		// read: that has always reported "no such table" and no reason is recorded, so it
		// is left as it is rather than widened by a resolution change.
		var data []byte
		var err error
		switch ref := s.resolveTable(table); ref.kind {
		case kindTable:
			data, err = s.diagJSON(ref.db)
		case kindArchive:
			data, err = s.archiveDiagJSON(ref.arch)
		default:
			return respErr(reqID, "no such table: "+table)
		}
		if err != nil {
			return respErr(reqID, err.Error())
		}
		return putStr(resp(reqID, stOK), string(data))

	case opAttrTypes:
		table := r.str()
		sampleMax := int(r.i32())
		if r.err != nil || sampleMax < 0 {
			return respBad(reqID)
		}
		var profs []db.AttrTypeProfile
		var sampled int
		switch ref := s.resolveTable(table); ref.kind {
		case kindTable:
			profs, sampled = ref.db.AttrTypeProfiles(sampleMax)
		case kindArchive:
			profs, sampled = ref.arch.AttrTypeProfiles(sampleMax)
		default:
			return respErr(reqID, "no such table: "+table)
		}
		rep := AttrTypeReport{
			Profiles: filterPrivateProfiles(profs, includePrivate),
			Sampled:  sampled,
		}
		js, err := json.Marshal(rep)
		if err != nil {
			return respErr(reqID, err.Error())
		}
		return putStr(resp(reqID, stOK), string(js))

	case opQueryStats:
		table := r.str()
		reset := r.u8() != 0
		if r.err != nil {
			return respBad(reqID)
		}
		qs := s.qstats.Load()
		if qs == nil {
			data, err := json.Marshal(QueryStatsReport{})
			if err != nil {
				return respErr(reqID, err.Error())
			}
			return putStr(resp(reqID, stOK), string(data))
		}
		// Exemplars name a user and quote the constraint they ran, so they are DAEMON-only
		// even though the shapes they belong to are not. Clearing the window is DAEMON-only
		// for the same reason the rest of the admin surface is: any reader could otherwise
		// erase what an operator was about to look at.
		if reset && !privileged {
			return respErr(reqID, "resetting query statistics requires DAEMON authorization")
		}
		// Snapshot BEFORE clearing, so a reset is "show me and start over" rather than a
		// request that throws away the very numbers it returns.
		rep := qs.report(table, privileged)
		if reset {
			qs.reset()
		}
		data, err := json.Marshal(rep)
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
		// The COUNT of rows a delete removed leaks the predicate as surely as a query would.
		if !includePrivate {
			if attr, dynamic := db.PrivateConstraintRef(constraint); attr != "" {
				return respErr(reqID, "cannot reference private attribute "+attr+" in a constraint")
			} else if dynamic {
				return respErr(reqID, "cannot use a dynamic attribute reference in a constraint")
			}
		}
		d, ok := s.cat.Table(table)
		if !ok {
			return respErr(reqID, "no such table: "+table)
		}
		// An unprivileged session matches with no key: how many rows a constraint removes is an answer
		// about a sealed value it read, whether or not the attribute is private by name.
		deleteWhere := d.DeleteWhere
		if !includePrivate {
			deleteWhere = d.DeleteWhereRedacted
		}
		removed, err := deleteWhere(constraint)
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
		// A plan describes which indexes and columns the predicate touches, naming them back.
		if !includePrivate {
			if attr, dynamic := db.PrivateConstraintRef(constraint); attr != "" {
				return respErr(reqID, "cannot reference private attribute "+attr+" in a constraint")
			} else if dynamic {
				return respErr(reqID, "cannot use a dynamic attribute reference in a constraint")
			}
		}
		// An archive (history) table is not a mutable table, so resolving only the mutable
		// catalog reported `.explain` on a history table as a nonexistent table -- while
		// queries against it worked. Both types can explain. A view is not answered here,
		// as in opDiag.
		var ex db.QueryExplain
		var err error
		switch ref := s.resolveTable(table); ref.kind {
		case kindTable:
			ex, err = ref.db.Explain(constraint)
		case kindArchive:
			ex, err = ref.arch.Explain(constraint)
		default:
			return respErr(reqID, "no such table: "+table)
		}
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
		// The selector picks the request, and the plan bakes the request's own values into the slot
		// predicate it shows: both read the request table, so both get the gates a query does.
		if !includePrivate {
			for _, c := range []string{selector, targetWhere} {
				if attr, dynamic := db.PrivateConstraintRef(c); attr != "" {
					return respErr(reqID, "cannot reference private attribute "+attr+" in a constraint")
				} else if dynamic {
					return respErr(reqID, "cannot use a dynamic attribute reference in a constraint")
				}
			}
		}
		reqDB, ok := s.cat.Table(reqTable)
		if !ok {
			return respErr(reqID, "no such table: "+reqTable)
		}
		resDB, ok := s.cat.Table(resTable)
		if !ok {
			return respErr(reqID, "no such table: "+resTable)
		}
		query := reqDB.Query
		if !includePrivate {
			query = reqDB.QueryRedacted // which request matches, and the values baked into its plan
		}
		seq, err := query(orTrue(selector))
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
		// An archive supports the layout-tuning subset of admin actions
		// (retrain/reindex/rewrite); a view is not answered here, as in opDiag. The archive
		// branch checks privilege itself and the mutable one checks inside s.admin, which
		// is why the two refusals differ in wording -- per-action for a mutable table,
		// whole-op for an archive.
		var msg string
		var err error
		switch ref := s.resolveTable(table); ref.kind {
		case kindTable:
			msg, err = s.admin(ref.db, action, args, privileged)
		case kindArchive:
			if !privileged {
				return respErr(reqID, fmt.Sprintf("admin action %q requires DAEMON authorization", action))
			}
			msg, err = archiveAdmin(ref.arch, action, args, s.maintainOpts)
		default:
			return respErr(reqID, "no such table: "+table)
		}
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
			return respErr(reqID, refusalMsg(err))
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
	// Tracked like the other managed loops: aborting a transaction touches the store, so
	// Close must stop this loop and wait for the pass in flight.
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
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
	stop = func() { once.Do(func() { close(done) }) }
	s.addStopBG(stop)
	return stop
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

// maintainArchives runs the append-only side of a maintenance pass. Archive tables are a
// separate catalog namespace, so the loop over cat.Tables() above does not reach them -- and
// until it did, nothing ever ran on an archive: no merge, no reindex, no tuning.
//
// That omission matters more for an archive than it would for a mutable table. An
// append-only table never compacts, so its segment count only grows, and every sealed
// segment costs a memory mapping at open plus a second for its sidecar. Exhausting
// vm.max_map_count stops the daemon STARTING, and the condition is not self-correcting: with
// nothing merging, the count only rises, and the symptom does not appear until a restart.
//
// The work is deliberately narrow -- merge cold runs, rebuild indexes left stale by an
// interrupted pass, carry the index set to older segments within a per-pass budget, and apply
// the demand-driven tuning that does not re-encode anything.
// Retraining and rewriting are excluded: both re-encode the whole archive, which is not
// something to do on a timer. The hot-set refresh is excluded from that reasoning because it
// rewrites nothing: it changes what future records are written under, and the merge pass
// above carries it to older records as it folds their segments together.
func (s *Server) maintainArchives(opts db.MaintainOptions, done <-chan struct{}) {
	// Resolve the sample bound ONCE, for the whole pass, as DB.Maintain does. Each step used
	// to default it for itself or not at all, and an unset SampleMax therefore meant two
	// different things inside one pass: diagSampleMax for the schema scan and the hot-set
	// refresh, and 4096 for the index tuner and the group derivation, because collections'
	// AutoTune and GroupSchemas both substitute their own maxDistinctSample for a
	// non-positive bound. Neither step was ever unsampled, so this is not a dark pass -- it
	// is two sample sizes where there should be one, with the steps that sample an archive's
	// records silently drawing the larger of them.
	//
	// The pass's own default is the one kept: sampling an archive decompresses records, and
	// the two steps that were drawing 4096 were taking it from a library fallback rather than
	// asking for it. A caller that wants a different bound sets SampleMax, which every step
	// has always honoured.
	if opts.SampleMax <= 0 {
		opts.SampleMax = diagSampleMax
	}
	for _, name := range s.cat.ArchiveTables() {
		// Between archives, as the table loop above does. A daemon mounting three
		// history-scale archives otherwise pays for all three before it notices the stop.
		select {
		case <-done:
			return
		default:
		}
		a, ok := s.cat.ArchiveTable(name)
		if !ok {
			continue
		}
		s.maintainMu.Lock()
		// Between the STEPS of one archive's pass, not only between archives. Each step below
		// honours StopMaintenance internally, but none of them knows it is one of seven: a stop
		// landing in the merge used to still pay for the reindex, the index backfill, the group
		// derivation, the schema scan (whose columnarize is the longest of the lot), the hot-set
		// refresh and the index auto-tune. stopping is checked before each expensive step; the
		// archive's own flag is used rather than done, because that is what Close sets and what
		// the steps themselves read, so the two can never disagree.
		stopping := a.MaintenanceStopping
		// ArchiveMergeDisabled gates the MERGE, and only the merge. It used to return from
		// the whole function, which was the same statement when the pass was a merge and the
		// reindex that follows one; every step added since -- hot-set refresh, index
		// auto-tune, the schema scan, the group derivation, the demand checkpoint -- was
		// silently switched off with it. None of those re-encode the archive, which is what
		// the knob's name is about, and the one that changes future writes has its own switch
		// in ArchiveHotRefreshDisabled.
		if !opts.ArchiveMergeDisabled && !stopping() {
			// Below its trigger watermark this is a segment count and a comparison, so the
			// common case costs nothing.
			a.MergePass(opts.ArchiveMerge)
		}
		// A merge leaves its output without a sidecar, and an interrupted reindex leaves
		// segments stale; either way queries fall back to scanning until it is rebuilt. Not
		// gated on the merge: a pass interrupted before the knob was set leaves the same
		// stale segments behind, and they stay stale until something rebuilds them.
		if stale, _ := a.StaleIndexSegments(); stale > 0 && !stopping() {
			a.Reindex()
		}
		// Carry the index set to older segments that predate it, a budgeted slice per pass,
		// newest first. Reindex stops at the backfill horizon, so without this an index added
		// to a large history only ever reaches its newest segments.
		if budget, ok := opts.ArchiveIndexBackfillBudget(); ok && !stopping() {
			if pending, _ := a.IndexBackfillPending(); pending > 0 {
				a.BackfillIndexes(budget)
			}
		}
		// Derive and checkpoint the candidate group schemas on the ARCHIVE's cadence, which is
		// far longer than a mutable table's -- so the stability gate's several derivations span
		// days rather than minutes. That is the intended shape: an archive's structure should be
		// established over the period it actually changes on.
		//
		// BEFORE the schema scan, as DB.Maintain does it: the gate reads the derivation
		// history that this call appends to, so deriving afterwards means the scan's
		// auto-promotion never sees the current pass and a qualifying group waits one extra
		// review interval -- a day on an archive -- to be committed. One derivation per pass
		// either way; the history still accumulates on the pass cadence and no faster.
		if opts.ArchiveSchemaScanHotTopN > 0 && !stopping() {
			a.GroupSchemas(opts.SampleMax, 0)
		}
		// The per-segment columnar accelerator, off unless configured (see
		// ArchiveSchemaScanHotTopN): the first build reads the whole history once. After that
		// this only extends coverage to segments sealed since, including those a merge just
		// produced.
		if opts.ArchiveSchemaScanHotTopN > 0 && !stopping() {
			a.BuildAndEnableSchemaScan(opts.SampleMax, opts.ArchiveSchemaScanHotTopN)
		}
		// Re-rank the hot header from accumulated read demand. This is the one archive pass
		// that reads nothing back: the hot header is per record, so sealed segments keep --
		// and still read correctly under -- the set they were written with, and nothing is
		// rebuilt here. A sample and a pointer swap, which is why it runs on the shared
		// HotTopN instead of a threshold of its own.
		//
		// It is placed after the merge deliberately. A merge re-encodes the records it folds
		// together under the current hot set, so the ordering decides which set this pass's
		// merged output carries: refreshing first would apply a set chosen from demand that
		// the merge has not yet been measured against. Refreshing after means the NEXT pass's
		// merge carries it, which is the conservative order.
		if opts.HotTopN > 0 && !opts.ArchiveHotRefreshDisabled && !stopping() {
			a.RefreshHotSet(opts.SampleMax, opts.HotTopN)
		}
		// Add the indexes observed demand justifies. Off unless a threshold is configured:
		// an archive index is paid for by reading history back, so it is not something to
		// start doing to an existing deployment on an upgrade.
		if opts.ArchiveIndexMinDemand > 0 && !stopping() {
			a.AutoTune(db.AutoTuneOptions{
				SampleMax: opts.SampleMax,
				MinDemand: opts.ArchiveIndexMinDemand,
				Reindex:   true,
			})
		}
		// Age and checkpoint the recorded query demand. An archive needs this more than a
		// mutable table does: indexing one is paid for by decompressing history, so the
		// decision wants evidence gathered over days, which is longer than a daemon can be
		// relied on to stay up.
		a.SaveDemand()
		s.maintainMu.Unlock()
	}
}
