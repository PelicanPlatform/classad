// Package db is an embedded ClassAd log: a persistent key->ClassAd store with
// optimistic multi-writer transactions, mirroring HTCondor's ClassAdLog
// (src/condor_utils/classad_log.h). It is the Go core that the cgo layer (package
// capi) exposes as C symbols for a C++ interface to sit on top of, and that the
// client/server module serves over CEDAR.
//
// It maps directly onto the collections store: the key->ClassAd table is a
// Collection, and each transaction is a collections.Txn (snapshot isolation,
// write-write conflicts, per-ad commit). Unlike classad_log.h -- which allows only
// one active transaction -- this supports any number of independent concurrent
// transactions, each a distinct *Txn.
package db

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// DB is an embedded ClassAd log. Safe for concurrent use.
type DB struct {
	c        *collections.Collection
	id       string // stable database identity (persisted; random for in-memory)
	instance string // this open's instance identity (fresh each Open)
	dir      string // on-disk directory ("" for in-memory); where index config persists

	// enc is the derived encryption state (data key for the live store, backup key +
	// master envelope for snapshots), or nil when encryption is disabled. See db/encrypt.go.
	enc *dbCrypto

	// snapMu is the DB-wide lock. Ordinary writes take it read-shared (many concurrent
	// commits proceed); Truncate/Restore take it exclusively so a reload is atomic against
	// all writers. See db/snapshot.go.
	snapMu sync.RWMutex

	// snapLock{Count,Nanos} accumulate how long snapMu was held exclusively (a
	// Truncate/Restore blocks every writer for the whole reload); surfaced via OpStats.
	snapLockCount atomic.Int64
	snapLockNanos atomic.Int64
	// snapLockWait{Count,Nanos} accumulate how long COMMITTERS blocked acquiring the shared
	// snapshot lock -- i.e. the time a Commit spent stalled behind an exclusive Truncate/Restore.
	// SnapshotLock (above) measures only the holder's time, so without this a commit stalled
	// behind a world-blocking op is attributable to nothing. Counted only when contended (see
	// Commit's TryRLock), so the uncontended hot path pays no clock read.
	snapLockWaitCount atomic.Int64
	snapLockWaitNanos atomic.Int64
}

// lockSnapExclusive takes the DB-wide snapshot lock exclusively and returns a release
// func that records the exclusive-hold duration -- the wall time the whole store was
// blocked. Use as: defer db.lockSnapExclusive()().
func (db *DB) lockSnapExclusive() func() {
	db.snapMu.Lock()
	held := time.Now()
	return func() {
		hold := time.Since(held)
		db.snapMu.Unlock()
		db.snapLockCount.Add(1)
		db.snapLockNanos.Add(int64(hold))
	}
}

// OrderSpec, SortKey, OrderedAd, OrderCursor configure and drive maintained ordered
// indexes (the schedd priority-queue / resource-request-list pattern). Re-exported
// from collections so callers of db need not import it.
type (
	OrderSpec   = collections.OrderSpec
	SortKey     = collections.SortKey
	OrderedAd   = collections.OrderedAd
	OrderCursor = collections.OrderCursor
)

// Config opens a DB with indexing and ordered-index configuration. Dir empty is
// in-memory; a non-empty Dir is persistent.
// DefaultDeltaMax is the delta-record chain bound applied when Config.DeltaMax is left at zero,
// which is to say: delta records are ON by default. Storing only what a write changed is the
// cheaper way to hold a queue that is updated far more often than it is created -- measured at
// -42% ingest CPU and -32% allocation replaying a real schedd's job_queue.log -- and a feature
// that ships switched off is a feature that rots.
//
// It is NOT reversible for data already written. A store that has written one delta record
// replays them for the rest of its life (the on-disk marker says so); setting DeltaMaxOff later
// stops new deltas and leaves the existing ones readable, it does not convert them back.
const DefaultDeltaMax = 16

// DeltaMaxOff disables delta records for a table. Any negative Config.DeltaMax means off; this
// spelling exists because zero means "use the default", so there has to be a way to say "none".
const DeltaMaxOff = -1

// resolveDeltaMax maps a configured DeltaMax onto what the store is given: zero takes the
// default, negative means off, and anything else is used as written.
func resolveDeltaMax(v int) int {
	switch {
	case v == 0:
		return DefaultDeltaMax
	case v < 0:
		return 0
	default:
		return v
	}
}

type Config struct {
	Dir string
	// Ordered configures maintained, filtered, sorted indexes -- e.g. the negotiator's
	// resource-request lists (partition by Owner, sort by JobPrio then QDate), iterated
	// in order via Ordered. Optional.
	Ordered []OrderSpec
	// HotAttrs / CategoricalAttrs / ValueAttrs / MatchClosureRoots tune storage and
	// query/match push-down (see collections.Options). Optional.
	HotAttrs                     []string
	CategoricalAttrs, ValueAttrs []string
	MatchClosureRoots            []string

	// DeltaMax bounds the delta-record chain length for this table; see DefaultDeltaMax and
	// DeltaMaxOff for what 0 and a negative value mean.
	//
	// It turns on delta records for this table and bounds the chain length: a
	// SetAttribute stores only the attributes it changed, until a key has accumulated
	// DeltaMax of them and the next write stores the whole ad again. See
	// collections.Options.DeltaMax for the trade.
	DeltaMax int

	// GroupSchemaCount is how many SECONDARY columnar schemas to derive: sets of attributes the
	// base schema does not carry which are present or absent together, stored columnar for the
	// ads that hold them without costing a slot in the ads that do not. 0 takes the default (4);
	// a NEGATIVE value builds none.
	//
	// A group's blocks are built only once its members have kept showing up together across
	// GroupStabilityRuns maintenance passes, so a freshly opened table builds none for a while --
	// SchemaScanInfo.GroupSchemas reports how many actually exist. See collections.Options.
	GroupSchemaCount    int
	GroupStabilityRuns  int
	GroupMergeJaccard   float64
	GroupMaxPartialFrac float64

	// SealMigrationWorkers bounds the concurrent segment rewrites when an existing store is migrated
	// to sealed private attributes at open (see collections.MigrateSealedAttrs). 0 takes a default
	// derived from GOMAXPROCS.
	SealMigrationWorkers int

	// OnSealMigration, if set, is called with the number of segments rewritten when the open migrated a
	// store to sealed private attributes. It exists so a daemon can log a one-off startup cost rather
	// than leaving an operator to wonder why the first open after an upgrade took longer.
	OnSealMigration func(segments int)

	// SegmentSize overrides the arena segment size in bytes (see collections.Options).
	// 0 uses the default (8 MiB). A smaller value seals segments sooner -- useful for
	// tests and for tuning the sealed-segment accelerators (columnar scan, sealed indexes).
	SegmentSize int

	// MutatingBlockCacheBytes and ArchiveBlockCacheBytes set the PROCESS-GLOBAL shared
	// decompressed-columnar-block cache budgets (see collections.Options): one budget shared by all
	// mutating tables and a separate one shared by all archive tables. They are global ceilings, not
	// per-DB or per-table, so a process opening many tables (as htcondordb does) no longer multiplies
	// a fixed per-table cache by the table count. 0 keeps the collections default (512 MiB each). The
	// same Config feeds both the mutating and archive opens, so setting them once configures the
	// whole process; the last non-zero value wins and resizes the live cache.
	MutatingBlockCacheBytes int64
	ArchiveBlockCacheBytes  int64

	// PoolKeys enables encryption at rest: the DB master key is wrapped under each of
	// these pool/signing keys (any one opens the DB; a rotated-in key is added on the
	// next open). Empty ⇒ encryption disabled. See db/encrypt.go.
	PoolKeys []KEK
	// EncryptedAttrs names the attributes whose values are sealed at rest (case-
	// insensitive). Only meaningful with PoolKeys set. An encrypted attribute may not
	// also be indexed. See collections.Options.EncryptedAttrs.
	EncryptedAttrs []string
}

// Open opens a ClassAd log with default configuration. A non-empty dir makes it
// persistent (memory-mapped arenas under dir, recovered on reopen); an empty dir is
// in-memory. See OpenConfig for indexing / ordered-index configuration.
func Open(dir string) (*DB, error) { return OpenConfig(Config{Dir: dir}) }

// OpenConfig opens a ClassAd log with the given configuration. Every DB is stamped
// with a stable DB id (persisted alongside a persistent store, so it survives reopen)
// and a fresh instance id for this open. Until high-availability DB servers exist they
// are effectively the same identity, but a follower/replica shares the DB id while
// carrying its own instance id.
func OpenConfig(cfg Config) (*DB, error) {
	enc, err := resolveCrypto(cfg.Dir, cfg.PoolKeys)
	if err != nil {
		return nil, err
	}
	opts := collections.Options{
		Dir:                 cfg.Dir,
		WatchHistory:        4096, // enables Watch
		Ordered:             cfg.Ordered,
		HotAttrs:            cfg.HotAttrs,
		CategoricalAttrs:    cfg.CategoricalAttrs,
		ValueAttrs:          cfg.ValueAttrs,
		MatchClosureRoots:   cfg.MatchClosureRoots,
		GroupSchemaCount:    cfg.GroupSchemaCount,
		GroupStabilityRuns:  cfg.GroupStabilityRuns,
		GroupMergeJaccard:   cfg.GroupMergeJaccard,
		GroupMaxPartialFrac: cfg.GroupMaxPartialFrac,
		DeltaMax:            resolveDeltaMax(cfg.DeltaMax),
		Codec:               chooseBaseCodec(cfg.Dir), // ZSTD by default for new stores
		DataKey:             enc.data(),
		EncryptedAttrs:      cfg.EncryptedAttrs,
		SegmentSize:         cfg.SegmentSize, // 0 ⇒ collections default (8 MiB)
		// Process-global shared block-cache budgets (0 ⇒ collections default). Both are set here at DB
		// open: collections.New applies BOTH the mutating and archive budgets to the process globals,
		// so archives opened later from a plain ArchiveConfig inherit the archive budget set now.
		MutatingBlockCacheBytes: cfg.MutatingBlockCacheBytes,
		ArchiveBlockCacheBytes:  cfg.ArchiveBlockCacheBytes,
		// Time travel is a persisted runtime toggle: read it before opening so recovery
		// rebuilds the time index (and scan-pruning counters) from the segment markers
		// instead of the directory snapshot. loadIndexConfig below keeps it in sync.
		TimeTravel: readPersistedTimeTravel(cfg.Dir),
	}
	var c *collections.Collection
	if cfg.Dir == "" {
		c = collections.New(opts)
	} else {
		var err error
		if c, err = collections.Open(opts); err != nil {
			return nil, err
		}
	}
	db := &DB{c: c, id: loadOrCreateDBID(cfg.Dir), instance: randID(), dir: cfg.Dir, enc: enc}
	// Reapply any index/hot-set configuration persisted by a previous run's
	// runtime changes (AddIndex/AddHotAttrs/...), so they survive a restart.
	db.loadIndexConfig()
	// A store written before private attributes were sealed still holds them in the clear, and nothing
	// would ever report it: sealing applies at encode time, so it protects new writes and leaves the
	// existing ones exactly as they were. Rewrite them now.
	//
	// It runs at open rather than in the background because a half-protected store is not a state to
	// serve reads from for an unbounded time. On a store that needs nothing it is one scan of the sealed
	// segments, and after the first clean pass a marker skips even that (see MigrateSealedAttrs).
	if n := db.c.MigrateSealedAttrs(cfg.SealMigrationWorkers); n > 0 && cfg.OnSealMigration != nil {
		cfg.OnSealMigration(n)
	}
	return db, nil
}

// ID is the stable database identity (same across reopens of a persistent store).
func (db *DB) ID() string { return db.id }

// InstanceID is this open's identity (fresh each Open). Equal in spirit to ID until
// HA replicas exist, when replicas of one DB share ID but differ by InstanceID.
func (db *DB) InstanceID() string { return db.instance }

// InMemory reports whether the table's data lives only in RAM (no on-disk backing),
// i.e. it was opened with an empty Dir. Such a table is not recovered across restarts.
func (db *DB) InMemory() bool { return db.dir == "" }

func randID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// loadOrCreateDBID reads dir/dbid, creating it with a fresh id on first open. An
// in-memory DB (empty dir) gets a random, non-persistent id.
func loadOrCreateDBID(dir string) string {
	if dir == "" {
		return randID()
	}
	p := filepath.Join(dir, "dbid")
	if data, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return s
		}
	}
	id := randID()
	_ = os.WriteFile(p, []byte(id+"\n"), 0o644)
	return id
}

// Close releases the log's resources.
func (db *DB) Close() error { return db.c.Close() }

// MaintainOptions configures one maintenance pass (DB.Maintain).
type MaintainOptions struct {
	// SampleMax caps the ads sampled for index tuning, hot-set frequency, and dictionary
	// training. Default 4096.
	SampleMax int
	// HotTopN refreshes the hot set to the topN most common attributes; 0 disables it.
	HotTopN int
	// Retrain retrains the ZSTD dictionary (recompacting + reindexing). Expensive on a
	// large store, so a server may run it on a longer cadence than the rest.
	Retrain bool
	// CompactInterval is the cadence of a lightweight, standalone compaction pass
	// (dbrpc.Server.StartMaintenance) that reclaims dead space independently of the
	// expensive retrain-dominated Maintain pass. Compaction is cheap and self-limiting
	// (it unlinks fully-dead segments for free and only recompacts shards past the
	// dead-byte threshold), so it runs far more often than Maintain -- essential for a
	// high-churn table (e.g. a collector re-advertising every few minutes) where dead
	// space would otherwise grow unbounded between the throttled retrain passes. 0 uses
	// a default cadence; a negative value disables the standalone compaction pass.
	CompactInterval time.Duration
	// MinIndexDemand is the minimum observed query count for the auto-tuner to add an
	// index; 0 leaves index auto-tune off (no demand-driven adds).
	MinIndexDemand int64
	// IndexBudgetHighFrac / IndexBudgetLowFrac / IndexBudgetSlackBytes bound auto-created
	// index memory as a fraction of the live data bytes (see collections.AutoTuneOptions);
	// 0 high frac disables the budget (auto indexes grow unbounded).
	IndexBudgetHighFrac   float64
	IndexBudgetLowFrac    float64
	IndexBudgetSlackBytes int64
	// ArchiveMerge tunes the merge pass run over each archive table. The zero value uses
	// the policy defaults, which target a segment count well clear of the process's mapping
	// budget. ArchiveMergeDisabled turns the archive side of maintenance off entirely.
	ArchiveMerge         MergeOptions
	ArchiveMergeDisabled bool
	// ArchiveIndexMinDemand is the minimum observed query demand for the auto-tuner to add
	// an index to an ARCHIVE table; 0 leaves archive index auto-tune off.
	//
	// It is separate from MinIndexDemand, and off by default, because the two costs are not
	// comparable. Indexing a mutable table is bounded by its live size; indexing an archive
	// is paid for by decompressing history, so the threshold wants to be set against
	// ArchiveConfig.IndexBackfillBytes -- how far back a new index is actually carried --
	// rather than inherited from the mutable side.
	//
	// Auto-DROP is deliberately not offered for archives at any setting. Dropping an index
	// is nearly free and re-adding it costs a backfill, so a wrong drop is expensive and
	// asymmetric in the direction that punishes acting on a weak signal.
	ArchiveIndexMinDemand int64
	// ArchiveSchemaScanHotTopN, when > 0, builds the per-segment columnar accelerator on
	// ARCHIVE tables too, keeping the topN most query-read numeric fields uncompressed.
	//
	// Separate from SchemaScanHotTopN, and off by default, for the same reason
	// ArchiveIndexMinDemand is: the first build reads every sealed record of the whole
	// history once, so turning it on is a deliberate decision about an existing deployment
	// rather than something an upgrade should start doing. Once built it is cheap -- an
	// archive's segments are immutable, so a block is never invalidated, and later passes
	// only cover newly-sealed segments.
	//
	// The payoff is confined to what the accelerator serves: single-int-field COUNT
	// comparisons (25-65x on a 50k-record archive). Other aggregates -- MAX, SUM, GROUP BY
	// -- take their existing paths and are unaffected.
	ArchiveSchemaScanHotTopN int
	// SchemaScanHotTopN, when > 0, builds/refreshes the per-segment adschema columnar
	// accelerator (used by CountConstraint's fast path) keeping the topN most query-read
	// numeric fields uncompressed. The first pass chooses a stable schema and hot set from
	// the current sample and demand; later passes only extend coverage to newly-sealed
	// segments, so a table opts into the columnar count path once maintenance runs and stays
	// covered as it grows. 0 leaves it off.
	SchemaScanHotTopN int
}

// Maintain runs one self-tuning pass: it auto-tunes indexes (adds demand-driven ones,
// trims auto indexes over the memory budget, never touches human-created ones), refreshes
// the hot-attribute set, and optionally retrains the compression dictionary. Index/hot
// changes are persisted. Synchronous; a server drives it on a schedule (see
// dbrpc.Server.StartMaintenance).
func (db *DB) Maintain(opts MaintainOptions) {
	if opts.SampleMax <= 0 {
		opts.SampleMax = 4096
	}
	if opts.MinIndexDemand > 0 || opts.IndexBudgetHighFrac > 0 {
		res := db.c.AutoTune(collections.AutoTuneOptions{
			SampleMax:        opts.SampleMax,
			MinDemand:        opts.MinIndexDemand,
			BudgetHighFrac:   opts.IndexBudgetHighFrac,
			BudgetLowFrac:    opts.IndexBudgetLowFrac,
			BudgetSlackBytes: opts.IndexBudgetSlackBytes,
			Reindex:          !opts.Retrain, // if retraining, its recompaction reindexes; else do it here
		})
		if res.Changed {
			db.saveIndexConfig() // persist auto add/drop (with provenance)
		}
	}
	if opts.HotTopN > 0 {
		db.c.RefreshHotSet(opts.SampleMax, opts.HotTopN)
	}
	if opts.Retrain {
		_, _ = db.c.RetrainDict(opts.SampleMax) // recompacts + reindexes
	}
	if opts.SchemaScanHotTopN > 0 {
		// Derive and checkpoint the candidate group schemas BEFORE enabling, so the history the
		// stability gate reads accumulates on this cadence and no faster. A group is committed to
		// storage only once its members have kept showing up together across several of these
		// passes, so the review interval sets how long that takes -- minutes for a mutable table,
		// a day for an archive.
		db.c.GroupSchemas(opts.SampleMax, 0)
		// Build the columnar accelerator on first run, extend it to newly-sealed segments
		// after (idempotent + refresh-safe -- keeps the stable schema/hot set).
		db.c.BuildAndEnableSchemaScan(opts.SampleMax, opts.SchemaScanHotTopN)
	}
	// Checkpoint the query demand these decisions are made from, and age it. Unconditional:
	// it is a small write, and the signal is worth as much to the next process as to this
	// one -- more, since that one starts with nothing else.
	db.c.SaveDemand()
}

// StartMaintenance starts a background goroutine that runs Maintain with the given
// options every interval, returning a stop function. Prefer the catalog-wide
// dbrpc.Server.StartMaintenance, which also covers tables created later.
func (db *DB) StartMaintenance(interval time.Duration, opts MaintainOptions) (stop func()) {
	if interval <= 0 {
		interval = collections.DefaultRetrainInterval
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				db.Maintain(opts)
			}
		}
	}()
	return func() { close(done) }
}

// SuggestIndexes samples the store and returns attributes that queries filter on but
// are not yet indexed (advisory; a server may log or auto-apply them).
func (db *DB) SuggestIndexes(sampleMax int) []collections.IndexSuggestion {
	return db.c.SuggestIndexes(sampleMax)
}

// Len returns the number of committed ads (including structural parent-only ads of a chained
// collection, which Query hides -- see Collection.Len / Chained).
func (db *DB) Len() int { return db.c.Len() }

// Chained reports whether the table has structural (parent-only) ads that Query hides, so a
// caller knows when Len equals the match-all row count (the COUNT(*) fast path).
func (db *DB) Chained() bool { return db.c.Chained() }

// EnableSchemaScan builds the per-segment adschema columnar accelerator over the table's sealed
// segments and enables it, choosing the uncompressed hot numeric tier as the top-hotTopN
// int/real fields by query demand (see collections.Collection.BuildAndEnableSchemaScan).
// sampleMax bounds the ad sample. Opt-in; a table that never calls this is unaffected.
func (db *DB) EnableSchemaScan(sampleMax, hotTopN int) bool {
	return db.c.BuildAndEnableSchemaScan(sampleMax, hotTopN)
}

// CountConstraint counts the rows matching constraint via the columnar schema scan when the
// constraint is columnar-eligible (Native, numeric comparisons on one int schema field) and
// schema-scan is enabled; ok=false ⇒ the caller should use the normal count path. See
// collections.Collection.CountConstraint.
func (db *DB) CountConstraint(constraint string) (int, bool) { return db.c.CountConstraint(constraint) }

// GroupStatsConstraint answers a per-group record count plus the aggregate inputs for each aggAttr over
// the rows matching constraint, via the columnar accelerator, or ok=false so the caller scans. This is
// the mutable-table half of what ArchiveTable already exposes; dbrpc's aggregate routes a grouped
// COUNT(*)/MIN/MAX/SUM/AVG through GroupedFromColumns for both.
//
// A CHAINED table declines. The columnar paths read a record's own columns and know nothing about
// parentKeyFor/mergeParent, so on a chained table a group or aggregate attribute inherited from the
// parent would be missing from the column and the answer would differ from the scan's. (The same is true
// of the existing CountConstraint fast path, which does not guard it; chaining has no production consumer
// today -- only classad's own tests set IsStructural -- so that is a latent gap rather than a live bug.)
func (db *DB) GroupStatsConstraint(constraint, groupAttr string, aggAttrs []string) ([]collections.GroupStats, bool) {
	if db.c.Chained() {
		return nil, false
	}
	return db.c.GroupStatsConstraint(constraint, groupAttr, aggAttrs)
}

// GroupStatsAll is GroupStatsConstraint over every row, for a constraint the caller has established is
// match-all. Declines on a chained table for the reason above.
func (db *DB) GroupStatsAll(groupAttr string, aggAttrs []string) ([]collections.GroupStats, bool) {
	if db.c.Chained() {
		return nil, false
	}
	return db.c.GroupStatsAll(groupAttr, aggAttrs)
}

// LookupClassAd returns the committed ad for key (the hash table, outside any
// transaction), or (nil, false).
func (db *DB) LookupClassAd(key string) (*classad.ClassAd, bool) {
	return db.c.Get([]byte(key))
}

// Keys returns every committed key at a consistent snapshot, in no particular
// order. Useful for administrative enumeration and for a replica that must clear
// its keyspace before a full re-sync (see the leader-follower replicator).
func (db *DB) Keys() []string { return db.c.Keys() }

// Diagnostic and management types, re-exported from collections for callers that
// only import db.
type (
	Stats           = collections.Stats
	IndexSuggestion = collections.IndexSuggestion
	DropSuggestion  = collections.DropSuggestion
	IndexSizes      = collections.IndexSizes
	IndexSize       = collections.IndexSize
	SidecarSizes    = collections.SidecarSizes
	Retention       = collections.Retention
	CodecStats      = collections.CodecStats
	SchemaScanInfo  = collections.SchemaScanInfo
	SchemaScanField = collections.SchemaScanField
	SchemaScanGroup = collections.SchemaScanGroup
	SchemaFieldFit  = collections.SchemaFieldFit
	QueryExplain    = collections.QueryExplain
	ProbeExplain    = collections.ProbeExplain
	MatchExplain    = collections.MatchExplain
)

// CodecStats reports the storage codec's state and effectiveness (name, dictionary
// size, last-retrain time, and a sampled compression ratio).
func (db *DB) CodecStats(sampleMax int) CodecStats { return db.c.CodecStats(sampleMax) }

// IndexSizes reports each configured index's resident bytes (with human/auto
// provenance) against the live data bytes -- the memory cost of indexing.
func (db *DB) IndexSizes() IndexSizes { return db.c.IndexSizes() }

// ExplainMatch reports how matchmaking job against this (resource) collection would
// execute: the job's Requirements rewritten over the slot with the job's attributes
// baked to constants, and which of the resulting probes prune via a configured index.
// targetConstraint, if non-empty, is the MATCH resource-side filter (WHERE TARGET /
// NOPREEMPT) melded into the explanation.
func (db *DB) ExplainMatch(job *classad.ClassAd, targetConstraint string) MatchExplain {
	return db.c.ExplainMatch(job, targetConstraint)
}

// Stats returns a snapshot of the store's storage (ad count, segment/arena/dead
// bytes) for observability.
func (db *DB) Stats() Stats { return db.c.Stats() }

// OpStat is one operation's cumulative call count and total wall-nanoseconds.
type OpStat = collections.OpStat

// OpStats is a snapshot of the store's operational timing counters -- where callers
// spent time blocked in, or holding, each stall point (see collections.OpStats) --
// plus the DB-wide snapshot lock's exclusive-hold time (Truncate/Restore). Every value
// is a monotonic cumulative total; a scraper derives rate and mean latency from deltas.
type OpStats struct {
	collections.OpStats
	SnapshotLock OpStat `json:"snapshotLock"`
	// SnapshotLockWait is the time committers spent BLOCKED on the shared snapshot lock (stalled
	// behind an exclusive Truncate/Restore) -- the waiters' side of SnapshotLock's holder time.
	SnapshotLockWait OpStat `json:"snapshotLockWait"`
}

// OpStats returns the store's operational timing counters (see the OpStats type).
func (db *DB) OpStats() OpStats {
	return OpStats{
		OpStats:          db.c.OpStats(),
		SnapshotLock:     OpStat{Count: db.snapLockCount.Load(), Nanos: db.snapLockNanos.Load()},
		SnapshotLockWait: OpStat{Count: db.snapLockWaitCount.Load(), Nanos: db.snapLockWaitNanos.Load()},
	}
}

// HotAttrs returns the current hot attributes (front-loaded in each ad's hot
// header for cheap access).
func (db *DB) HotAttrs() []string { return db.c.HotAttrNames() }

// SidecarSizes reports the sealed-segment sidecar index bytes (mmap-backed, evictable), matching
// ArchiveTable.SidecarSizes. A mutable table has these sidecars too -- only the accessor was missing,
// so its .stats could not report the on-disk footprint an archive's could.
func (db *DB) SidecarSizes() SidecarSizes { return db.c.SidecarSizes() }

// StaleIndexSegments reports how many sealed segments still carry an index built under an older
// configuration, and how many are sealed in total, matching ArchiveTable.StaleIndexSegments.
func (db *DB) StaleIndexSegments() (stale, sealed int) { return db.c.StaleIndexSegments() }

// SchemaScanInfo reports the columnar (adschema) accelerator's state: whether it is enabled (so a
// numeric COUNT(*) WHERE routes to the columnar fast path), its hot columns, and segment coverage.
func (db *DB) SchemaScanInfo() SchemaScanInfo { return db.c.SchemaScanInfo() }

// SchemaFit measures the current derived schema against a fresh sample, reporting each field's
// escape rate -- how often its value is not in the fixed slot, and how much of that is the
// attribute simply being absent. This is how you tell whether the schema still matches the data
// (see collections.Collection.SchemaFit). Returns nil when the accelerator is not enabled.
func (db *DB) SchemaFit(sampleMax int) ([]SchemaFieldFit, int) { return db.c.SchemaFit(sampleMax) }

// ReschemaScan derives a new schema from a fresh sample and rebuilds every sealed segment's
// columnar block against it, replacing the one pinned at first enable. Heavy -- a block per
// sealed segment is re-encoded and re-persisted -- and never done by routine maintenance, which
// deliberately keeps the schema stable. Returns false if there was nothing to sample or the
// accelerator cannot run here.
func (db *DB) ReschemaScan(sampleMax, hotTopN int) bool { return db.c.ReschemaScan(sampleMax, hotTopN) }

// IndexedAttrs returns the currently-indexed attribute names, split into
// categorical (string equality/membership) and value (numeric + range) indexes.
func (db *DB) IndexedAttrs() (categorical, value []string) { return db.c.IndexedAttrs() }

// SuggestDrops recommends indexes to drop (unused or low-cardinality) from
// observed demand and a sample of up to sampleMax live ads.
func (db *DB) SuggestDrops(sampleMax int) []DropSuggestion { return db.c.SuggestDrops(sampleMax) }

// Explain reports how the store would execute a constraint query -- which
// conjuncts are index-usable and the resulting access path.
func (db *DB) Explain(constraint string) (QueryExplain, error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return QueryExplain{}, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.ExplainQuery(q), nil
}

// AddIndex adds categorical and/or value indexes at runtime, returning whether
// the configuration changed. The spec is updated immediately; call Reindex to
// build the new index over existing ads. A change is persisted so it survives a
// restart.
func (db *DB) AddIndex(categorical, value []string) bool {
	changed := db.c.AddIndex(categorical, value)
	if changed {
		db.saveIndexConfig()
	}
	return changed
}

// DropIndex removes the named attributes from the configured indexes, returning
// whether the configuration changed. A change is persisted.
func (db *DB) DropIndex(names ...string) bool {
	changed := db.c.DropIndex(names...)
	if changed {
		db.saveIndexConfig()
	}
	return changed
}

// Reindex rebuilds all configured indexes from the live ads.
func (db *DB) Reindex() { db.c.Reindex() }

// AddHotAttrs pins the named attributes into the hot set and returns the
// resulting hot attributes. The hot set is persisted.
func (db *DB) AddHotAttrs(names ...string) []string {
	hot := db.c.AddHotAttrs(names...)
	db.saveIndexConfig()
	return hot
}

// RefreshHotSet recomputes the hot set as the topN most frequent attributes from
// a sample of up to sampleMax live ads, returning how many were chosen. The
// resulting hot set is persisted.
func (db *DB) RefreshHotSet(sampleMax, topN int) int {
	n := db.c.RefreshHotSet(sampleMax, topN)
	db.saveIndexConfig()
	return n
}

// SetEncryptedAttrs replaces the explicit set of attributes encrypted at rest (the
// human-toggled set; private attributes are always encrypted). It errors if encryption
// is disabled or a named attribute is indexed. The policy is persisted so it survives a
// restart and, in HA, lets a follower converge on reload. New writes seal the new set;
// existing records re-seal when next rewritten (compaction/Rewrite).
func (db *DB) SetEncryptedAttrs(attrs []string) error {
	if err := db.c.SetEncryptedAttrs(attrs); err != nil {
		return err
	}
	db.saveIndexConfig()
	return nil
}

// EncryptedAttrNames returns the explicit encrypted-attribute set (not the always-on
// private attributes). EncryptionEnabled reports whether encryption at rest is active.
func (db *DB) EncryptedAttrNames() []string { return db.c.EncryptedAttrNames() }

// EncryptionEnabled reports ENCRYPTION AT REST: the master key is wrapped under pool keys, so the data
// is protected from someone holding the disk. It is not "are values sealed" -- private attributes are
// always sealed, and without pool keys the master sits beside the data in the clear, which protects
// nothing and is reported here as false.
func (db *DB) EncryptionEnabled() bool { return db.enc.protected() }

// Compact reclaims dead space in shards whose dead-byte ratio warrants it,
// returning the number of shards compacted.
func (db *DB) Compact() int { return db.c.Compact() }

// Rewrite re-encodes every live ad with the current hot set (so a changed hot
// set applies to existing ads) and force-compacts, returning the number of ads
// rewritten. A maintenance operation -- see collections.Collection.Rewrite.
func (db *DB) Rewrite() int { return db.c.Rewrite() }

// RetrainDict trains a fresh ZSTD dictionary from a sample of up to sampleMax live ads,
// switches new writes to it, and recompresses existing records under it. It returns the
// new dictionary's size in bytes. This is what turns on (or refreshes) compression for a
// collection that started with the identity codec. See collections.Collection.RetrainDict.
func (db *DB) RetrainDict(sampleMax int) (int, error) { return db.c.RetrainDict(sampleMax) }

// ForEach calls fn for every committed ad, in no particular order, until fn returns
// false. It reads a consistent snapshot (concurrent writers do not block it).
func (db *DB) ForEach(fn func(ad *classad.ClassAd) bool) {
	for ad := range db.c.Scan() {
		if !fn(ad) {
			return
		}
	}
}

// ForEachSystemAd calls fn for every internal system-keyed ad and its key -- the reaper
// enumeration path. System records are durable bookkeeping (e.g. idempotency markers)
// stored in the same table as data but hidden from every client scan/query/DeleteWhere;
// this is the only way to enumerate them. Reads a consistent snapshot. See SystemKey.
func (db *DB) ForEachSystemAd(fn func(key string, ad *classad.ClassAd) bool) {
	db.c.ForEachSystemAd(fn)
}

// IsSystemKey and SystemKey re-export the collections helpers so callers holding only a
// *db.DB (e.g. dbrpc building marker keys) can classify or construct a reserved system
// key without importing collections directly. A system key begins with a NUL byte and
// names a record hidden from client reads but retrievable by explicit LookupClassAd.
func IsSystemKey(key string) bool { return collections.IsSystemKey(key) }

// SystemKey builds a reserved system key from name (prefixing the NUL sentinel).
func SystemKey(name string) string { return collections.SystemKey(name) }

// Query returns the committed ads matching the constraint expression (an "old
// ClassAd" boolean expression over each ad, e.g. `JobStatus == 2 && Owner == "alice"`).
// The store pushes the filter down -- indexed constraints visit only candidates -- so
// this is far cheaper than ForEach + client-side filtering. Errors only on a malformed
// constraint.
func (db *DB) Query(constraint string) (iter.Seq[*classad.ClassAd], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.Query(q), nil
}

// QueryRedacted is Query for a caller NOT entitled to sealed values: the ads are decoded with no key, so
// a sealed attribute arrives undefined rather than opened. See collections.Collection.QueryRedacted.
//
// Query decodes with the table's key and leaves it to the serializer to drop private attributes, which
// means an unprivileged reader's secret is decrypted in this process and then filtered on the way out.
func (db *DB) QueryRedacted(constraint string) (iter.Seq[*classad.ClassAd], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.QueryRedacted(q), nil
}

// LookupClassAdRedacted is LookupClassAd for a caller not entitled to sealed values; see QueryRedacted.
func (db *DB) LookupClassAdRedacted(key string) (*classad.ClassAd, bool) {
	return db.c.GetRedacted([]byte(key))
}

// BeginRedacted is Begin for a caller not entitled to sealed values: reads through the transaction decode
// with no key. Writes are unaffected. See collections.Collection.BeginRedacted.
func (db *DB) BeginRedacted() *Txn { return &Txn{tx: db.c.BeginRedacted(), db: db} }

// QueryAsOf runs a point-in-time ("AS OF") query: it returns the ads matching the
// constraint as they were at time t. It errors on a malformed constraint, when time
// travel is not enabled on this table, or when t is older than the retained window.
func (db *DB) QueryAsOf(constraint string, t time.Time) (iter.Seq[*classad.ClassAd], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.QueryAsOf(q, t)
}

// SetTimeTravel enables (with a positive maxDistance), retunes, or disables (maxDistance
// <= 0) point-in-time queries for this table, and persists the setting so it survives a
// restart. A zero checkpoint interval uses the collections default (1 minute). Enabling
// is not retroactive -- only changes from now on are travelable.
func (db *DB) SetTimeTravel(maxDistance, checkpoint time.Duration) {
	if maxDistance <= 0 {
		db.c.SetTimeTravel(nil)
	} else {
		db.c.SetTimeTravel(&collections.TimeTravelOptions{MaxDistance: maxDistance, CheckpointInterval: checkpoint})
	}
	db.saveIndexConfig()
}

// TimeTravel reports the table's current point-in-time settings and whether enabled.
func (db *DB) TimeTravel() (maxDistance, checkpoint time.Duration, enabled bool) {
	o, on := db.c.TimeTravelConfig()
	return o.MaxDistance, o.CheckpointInterval, on
}

// QueryProject returns, for each ad matching the constraint, just the named
// attributes' values (aligned with attrs), read wire-native where possible so an
// aggregate or projection does not pay the full-ad decode Query costs. The
// yielded slice is reused across iterations; copy any value to retain it past the
// next step. Errors only on a malformed constraint.
func (db *DB) QueryProject(constraint string, attrs []string) (iter.Seq[[]classad.Value], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.QueryProject(q, attrs), nil
}

// Match returns the ads that symmetrically match job (bilateral Requirements), pushed
// down to the store. For the negotiator's pick-best pattern, prefer MatchSorted.
func (db *DB) Match(job *classad.ClassAd) iter.Seq[*classad.ClassAd] {
	return db.c.Match(job)
}

// RecordDemand notes the attributes constraint filters on (for index suggestions)
// without scanning. Use it for filters applied outside the normal Query path -- e.g. a
// MATCH's resource-side WHERE TARGET constraint -- so those attributes still surface in
// SuggestIndexes. A malformed constraint is ignored.
func (db *DB) RecordDemand(constraint string) {
	if constraint == "" {
		return
	}
	if q, err := vm.Parse(constraint); err == nil {
		db.c.RecordDemand(q.Probes())
	}
}

// MatchSorted returns job's matches ranked by the job's Rank, best first, at most
// limit (<=0 = all) -- the negotiator resource-request path, with the store's deferred
// materialization so only the returned top-N are built.
func (db *DB) MatchSorted(job *classad.ClassAd, limit int) []*classad.ClassAd {
	return db.c.MatchSorted(job, limit)
}

// RankedMatch is a matched ad with the job's Rank of it.
type RankedMatch = collections.RankedMatch

// MatchSortedRanked is MatchSorted that also returns each match's Rank. The job's
// Requirements prunes candidate slots via any covering index (the matchmaking
// pushdown) rather than bilaterally evaluating every slot.
func (db *DB) MatchSortedRanked(job *classad.ClassAd, limit int) []RankedMatch {
	return db.c.MatchSortedRanked(job, limit)
}

// MatchSortedRankedFiltered is MatchSortedRanked restricted to slots that also satisfy
// targetConstraint (a MATCH's WHERE TARGET / NOPREEMPT filter over the resource ad). The
// constraint's index probes narrow the candidate scan (pushdown) and it is re-checked on
// each matched slot. An empty constraint is exactly MatchSortedRanked.
func (db *DB) MatchSortedRankedFiltered(job *classad.ClassAd, targetConstraint string, limit int) ([]RankedMatch, error) {
	return db.c.MatchSortedRankedFiltered(job, targetConstraint, limit)
}

// MatchSignature is HTCondor's autocluster key: a 64-bit checksum over the given
// significant attributes' expression text in ad. Two ads with textually identical
// significant attributes (same Requirements, same RequestCpus literal, ...) hash
// equal, so a matchmaker can compute one candidate list per distinct signature
// and reuse it for every identical request.
func MatchSignature(ad *classad.ClassAd, significantAttrs []string) uint64 {
	return collections.ProjectionChecksum(ad, significantAttrs)
}

// Constraint is a compiled ClassAd boolean expression, for evaluating the same
// filter against many ads without re-parsing.
type Constraint struct{ q *vm.Query }

// ParseConstraint compiles a ClassAd boolean expression.
func ParseConstraint(expr string) (*Constraint, error) {
	q, err := vm.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", expr, err)
	}
	return &Constraint{q: q}, nil
}

// Matches reports whether ad satisfies the constraint.
func (c *Constraint) Matches(ad *classad.ClassAd) bool { return c.q.Matches(ad) }

// Ordered iterates one partition of the index-th configured ordered index in sort
// order (Config.Ordered), yielding each member ad with a resume cursor and its cluster
// signature (for run-length folding into resource-request lists). partition selects the
// run (e.g. an Owner); it is ignored for an index with no Partition. A zero resume
// starts at the beginning. The snapshot is O(1) and stable under concurrent churn.
func (db *DB) Ordered(index int, partition string, resume OrderCursor) iter.Seq[OrderedAd] {
	return db.c.Ordered(index, classad.NewStringValue(partition), resume)
}

// OrderedRedacted is Ordered for a caller NOT entitled to sealed values; see QueryRedacted.
func (db *DB) OrderedRedacted(index int, partition string, resume OrderCursor) iter.Seq[OrderedAd] {
	return db.c.OrderedRedacted(index, classad.NewStringValue(partition), resume)
}

// ConflictError reports the keys whose writes lost an optimistic write-write race at
// commit. The other writes in the transaction committed; the caller re-reads and
// retries the conflicted keys.
type ConflictError struct{ Keys []string }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("classad-db: %d key(s) conflicted: %v", len(e.Keys), e.Keys)
}

// Txn is an independent optimistic transaction. Operations are buffered and applied
// at Commit under snapshot-isolation OCC. A *Txn is not safe for concurrent use by
// multiple goroutines; independent transactions are.
type Txn struct {
	tx   *collections.Txn
	db   *DB
	done bool
	// patch records, per key, which attributes this transaction changed -- the information
	// the store needs to keep a delta instead of rewriting the whole ad. SetAttribute knows
	// the name it is setting, so nothing above this layer has to be told about deltas.
	patch map[string]*keyPatch
}

// keyPatch is one key's accumulated change set within a transaction. full marks a key whose
// whole ad was written (NewClassAd, or a Put), after which a delta would be wrong: the
// buffered ad is not derived from any stored base.
type keyPatch struct {
	changed []string
	removed bool
	full    bool
	// patch accumulates this transaction's changed attributes WITHOUT reading the stored ad,
	// and removed names what it deleted. Together they are all the store needs to write a
	// delta, so the common update costs no read at all.
	patch *classad.ClassAd
	// removals is a SET, not a list: a later SetAttribute of a removed name must take the name
	// back OUT, or the removal is applied after the patch and silently eats the set. It was
	// append-only, and `DeleteAttribute(k,"A"); SetAttribute(k,"A",...)` lost the set -- in
	// every configuration, delta mode or not, because these paths route through PatchAttrs
	// unconditionally.
	removals map[string]struct{}
}

// removalList renders the removal set for the store, which wants a slice.
func (p *keyPatch) removalList() []string {
	if len(p.removals) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.removals))
	for n := range p.removals {
		out = append(out, n)
	}
	return out
}

// patchFor returns key's change set, creating it on first touch.
func (t *Txn) patchFor(key string) *keyPatch {
	if t.patch == nil {
		t.patch = make(map[string]*keyPatch)
	}
	p := t.patch[key]
	if p == nil {
		p = &keyPatch{}
		t.patch[key] = p
	}
	return p
}

// putPatched buffers key's ad, as a delta when this transaction's writes to it are expressible
// as one and the store has delta records enabled.
func (t *Txn) putPatched(key string, ad *classad.ClassAd) {
	p := t.patch[key]
	if p == nil || p.full || len(p.changed) == 0 {
		t.tx.Put([]byte(key), ad)
		return
	}
	t.tx.PutPatch([]byte(key), ad, p.changed, p.removed)
}

// Begin starts a new independent transaction.
func (db *DB) Begin() *Txn { return &Txn{tx: db.c.Begin(), db: db} }

// Commit applies the buffered operations. It returns a *ConflictError if any key was
// modified by another committer since this transaction's snapshot (the non-conflicted
// operations still committed), or nil on full success.
func (t *Txn) Commit() error {
	t.done = true
	// The DB-wide lock, held shared: many commits proceed concurrently, but a Truncate
	// or Restore (exclusive) is atomic against them. A transaction whose snapshot predates
	// a Truncate additionally conflicts via the shard gcFloor, so a stale write cannot land
	// on the restored state even if it commits just after the exclusive section releases.
	// TryRLock takes the uncontended fast path with no clock read (the overwhelming common case).
	// It fails only when a Truncate/Restore holds -- or is waiting for -- the exclusive lock, so
	// the timed blocking acquire runs precisely when this commit is stalled behind a world-blocking
	// operation; that wait is otherwise recorded nowhere (SnapshotLock is the holder's time only).
	if !t.db.snapMu.TryRLock() {
		waitStart := time.Now()
		t.db.snapMu.RLock()
		t.db.snapLockWaitCount.Add(1)
		t.db.snapLockWaitNanos.Add(int64(time.Since(waitStart)))
	}
	res := t.tx.Commit()
	t.db.snapMu.RUnlock()
	if res.Conflicted() {
		keys := make([]string, len(res.Conflicts))
		for i, k := range res.Conflicts {
			keys[i] = string(k)
		}
		return &ConflictError{Keys: keys}
	}
	return nil
}

// CommitNondurable is Commit that defers the disk durability sync (classad_log.h
// CommitNondurableTransaction): the writes are visible immediately but their flush is
// batched to a later durable commit. On an in-memory DB it is identical to Commit.
func (t *Txn) CommitNondurable() error {
	t.tx.SetDurable(false)
	return t.Commit()
}

// Abort discards the transaction's buffered operations. Nothing is written.
func (t *Txn) Abort() { t.done = true }

// NewClassAd stores ad under key (classad_log.h LogNewClassAd). An existing ad at
// key is replaced.
func (t *Txn) NewClassAd(key string, ad *classad.ClassAd) {
	// A whole new ad: nothing about it derives from a stored base, so neither this write nor
	// any later write to the same key in this transaction may be stored as a delta.
	t.patchFor(key).full = true
	t.tx.Put([]byte(key), ad)
}

// NewClassAdOld stores an ad supplied as old-ClassAd text under key, encoding it
// straight to the stored wire form instead of parsing it into a ClassAd for the commit
// to re-encode. The transaction is unaffected: the write is buffered, conflict-checked
// and committed exactly as NewClassAd's is.
//
// It reports whether the wire-native path was taken. False means the caller must parse
// the text and use NewClassAd -- an encrypted store seals its values and the streaming
// encoder does not seal, and a few ad shapes (a repeated attribute name, an escape the
// fast lexer would read differently) defer to the reference parser by design.
func (t *Txn) NewClassAdOld(key, text string) bool {
	return t.tx.PutOld([]byte(key), text)
}

// DestroyClassAd removes key (classad_log.h LogDestroyClassAd).
func (t *Txn) DestroyClassAd(key string) {
	// Mark the key full for the rest of the transaction. Without this a following
	// SetAttribute took the patch path, whose PatchAttrs REPLACED the buffered delete and then
	// merged over the pre-delete stored ad -- so the destroy vanished and the old attributes
	// came back. Marking it full routes any later write through the read-modify-write path,
	// which composes with the delete correctly.
	p := t.patchFor(key)
	p.full = true
	p.patch = nil
	p.changed = nil
	t.tx.Delete([]byte(key))
}

// SetAttribute sets one attribute of key to the expression parsed from expr
// (classad_log.h LogSetAttribute) -- a read-modify-write within the transaction, so
// it composes with the transaction's own earlier writes to key. The ad is created if
// absent.
func (t *Txn) SetAttribute(key, name, expr string) error {
	e, err := classad.ParseExpr(expr)
	if err != nil {
		return fmt.Errorf("classad-db: SetAttribute %s[%s]: %w", key, name, err)
	}
	p := t.patchFor(key)
	if p.full {
		// The whole ad was written in this transaction, so it is already buffered and the
		// update composes with it directly -- there is nothing to read.
		ad, ok := t.tx.Get([]byte(key))
		if !ok {
			ad = classad.New()
		}
		ad.InsertExpr(name, e)
		p.changed = append(p.changed, name)
		t.putPatched(key, ad)
		return nil
	}
	// No read: buffer the changed attribute and let the store decide whether it can be
	// stored as a delta. This is the whole point of delta records -- the old path read the
	// stored ad back on EVERY SetAttribute purely to hand a copy to the encoder.
	if p.patch == nil {
		p.patch = classad.New()
	}
	p.patch.InsertExpr(name, e)
	delete(p.removals, name) // this transaction set it again: it is no longer removed
	p.changed = append(p.changed, name)
	t.tx.PatchAttrs([]byte(key), p.patch, p.removalList())
	return nil
}

// DeleteAttribute removes one attribute of key (classad_log.h LogDeleteAttribute).
// A no-op if key or the attribute is absent.
func (t *Txn) DeleteAttribute(key, name string) {
	p := t.patchFor(key)
	if p.full {
		ad, ok := t.tx.Get([]byte(key))
		if !ok {
			return
		}
		if ad.Delete(name) {
			p.removed = true
			t.putPatched(key, ad)
		}
		return
	}
	// Recorded without reading. A removal cannot be expressed as a delta of present
	// attributes, so the store composes a whole record for this key at commit -- which is
	// also where it learns whether the attribute was there at all, making this the no-op
	// the contract promises when it was not.
	p.removed = true
	if p.removals == nil {
		p.removals = make(map[string]struct{})
	}
	p.removals[name] = struct{}{}
	if p.patch == nil {
		p.patch = classad.New()
	}
	p.patch.Delete(name)
	t.tx.PatchAttrs([]byte(key), p.patch, p.removalList())
}

// LookupClassAd returns key's ad as the transaction sees it: its own buffered writes
// (read-your-writes) merged over the snapshot (classad_log.h Lookup + the
// LookupInTransaction overlay in one call).
func (t *Txn) LookupClassAd(key string) (*classad.ClassAd, bool) {
	return t.tx.Get([]byte(key))
}

// Has reports whether key exists as the transaction sees it (its own buffered writes over
// the snapshot, exactly as LookupClassAd does) without decoding the stored record. Use it
// wherever only presence matters: LookupClassAd's decode of a wide ad dominates the cost of
// an ingest that asks the question once per operation.
func (t *Txn) Has(key string) bool {
	return t.tx.Has([]byte(key))
}

// Query returns the ads matching the constraint as the transaction sees them: the
// committed rows with the transaction's own buffered writes overlaid, so a query inside
// a transaction observes work the transaction has not committed yet. DB.Query cannot --
// it reads the committed store -- which is why a caller that has staged writes and then
// wants to read them back must go through the transaction.
//
// Isolation is snapshot plus read-your-writes: the committed half is read at the
// transaction's own snapshot sequence, the same one Get reads at, so a scan and a point
// lookup in one transaction agree and a concurrent commit is invisible to both. The cost
// is a full scan -- reading at a past sequence and overlaying by key both need the
// per-record walk the indexed query path skips. See collections.Txn.Query. Errors only on
// a malformed constraint.
func (t *Txn) Query(constraint string) (iter.Seq[*classad.ClassAd], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return t.tx.Query(q), nil
}

// KeysWhere returns the storage keys of the rows matching the constraint as the
// transaction sees them (DB.KeysWhere with the transaction's writes overlaid). It is
// what lets an UPDATE or DELETE inside a transaction address a row the transaction
// itself created. Errors only on a malformed constraint.
func (t *Txn) KeysWhere(constraint string) (iter.Seq[string], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return t.tx.KeysWhere(q), nil
}

// LookupAttr returns the unparsed expression of one attribute as the transaction
// sees it (classad_log.h LookupInTransaction), or ("", false).
func (t *Txn) LookupAttr(key, name string) (string, bool) {
	ad, ok := t.tx.Get([]byte(key))
	if !ok {
		return "", false
	}
	e, ok := ad.Lookup(name)
	if !ok {
		return "", false
	}
	return e.String(), true
}

// GroupSchemaInfo, GroupSchemaEntry, GroupSchemaDrift and GroupSchemaAgreement are re-exported so
// a caller can read the group-schema reports without importing collections.
type GroupSchemaInfo = collections.GroupSchemaInfo
type GroupSchemaEntry = collections.GroupSchemaEntry
type GroupSchemaDrift = collections.GroupSchemaDrift
type GroupSchemaAgreement = collections.GroupSchemaAgreement
type GroupSchemaChange = collections.GroupSchemaChange
type GroupSchemaDelta = collections.GroupSchemaDelta
type GroupSchemaLastAgreement = collections.GroupSchemaLastAgreement
type GroupAgreementItem = collections.GroupAgreementItem

// GroupSchemas derives and reports candidate group schemas: sets of attributes the base schema
// does not carry which are present or absent together, and could therefore be stored columnar for
// the ads that have them without costing a slot in the ads that do not. Report-only.
func (db *DB) GroupSchemas(sampleMax, k int) GroupSchemaInfo {
	return db.c.GroupSchemas(sampleMax, k)
}

// GroupSchemaDrift reports how the derived groups have moved across retained derivations.
func (db *DB) GroupSchemaDrift() GroupSchemaDrift { return db.c.GroupSchemaDrift() }

// GroupSchemaAgreement reports how well per-segment derivations agree with the table-wide one.
func (db *DB) GroupSchemaAgreement(sampleMax, k int) GroupSchemaAgreement {
	return db.c.GroupSchemaAgreement(sampleMax, k)
}

// GroupSchemaChanges returns the committed-group change log: the moments the accelerator adopted a
// different set of secondary schemas, with each change's diff and reason. Reads persisted state only
// (no sampling), so it is available at READ.
func (db *DB) GroupSchemaChanges() []GroupSchemaChange { return db.c.GroupSchemaChanges() }

// GroupSchemaLastAgreement returns the last persisted per-segment agreement result, if any.
func (db *DB) GroupSchemaLastAgreement() (GroupSchemaLastAgreement, bool) {
	return db.c.GroupSchemaLastAgreement()
}

// DeltaStats reports how many of this table's writes were stored as delta records versus in
// full. Both zero means delta records are off (or nothing has been written). It is the only way
// to tell a working delta store from one that has quietly fallen back to storing whole ads --
// which reads back perfectly correctly and saves nothing.
func (db *DB) DeltaStats() (deltas, fulls int64) { return db.c.DeltaStats() }

// FallbackReasons reports, process-wide, why patch writes had to store a whole record rather than
// a delta: an attribute removal (which a delta cannot express), the chain reaching its bound, no
// whole record to chain to yet, or delta records not being in use. Each fallback costs a read of
// the stored ad, so this says which of those reads are worth attacking.
func FallbackReasons() (removal, bound, noBase, ineligible int64) {
	return collections.FallbackReasons()
}

// CompactInternPaths reports, process-wide, how compaction re-interned records: by transcoding the
// wire bytes, or by decoding into an ast and encoding it back. All-ast means the transcode is being
// attempted and refused on every record.
func CompactInternPaths() (wirePath, astPath int64) { return collections.CompactInternPaths() }

// SpliceStats reports, process-wide, how delta merges were served: by splicing attribute bytes,
// by decoding after the splice refused, and by decoding because the caller wanted an object
// rather than bytes. All three matter -- a merge that never attempts a splice and one that
// attempts and refuses look identical in a one-number report.
func SpliceStats() (spliced, spliceRefused, objectPath int64) { return collections.SpliceStats() }

// TrackedKeys reports how many keys the delta tracker holds in memory (0 when delta records
// are off). It is the feature's one new in-memory structure, so it is the number to watch.
func (db *DB) TrackedKeys() int { return db.c.TrackedKeys() }
