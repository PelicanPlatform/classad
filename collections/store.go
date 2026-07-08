package collections

import (
	"iter"
	"runtime"
	"strings"
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
	// SegmentSize is the arena segment size in bytes. Default 1 MiB.
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
	dir    string                       // persistence directory; "" ⇒ in-memory (see persist.go)

	// inline is set for a persistent collection: records store attribute names
	// inline (no interning), so segment files are self-contained and recoverable.
	// hotNames (case-folded) drives the hot header in inline mode. See inline.go.
	inline   bool
	hotNames map[string]struct{}

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
		intern: wire.NewInternTable(),
		demand: newDemandTracker(),
	}
	c.codec.Store(&codecHolder{codec})
	c.dicts = newDictReg(codec) // base codec is dictionary id 0
	if len(opts.HotAttrs) > 0 {
		set := make(map[uint32]struct{}, len(opts.HotAttrs))
		for _, name := range opts.HotAttrs {
			set[c.intern.Intern(name)] = struct{}{}
		}
		c.hotSet.Store(&hotSetHolder{set})
	}
	c.spec.Store(newIndexSpec(c.intern, opts.CategoricalAttrs, opts.ValueAttrs))
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
	c.parallelMinBytes = defaultParallelMinBytes
	c.queryPar = resolveQueryParallelism(opts.QueryParallelism)
	if c.queryPar >= 2 {
		// Machine-wide worker budget: bounds total scan goroutines across all
		// concurrent queries so per-query fan-out never oversubscribes the cores.
		c.qsem = make(chan struct{}, runtime.GOMAXPROCS(0))
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
		si := int(h & c.mask)
		byShard[si] = append(byShard[si], pendingPut{hash: h, key: batch[i].Key, ad: stored, codec: codec})
	}
	for si, writes := range byShard {
		c.shards[si].commit(writes)
	}
	return c.writeError()
}

// Put inserts or updates a single ad. It is the hot path for the common
// one-ad-at-a-time daemon pattern, so unlike Update it takes no per-call slice or
// map allocations: it encodes, compresses, and commits directly to the one shard.
func (c *Collection) Put(key []byte, ad *classad.ClassAd) error {
	codec := c.currentCodec()
	stored := codec.Compress(nil, c.encodeAd(ad.AST()))
	h := c.h.Hash(key)
	c.shards[h&c.mask].commitOne(pendingPut{hash: h, key: key, ad: stored, codec: codec})
	return c.writeError()
}

// Delete removes key, returning whether it was present. The removal is an MVCC
// tombstone: scans already in progress still see the pre-delete version.
func (c *Collection) Delete(key []byte) bool {
	h := c.h.Hash(key)
	sh := c.shards[h&c.mask]
	sh.mu.Lock()
	seq := sh.commitSeq + 1
	ok := sh.del(h, key, seq)
	if ok {
		sh.commitSeq = seq
	}
	sh.mu.Unlock()
	if ok {
		sh.sync() // durability point for the tombstone (not coalesced)
		if sh.delLog != nil {
			sh.delLog.record(key, seq) // retain for resuming watchers
			sh.hub.publish(sh.idx, seq, key, nil, nil, true)
		}
	}
	return ok
}

// Get returns the current ad for key, decoded, or (nil, false).
func (c *Collection) Get(key []byte) (*classad.ClassAd, bool) {
	h := c.h.Hash(key)
	sh := c.shards[h&c.mask]
	stored, codec, ok := sh.get(h, key)
	if !ok {
		return nil, false
	}
	ad, err := c.decodeAd(stored, codec)
	if err != nil {
		return nil, false
	}
	return ad, true
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

// Scan returns an iterator over every ad in the collection. It is scan-exactly-
// once: each key present at the moment a shard's scan begins is yielded exactly
// once (never duplicated, never skipped), even while concurrent updates and
// compaction run. An ad updated mid-scan is seen at whichever version was current
// when that shard's scan started.
func (c *Collection) Scan() iter.Seq[*classad.ClassAd] {
	return func(yield func(*classad.ClassAd) bool) {
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
		// Record which attributes the query filters on (for SuggestIndexes), then,
		// if the query has an index-usable constraint, visit only candidate ads;
		// otherwise fall back to a full scan. Both re-verify the full predicate.
		probes := q.Probes()
		c.demand.record(probes)
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
	cont := true
	var dbuf []byte // decompression buffer reused across ads (single-threaded scan)
	forEachVisible(s0, wins, func(ad []byte, codec Codec) bool {
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
		return qp.m.Matches(partialDecodeWire(qp.ws.ctx, w, qp.plan.Seeds))
	}
	a, err := qp.ws.ctx.decodeWire(w)
	if err != nil {
		return false
	}
	return qp.m.Matches(classad.FromAST(a))
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
