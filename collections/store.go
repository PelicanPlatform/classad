package collections

import (
	"iter"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// codecHolder wraps a Codec so it can be swapped atomically (atomic.Pointer is
// type-safe; atomic.Value would panic when the concrete codec type changes, e.g.
// identityCodec -> *zstdCodec after RetrainDict).
type codecHolder struct{ c Codec }

// Options configures a Collection.
type Options struct {
	// Shards is the number of independently-locked shards; rounded up to a power
	// of two. Default 16.
	Shards int
	// SegmentSize is the arena segment size in bytes. Default 8 MiB.
	SegmentSize int
	// Hasher routes keys to shards / directory buckets. Default 64-bit FNV-1a.
	Hasher Hasher
	// Codec compresses stored ad bytes. Default identity (no compression).
	Codec Codec
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
	// existing records keep their prior form until rewritten.
	EncryptedAttrs []string

	// TimeTravel, if non-nil, enables point-in-time ("AS OF") queries: superseded
	// record versions committed within MaxDistance of now are retained (not reclaimed
	// by compaction), and a sparse time->seq checkpoint is recorded so a wall-clock
	// time resolves to the commit sequence that was current then. Nil (default)
	// disables it with zero write-path cost -- retention collapses to the current seq,
	// exactly as before. See timeseq.go. It can be toggled at runtime.
	TimeTravel *TimeTravelOptions
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
	codec  atomic.Pointer[codecHolder]  // current codec for new writes; swapped by RetrainDict
	intern *wire.InternTable            // shared attribute-name interning across the store
	hotSet atomic.Pointer[hotSetHolder] // interned ids to front-load; swapped by RefreshHotSet
	spec   atomic.Pointer[indexSpec]    // configured indexes; swapped by AddIndex/DropIndex (never nil after New)

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

	demand *demandTracker // per-attribute query demand, for SuggestIndexes

	// Query fan-out (see parallel_scan.go). queryPar is the per-query worker cap
	// (0/1 ⇒ serial). qsem is a collection-wide token pool bounding total scan
	// workers across concurrent queries; nil when parallelism is disabled.
	// parallelMinBytes gates fan-out to scans large enough to amortize it.
	queryPar         int
	qsem             chan struct{}
	parallelMinBytes int

	// Watch (see watch.go / docs/WATCH.md). hub is nil unless WatchHistory > 0.
	hub           *watchHub
	watchBuf      int
	watchCoalesce time.Duration

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
	lastRetrainUnix atomic.Int64
	lastDictBytes   atomic.Int64

	// Encryption at rest (see encrypt.go). sealer is the DB-wide data-key Sealer (nil
	// disables encryption); encAttrs is the case-folded explicit set of attributes whose
	// values are sealed, swapped atomically by the toggle meta-command (private attributes
	// are always sealed regardless). privCache memoizes the immutable per-name private
	// determination. Encrypted attributes are never indexed and never hot.
	sealer    wire.Sealer
	encAttrs  atomic.Pointer[encSetHolder]
	privCache sync.Map // attribute name -> bool (classad.IsPrivateAttribute), memoized

	// opm accumulates the collection-wide maintenance timings (compact/retrain/reindex)
	// for OpStats; per-shard write/segment/sync timings live on each shard. See opstats.go.
	opm opMetrics

	// ttCfg is the active time-travel configuration, or nil when disabled (the
	// default). Swappable at runtime via SetTimeTravel. See timeseq.go.
	ttCfg atomic.Pointer[ttConfig]
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

// writeError returns the first sticky segment-allocation error across shards
// (persistent stores), or nil. Surfaced by Put/Update.
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
	}
	return nil
}

// hotSetHolder wraps the hot-attribute id set so it can be swapped atomically.
type hotSetHolder struct{ set map[uint32]struct{} }

// currentCodec returns the codec new writes are compressed with.
func (c *Collection) currentCodec() Codec { return c.codec.Load().c }

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
	n := opts.Shards
	if n <= 0 {
		n = 16
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
	}
	c := &Collection{
		shards: shards,
		mask:   uint64(n - 1),
		h:      h,
		// Private attributes are flagged once per unique name at intern time, so a
		// redacted query (ScanRawRedacted/QueryRawRedacted) strips them with a per-id
		// bool check instead of re-classifying every attribute of every ad.
		intern: wire.NewInternTableWithPrivacy(classad.IsPrivateAttribute),
		demand: newDemandTracker(),
	}
	c.codec.Store(&codecHolder{codec})
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
	codec := c.currentCodec()
	byShard := make(map[int][]pendingPut, len(c.shards))
	for i := range batch {
		stored := codec.Compress(nil, c.encodeAd(batch[i].Ad.AST()))
		h := c.h.Hash(batch[i].Key)
		si := c.shardOf(batch[i].Key, h)
		byShard[si] = append(byShard[si], pendingPut{hash: h, key: batch[i].Key, ad: stored, codec: codec})
	}
	for si, writes := range byShard {
		c.shards[si].commit(writes)
	}
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
func (c *Collection) Put(key []byte, ad *classad.ClassAd) error {
	codec := c.currentCodec()
	stored := codec.Compress(nil, c.encodeAd(ad.AST()))
	h := c.h.Hash(key)
	c.shards[c.shardOf(key, h)].commitOne(pendingPut{hash: h, key: key, ad: stored, codec: codec})
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
	ok, parentEmptied := sh.del(h, key, seq)
	if ok {
		sh.commitSeq = seq
		sh.maybeCheckpoint(seq)
	}
	sh.unlockWrite(acq, held)
	if ok {
		c.removeOrdered(key)
		sh.sync() // durability point for the tombstone (group-committed via syncFor)
		if sh.delLog != nil {
			sh.delLog.record(key, seq) // retain for resuming watchers
			sh.hub.publish(sh.idx, seq, key, nil, nil, true)
		}
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
	h := c.h.Hash(key)
	sh := c.shards[c.shardOf(key, h)]
	stored, codec, ok := sh.get(h, key)
	if !ok {
		return nil, false
	}
	ad, err := c.decodeAd(stored, codec)
	if err != nil {
		return nil, false
	}
	// Chain to the parent (same shard) so the returned ad resolves inherited
	// attributes -- Get mirrors query semantics.
	if c.parentKeyFor != nil {
		if pk := c.parentKeyFor(key); pk != nil {
			ph := c.h.Hash(pk)
			if pad, pcodec, ok := sh.get(ph, pk); ok {
				if parent, err := c.decodeAd(pad, pcodec); err == nil {
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

// Len returns the number of live keys across all shards.
func (c *Collection) Len() int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		n += sh.count
		sh.mu.RUnlock()
	}
	return n
}

// Keys returns every visible key in the collection at a consistent per-shard
// snapshot, in no particular order. Structural (parent-only) keys of a chained
// collection are excluded, matching Scan's output. Each key is a fresh copy the
// caller owns. Useful for administrative enumeration and for a replica that must
// clear its keyspace before a full re-sync.
func (c *Collection) Keys() []string {
	var out []string
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		forEachVisibleKeyed(s0, wins, func(key, _ []byte, _ Codec) bool {
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
				if !c.scanShardChained(sh, qp, yield) {
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

// queryPlan bundles a compiled query with everything a scan needs to evaluate it,
// including reusable per-scan state.
type queryPlan struct {
	q        *vm.Query
	plan     vm.ReadPlan
	m        *vm.Matcher
	wireOK   bool // native + partial-safe: try wire-native evaluation
	ws       *wireScope
	resolver func(name string, scope ast.AttributeScope) classad.Value
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
	return func(yield func(*classad.ClassAd) bool) {
		plan := q.ReadPlan()
		ws := &wireScope{ctx: c}
		qp := queryPlan{
			q:        q,
			plan:     plan,
			m:        q.Matcher(), // reused across ads; the iterator runs single-threaded
			wireOK:   q.Native() && plan.PartialSafe,
			ws:       ws,
			resolver: ws.resolve, // bound once to avoid a per-ad closure allocation
		}
		// Chained collection: a serial two-pass scan per shard resolves each ad's
		// inherited (parent) attributes. Indexes and query fan-out are bypassed here
		// (they don't yet understand chaining); correctness first.
		if c.parentKeyFor != nil {
			for _, sh := range c.shards {
				if !c.scanShardChained(sh, qp, yield) {
					return
				}
			}
			return
		}
		// Disjunctive queries: if the top-level OR spine yields more than one probe
		// group and every group is index-usable, prune via the union of the groups'
		// candidate sets (DNF: OR of AND-of-probes), re-verifying each candidate. A
		// single group falls through to the conjunctive path below unchanged.
		if plan := q.ProbePlan(); len(plan) > 1 {
			for _, g := range plan {
				c.demand.record(g.Probes)
			}
			if groups, prunable := c.planIndexGroups(plan); prunable && !overSelectivityGate(c, groups) {
				emit := c.yieldAd(yield)
				for _, sh := range c.shards {
					if !c.scanShardIndexedGroups(sh, groups, qp, emit) {
						return
					}
				}
				return
			}
			// Not prunable (some disjunct is unconstrained): fall through to a full scan.
		}
		// Record which attributes the query filters on (for SuggestIndexes), then,
		// if the query has an index-usable constraint, visit only candidate ads;
		// otherwise fall back to a full scan. Both re-verify the full predicate.
		probes := q.Probes()
		c.demand.record(probes)
		c.demand.recordReads(q.ReadAttrs()) // hot-set signal: attributes the query evaluates
		usable := c.planIndex(probes)
		// A large full-scan query (no index) can fan out across segments; the helper
		// falls back to a serial scan of the same snapshot when it is not worthwhile.
		if c.queryPar > 1 && len(usable) == 0 {
			c.runParallelQuery(q, yield)
			return
		}
		emit := c.yieldAd(yield)
		for _, sh := range c.shards {
			var cont bool
			if len(usable) > 0 {
				cont = c.scanShardIndexed(sh, usable, qp, emit)
			} else {
				cont = c.scanShard(sh, qp, emit)
			}
			if !cont {
				return
			}
		}
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
func (c *Collection) scanShard(sh *shard, qp queryPlan, emit func(w []byte) bool) bool {
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	return c.scanWindows(s0, wins, qp, emit)
}

// scanShardAt is scanShard for a historical snapshot sequence s0 (point-in-time / AS
// OF queries): it scans the versions visible at s0 rather than the current commit
// sequence.
func (c *Collection) scanShardAt(sh *shard, s0 uint64, qp queryPlan, emit func(w []byte) bool) bool {
	wins := sh.snapshotAt(s0)
	defer releaseWindows(wins)
	return c.scanWindows(s0, wins, qp, emit)
}

// scanWindows match-tests and emits the visible records of a frozen window set at
// snapshot s0. Shared by the current-time and AS OF scan paths.
func (c *Collection) scanWindows(s0 uint64, wins []segWindow, qp queryPlan, emit func(w []byte) bool) bool {
	cont := true
	var dbuf []byte // decompression buffer reused across ads (single-threaded scan)
	forEachVisibleKeyed(s0, wins, func(key, ad []byte, codec Codec) bool {
		if isSystemKeyBytes(key) {
			return true // internal system record: hidden from client scans/queries
		}
		w, err := codec.Decompress(dbuf[:0], ad)
		if err != nil {
			return true // skip a record we cannot decode rather than abort the scan
		}
		dbuf = w // retain the (possibly grown) backing for the next ad
		if qp.q != nil && !matchWire(w, qp) {
			return true
		}
		if !emit(w) {
			cont = false
			return false
		}
		return true
	})
	return cont
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
func (c *Collection) scanShardChained(sh *shard, qp queryPlan, yield func(*classad.ClassAd) bool) bool {
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)

	// Pass 1: collect structural (parent) ads' decompressed wire bytes. Parents
	// are few (one per family), so this map stays small.
	parents := map[string][]byte{}
	forEachVisibleKeyed(s0, wins, func(key, ad []byte, codec Codec) bool {
		if isSystemKeyBytes(key) {
			return true // internal system record: never a family parent
		}
		if c.isStructural != nil && c.isStructural(key) {
			if w, err := codec.Decompress(nil, ad); err == nil {
				parents[string(key)] = w // owns its buffer (nil dst)
			}
		}
		return true
	})

	// Pass 2: evaluate children (and standalone ads); skip structural ads.
	cont := true
	var dbuf []byte
	forEachVisibleKeyed(s0, wins, func(key, ad []byte, codec Codec) bool {
		if isSystemKeyBytes(key) {
			return true // internal system record: hidden from client scans/queries
		}
		if c.isStructural != nil && c.isStructural(key) {
			return true // structural ads are hidden from results
		}
		w, err := codec.Decompress(dbuf[:0], ad)
		if err != nil {
			return true
		}
		dbuf = w
		var parentW []byte
		if pk := c.parentKeyFor(key); pk != nil {
			parentW = parents[string(pk)]
		}
		qp.ws.parent = wire.Ad(parentW) // nil ⇒ no parent
		if qp.q != nil && !matchWire(w, qp) {
			return true
		}
		a, err := c.decodeWire(w)
		if err != nil {
			return true
		}
		child := classad.FromAST(a)
		if parentW != nil {
			if pa, err := c.decodeWire(parentW); err == nil {
				c.mergeParent(child, classad.FromAST(pa))
			}
		}
		if !yield(child) {
			cont = false
			return false
		}
		return true
	})
	qp.ws.parent = nil
	return cont
}

// yieldAd is the emit callback for the classic Query/Scan path: decode w to a
// *classad.ClassAd (skipping malformed records) and yield it.
func (c *Collection) yieldAd(yield func(*classad.ClassAd) bool) func(w []byte) bool {
	return func(w []byte) bool {
		a, err := c.decodeWire(w)
		if err != nil {
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
		child := partialDecodeWire(qp.ws.ctx, w, qp.plan.Seeds)
		if qp.ws.parent != nil {
			// The child inherits any seed it lacks from the parent, so decode the
			// same seeds from the parent and chain.
			child.SetParent(partialDecodeWire(qp.ws.ctx, []byte(qp.ws.parent), qp.plan.Seeds))
		}
		return qp.m.Matches(child)
	}
	a, err := qp.ws.ctx.decodeWire(w)
	if err != nil {
		return false
	}
	child := classad.FromAST(a)
	if qp.ws.parent != nil {
		if pa, err := qp.ws.ctx.decodeWire([]byte(qp.ws.parent)); err == nil {
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
func partialDecodeWire(ctx wireCtx, w []byte, seeds []string) *classad.ClassAd {
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
		node, ok := ctx.wireLookup(a, name)
		if !ok {
			continue // this ad lacks it -> undefined
		}
		expr, err := ctx.decodeNode(node)
		if err != nil {
			continue
		}
		out.Insert(name, expr)
		work = append(work, vm.SelfRefs(expr)...) // expand transitive references
	}
	return out
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
		forEachVisible(s0, wins, func(ad []byte, codec Codec) bool {
			w, err := codec.Decompress(buf[:0], ad)
			if err != nil {
				return true
			}
			buf = w
			cp := make([]byte, len(w))
			copy(cp, w)
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
