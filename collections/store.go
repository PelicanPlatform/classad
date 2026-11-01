package collections

import (
	"fmt"
	"iter"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// codecHolder wraps a Codec so it can be swapped atomically (atomic.Pointer is
// type-safe; atomic.Value would panic when the concrete codec type changes, e.g.
// identityCodec -> *zstdCodec after RetrainDict).
type codecHolder struct{ c Codec }

// Options configures a Collection.
// schemaSeedState is a schema and hot set recovered without their blocks.
type schemaSeedState struct {
	schema *adSchema
	hot    []int
}

type Options struct {
	// Shards is the number of independently-locked shards; rounded up to a power
	// of two. Default 16. Forced to 1 when AppendOnly (an append log is a single
	// ordered sequence: it needs one segment chain, not sharded key routing).
	Shards int
	// AppendOnly makes the collection a pure append log: every Put appends a new
	// record (no per-key supersession, no directory, no compaction, no deletes) --
	// the model a history archive needs. Records accumulate in commit order and are
	// reclaimed only by whole-segment retention, not compaction. Scan/Query see every
	// appended record; Get/Delete-by-key are not meaningful (there is no key index).
	AppendOnly bool
	// ReverseScan makes Scan/Query yield records newest-first (most recently
	// appended first) instead of in stored order. It walks segments from the last
	// (newest) backward and, within a segment, records back-to-front -- the natural
	// "last K" order a history archive presents (condor_history shows the most
	// recent jobs first). A consumer that stops early (yield/emit returns false)
	// after K records therefore gets the K newest, a pushed-down LIMIT. Reverse
	// scans always run serial (never fan out). Default false ⇒ stored order.
	// Most meaningful with AppendOnly, where stored order is commit order.
	ReverseScan bool
	// Retention bounds how much an AppendOnly collection keeps: Rotate(now) drops
	// whole oldest segments until the collection is back within these bounds (a
	// history archive's aging-out). The zero value keeps everything (Rotate is
	// then inert). Only meaningful with AppendOnly. See Rotate.
	Retention Retention
	// IndexBackfillBytes bounds how far back Reindex carries an index configuration change.
	// When set, Reindex rebuilds the existing index of only the segments within the newest
	// IndexBackfillBytes; older ones keep the index they already have until BackfillIndexes
	// (or ReindexAll) reaches them.
	//
	// The point is that rebuilding is not free -- it decompresses every record it covers --
	// while an out-of-date index is still perfectly usable for the attributes it does hold
	// (recovery uses whatever a segment's index covers, deciding per probe). Reindex runs
	// after every seal of an archive and inside every AddIndex, so it must not pay for the
	// whole history; BackfillIndexes pays for it a budgeted slice at a time instead.
	//
	// 0 carries a change across every segment, which is the historical behaviour.
	IndexBackfillBytes int64

	// InternAtSeal makes an AppendOnly collection intern each segment as soon as it
	// seals (when the active segment fills), rather than only when RetrainDict/Rewrite
	// later reseal it. Interning stores segment-local ids + a per-segment name dictionary
	// instead of inline names, halving raw mmap bytes and speeding decode (see the
	// interning design). This trades a per-seal transcode (off the write lock) for the
	// density/decode win landing immediately on an archive that never retrains. Only
	// meaningful with AppendOnly (a mutable segment's records can be superseded in place,
	// which an off-lock transcode would race -- there interning rides compaction instead).
	// Optional; default off preserves exactly today's behavior. See InternSealed.
	InternAtSeal bool
	// ZoneAttrs names numeric attributes to keep a per-segment [min,max] zone map on,
	// so a range/equality query on one (e.g. CompletionDate > T) skips whole sealed
	// segments whose span cannot match, and age-based Retention (MaxAgeAttr) can drop
	// a segment by its newest timestamp. Value-indexed attributes (ValueAttrs) are
	// zoned automatically. Only meaningful with AppendOnly. Optional. See zonemap.go.
	ZoneAttrs []string
	// SegmentSize is the arena segment size in bytes. Default 8 MiB.
	SegmentSize int
	// Hasher routes keys to shards / directory buckets. Default 64-bit FNV-1a.
	Hasher Hasher
	// Codec compresses stored ad bytes. Default identity (no compression).
	Codec Codec
	// DeltaMax turns on DELTA RECORDS and bounds the chain: it is IGNORED on an AppendOnly
	// collection, where there is no earlier version of a key to merge with and the tracker
	// would grow per record forever. A write that names the attributes
	// it changed stores only those, until a key has accumulated DeltaMax deltas, at which
	// point the next write stores the whole ad again. 0 (default) stores whole ads always,
	// which is the historical behavior.
	//
	// It trades write volume for read work. A live job ad averages ~7.3 KB while one
	// transaction's changes average ~150 B, so writes shrink by ~49x; a point read has to
	// merge up to DeltaMax fragments over a full record. Requires inline-name records (a
	// persistent store) and is not combined with columnarization, which assumes each record
	// holds a whole ad.
	DeltaMax int
	// HotAttrs names the "popular" attributes to front-load in each ad's hot
	// header, so a query filtering on them resolves each in O(1) instead of
	// scanning the ad body. Typically the attributes common queries filter on
	// (e.g. Cpus, Memory, Arch, OpSys, State). Optional.
	HotAttrs []string
	// MatchClosureRoots names attributes (typically "Requirements") whose transitive
	// self-reference closure is front-loaded into each ad's hot header at encode time,
	// so matching a wide ad reads only the match-relevant attributes (via the hot
	// header) instead of decoding the whole ad. Optional; enables the hot-closure match
	// fast path. The frequency/HotAttrs hot set is unioned in, so queries are unaffected.
	MatchClosureRoots []string
	// SchemaRequireAttrs names attributes that enter the derived columnar schema whatever their
	// presence rate -- the override on the rule that an attribute becomes a fixed-slot field only
	// when its dominant storable kind appears in at least 90% of sampled records.
	//
	// That floor bounds a real cost (a slot is bytes in EVERY record, including those that lack
	// the attribute) and it is the right default, but it is the only test of membership: the
	// demand ranking that picks the hot tier chooses among fields the schema already has, so no
	// amount of reading ever admits one. An attribute written only once a record reaches some
	// state can sit far below the floor and still be the most-queried attribute in the workload.
	//
	// Required bypasses PRESENCE only. The attribute must still have a dominant storable literal
	// kind -- one that is usually an expression is refused, because a slot that escapes in every
	// record is strictly worse than no slot: the value is read from the record either way, every
	// record pays the escape bit and the layout, and a numeric column of escaped expressions makes
	// the vectorized count hand the query back per record. An int's width is still chosen by Fit
	// from the sampled values. One with no occurrences at all
	// is skipped, since there is no kind or width to lay a slot out with. Both refusals are
	// reported (SchemaRequireSkipped, SchemaScanInfo.RequireSkipped).
	//
	// Nothing in this package reads an attribute NAME. Which attributes deserve a slot they have
	// not earned by presence depends on the lifecycle of the records being stored, which only the
	// use case knows; this is the knob it supplies them through.
	//
	// Changing the set on a live collection re-derives nothing by itself; the next maintenance
	// review (BuildAndEnableSchemaScan) adopts the schema it produces and converges the sealed
	// segments onto it, ColumnarSegmentBudget per pass. See SetSchemaRequireAttrs.
	SchemaRequireAttrs []string
	// GroupSchemaCount is how many SECONDARY columnar schemas to derive and build alongside the
	// base one, for attributes the base schema does not carry which are present or absent
	// together. 0 uses defaultGroupSchemas; a NEGATIVE value builds none.
	//
	// On by default because the base schema's coverage is otherwise hostage to the mix of ads in
	// the table: measured on a production AP, a history table's base schema covers 90.3% of
	// attribute occurrences until jobs removed before ever running are mixed in, at which point
	// it falls to 51.3%, while base plus four groups holds 87-94% across the range. Enabling by
	// default still commits no storage on the strength of one sample -- nothing is built until a
	// group's members have kept recurring across GroupStabilityRuns maintenance passes.
	GroupSchemaCount int
	// ColumnarSegmentBudget is how many sealed segments one maintenance pass may rewrite into
	// COLUMNAR-NATIVE form -- each attribute the schema carries stored once, in the segment's own
	// columnar payload, and removed from the records (see colnative.go). 0 uses
	// defaultColumnarBudget; a NEGATIVE value disables the rewrite and keeps every segment
	// whole-record with a columnar copy beside it in the sidecar.
	//
	// On by default because the duplication is pure cost: without it every value the schema
	// carries is stored twice, row-form in the arena and again in the sidecar block. Measured on
	// 1500 real OSPool machine ads, moving them removed 43% of the table (4104 -> 2327 bytes per
	// record), and the sidecar megabyte it reclaimed was entirely the duplicate copy.
	//
	// Budgeted rather than unbounded because turning it on over an existing archive rewrites every
	// sealed segment once, and at history scale doing that in a single pass is hours of I/O and a
	// flushed page cache -- the same failure a retrain caused when it resealed a whole archive.
	// Each segment is rewritten at most once, so a bounded budget still converges.
	ColumnarSegmentBudget int
	// RemnantDictionary decides whether columnarizing a segment also stores, per row group, the
	// non-literal values its records repeat -- expressions, nested records and lists such as a
	// cluster's shared Requirements -- once, with each record holding a short reference in their place
	// (see colremdict.go). Measured on a production AP's history, that removed 13-30% of columnarized
	// segment bytes (8-16% on epoch history). 0 takes the default, which is ON; a negative value turns
	// it off, and a positive one on.
	//
	// What an older build does with such a segment: it refuses to open the collection
	// (ErrUnsupportedColumnarVersion) rather than misread a reference, and its fsck reports the segment's
	// payload version as unsupported. Nothing is lost or modified, and rolling forward reads every
	// record. A caller that must stay openable by a build that predates dictionaries sets this negative.
	// Every build from this one on reads them whatever this is set to.
	RemnantDictionary int
	// RowGroupBytes is the uncompressed record-bytes budget for one columnar row group. 0 uses
	// colGroupTargetBytes (128 KiB).
	//
	// This is the knob that decides how much of a segment has to be decompressed to read a single
	// record, against how far compression can see across records -- larger groups compress better and
	// cost more per point lookup. The default is measured (see colGroupTargetBytes); it is exposed
	// because the right point depends on the read mix and on how much of the working set fits the
	// block cache, which a large production table is the only honest place to find out.
	//
	// The row cap and the 8-row group alignment are deliberately not configurable.
	RowGroupBytes int
	// GroupStabilityRuns is how many consecutive derivations a group's members must have
	// co-occurred in before its blocks are built. 0 uses the default; 1 disables the gate.
	GroupStabilityRuns int
	// GroupMergeJaccard widens a group by absorbing attributes whose presence pattern is at least
	// this similar. 0 uses defaultGroupJaccard; a NEGATIVE value keeps exact co-occurrence only.
	// GroupMaxPartialFrac bounds the fraction of ads that may then hold only PART of a group and
	// take the slow path for it; 0 uses defaultGroupMaxPartial.
	//
	// Measured against a later snapshot of the same table, widening still recovered more than
	// exact grouping (25.14% of attribute occurrences against 23.53%), because a partial ad reads
	// from the cold tail -- where it would have read anyway -- rather than paying a penalty. The
	// in-sample partial rate does NOT predict the later one, though, and no widened member set
	// reproduced across snapshots, so the stability gate refuses to build one. See
	// mergeNearPatterns.
	GroupMergeJaccard   float64
	GroupMaxPartialFrac float64
	// DemandHalfLife is how quickly recorded query demand fades, so index decisions
	// track the current workload rather than everything the process has ever seen. 0
	// uses defaultDemandHalfLife; negative disables decay (counters accumulate for the
	// lifetime of the data, as they did before decay existed).
	DemandHalfLife time.Duration
	// CommitSync, if set, is called once per committed (possibly group-coalesced)
	// batch on a shard, after the writes are applied — the point at which a future
	// durable collection would fsync/serialize the batch. Group commit amortizes
	// this across all writers that commit together. It must be safe for concurrent
	// use across shards. Optional; default no-op (in-memory store).
	CommitSync func()
	// CategoricalAttrs names string-valued attributes to index for equality and
	// set-membership (`Attr == "x"`, `Attr == "x" || Attr == "y"`). ValueAttrs
	// names numeric attributes to index for equality and range (`Attr >= n`). A
	// query filtering on an indexed attribute visits only candidate ads instead of
	// scanning all of them. Optional.
	CategoricalAttrs []string
	ValueAttrs       []string
	// Ordered configures maintained, filtered, ordered indexes -- the schedd
	// priority-queue pattern (see docs/MATCH.md §3 and Collection.Ordered). Each
	// spec groups members into partitions and keeps them sorted on the write path,
	// so a negotiator can iterate a partition in order (and resume) without
	// re-sorting each cycle. A malformed expression in a spec panics New. Optional.
	Ordered []OrderSpec
	// DisableReadVerification turns OFF the check that a record's stored checksum
	// matches its bytes before they are used.
	//
	// Verification is ON by default, which is why this option is phrased as a
	// negative: Go's zero value is false, and the safe setting is the one you get
	// without thinking about it. Without the check, a bit flip inside a sealed
	// record's encoded ad is served as a plausible value -- Owner "urer3" where
	// "user3" was written -- with no error anywhere, because the checksum that would
	// catch it was only ever read at recovery.
	//
	// It covers point reads and the iterator path every scan and query goes through
	// (wire/wireAt and adBytes, the two places the package already reassembles a
	// partial record), so one switch governs both. A refused record is skipped, never
	// half-served, and counted by VerifyReadSkips -- a shrunken answer has to be
	// observable, since "fewer rows came back" is otherwise indistinguishable from
	// "there were fewer rows".
	//
	// The cost is about 17ns a record: a hardware CRC-32C over a 160-byte record,
	// against roughly 760ns to decode one on a scan. It adds NO storage -- the
	// checksum has always been written. Turn this on only for a read-bound workload
	// that has measured the difference and prefers a wrong answer to a slow one.
	//
	// What it is worth depends on the CODEC, and the difference is stark. A ZSTD frame
	// carries its own content checksum, so a flipped bit in a compressed ad is refused
	// either way and verification adds little for the ad body (it still covers the key,
	// the framing and the commit sequence, which no codec touches). Under the identity
	// codec nothing covers the ad, and the record CRC is the only thing between a
	// flipped bit and a plausible wrong value: Owner "urer3" where "user3" was written.
	// See TestVerificationValueDependsOnTheCodec, which measures both.
	//
	// That is not an academic distinction. db's chooseBaseCodec gives NEW stores ZSTD
	// but keeps identity for one that already held data, so the stores where this is
	// load-bearing are the oldest -- usually the largest and busiest.
	DisableReadVerification bool

	// Dir, if set, makes the collection persistent: arenas are memory-mapped files
	// under this directory and committed writes are flushed to disk (see Open).
	// Empty ⇒ in-memory (the default). Unix-only.
	Dir string
	// QueryParallelism controls cross-segment fan-out for large full-scan queries:
	//   0 (default) ⇒ auto — the library picks the policy (currently: up to 6 workers
	//                 per query, clamped to GOMAXPROCS). The meaning of auto may change
	//                 across releases.
	//   1           ⇒ serial (never fan out).
	//   N ≥ 2       ⇒ fan out with up to N workers per query.
	// In every non-serial case fan-out is still gated by a work-size threshold, never
	// exceeds the segment count, and draws from a machine-wide worker budget shared
	// across concurrent queries (so it degrades to serial under load rather than
	// oversubscribing). Small scans and indexed queries run serial regardless.
	// See parallel_scan.go / docs/PARALLEL_QUERY.md.
	QueryParallelism int
	// WatchHistory enables the Watch subscription verb (see docs/WATCH.md). It is the
	// per-shard capacity of the delete journal — how many recent deletes are retained
	// so a resuming watcher can be told precisely which keys were removed. 0 (default)
	// disables Watch entirely (no journal, no live notification, zero write-path cost).
	// A larger value lets clients resume incrementally from further back before
	// falling to a full replay.
	WatchHistory int
	// WatchBuffer is the per-watcher live event-channel capacity (default 1024). A
	// watcher that overflows it is told to resync rather than stalling writers.
	WatchBuffer int
	// WatchCoalesce, if > 0, batches live Watch events into windows of this
	// duration and emits only the newest event per key in each window (default 0 =
	// off, one event per change). It smooths bursty churn -- e.g. a freshly
	// submitted job that takes many SetAttribute updates in quick succession is
	// delivered as a single Upsert of its settled state. Only the last event of a
	// flushed window carries a cursor, so a mid-window consumer crash resumes from
	// the prior window and re-delivers (at-least-once is preserved). Catch-up is
	// never coalesced.
	WatchCoalesce time.Duration

	// ParentKeyFor, if set, gives an ad a parent: it maps a child's key to its
	// parent's key (return nil for a top-level ad with no parent). A child then
	// resolves attribute references it lacks by falling through to its parent --
	// the primitive "join" the job queue needs, where a proc ad chains to its
	// cluster ad so a query like `DAGManJobId == 42` sees the cluster's attribute.
	// A family (a root and all its descendants) is co-located in one shard -- the
	// collection routes every key to the shard of its ultimate root -- so chained
	// evaluation is consistent and lock-free. Parent bindings are immutable: a
	// key's parent must not change across updates. Default nil disables chaining
	// (every ad is standalone) and keeps the plain single-key routing/scan.
	ParentKeyFor func(key []byte) []byte

	// IsStructural, if set, marks a key as a structural (parent-only) ad that
	// exists solely to be chained to -- e.g. a job cluster ad. Structural ads are
	// stored and used as parents but are hidden from Query/Scan/Watch results by
	// default (like condor_q omitting cluster ads). Default nil treats every ad as
	// a normal, visible ad. Only meaningful together with ParentKeyFor.
	IsStructural func(key []byte) bool

	// ParentPrivateAttrs are parent attributes children do NOT inherit and that do
	// NOT trigger a watch fan-out when they change -- e.g. a job cluster ad's
	// factory bookkeeping (JobMaterializeNextProcId, ...), which mutates per proc
	// materialized but is meaningless to a proc. They are excluded from flattening
	// and from the parent-change diff, so high-frequency parent-internal churn does
	// not fan out to every child. Case-insensitive. Only meaningful with ParentKeyFor.
	ParentPrivateAttrs []string

	// DataKey enables encryption at rest: the named attributes' values are sealed with
	// AES-256-GCM under this key before being written to a segment. It must be the DB
	// master key's DataInfo subkey (crypt.Subkey(master, crypt.DataInfo)) -- distinct
	// from the master itself -- so a stolen database is useless without a pool key.
	// Empty ⇒ encryption disabled (EncryptedAttrs is then inert). See encrypt.go.
	DataKey []byte
	// EncryptedAttrs names the attributes to encrypt at rest (case-insensitive). Only
	// meaningful with DataKey set. An encrypted attribute may NOT also be indexed
	// (CategoricalAttrs/ValueAttrs) -- New panics on overlap -- since its value is
	// opaque at rest. The set can be changed at runtime via the toggle meta-command;
	// existing records keep their prior form until MigrateSealedAttrs rewrites them.
	//
	// An encrypted attribute's value must not depend on other attributes: it should be a
	// literal -- a token or a secret string, as every private attribute is. Reads rely on
	// this and treat a sealed value as referring to nothing: a reference inside one is not
	// followed by the projections that close over references (QueryRawProjected with
	// chaseRefs, QueryRawWireRefs), nor by the narrowed or partial decode a query is
	// evaluated on, so such a value is evaluated as if the attributes it names were absent
	// unless the read loaded them for another reason.
	EncryptedAttrs []string

	// TimeTravel, if non-nil, enables point-in-time ("AS OF") queries: superseded
	// record versions committed within MaxDistance of now are retained (not reclaimed
	// by compaction), and a sparse time->seq checkpoint is recorded so a wall-clock
	// time resolves to the commit sequence that was current then. Nil (default)
	// disables it with zero write-path cost -- retention collapses to the current seq,
	// exactly as before. See timeseq.go. It can be toggled at runtime.
	TimeTravel *TimeTravelOptions

	// SeqCursorLease is how long a paged read (QueryRawFromSeq) keeps a shard's snapshot
	// readable after taking it: compaction retains the versions it sees, or in a collection
	// with delta records defers reclaiming the shard, for at most this long. A cursor resumed
	// after maintenance has reclaimed past its snapshot gets ErrSeqCursorExpired rather than
	// a short listing. Zero means 10 minutes; negative disables the lease, leaving only the
	// refusal. See seqlease.go.
	SeqCursorLease time.Duration

	// MutatingBlockCacheBytes and ArchiveBlockCacheBytes set the PROCESS-GLOBAL shared
	// decompressed-columnar-block cache budgets, in bytes, for the two workload kinds: one budget
	// shared by ALL mutating (sharded, key-superseding) collections, and a separate one shared by ALL
	// append-only ARCHIVE collections. This replaces the old fixed 256 MB-per-collection cache, whose
	// footprint grew as 256 MB × (number of columnarized tables) with no knob. Both are global
	// ceilings, not per-collection budgets. A value ≤ 0 leaves the current/default budget
	// (defaultMutatingCacheBytes / defaultArchiveCacheBytes, 512 MiB each). Setting either here is
	// equivalent to calling Set{Mutating,Archive}BlockCacheBudget; the last non-zero setting wins and
	// resizes the live shared cache. Because the budget is global, prefer setting it once (e.g. from
	// db/ at startup) rather than per table. An archive collection uses the archive budget; every
	// other collection uses the mutating budget.
	MutatingBlockCacheBytes int64
	ArchiveBlockCacheBytes  int64
}

// TimeTravelOptions configures point-in-time queries for a collection (see the
// TimeTravel option and timeseq.go).
type TimeTravelOptions struct {
	// MaxDistance is how far back queries may travel: versions superseded more than
	// this long ago are eligible for reclamation. Larger keeps more history (more
	// retained dead bytes). Must be > 0.
	MaxDistance time.Duration
	// CheckpointInterval is the granularity of the time->seq map: a checkpoint is
	// recorded at most once per interval, on the first commit that crosses the
	// boundary, so an AS OF time resolves to within one interval. Default 1 minute;
	// lower for finer resolution at proportionally more checkpoints.
	CheckpointInterval time.Duration
}

// AdUpdate is one insert-or-update in a batch.
type AdUpdate struct {
	Key []byte
	Ad  *classad.ClassAd
}

// Collection is an in-memory, sharded, memory-dense store of ClassAds. It is safe
// for concurrent use.
type Collection struct {
	shards []*shard
	mask   uint64
	h      Hasher
	// maintStop is set by StopMaintenance to ask an in-flight maintenance pass to stop at
	// its next safe boundary, so closing does not wait out a whole bounded pass.
	maintStop atomic.Bool
	// maintProg is the in-flight maintenance pass's published progress, or nil when no pass
	// is running. Deliberately an atomic pointer and NOT guarded by maintMu: it exists to be
	// read while a pass holds that lock. See maintprogress.go.
	maintProg atomic.Pointer[MaintProgress]
	// ctQueried is set once this process has run a query reading CurrentTime, from which point
	// Reindex proves the segments Open loaded without a CurrentTime proof (ctproof.go).
	ctQueried atomic.Bool
	// openIdxDiag accumulates, during Open's loadShard passes, how each sealed segment's
	// persisted index sidecar was handled (adopted vs. to-be-rebuilt). Emitted once at the end
	// of Open via OpenIndexDiagHook. Written only on the single-threaded Open path.
	openIdxDiag OpenIndexDiag
	// recWalked counts records examined one at a time by the phases that walk them: extent
	// recovery, the directory rebuild, the searches for a segment's dictionary and columnar
	// payload records, index and zone builds, and the seal-collapse check. Open reads it into
	// OpenTiming.RecordsWalked, which is how the "open is O(segments), not O(records)"
	// invariant is actually checked.
	recWalked atomic.Int64
	codec     atomic.Pointer[codecHolder] // current codec for new writes; swapped by RetrainDict
	// deltaMax and deltas implement delta records (see delta.go). deltaMax 0 disables them.
	deltaMax int
	deltas   *deltaTracker
	// deltaDir is the store's directory, retained so the delta-mode marker can be written
	// lazily on the first delta rather than at open; deltaMarked/deltaMarkMu make that happen
	// once. Empty for an in-memory collection, which persists nothing.
	deltaDir    string
	deltaMarked atomic.Bool
	deltaMarkMu sync.Mutex
	// deltaRead enables REPLAY. It is set from the on-disk marker as well as from DeltaMax,
	// because a store that already holds deltas must replay them even if this process was not
	// told to write any. See initDeltaMode.
	deltaRead bool
	// collapsing guards collapseSealedChains against re-entering itself: the whole records it
	// writes can seal another segment, which flags the shard again.
	collapsing atomic.Bool

	// collapseRetry holds keys whose collapse did not complete -- a commit conflict, or a
	// materialize that failed -- so a later pass tries again. It exists because losing one is
	// not recoverable: liveDeltaKeys CONSUMES sh.pendingSeal, so a key skipped after its
	// segment sealed is in a segment no future scan looks at, and its live delta stays in a
	// sealed segment forever. That is the one thing the seal-collapse invariant promises will
	// never happen, and everything that rewrites a sealed segment relies on it.
	collapseMu    sync.Mutex
	collapseRetry map[string]struct{}
	// regionCodecCache holds the dictionary-less codec a columnar block's regions are compressed
	// with (see regionCodec). Created on first use and never swapped, so a block built at any point
	// in this collection's life decodes with the same codec.
	regionCodecCache atomic.Pointer[codecHolder]
	intern           *wire.InternTable            // shared attribute-name interning across the store
	hotSet           atomic.Pointer[hotSetHolder] // interned ids to front-load; swapped by RefreshHotSet
	spec             atomic.Pointer[indexSpec]    // configured indexes; swapped by AddIndex/DropIndex (never nil after New)

	// matchRoots (case-folded) are the roots whose closure is front-loaded into the
	// hot header for the match fast path (Options.MatchClosureRoots); nil if disabled.
	matchRoots []string
	dir        string // persistence directory; "" ⇒ in-memory (see persist.go)

	// inline is set for a persistent collection: records store attribute names
	// inline (no interning), so segment files are self-contained and recoverable.
	// hotNames (case-folded) drives the hot header in inline mode; swapped atomically by
	// RefreshHotSet/AddHotAttrs (the write path reads it). See inline.go.
	inline   bool
	hotNames atomic.Pointer[hotNamesHolder]

	// dicts tracks the ZSTD dictionaries trained over the collection's life so each
	// persistent segment can be tagged (in its file name) with the dictionary it was
	// compressed under, and recovery can reconstruct the right codec per segment.
	// Non-nil after New; persists dictionary bytes only when the collection has a Dir.
	dicts *dictReg

	// maintMu serializes whole-collection maintenance passes -- Compact and RetrainDict
	// -- which htcondordb drives from SEPARATE tickers. Unserialized they race twice
	// over: two concurrent compactShard calls on one shard duplicate its working set,
	// and Compact's captured currentCodec can be pruned mid-pass by a concurrent
	// retrain (orphaning the new segments' dictionary, which recovery then cannot
	// load). Commits and queries never take it.
	maintMu sync.Mutex
	// hzMu serializes writes of the horizon file (horizon.go).
	hzMu sync.Mutex

	// reindexMu serializes Reindex so concurrent callers (the archive's eager per-seal
	// reindex, the periodic maintenance reindex, and compaction's reindexAfterCompaction)
	// cannot both build and install the same segment's sidecar at once. Distinct from
	// maintMu, which retrain/rotate/rewrite hold across their whole pass (and which those
	// ops' internal reindexAfterCompaction must not re-take).
	reindexMu sync.Mutex

	demand     *demandTracker // per-attribute query demand, for SuggestIndexes
	demandHalf time.Duration  // Options.DemandHalfLife, verbatim (0 = default, <0 = no decay)
	// require is the attribute set that enters the derived schema whatever its presence rate,
	// guarded by requireMu because the admin surface changes it on a live table; requireReport
	// holds the last derivation's refusals. See schemarequire.go.
	requireMu        sync.Mutex
	require          schemaRequireSet
	requireReport    atomic.Pointer[schemaRequireReport]
	groupSchemaCount int     // Options.GroupSchemaCount
	groupStability   int     // Options.GroupStabilityRuns
	groupJac         float64 // Options.GroupMergeJaccard
	colBudget        int     // Options.ColumnarSegmentBudget
	remDict          int     // Options.RemnantDictionary
	// rowGroupBytes is Options.RowGroupBytes, settable at runtime (SetRowGroupBytes) because it only
	// governs row groups sealed from now on -- blocks already written record their own layout. Atomic
	// because every scan that seals a segment reads it.
	rowGroupBytes atomic.Int64
	groupMaxPart  float64 // Options.GroupMaxPartialFrac

	// Query fan-out (see parallel_scan.go). queryPar is the per-query worker cap
	// (0/1 ⇒ serial). qsem is a collection-wide token pool bounding total scan
	// workers across concurrent queries; nil when parallelism is disabled.
	// parallelMinBytes gates fan-out to scans large enough to amortize it.
	queryPar         int
	qsem             chan struct{}
	parallelMinBytes int

	// internAtSeal interns each append-only segment as soon as it seals (see
	// Options.InternAtSeal), via InternSealed from the Archive.Append eager-seal hook.
	internAtSeal bool
	// verifyReads checks each record's CRC before its bytes are used; see
	// Options.DisableReadVerification. On by default for a persistent collection.
	verifyReads bool

	// reverseScan yields records newest-first (see Options.ReverseScan). Forces
	// serial scans (no fan-out), since fan-out has no cross-segment order.
	reverseScan bool

	// ret bounds an append-only collection's retained segments (see Options.Retention
	// and Rotate). Zero value ⇒ keep everything.
	ret Retention

	// schemaScan holds the adschema columnar scan state (resolved schema, hot fields, and the
	// decompressed-block cache) once EnableSchemaScan is called; nil otherwise. See colscan.go.
	schemaScan atomic.Pointer[schemaScanState]
	// schemaMu serializes changes to the committed schema-scan state -- the maintenance review, a
	// first enable and ReschemaScan -- together with the on-disk record of it, so two adopters cannot
	// interleave a publish with the other's commit. Never held across a segment rewrite (those take
	// maintMu), so the lock order between the two never arises.
	schemaMu sync.Mutex
	// baseCommitted records that the committed base schema is on disk (see commitBaseSchema).
	baseCommitted atomic.Bool
	// reviewed is when the base schema was last re-derived by maintenance and against which
	// required set, which decides whether the next pass re-derives at all (see schemaReviewDue).
	// Guarded by schemaMu.
	reviewed schemaReviewMark

	// colCache memoizes this collection's decompressed-columnar-block cache -- now the PROCESS-GLOBAL
	// cache for the collection's kind (archive vs mutating), SHARED across every collection of that
	// kind, not one cache per collection (see sharedColCache / blockcache.go). It is deliberately not
	// per-segment nor per-collection: ristretto sizes its admission metadata (count-min sketch +
	// bloom) to NumCounters regardless of how much is cached, so a cache per segment -- or per one of
	// htcondordb's many tables -- made that fixed overhead scale with segment/table count (the reopen
	// heap leak, and ~1 GB of block cache on a busy host). Block ids are process-unique (colBlockSeq),
	// so one shared cache keyed by (blockID,stream) never collides across collections. Resolved lazily
	// on the first columnarized segment.
	colCache     *blockCache
	colCacheOnce sync.Once

	// schemaSeed holds a schema recovered from a sidecar whose BLOCK format this build cannot read, so a
	// format bump keeps the accelerator's schema instead of switching the accelerator off. Only ever read by
	// adoptPersistedSchemaScan, and only when no block loaded. See readColSectionSchemaOnly.
	schemaSeed atomic.Pointer[schemaSeedState]

	// gcFloor is a runtime-only GC watermark in Retention.MinAgeAttr units: Rotate may
	// reclaim a fully-consumed segment (its newest age value < gcFloor) EARLY -- before it
	// hits MaxAge -- so a change-feed source drains records every live subscriber has already
	// acknowledged (see changefeed's GC floor). It only ever shortens retention: it never
	// keeps data past the configured ceilings, and never drops anything younger than
	// Retention.MinAge. Read/written only under maintMu, alongside ret. Deliberately NOT
	// persisted: it is recomputed from live subscribers each pass, and a stale saved value
	// must never silently GC data across a restart. Zero ⇒ no early GC.
	gcFloor float64

	// hasZones caches whether any per-segment zone maps are configured (see
	// Options.ZoneAttrs / zonemap.go), so the query hot path skips probe extraction
	// when zone pruning is off. Append-only only. Atomic because a runtime AddIndex on
	// an append-only collection can turn zone maps on (see addZoneAttrs) while queries
	// are reading it.
	hasZones atomic.Bool

	// Watch (see watch.go / docs/WATCH.md). hub is nil unless WatchHistory > 0.
	hub           *watchHub
	watchBuf      int
	watchCoalesce time.Duration
	// watchEpochDir is set for a persistent append-only collection with Watch enabled: its
	// epoch survives a clean Close/Open via <watchEpochDir>/watch.epoch (see watchepoch.go).
	watchEpochDir string

	// Parent/child chaining (see docs/PARENT_CHILD.md). parentKeyFor derives a
	// child's parent key (nil ⇒ no chaining); isStructural marks parent-only ads
	// hidden from results; parentPrivate (case-folded) are parent attributes
	// children don't inherit and that don't trigger fan-out. All nil unless configured.
	parentKeyFor  func(key []byte) []byte
	isStructural  func(key []byte) bool
	parentPrivate map[string]struct{}

	// ordered holds the maintained ordered indexes (Options.Ordered), updated on the
	// write path and read via Collection.Ordered. Empty unless configured.
	ordered []*orderedIndex

	// Codec retrain bookkeeping for diagnostics (CodecStats): the wall-clock time of the
	// last successful RetrainDict in this process (unix nanos; 0 = never) and the trained
	// dictionary's size in bytes.
	indexBackfillBytes int64 // see Options.IndexBackfillBytes; 0 = carry changes everywhere

	lastRetrainUnix atomic.Int64
	lastDictBytes   atomic.Int64

	// Encryption at rest (see encrypt.go). sealer is the DB-wide data-key Sealer (nil
	// disables encryption); encAttrs is the case-folded explicit set of attributes whose
	// values are sealed, swapped atomically by the toggle meta-command (private attributes
	// are always sealed regardless). Encrypted attributes are never indexed and never hot.
	sealer   wire.Sealer
	encAttrs atomic.Pointer[encSetHolder]

	// opm accumulates the collection-wide maintenance timings (compact/retrain/reindex/
	// columnarize/rotate/merge) for OpStats; per-shard write/segment/sync timings live on
	// each shard. See opstats.go.
	opm opMetrics

	// ttCfg is the active time-travel configuration, or nil when disabled (the
	// default). Swappable at runtime via SetTimeTravel. See timeseq.go.
	ttCfg atomic.Pointer[ttConfig]

	// gate accounts for readers that are inside the collection across more than one segment
	// pin, so Close/Discard cannot unmap memory under them. See closegate.go.
	gate closeGate
}

// rootKey returns the key of the family root for key: it follows parentKeyFor to
// the top (a key with no parent). With no chaining configured it returns key
// unchanged. The result selects the shard, co-locating a whole family.
func (c *Collection) rootKey(key []byte) []byte {
	if c.parentKeyFor == nil {
		return key
	}
	for {
		p := c.parentKeyFor(key)
		if p == nil {
			return key
		}
		key = p
	}
}

// shardOf selects the shard for key by the family root, so a parent and all its
// descendants share one shard (enabling consistent, lock-free chained
// evaluation). h is key's own hash (already computed by the caller for the bucket
// chain); without chaining the shard is just h & mask, no re-hash.
func (c *Collection) shardOf(key []byte, h uint64) int {
	if c.parentKeyFor == nil {
		return int(h & c.mask)
	}
	return int(c.h.Hash(c.rootKey(key)) & c.mask)
}

// syncAll is the durability barrier: it returns once everything committed to every shard
// before the call is on disk, and every deferred append it covers has been published to
// watchers. It reports the first sticky write or sync error, as Put does.
func (c *Collection) syncAll() error {
	for _, sh := range c.shards {
		sh.sync()
	}
	return c.writeError()
}

// writeError returns the first sticky durability error across shards (persistent stores) -- a
// segment-allocation failure (writeErr) or a durability-sync/msync failure (syncErr) -- or nil.
// Surfaced by Put/Update so a caller learns a write did not land or did not reach disk.
func (c *Collection) writeError() error {
	if c.dir == "" {
		return nil
	}
	for _, sh := range c.shards {
		sh.mu.RLock()
		err := sh.writeErr
		sh.mu.RUnlock()
		if err != nil {
			return err
		}
		if p := sh.syncErr.Load(); p != nil {
			return p.err
		}
	}
	return nil
}

// hotSetHolder wraps the hot-attribute id set so it can be swapped atomically.
type hotSetHolder struct{ set map[uint32]struct{} }

// currentCodec returns the codec new writes are compressed with.
func (c *Collection) currentCodec() Codec { return c.codec.Load().c }

// releaseIdleEncoders drops the warmed zstd encoder of every registered dictionary codec
// except the current write codec. Called at the end of a maintenance pass that compresses
// through older, non-current dictionary codecs (columnarize warms each sealed segment's own
// codec; an append-only retrain leaves the previous generation's write codec resident and
// read-only) so their multi-MB match-finder histories do not linger for the life of the
// collection. Each encoder rebuilds lazily if that codec ever compresses again.
func (c *Collection) releaseIdleEncoders() { c.dicts.releaseEncodersExcept(c.currentCodec()) }

// currentHotSet returns the set of interned ids to front-load in the hot header,
// or nil if none are configured.
func (c *Collection) currentHotSet() map[uint32]struct{} {
	if h := c.hotSet.Load(); h != nil {
		return h.set
	}
	return nil
}

// hotNamesHolder wraps the inline-mode hot-name set so it can be swapped atomically. It
// keeps the folded set the encoder matches on and the original-case spellings for display.
type hotNamesHolder struct {
	folded  map[string]struct{} // foldASCII(name) -> {}, matched by the inline encoder
	display []string            // original-case spellings, sorted (for HotAttrNames)
}

// newHotNamesHolder builds a holder from original-case names.
func newHotNamesHolder(names []string) *hotNamesHolder {
	folded := make(map[string]struct{}, len(names))
	display := make([]string, 0, len(names))
	for _, n := range names {
		f := strings.ToLower(n)
		if _, dup := folded[f]; dup {
			continue
		}
		folded[f] = struct{}{}
		display = append(display, n)
	}
	sort.Strings(display)
	return &hotNamesHolder{folded: folded, display: display}
}

// currentHotNames returns the inline-mode hot attribute set (folded names), or nil.
func (c *Collection) currentHotNames() map[string]struct{} {
	if h := c.hotNames.Load(); h != nil {
		return h.folded
	}
	return nil
}

// currentHotDisplay returns the inline-mode hot attributes in original case, sorted.
func (c *Collection) currentHotDisplay() []string {
	if h := c.hotNames.Load(); h != nil {
		return h.display
	}
	return nil
}

// New creates an empty Collection.
func New(opts Options) *Collection {
	// Apply any configured process-global block-cache budgets. These are whole-process ceilings
	// (see Options), so this is intentionally a global side effect: the last non-zero value wins and
	// resizes the live shared cache. A ≤0 value is ignored inside the setters.
	SetMutatingBlockCacheBudget(opts.MutatingBlockCacheBytes)
	SetArchiveBlockCacheBudget(opts.ArchiveBlockCacheBytes)
	n := opts.Shards
	if n <= 0 {
		n = 16
	}
	if opts.AppendOnly {
		n = 1 // an append log is a single ordered sequence, not sharded
	}
	n = nextPow2(n)
	segSize := opts.SegmentSize
	if segSize <= 0 {
		segSize = defaultSegmentSize
	}
	var h Hasher = opts.Hasher
	if h == nil {
		h = fnvHasher{}
	}
	var codec Codec = opts.Codec
	if codec == nil {
		codec = identityCodec{}
	}
	shards := make([]*shard, n)
	for i := range shards {
		shards[i] = newShard(segSize, opts.CommitSync)
		shards[i].appendOnly = opts.AppendOnly
		if opts.SeqCursorLease != 0 {
			shards[i].leaseFor = opts.SeqCursorLease
		}
	}
	c := &Collection{
		shards: shards,
		mask:   uint64(n - 1),
		h:      h,
		// Private attributes are flagged once per unique name at intern time, so a
		// redacted query (ScanRawRedacted/QueryRawRedacted) strips them with a per-id
		// bool check instead of re-classifying every attribute of every ad.
		intern:           wire.NewInternTableWithPrivacy(classad.IsPrivateAttribute),
		demand:           newDemandTracker(),
		demandHalf:       opts.DemandHalfLife,
		require:          schemaRequireSet{configured: foldUnique(opts.SchemaRequireAttrs)},
		groupSchemaCount: opts.GroupSchemaCount,
		groupStability:   opts.GroupStabilityRuns,
		groupJac:         opts.GroupMergeJaccard,
		colBudget:        opts.ColumnarSegmentBudget,
		remDict:          opts.RemnantDictionary,
		groupMaxPart:     opts.GroupMaxPartialFrac,
	}
	c.rowGroupBytes.Store(int64(opts.RowGroupBytes))
	c.codec.Store(&codecHolder{codec})
	// Delta records need inline names, which the persistent-open path sets LATER (see
	// persist.go), so that condition is checked at encode time rather than here -- reading
	// c.inline now would always find it false and silently disable the feature.
	// Delta records are meaningless on an append-only log and actively harmful there. An
	// append log has no per-key supersession and no key index, so there is no earlier version
	// of a key to merge a fragment with -- and the tracker would accumulate an entry per
	// RECORD rather than per key, since archive records have no reused identity and nothing
	// ever deletes one. That is the O(#records) resident cost an archive avoids by having no
	// key directory at all: a 5M-record history growing 500k/day would carry a few hundred MB
	// of tracker and add tens of MB a day, forever. Refused rather than ignored quietly.
	// Delta records are also refused alongside TIME TRAVEL, for a reason specific to how
	// versions are reclaimed. Compaction retains superseded versions ABOVE the retain floor
	// for as-of reads and drops the rest -- so a retained delta can outlive the whole record
	// it merges from, and the interning re-encode that moves it into the history stream does
	// not carry the delta flag either. An as-of read straddling that boundary gets a fragment
	// presented as a complete ad.
	//
	// The fix that would lift this is to MATERIALIZE on the way into the history stream, so
	// every retained record is self-contained and as-of reads never replay at all. That is a
	// change inside compactShard, which runs off-lock over pinned segments and would need the
	// chain resolved there; it is not attempted here. Refused rather than left to corrupt.
	if opts.DeltaMax > 0 && opts.TimeTravel != nil {
		opts.DeltaMax = 0
	}
	if opts.DeltaMax > 0 && !opts.AppendOnly {
		c.deltaMax = opts.DeltaMax
		c.deltas = newDeltaTracker()
		c.deltaRead = true // an in-memory collection has no marker file; Open overwrites this
	}
	if cfg := newTTConfig(opts.TimeTravel); cfg != nil {
		c.ttCfg.Store(cfg)
	}
	for _, sh := range shards {
		sh.tt = &c.ttCfg // share the collection's swappable time-travel config
	}
	c.dicts = newDictReg(codec) // base codec is dictionary id 0
	// Always front-load the configured hot attributes plus the match-critical defaults
	// (Requirements, Rank). A persistent collection re-installs these inline in Open.
	c.installHotNames(opts.HotAttrs)
	for _, r := range opts.MatchClosureRoots {
		c.matchRoots = append(c.matchRoots, strings.ToLower(r))
	}
	c.spec.Store(newIndexSpec(c.intern, opts.CategoricalAttrs, opts.ValueAttrs))
	// Zone maps (append-only only): cover the explicit ZoneAttrs plus every value-indexed
	// attribute (a range query on an indexed attr should prune whole segments too), matching
	// the archive. Each attribute carries both its folded name and its interned id so the
	// per-segment scan can read the value whether records are inline- or id-encoded; the
	// zone map is keyed by id, which zonePrune/age-retention resolve names to via c.intern.
	// zoneInline is finalized in Open (a persistent collection stores inline names).
	if opts.AppendOnly && (len(opts.ZoneAttrs) > 0 || len(opts.ValueAttrs) > 0) {
		var zattrs []zoneAttr
		seen := map[uint32]struct{}{}
		addZone := func(name string) {
			id := c.intern.Intern(name)
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				// LookupByName folds case internally, so the raw name is fine for inline lookup.
				zattrs = append(zattrs, zoneAttr{name: name, id: id})
			}
		}
		for _, n := range opts.ZoneAttrs {
			addZone(n)
		}
		for _, n := range opts.ValueAttrs { // value-indexed attrs are zoned automatically
			addZone(n)
		}
		c.hasZones.Store(len(zattrs) > 0)
		for _, sh := range shards {
			sh.zoneAttrs = zattrs
		}
	}
	// In-memory sealing: when this collection is in-memory (no Dir), mmap is supported, and
	// indexes are configured, seal each sealed RAM segment's index into an anonymous mmap
	// sidecar (off the Go heap: no GC scan of the sealed majority's bitmaps, RSS reclaimable
	// under pressure). The active segment stays in-RAM. A collection with no indexes at
	// construction keeps heap indexes (the flag is fixed here; a later AddIndex does not flip
	// already-born segments), so pure key/value in-memory stores pay no pin/reap cost.
	if opts.Dir == "" && mmapSupported && c.spec.Load().any() {
		for _, sh := range c.shards {
			sh.sealRAM = true
		}
	}
	// Encryption at rest: build the DB-wide data-key sealer and the explicit encrypted-attr
	// set. An encrypted attribute (explicit, or an always-encrypted private attribute) must
	// not also be indexed -- its value is stored opaque, so it could never satisfy an index.
	if c.sealer = newDataKeySealer(opts.DataKey); c.sealer != nil {
		enc := foldedSet(opts.EncryptedAttrs)
		for _, idxAttr := range append(append([]string{}, opts.CategoricalAttrs...), opts.ValueAttrs...) {
			_, explicit := enc[strings.ToLower(idxAttr)]
			if explicit || classad.IsPrivateAttribute(idxAttr) {
				panic("collections.New: attribute " + idxAttr + " cannot be both encrypted and indexed")
			}
		}
		c.encAttrs.Store(&encSetHolder{set: enc})
	}
	if opts.WatchHistory > 0 {
		c.hub = newWatchHub()
		c.watchBuf = opts.WatchBuffer
		if c.watchBuf <= 0 {
			c.watchBuf = 1024
		}
		c.watchCoalesce = opts.WatchCoalesce
		for i, sh := range c.shards {
			sh.idx = i
			sh.hub = c.hub
			sh.delLog = newDeleteLog(opts.WatchHistory, 0)
		}
	}
	c.parentKeyFor = opts.ParentKeyFor
	c.isStructural = opts.IsStructural
	if c.parentKeyFor != nil {
		// Give each shard a pointer-free child counter keyed by the parent's dir-hash,
		// so Delete detects an emptied structural parent in O(1). A key is a chained
		// child iff parentKeyFor returns a parent for it (nil for structural/root ads).
		for _, sh := range c.shards {
			sh.childParentHash = func(key []byte) (uint64, bool) {
				pk := c.parentKeyFor(key)
				if pk == nil {
					return 0, false
				}
				return c.h.Hash(pk), true
			}
		}
	}
	if len(opts.ParentPrivateAttrs) > 0 {
		c.parentPrivate = make(map[string]struct{}, len(opts.ParentPrivateAttrs))
		for _, a := range opts.ParentPrivateAttrs {
			c.parentPrivate[strings.ToLower(a)] = struct{}{}
		}
	}
	c.parallelMinBytes = defaultParallelMinBytes
	c.reverseScan = opts.ReverseScan
	// Persistent (Dir set) append-only only: c.inline (set later in persist setup) is exactly
	// Dir != "", and InternSealed is a no-op otherwise, so gate on the option's preconditions here.
	c.internAtSeal = opts.InternAtSeal && opts.AppendOnly && opts.Dir != ""
	// Reads are only worth verifying against a persistent segment: an in-memory
	// arena cannot have been corrupted on disk, and its records carry the same CRC
	// only incidentally.
	// On unless explicitly disabled, and only for a persistent collection: an
	// in-memory arena cannot have been corrupted on disk.
	c.verifyReads = !opts.DisableReadVerification && opts.Dir != ""
	c.indexBackfillBytes = opts.IndexBackfillBytes
	c.ret = opts.Retention
	c.queryPar = resolveQueryParallelism(opts.QueryParallelism)
	if c.queryPar >= 2 {
		// Machine-wide worker budget: bounds total scan goroutines across all
		// concurrent queries so per-query fan-out never oversubscribes the cores.
		c.qsem = make(chan struct{}, runtime.GOMAXPROCS(0))
	}
	for _, spec := range opts.Ordered {
		oi := newOrderedIndex(spec)
		if err := oi.compile(); err != nil {
			panic("collections.New: " + err.Error())
		}
		c.ordered = append(c.ordered, oi)
	}
	return c
}

// resolveQueryParallelism turns the Options knob into the per-query worker cap.
// 0 (or negative) means "auto": the library's current default policy, which may
// change over releases. 1 forces serial; N>=2 is an explicit cap.
func resolveQueryParallelism(opt int) int {
	switch {
	case opt == 1:
		return 1 // serial (explicit)
	case opt >= 2:
		return opt // explicit per-query cap
	default:
		// Auto. Today: a conservative per-query cap (defaultAutoQueryWorkers), not all
		// cores -- returns diminish past a handful of workers, and a smaller cap lets
		// several concurrent queries each fan out (fairness) instead of one taking the
		// whole machine-wide budget and starving the rest to serial. Fan-out is still
		// gated by a work-size threshold, capped at the segment count, and shared via
		// the budget, so it degrades to serial under load. This policy may change.
		if n := runtime.GOMAXPROCS(0); n < defaultAutoQueryWorkers {
			return n // GOMAXPROCS==1 naturally yields serial
		}
		return defaultAutoQueryWorkers
	}
}

// Update applies a batch of inserts/updates and returns only once every ad is
// committed and visible to new scans. Ads are encoded (and compressed) outside
// the shard locks; each affected shard then applies its updates under one lock
// acquisition at a single new commit sequence.
func (c *Collection) Update(batch []AdUpdate) error {
	if len(batch) == 0 {
		return nil
	}
	// Every ad is checked before any is encoded, so a refused ad leaves the whole batch unapplied
	// rather than half of it committed.
	for i := range batch {
		if err := wire.CheckDepth(batch[i].Ad.AST()); err != nil {
			return fmt.Errorf("collections: ad %d: %w", i, err)
		}
	}
	codec := c.currentCodec()
	byShard := make(map[int][]pendingPut, len(c.shards))
	for i := range batch {
		raw := c.encodeAd(batch[i].Ad.AST())
		stored := codec.Compress(nil, raw)
		h := c.h.Hash(batch[i].Key)
		si := c.shardOf(batch[i].Key, h)
		byShard[si] = append(byShard[si], pendingPut{hash: h, key: batch[i].Key, ad: stored, codec: codec, ct: c.namesCurrentTime(raw)})
	}
	for si, writes := range byShard {
		c.shards[si].commit(writes)
	}
	c.indexSealedSegments()  // see Put
	c.collapseSealedChains() // see Put: a seal here must schedule its own drain
	err := c.writeError()
	if err == nil && len(c.ordered) > 0 {
		for i := range batch {
			c.maintainOrdered(batch[i].Key, batch[i].Ad)
		}
	}
	return err
}

// Put inserts or updates a single ad. It is the hot path for the common
// one-ad-at-a-time daemon pattern, so unlike Update it takes no per-call slice or
// map allocations: it encodes, compresses, and commits directly to the one shard.
// appendOnly reports whether this is an append-log collection (Options.AppendOnly). It is a
// whole-collection property, set once at construction, so the first shard's flag speaks for all.
func (c *Collection) appendOnly() bool { return len(c.shards) > 0 && c.shards[0].appendOnly }

// appendSealSeq returns the append-only shard's segment-seal counter (0 if not append-only),
// so a caller can detect when a segment has sealed and index it eagerly. See shard.sealSeq.
func (c *Collection) appendSealSeq() uint64 {
	if len(c.shards) == 0 {
		return 0
	}
	return c.shards[0].sealSeq.Load()
}

// hasSegmentIndexes reports whether any per-segment categorical/value index is configured,
// so eager reindexing is skipped when there is nothing to build.
func (c *Collection) hasSegmentIndexes() bool { return c.spec.Load().any() }

func (c *Collection) Put(key []byte, ad *classad.ClassAd) error { return c.put(key, ad, false) }

// putDeferred is Put without the durability sync, for an append log: the record is
// readable on return and durable at the shard's next sync (see shard.commitDeferred
// and Collection.syncAll). Anything other than a persistent append-only shard commits
// durably, as Put does.
func (c *Collection) putDeferred(key []byte, ad *classad.ClassAd) error { return c.put(key, ad, true) }

func (c *Collection) put(key []byte, ad *classad.ClassAd, deferred bool) error {
	if err := wire.CheckDepth(ad.AST()); err != nil {
		return err
	}
	codec := c.currentCodec()
	raw := c.encodeAd(ad.AST())
	stored := codec.Compress(nil, raw)
	h := c.h.Hash(key)
	sh := c.shards[c.shardOf(key, h)]
	p := pendingPut{hash: h, key: key, ad: stored, codec: codec, ct: c.namesCurrentTime(raw)}
	if deferred && sh.appendOnly && sh.alloc != nil {
		sh.commitDeferred(p)
	} else {
		sh.commitOne(p)
	}
	// A write through this path can roll a segment, and the sealed segment needs a key index.
	// Only Txn.Commit used to build one, so on a store written through Put every sealed segment
	// stayed unindexed until a reindex pass, and each later insert of a new key walked every
	// one of them looking for a prior version: quadratic in the record count (60k Puts into a
	// 64KiB-segment store took 23s, most of it in that walk). Indexed first, so the collapse
	// below can find chain bases in the segment that just sealed.
	c.indexSealedSegments()
	// Rolling a segment is also what flags a seal whose open chains still need collapsing. Only
	// Txn.Commit used to schedule that drain, so a seal caused here left the work parked in RAM
	// until some later transaction happened along -- and a restart in between stranded every
	// fragment in the sealed segment.
	c.collapseSealedChains()
	err := c.writeError()
	if err == nil {
		c.maintainOrdered(key, ad)
	}
	return err
}

// Delete removes key, returning whether it was present. The removal is an MVCC
// tombstone: scans already in progress still see the pre-delete version.
func (c *Collection) Delete(key []byte) bool {
	h := c.h.Hash(key)
	sh := c.shards[c.shardOf(key, h)]
	acq, held := sh.lockWrite()
	seq := sh.commitSeq + 1
	ok, parentEmptied := sh.del(h, key, seq, c.currentCodec())
	if ok {
		sh.commitSeq = seq
		// Journal under the same lock that advances commitSeq: a watcher that snapshots
		// a commitSeq covering this delete must find it in the journal (see watchAs).
		sh.journalDeleteLocked(key, seq)
		sh.trackPublishLocked(seq)
		sh.maybeCheckpoint(seq)
	}
	sh.unlockWrite(acq, held)
	if ok {
		c.removeOrdered(key)
		sh.sync() // durability point for the tombstone (group-committed via syncFor)
		pub := sh.beginPublish(seq)
		pub.add(key, nil, nil, true)
		pub.end()
		// Auto-delete a structural parent whose last child just left (HTCondor
		// ClusterCleanup): a structural ad exists only to be chained to, so once no
		// child references it, remove it. sh.del maintained the live-child count and
		// flags when it hit zero, so this is O(1) -- no shard scan. The parent is
		// co-located in this shard.
		if parentEmptied && c.parentKeyFor != nil && c.isStructural != nil {
			if pk := c.parentKeyFor(key); pk != nil && c.isStructural(pk) {
				c.Delete(pk)
			}
		}
	}
	return ok
}

// Get returns the current ad for key, decoded, or (nil, false).
func (c *Collection) Get(key []byte) (*classad.ClassAd, bool) {
	return c.getAs(key, false)
}

// GetRedacted is Get for a reader NOT entitled to sealed values: the decode holds no key, so a sealed
// attribute comes back undefined rather than opened. See QueryRedacted.
func (c *Collection) GetRedacted(key []byte) (*classad.ClassAd, bool) {
	return c.getAs(key, true)
}

func (c *Collection) getAs(key []byte, redact bool) (*classad.ClassAd, bool) {
	h := c.h.Hash(key)
	sh := c.shards[c.shardOf(key, h)]
	stored, codec, dict, obj, ok := sh.get(c, h, key, readWant(redact))
	if !ok {
		return nil, false
	}
	// A merged delta chain comes back as the object those bytes were encoded from, so decoding
	// them would rebuild what we already hold. Never for a redacting read: redaction is applied by
	// the decode and the object has every sealed value open.
	ad := obj
	if ad == nil || redact {
		var err error
		if ad, err = c.decodeAdDictAs(dict, stored, codec, redact); err != nil {
			noteReadSkip(err)
			return nil, false
		}
	}
	// Chain to the parent (same shard) so the returned ad resolves inherited
	// attributes -- Get mirrors query semantics.
	if c.parentKeyFor != nil {
		if pk := c.parentKeyFor(key); pk != nil {
			ph := c.h.Hash(pk)
			if pad, pcodec, pdict, pobj, ok := sh.get(c, ph, pk, readWant(redact)); ok {
				if pobj != nil && !redact {
					c.mergeParent(ad, pobj)
				} else if parent, err := c.decodeAdDictAs(pdict, pad, pcodec, redact); err == nil {
					c.mergeParent(ad, parent)
				}
			}
		}
	}
	return ad, true
}

// Flatten returns a standalone, self-contained copy of the ad at key with every
// inherited parent attribute materialized into it (the child's own values
// winning, parent-private attributes excluded) and no residual parent link. It
// is the form to archive as a history record: like condor_history, which stores
// each completed job flattened rather than as a live cluster/proc pair, so the
// record stays readable after its structural parent is gone. For a collection
// without parent chaining this is identical to Get.
func (c *Collection) Flatten(key []byte) (*classad.ClassAd, bool) {
	return c.Get(key)
}

// Len returns the number of live keys across all shards. Note this includes structural
// (parent-only) ads of a chained collection, which Scan/Query hide -- so Len equals the
// number of rows a match-all query returns only when the collection is not Chained.
func (c *Collection) Len() int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		n += sh.count
		sh.mu.RUnlock()
	}
	return n
}

// Chained reports whether the collection has structural (parent-only) ads that Scan/Query
// hide (HTCondor cluster/proc chaining). When false, Len is exactly the match-all row count,
// which the COUNT(*) fast path relies on.
func (c *Collection) Chained() bool { return c.isStructural != nil }

// Persistent reports whether this collection is backed by a directory, which is also what
// decides its record encoding: a persistent collection stores attribute names inline (so its
// segment files recover without a table), an in-memory one interns them against the shared
// table. Callers that used to read SupportsRawWire for this -- it was c.inline -- want this.
func (c *Collection) Persistent() bool { return c.inline }

// SupportsRawWire reports whether ScanRawWire/QueryRawWire can serve this collection.
// Every collection can: a persistent one stores inline names already, and an in-memory one's
// interned keys are rewritten to names while the row is assembled (see appendWireRow). It
// returned c.inline while that rewrite did not exist, and the relay scans then yielded nothing
// for a RAM table -- indistinguishable from a query that matched nothing, which is why the
// callers were made to check first. It is kept, always true, so those callers still compile
// and so a future storage form that genuinely cannot relay has somewhere to say so.
func (c *Collection) SupportsRawWire() bool { return true }

// Keys returns every visible key in the collection at a consistent per-shard
// snapshot, in no particular order. Structural (parent-only) keys of a chained
// collection are excluded, matching Scan's output. Each key is a fresh copy the
// caller owns. Useful for administrative enumeration and for a replica that must
// clear its keyspace before a full re-sync.
func (c *Collection) Keys() []string {
	var out []string
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		c.forEachVisibleKeyed(s0, wins, func(key, _ []byte, _ Codec, _ *segDictHandle) bool {
			if c.isStructural != nil && c.isStructural(key) {
				return true // parent-only ads are hidden, as in Scan
			}
			out = append(out, string(key))
			return true
		})
		releaseWindows(wins)
	}
	return out
}

// Scan returns an iterator over every ad in the collection. It is scan-exactly-
// once: each key present at the moment a shard's scan begins is yielded exactly
// once (never duplicated, never skipped), even while concurrent updates and
// compaction run. An ad updated mid-scan is seen at whichever version was current
// when that shard's scan started.
func (c *Collection) Scan() iter.Seq[*classad.ClassAd] {
	return func(yield func(*classad.ClassAd) bool) {
		if c.parentKeyFor != nil {
			qp := queryPlan{ws: &wireScope{ctx: c}} // q nil ⇒ no filter, just chain + hide structural
			for _, sh := range c.shards {
				if !c.scanShardChained(sh, qp, yield, false) {
					return
				}
			}
			return
		}
		q := queryPlan{}
		emit := c.yieldAd(yield)
		for _, sh := range c.shards {
			if !c.scanShard(sh, q, emit) {
				return
			}
		}
	}
}

// Query returns an iterator over the ads matching q, with the same
// scan-exactly-once guarantee as Scan.
//
// Each ad is match-tested cheaply and only matching ads are fully decoded to be
// yielded. The match test uses, in order of preference: wire-native evaluation
// (the query reads scalar-literal attributes directly from the encoded ad,
// building no ClassAd); partial decode (decode only the attributes the query
// reads, transitively) when an attribute is a non-literal expression; or full
// decode for queries that read attributes by a runtime-computed name (eval()).
func (c *Collection) Query(q *vm.Query) iter.Seq[*classad.ClassAd] {
	return c.queryAs(q, false)
}

// QueryRedacted is Query for a reader NOT entitled to sealed values: the decode holds no key, so a sealed
// attribute comes back undefined rather than opened (see wire.DecodeResolveEncRedact).
//
// This is the full-decode counterpart of QueryRawRedacted. Query decodes with the collection's key and
// leaves it to the SERIALIZER to drop private attributes -- so the secret was decrypted in this process
// for every unentitled read and then filtered on the way out. A caller that is not entitled to a value
// should not be able to obtain it, rather than obtain it and be trusted to drop it.
func (c *Collection) QueryRedacted(q *vm.Query) iter.Seq[*classad.ClassAd] {
	return c.queryAs(q, true)
}

func (c *Collection) queryAs(q *vm.Query, redact bool) iter.Seq[*classad.ClassAd] {
	return func(yield func(*classad.ClassAd) bool) {
		qp := c.newQueryPlan(q, nil, nil)
		// Chained collection: a serial two-pass scan per shard resolves each ad's
		// inherited (parent) attributes. Indexes and query fan-out are bypassed here
		// (they don't yet understand chaining); correctness first.
		if c.parentKeyFor != nil {
			for _, sh := range c.shards {
				if !c.scanShardChained(sh, qp, yield, redact) {
					return
				}
			}
			return
		}
		rp := c.planReadQuery(qp)
		// A large full-scan query (no index) can fan out across segments; the helper
		// falls back to a serial scan of the same snapshot when it is not worthwhile.
		if c.queryPar > 1 && !rp.disjunctive() && len(rp.usable) == 0 && !c.reverseScan {
			c.runParallelQuery(qp, yield, redact)
			return
		}
		c.runQuery(qp, rp, c.yieldAdAs(yield, redact))
	}
}

// scanShard snapshots one shard and yields its visible ads. When qp.q is non-nil,
// each ad is match-tested (wire-native, partial decode, or full decode) and only
// matching ads are fully decoded and yielded. Returns false if the consumer
// stopped iteration.
// scanShard visits every matching visible record in a shard and passes its
// decompressed wire bytes w to emit; emit returns false to stop the whole scan.
// Decoding w (to a *classad.ClassAd, or straight to wire text for QueryRaw) is the
// caller's job, so one scan path serves both Query and QueryRaw. w aliases a
// reused buffer -- emit must not retain it.
func (c *Collection) scanShard(sh *shard, qp queryPlan, emit scanEmit) bool {
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	return c.scanWindows(s0, wins, qp, emit)
}

// scanShardAt is scanShard for a historical snapshot sequence s0 (point-in-time / AS
// OF queries): it scans the versions visible at s0 rather than the current commit
// sequence.
func (c *Collection) scanShardAt(sh *shard, s0 uint64, qp queryPlan, emit scanEmit) bool {
	wins := sh.snapshotAt(s0)
	defer releaseWindows(wins)
	return c.scanWindows(s0, wins, qp, emit)
}

// scanWindows match-tests and emits the visible records of a frozen window set at
// snapshot s0. Shared by the current-time and AS OF scan paths.
func (c *Collection) scanWindows(s0 uint64, wins []segWindow, qp queryPlan, emit scanEmit) bool {
	visit, stopped := c.refVisitor(s0, qp, emit)
	walk := c.zoneWalk(wins, qp)
	if c.reverseScan {
		forEachVisibleRefReverse(s0, walk, visit)
	} else {
		forEachVisibleRef(s0, walk, visit)
	}
	return !stopped()
}

// zoneWalk drops the sealed segments whose [min,max] zone map cannot contain a record
// satisfying a required conjunct (e.g. CompletionDate > T skips every older segment), and
// records the pruning in qp.stats. A nil-zones window (the active segment, or any collection
// without ZoneAttrs) is never prunable. The caller still releases every original window; this
// only narrows what is walked.
func (c *Collection) zoneWalk(wins []segWindow, qp queryPlan) []segWindow {
	walk := wins
	if len(qp.probes) > 0 && c.hasZones.Load() {
		ct := qp.prunesCT()
		walk = make([]segWindow, 0, len(wins))
		for _, w := range wins {
			if c.zoneExcludes(w, qp.probes, ct) {
				continue
			}
			walk = append(walk, w)
		}
	}
	qp.stats.segments(len(wins), len(wins)-len(walk))
	return walk
}

// zoneExcludes reports whether w's zone map proves that no record visible through it satisfies
// probes, a query's required conjuncts. ct marks probes computed with CurrentTime folded to the
// query's pinned time (queryPlan.prunesCT): they describe only records that do not define
// CurrentTime, so they prune a window proven free of it (segWindow.ctFree) and no other.
//
// The plain scan, the parallel fan-out and the index candidate walks all prune through it, so a
// query skips the same segments whichever of them its plan picks.
func (c *Collection) zoneExcludes(w segWindow, probes []vm.Probe, ct bool) bool {
	return w.zones != nil && (!ct || w.ctFree) && zonePrune(w.zones, probes, c.intern)
}

// zoneExcludesGroups is zoneExcludes for a disjunctive plan (vm.Query.ProbePlan): w is excluded
// only when its zone map rules out EVERY group, since a match satisfies all the probes of at
// least one. A group with no probe is unconstrained and rules nothing out, and neither does an
// empty plan.
func (c *Collection) zoneExcludesGroups(w segWindow, plan []vm.ProbeGroup, ct bool) bool {
	if len(plan) == 0 || w.zones == nil || (ct && !w.ctFree) {
		return false
	}
	for _, g := range plan {
		if !zonePrune(w.zones, g.Probes, c.intern) {
			return false
		}
	}
	return true
}

// scanVisitState is the working state a refVisitor reuses from record to record: the
// decompression buffer, the columnar prefilters and the columnar projection scope.
//
// It is separable from refVisitor because a refVisitor is bound to ONE snapshot sequence, while
// a parallel worker steals windows from several shards and so needs a visitor per window. With
// the state in the closure that worker would rebuild the prefilter -- and regrow the
// decompression buffer from nil -- once per segment rather than once per scan.
//
// Not safe for concurrent use: the colScopes inside the prefilters cache per-block state. One per
// goroutine, which is exactly what makes the prefilter usable from the parallel workers at all.
type scanVisitState struct {
	dbuf        []byte
	pre         *colPrefilter
	preFold     *colPrefilter
	projCS      *colScope
	projScratch projectScratch
	selfClosed  selfClosedScratch
	// narrow / narrowFold are the attributes the query (and its CurrentTime-folded form) reads, as a
	// plan for building a record narrowed to them; nil when the query cannot be matched that way.
	// matchCS and matchScratch are that build's column scope and buffers, and narrowMiss counts
	// CONSECUTIVE records it could not use, with the same give-up rule as closureMiss.
	narrow, narrowFold *projPlan
	matchCS            *colScope
	matchScratch       projectScratch
	narrowMiss         int
	// closureMiss counts CONSECUTIVE records whose projected ad was not self-contained, so a
	// scan whose projection names an expression reaching outside itself stops paying for the
	// attempt. Without it that scan reads the columns, finds the ad unusable and reassembles
	// anyway -- strictly worse than never having tried. Same adaptive shape as the prefix
	// gate in the parallel wire scan, and reset by any record that does stay on the fast path.
	closureMiss int
	built       bool // prefilters/projection scope built; they depend only on the plan
}

// narrowMatchOff and conjunctRejectOff turn off the narrowed match and the conjunct rejection, so a
// benchmark can time the scan with and without them over one store in one process. Test seams:
// production leaves both false, and nothing may change them while a scan runs.
var narrowMatchOff, conjunctRejectOff bool

// refVisitor is how a query treats one visible record, whichever walk -- every record, or an
// index's candidates -- reached it: hide system records, ask the columns before rebuilding the
// record, project from the columns when nothing needs the whole ad, else reassemble, match and
// emit. visit returns false to stop the walk; stopped reports whether the consumer asked to.
func (c *Collection) refVisitor(s0 uint64, qp queryPlan, emit scanEmit) (visit func(recRef) bool, stopped func() bool) {
	return c.refVisitorState(&scanVisitState{}, s0, qp, emit)
}

// refVisitorState is refVisitor over caller-owned state, so one set survives across the windows a
// parallel worker visits. st must belong to the calling goroutine and must be used with plans that
// carry the same query (the prefilters are built from it once).
func (c *Collection) refVisitorState(st *scanVisitState, s0 uint64, qp queryPlan, emit scanEmit) (visit func(recRef) bool, stopped func() bool) {
	cont := true
	if !st.built {
		st.built = true
		// Ask the columns before rebuilding a record: over a columnarized segment a rejected record
		// would otherwise be reassembled in full and then thrown away. nil when the columns cannot
		// usefully decide this query, which leaves the scan exactly as it was.
		st.pre = c.newColPrefilter(qp.q)
		if qp.fold != nil {
			st.preFold = c.newColPrefilter(qp.fold.q)
		}
		// A projected read can be built from the named columns instead of reassembling the record and
		// then discarding most of it -- but only when the match did not need the ad. See colproject.go.
		if qp.proj != nil {
			if sc := c.schemaScan.Load(); sc != nil {
				st.projCS = &colScope{bc: sc.cache, c: c}
			}
		}
		// A record the columns cannot decide can still be matched without rebuilding it: on the
		// record narrowed to what the query reads. See narrowMatchPlan.
		if sc := c.schemaScan.Load(); sc != nil && !narrowMatchOff {
			st.narrow = c.narrowMatchPlan(&qp)
			if qp.fold != nil {
				st.narrowFold = c.narrowMatchPlan(qp.fold)
			}
			if st.narrow != nil || st.narrowFold != nil {
				st.matchCS = &colScope{bc: sc.cache, c: c}
			}
		}
	}
	pre, preFold, projCS := st.pre, st.preFold, st.projCS
	// A query that reads no attribute has the same answer for every record. Only a TRUE one lets a record
	// skip the match; a constant false or undefined must still reject it.
	constQuery := qp.q == nil || constTrue(&qp)
	constFold := qp.fold != nil && constTrue(qp.fold)
	visit = func(r recRef) bool {
		if isSystemKeyBytes(r.key()) {
			return true // internal system record: hidden from client scans/queries
		}
		qp.stats.visit()
		// The CurrentTime-folded form where the window is proven free of CurrentTime (ctproof.go).
		cur, prefilter, isConst, narrow := &qp, pre, constQuery, st.narrow
		if qp.fold != nil && r.w.ctFree {
			cur, prefilter, isConst, narrow = qp.fold, preFold, constFold, st.narrowFold
		}
		dict := r.dict
		cn := r.w.seg.colNative.Load()
		k, inCols := 0, false
		if cn != nil {
			k, inCols = cn.indexOf(r.off)
			// A delta's row would hold only the attributes one write changed; the whole record is a
			// merge of its chain, which only the full read performs. None should be here (sealing
			// collapses every live chain), and if one is, nothing is decided from its row.
			if inCols && c.deltaRead && recIsDelta(r.w.data, r.off) {
				inCols = false
			}
		}
		colDecided, matched := false, false
		// The integrity gate wireAt applies before reassembling (checksum, damage, segment
		// dictionary), for the paths below that read the record's own bytes without reassembling
		// it. Asked at most once per record: gated records that it ran, refused its answer.
		gated, refused := false, error(nil)
		if prefilter != nil && prefilter.m != nil && inCols {
			matches, decided := prefilter.test(cn, k)
			if decided {
				qp.stats.columnDecided()
				if !matches {
					return true // decided from columns alone: never reassembled
				}
				colDecided = true
			}
		}
		// The column conjuncts are normally asked after the narrowed match (see below). A narrowed
		// match that reads a sealed attribute is the exception: it opens the value for every record
		// it decides, so the conjuncts, which read only columns, go first and spare it the records
		// they reject.
		conjAsked := false
		if !colDecided && narrow != nil && narrow.readsSealed && prefilter != nil && prefilter.m == nil && inCols {
			conjAsked = true
			if prefilter.rejects(cn, k) {
				qp.stats.columnDecided()
				qp.stats.conjunctRejected()
				return true
			}
		}
		// Match on the record narrowed to what the query reads, when the columns could not decide:
		// the record's columns, cold tail and remnant hold every attribute it has, so the few the
		// query names can be gathered without rebuilding the rest. Exact only when the narrowed
		// record is closed under the query's references (adClosed), and only for a record the
		// integrity gate admits: one it refuses falls through to the full read, which refuses and
		// counts it as it always has.
		if !colDecided && !isConst && narrow != nil && inCols && st.narrowMiss < closureMissGiveUp {
			gated, refused = true, c.recordRefused(r.w.seg, r.w.data, r.off)
		}
		if gated && refused == nil {
			out, ok := c.projectFromColumns(cn, r, k, narrow, st.matchCS, &st.matchScratch)
			if ok && c.adClosed(out, narrow, cn.dict, &st.selfClosed) {
				st.narrowMiss = 0
				qp.stats.columnDecided()
				qp.stats.narrowMatched()
				if qp.ws != nil {
					qp.ws.dict = dict
				}
				if !matchWire(out, *cur) {
					return true // matched on the narrowed record: never reassembled
				}
				colDecided, matched = true, true
			} else {
				st.narrowMiss++
			}
		}
		// The query's conjuncts that read only schema fields, to reject what the narrowed match did
		// not decide. Asked after it rather than before: the narrowed match evaluates the whole query
		// for little more than these conjuncts cost, so asking them first only pays twice for the
		// records they pass. Where it did not run -- a query it cannot serve, a record it could not
		// use -- a rejection here still saves the reassembly.
		if !colDecided && !conjAsked && prefilter != nil && prefilter.m == nil && inCols && prefilter.rejects(cn, k) {
			qp.stats.columnDecided()
			qp.stats.conjunctRejected()
			return true
		}
		// Project straight from the columns when nothing still needs the whole ad: either the query
		// reads no attribute (so it is constant and the ad cannot change its answer) or the match was
		// just decided without it -- from the columns, or on the narrowed record. Anything less
		// certain reassembles.
		if projCS != nil && inCols && (isConst || colDecided) && st.closureMiss < closureMissGiveUp {
			// The record is about to be SERVED, and partly from its own bytes (the remnant and
			// header), so it takes the same integrity gate wireAt applies before reassembling:
			// a record whose checksum fails, or whose segment dictionary is unusable, is skipped
			// and counted exactly as the reassembling path skips and counts it. Without this the
			// projection served rows the plain scan refuses. A record the narrowed match decided
			// has already passed it, and is not checked twice.
			//
			// A record the columns REJECT above needs no gate: it is skipped whichever path
			// runs, and the plain scan does not read its bytes either.
			if !gated {
				refused = c.recordRefused(r.w.seg, r.w.data, r.off)
			}
			if refused != nil {
				noteReadSkip(refused)
				return true
			}
			// A plan whose consumer evaluates takes the fast path only for records whose
			// projected ad is closed; the rest fall through and reassemble. See adClosed for
			// why that is the real condition rather than "has a plan".
			out, ok := c.projectFromColumns(cn, r, k, qp.proj, projCS, &st.projScratch)
			if ok && qp.proj.closure && !c.adClosed(out, qp.proj, cn.dict, &st.selfClosed) {
				ok = false
				st.closureMiss++
				qp.stats.closureReassembled()
			}
			if ok {
				st.closureMiss = 0
				qp.stats.matched() // projected straight from columns; no reassembly
				if qp.ws != nil {
					qp.ws.dict = dict
				}
				if !emit(out, dict) {
					cont = false
					return false
				}
				return true
			}
		}
		// The record's FULL ad, which in a columnarized segment means its own bytes spliced with
		// the attributes held in the segment's columnar payload. Asking the ref rather than
		// decompressing here is what lets a columnarized segment be scanned at all: its records
		// carry only what the schema does not cover.
		w, err := c.wireAt(r, s0, st.dbuf)
		if err != nil {
			return true // skip a record we cannot decode rather than abort the scan; wireAt counted it
		}
		qp.stats.reassembled() // the record was rebuilt from the arena (~O(ad width))
		st.dbuf = w            // retain the (possibly grown) backing for the next ad
		// Publish the record's segment dict on the wireScope so the match eval resolves an
		// interned segment's ids (nil for inline; ws is nil on a plain no-query scan). The
		// emit consumer gets the dict via a separate channel (emit-widening; TODO make-live).
		if qp.ws != nil {
			qp.ws.dict = dict
		}
		if cur.q != nil && !matched && !matchWire(w, *cur) {
			return true
		}
		qp.stats.matched()
		if !emit(w, dict) {
			cont = false
			return false
		}
		return true
	}
	return visit, func() bool { return !cont }
}

// narrowMatchPlan is the plan for matching qp's query on a record narrowed to the attributes it reads
// (its read plan's seeds), or nil when the query cannot be matched that way.
//
// The query must read a statically known set (no eval() in the query itself), and some attribute at
// all -- a constant query needs no record. A record narrowed to that set evaluates the query exactly
// as the whole record does when adClosed holds for it, which the scan checks per record: matchWire
// reads each seed, and resolves an expression-valued one by decoding the seeds and what they refer to,
// transitively -- all of it in the narrowed record or absent from both.
func (c *Collection) narrowMatchPlan(qp *queryPlan) *projPlan {
	if qp.q == nil || !qp.plan.PartialSafe || len(qp.plan.Seeds) == 0 {
		return nil
	}
	p := c.newProjPlan(qp.plan.Seeds)
	if p != nil && c.sealer != nil {
		p.readsSealed = slices.ContainsFunc(qp.plan.Seeds, c.shouldEncrypt)
	}
	return p
}

// mergeParent flattens into child every (non-private) attribute of parent that
// child does not already define (child overrides parent), producing a standalone
// ad. This is needed for output because SetParent only chains expression
// evaluation, not direct attribute reads (EvaluateAttr*, GetAttributes) or
// serialization -- a query result or watch event must carry the inherited
// attributes inline, as condor_q shows a job's full ad and condor_history
// flattens it. Parent-private attributes (factory bookkeeping) are not inherited.
func (c *Collection) mergeParent(child, parent *classad.ClassAd) {
	for _, name := range parent.GetAttributes() {
		if c.parentPrivate != nil {
			if _, priv := c.parentPrivate[strings.ToLower(name)]; priv {
				continue // parent-private: not inherited
			}
		}
		if _, has := child.Lookup(name); has {
			continue // child overrides
		}
		if expr, ok := parent.Lookup(name); ok {
			child.InsertExpr(name, expr)
		}
	}
}

// scanShardChained scans a shard for a collection with parent/child chaining. It
// first collects the shard's structural (parent) ads at the snapshot, then
// evaluates every non-structural ad with its parent chained in -- so a query
// resolves attributes the child inherits from its parent -- and yields matches
// with the parent set (SetParent) so the consumer sees the inherited attributes
// too. Structural (parent-only) ads are hidden from the output. The whole family
// is co-located in this shard, so both passes read one consistent snapshot with
// no cross-shard fetch.
func (c *Collection) scanShardChained(sh *shard, qp queryPlan, yield func(*classad.ClassAd) bool, redact bool) bool {
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)

	// Pass 1: collect structural (parent) ads' decompressed wire bytes. Parents
	// are few (one per family), so this map stays small.
	parents := map[string]parentWire{}
	c.forEachVisibleKeyed(s0, wins, func(key, ad []byte, codec Codec, dict *segDictHandle) bool {
		if isSystemKeyBytes(key) {
			return true // internal system record: never a family parent
		}
		if c.isStructural != nil && c.isStructural(key) {
			if w, err := codec.Decompress(nil, ad); err == nil {
				// Capture the parent's segment dict too: the parent may live in an interned
				// segment while the child is inline (or a different interned segment), and
				// pass 2 resolves the parent's wire through THIS dict. Same snapshot pins the
				// segment, so the handle stays valid across both passes.
				parents[string(key)] = parentWire{w: w, dict: dict} // owns its buffer (nil dst)
			}
		}
		return true
	})

	// Pass 2: evaluate children (and standalone ads); skip structural ads.
	cont := true
	var dbuf []byte
	c.forEachVisibleKeyed(s0, wins, func(key, ad []byte, codec Codec, dict *segDictHandle) bool {
		if isSystemKeyBytes(key) {
			return true // internal system record: hidden from client scans/queries
		}
		if c.isStructural != nil && c.isStructural(key) {
			return true // structural ads are hidden from results
		}
		w, err := codec.Decompress(dbuf[:0], ad)
		if err != nil {
			noteReadSkip(err)
			return true
		}
		dbuf = w
		var parentW []byte
		var parentDict *segDictHandle
		if pk := c.parentKeyFor(key); pk != nil {
			if pe, ok := parents[string(pk)]; ok {
				parentW, parentDict = pe.w, pe.dict
			}
		}
		qp.ws.parent = wire.Ad(parentW) // nil ⇒ no parent
		qp.ws.dict, qp.ws.parentDict = dict, parentDict
		if qp.q != nil && !matchWire(w, qp) {
			return true
		}
		a, err := c.decodeWireDictAs(dict, w, redact)
		if err != nil {
			noteReadSkip(err)
			return true
		}
		child := classad.FromAST(a)
		if parentW != nil {
			if pa, err := c.decodeWireDictAs(parentDict, parentW, redact); err == nil {
				c.mergeParent(child, classad.FromAST(pa))
			}
		}
		if !yield(child) {
			cont = false
			return false
		}
		return true
	})
	qp.ws.parent, qp.ws.dict, qp.ws.parentDict = nil, nil, nil
	return cont
}

// parentWire is a captured chained-parent ad's decompressed wire bytes plus the segment
// dictionary needed to resolve them (nil for an inline/global parent). Held only for the
// lifetime of one scanShardChained snapshot, which pins the parent's segment.
type parentWire struct {
	w    []byte
	dict *segDictHandle
}

// yieldAd is the emit callback for the classic Query/Scan path: decode w to a
// *classad.ClassAd (skipping malformed records) and yield it.
func (c *Collection) yieldAd(yield func(*classad.ClassAd) bool) scanEmit {
	return c.yieldAdAs(yield, false)
}

// yieldAdAs is yieldAd for a read that may not be entitled to sealed values (see QueryRedacted).
func (c *Collection) yieldAdAs(yield func(*classad.ClassAd) bool, redact bool) scanEmit {
	return func(w []byte, dict *segDictHandle) bool {
		a, err := c.decodeWireDictAs(dict, w, redact)
		if err != nil {
			noteReadSkip(err)
			return true // skip malformed record, keep scanning
		}
		return yield(classad.FromAST(a))
	}
}

// matchWire reports whether the query matches the ad with wire bytes w, using the
// cheapest evaluation path that is exact for this query and ad. It is mode-agnostic:
// the wire touchpoints come from qp.ws.ctx, so both Collection and Archive scans use
// it.
func matchWire(w []byte, qp queryPlan) bool {
	if qp.wireOK {
		qp.ws.ad = wire.Ad(w)
		qp.ws.fellBack = false
		v := qp.m.EvalResolved(qp.resolver)
		if !qp.ws.fellBack {
			return isTrueValue(v)
		}
		// A queried attribute was a non-literal expression; fall back.
	}
	if qp.plan.PartialSafe {
		child, complete := partialDecodeWire(qp.ws.ctx, qp.ws.dict, w, qp.plan.Seeds)
		if complete && qp.ws.parent != nil {
			// The child inherits any seed it lacks from the parent, so decode the
			// same seeds from the parent and chain.
			var parent *classad.ClassAd
			parent, complete = partialDecodeWire(qp.ws.ctx, qp.ws.parentDict, []byte(qp.ws.parent), qp.plan.Seeds)
			child.SetParent(parent)
		}
		if complete {
			return qp.m.Matches(child)
		}
		// A stored expression calls eval(): what it reads is known only when it runs, so the
		// partial ad may lack it. Decode the whole ad instead.
	}
	a, err := decodeWireDictCtx(qp.ws.ctx, qp.ws.dict, w)
	if err != nil {
		return false
	}
	child := classad.FromAST(a)
	if qp.ws.parent != nil {
		if pa, err := decodeWireDictCtx(qp.ws.ctx, qp.ws.parentDict, []byte(qp.ws.parent)); err == nil {
			child.SetParent(classad.FromAST(pa))
		}
	}
	return qp.m.Matches(child)
}

// partialDecodeWire builds a ClassAd containing only the attributes named in
// seeds plus any they transitively reference, decoded directly from the ad's wire
// bytes without materializing its other (potentially many) attributes. An
// attribute the ad lacks is simply omitted, so a reference to it evaluates to
// undefined — exactly as it would against a full decode.
//
// complete is false when a decoded attribute's expression calls eval(), whose
// argument names an attribute only when it runs: the static references cannot
// say what it reads, so the partial ad may lack it and is not a stand-in for the
// whole one.
//
// A sealed attribute is decoded (opened) for its value, but its references are
// not followed: a sealed value refers to no other attribute by the encryption-at-
// rest contract, and the narrowed match (adClosed) and the projections stop at one
// too, so following them here would make the plain match disagree with those.
func partialDecodeWire(ctx wireCtx, dict *segDictHandle, w []byte, seeds []string) (ad *classad.ClassAd, complete bool) {
	a := wire.Ad(w)
	out := classad.New()
	done := make(map[string]bool, len(seeds))
	work := append([]string(nil), seeds...)
	for len(work) > 0 {
		name := work[len(work)-1]
		work = work[:len(work)-1]
		fold := strings.ToLower(name)
		if done[fold] {
			continue
		}
		done[fold] = true
		node, ok := wireLookupDictCtx(ctx, dict, a, name)
		if !ok {
			continue // this ad lacks it -> undefined
		}
		expr, err := decodeNodeDictCtx(ctx, dict, node)
		if err != nil {
			continue
		}
		out.Insert(name, expr)
		if wire.IsEncryptedNode(node) {
			continue
		}
		refs, safe := vm.SelfRefsSafe(expr)
		if !safe {
			return out, false
		}
		work = append(work, refs...) // expand transitive references
	}
	return out, true
}

func (c *Collection) decodeAd(stored []byte, codec Codec) (*classad.ClassAd, error) {
	wireBytes, err := codec.Decompress(nil, stored)
	if err != nil {
		return nil, err
	}
	a, err := c.decodeWire(wireBytes)
	if err != nil {
		return nil, err
	}
	return classad.FromAST(a), nil
}

// CollectSamples returns the decompressed wire bytes of up to max ads, for
// training a ZSTD dictionary (see TrainDict). It samples across shards, decoding
// each record with the codec it was stored under.
func (c *Collection) CollectSamples(max int) [][]byte {
	out := make([][]byte, 0, max)
	var buf []byte // reused decompression scratch; each sample is copied out exactly
	for _, sh := range c.shards {
		if len(out) >= max {
			break
		}
		s0, wins := sh.snapshot()
		c.forEachVisible(s0, wins, func(ad []byte, codec Codec, dict *segDictHandle) bool {
			w, err := codec.Decompress(buf[:0], ad)
			if err != nil {
				return true
			}
			buf = w
			// Samples are consumed later (schema build / ForEachNamed) with no segment in
			// scope, so an interned record must be flattened to inline wire now.
			iw, ok := c.wireToInline(dict, w)
			if !ok {
				return true
			}
			cp := make([]byte, len(iw))
			copy(cp, iw)
			out = append(out, cp)
			return len(out) < max
		})
		releaseWindows(wins)
	}
	return out
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// SetRowGroupBytes changes the uncompressed record-bytes budget for a columnar row group. 0 restores
// the default.
//
// Safe on a live collection, and it needs no reconciliation pass: every block records the layout it
// was written with, so a new budget governs row groups sealed from now on while everything already on
// disk keeps reading exactly as before. That is what makes this tunable against a real archive rather
// than only at creation.
func (c *Collection) SetRowGroupBytes(n int) { c.rowGroupBytes.Store(int64(n)) }

// RowGroupBytes reports the budget currently in effect (0 meaning the default).
func (c *Collection) RowGroupBytes() int { return int(c.rowGroupBytes.Load()) }
