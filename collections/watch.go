package collections

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"iter"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// Watch lets a client subscribe to full-ad updates with a resumable, opaque cursor.
// See docs/WATCH.md for the design. Semantics are at-least-once: over-delivery is
// possible, a net change is never missed. Deletes are the only subtlety -- their
// evidence (a tombstone) is retained only within a bounded window (Options.
// WatchHistory), so a resume from before that window falls back to a full replay.

// WatchKind is the type of a WatchEvent.
type WatchKind uint8

const (
	// WatchUpsert carries the full ad for an added or updated key (Ad is set).
	WatchUpsert WatchKind = iota
	// WatchDelete signals a key was removed (Ad is nil).
	WatchDelete
	// WatchReset tells the client to discard its state (build into a shadow): an
	// authoritative full snapshot of Upserts follows, ending at WatchSynced. Emitted
	// when a precise incremental resume is impossible: first subscribe; a cursor older
	// than the delete-retention window or append floor, which includes every cursor
	// issued before a Truncate; a cursor ahead of the head; or a different store
	// generation -- any restart of a mutable collection and an unclean restart of an
	// append-only one (see watchepoch.go).
	WatchReset
	// WatchSynced marks the end of the initial catch-up/snapshot: the client is now
	// live. Its Cursor is a durable resume point (and, after a Reset, the point to
	// swap the shadow state live).
	WatchSynced
	// WatchResync tells the client the live stream fell behind, or the collection was
	// truncated, and it must reconnect with its last persisted cursor (which re-enters
	// catch-up; after a Truncate that cursor gets a Reset). No state is implied.
	WatchResync
)

// WatchEvent is one item in a Watch stream. Cursor, when non-nil, is an opaque token
// the client persists after processing the event and passes back to Watch on resume.
// Catch-up data events carry no cursor (persist at WatchSynced); live events do.
type WatchEvent struct {
	Kind   WatchKind
	Key    []byte
	Ad     *classad.ClassAd
	Cursor []byte
	// Stamp names this change in a clock shared by every shard, so two readers of the same
	// collection give the same change the same number and a consumer needing a single
	// comparable version (a Kubernetes resourceVersion, say) can take it from here rather
	// than inventing one. It is NOT a position: resuming is Cursor's job.
	//
	// Live events carry it. Catch-up data events do not, for the same reason they carry no
	// cursor; WatchSynced carries the collection's watermark, which is where a catch-up's
	// version comes from.
	//
	// Stamps from one shard strictly increase. ACROSS shards they are comparable but not
	// delivery-ordered -- two shards publish independently -- so a consumer that needs a
	// non-decreasing version takes a running maximum. WatchWatermark is the value that is
	// closed across every shard.
	Stamp uint64
}

// --- opaque cursor: {epoch, perShardSeq[]} ---

func encodeCursor(epoch uint64, seqs []uint64) []byte {
	b := make([]byte, 16+8*len(seqs))
	binary.LittleEndian.PutUint64(b[0:], epoch)
	binary.LittleEndian.PutUint64(b[8:], uint64(len(seqs)))
	for i, s := range seqs {
		binary.LittleEndian.PutUint64(b[16+8*i:], s)
	}
	return b
}

func decodeCursor(b []byte) (epoch uint64, seqs []uint64, ok bool) {
	if len(b) < 16 {
		return 0, nil, false
	}
	epoch = binary.LittleEndian.Uint64(b[0:])
	n := binary.LittleEndian.Uint64(b[8:])
	if uint64(len(b)) != 16+8*n {
		return 0, nil, false
	}
	seqs = make([]uint64, n)
	for i := range seqs {
		seqs[i] = binary.LittleEndian.Uint64(b[16+8*i:])
	}
	return epoch, seqs, true
}

func randomEpoch() uint64 {
	var b [8]byte
	_, _ = cryptorand.Read(b[:])
	e := binary.LittleEndian.Uint64(b[:])
	if e == 0 {
		e = 1 // reserve 0 as "no epoch"
	}
	return e
}

// --- per-shard delete journal ---
//
// Deletes write no record, so their evidence would vanish; the journal retains the
// most recent deletes so a resuming watcher can be told precisely which keys went
// away. It holds between cap and 2*cap entries; older ones are trimmed and horizon
// advances to the newest trimmed seq -- a cursor below horizon may have missed
// a trimmed delete and must fall back to a full replay.
//
// A delete is journaled under the shard write lock, in the same critical section that
// advances commitSeq past it (see journalDeleteLocked), so the journal holds every
// delete at or below any commitSeq a watcher can snapshot.

type delEntry struct {
	key []byte
	seq uint64
}

type deleteLog struct {
	mu      sync.Mutex
	entries []delEntry
	cap     int
	horizon uint64
}

func newDeleteLog(capacity int, start uint64) *deleteLog {
	return &deleteLog{cap: capacity, horizon: start}
}

func (d *deleteLog) record(key []byte, seq uint64) {
	d.mu.Lock()
	d.entries = append(d.entries, delEntry{append([]byte(nil), key...), seq})
	if len(d.entries) >= 2*d.cap {
		drop := len(d.entries) - d.cap
		d.horizon = d.entries[drop-1].seq
		d.entries = append([]delEntry(nil), d.entries[drop:]...) // compact
	}
	d.mu.Unlock()
}

// deleteMarkersSince returns the deletes with seq in (cursor, upTo] read from the delete
// markers in storage, for a cursor the in-memory journal has already trimmed past. It
// reports false when storage cannot be trusted to hold them all, which is the honest answer
// and costs only a full replay.
//
// Two things have to hold. The retention window (Options.WatchRetain) must still cover
// the cursor, which is what keeps reclamation from unlinking a segment whose markers are
// above the floor. And no Truncate can have happened since, because that drops the segments
// outright whatever the window says.
//
// The walk runs under the read lock rather than over pinned windows: the window builder
// skips segments with nothing visible, which is precisely where a delete marker lives once
// the records around it have been superseded. The keys are copied out, so nothing escapes
// the lock.
func (sh *shard) deleteMarkersSince(cursor, upTo uint64) ([]delEntry, bool) {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	if sh.watchRetain.Load() <= 0 || cursor < sh.watchFloorLocked() || cursor < sh.delFloor {
		return nil, false
	}
	var out []delEntry
	for _, seg := range sh.segs {
		if seg == nil || seg.used == 0 || seg.maxSeq <= cursor {
			continue
		}
		forEachCarriedMarker(seg, func(seq uint64, body []byte) {
			if seq <= cursor || seq > upTo || len(body) <= 8 {
				return
			}
			out = append(out, delEntry{key: append([]byte(nil), body[8:]...), seq: seq})
		})
	}
	// The journal yields deletes in commit order and catch-up emits them before the upserts
	// of the same shard, so a key deleted and re-added since the cursor still ends present.
	// Segments are walked in id order, which is not sequence order after a rewrite.
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out, true
}

// truncate forgets every journaled delete and raises the horizon to seq: the collection
// was emptied at seq, and a cursor below it must fall back to a full replay rather than
// replay the journal. Caller holds the shard write lock, in the critical section that sets
// commitSeq = seq (Truncate).
func (d *deleteLog) truncate(seq uint64) {
	d.mu.Lock()
	d.entries = nil
	d.horizon = max(d.horizon, seq)
	d.mu.Unlock()
}

// setHorizon raises the journal's horizon to seq. A reopened collection calls it: the
// journal is in memory, so it starts empty, and an empty journal with a zero horizon claims
// that nothing was ever deleted rather than that it cannot say. A cursor from before the
// restart has to be told to look elsewhere.
func (d *deleteLog) setHorizon(seq uint64) {
	d.mu.Lock()
	d.horizon = max(d.horizon, seq)
	d.mu.Unlock()
}

// window returns the deletes with seq in (cursor, upTo] (a copy; the keys are never
// mutated) and the horizon, read together: a cursor below the returned horizon may have
// lost a trimmed delete, and checking the horizon separately from the read would let a
// trim slip in between.
func (d *deleteLog) window(cursor, upTo uint64) ([]delEntry, uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []delEntry
	for _, e := range d.entries {
		if e.seq > cursor && e.seq <= upTo {
			out = append(out, e)
		}
	}
	return out, d.horizon
}

// journalDeleteLocked journals a delete committed at seq. Caller holds the shard write
// lock and is about to (or just did) set commitSeq = seq in the same critical section.
func (sh *shard) journalDeleteLocked(key []byte, seq uint64) {
	if sh.delLog != nil {
		sh.delLog.record(key, seq)
	}
}

// --- publish ordering ---
//
// A commit advances commitSeq under the shard lock but publishes its events only after
// unlocking and syncing, so two commits to one shard can reach the hub in either order,
// and a batch's events reach it one at a time. A live event's cursor tells the client
// "you have everything at or below this seq", so it may only name a seq once every
// event at or below it has been handed to the watcher. pubOrder tracks which seqs are
// still being published, and each event carries the highest seq that is safe to name
// (rawEvent.through).
//
// Only commits made while a watcher is attached are tracked. A commit that saw no
// watcher (hub.active false under the shard write lock) is at or below the S_reg of
// every watcher that registers later -- registration sets active before snapshotting
// commitSeq under the shard lock -- so every watcher covers it in catch-up and drops
// its live event, and it can never hold a cursor back.

// pendingPub is a tracked commit: the sequence it was issued, and the stamp that names
// it in the shard's clock (see stampLocked).
type pendingPub struct {
	seq   uint64
	stamp uint64
}

type pubOrder struct {
	n       atomic.Int32 // len(pending), for publish's no-watcher fast path
	mu      sync.Mutex
	pending []pendingPub // tracked commits not yet fully published, ascending by seq
	done    uint64       // highest seq fully published
	// doneStamp is the stamp of the highest seq fully published, and last is the highest
	// stamp this shard has handed out. They are separate because a stamp is assigned when
	// a commit is tracked and retired when it finishes publishing, so the newest stamp
	// assigned is usually ahead of the newest one that has gone out.
	doneStamp uint64
	last      uint64
}

// throughLocked is the highest seq whose events have all been handed to the hub (or
// need not be: untracked, or a seq that published nothing). Caller holds o.mu.
func (o *pubOrder) throughLocked() uint64 {
	if len(o.pending) > 0 {
		return o.pending[0].seq - 1
	}
	return o.done
}

// stampThroughLocked is this shard's closed watermark: the highest stamp at or below
// which every event has been handed to the hub. Stamps are assigned in sequence order
// under the shard write lock, so everything still pending is at or above pending[0], and
// one below it is the furthest this shard can vouch for. Caller holds o.mu.
func (o *pubOrder) stampThroughLocked() uint64 {
	if len(o.pending) > 0 {
		return o.pending[0].stamp - 1
	}
	return o.doneStamp
}

// stampLocked issues the next stamp: wall-clock microseconds, forced past the last one so
// a shard's stamps strictly increase whatever the clock does. Caller holds o.mu.
func (o *pubOrder) stampLocked() uint64 {
	st := nowMicros()
	if st <= o.last {
		st = o.last + 1
	}
	o.last = st
	return st
}

// trackPublishLocked registers a commit at seq that will publish, reporting whether it
// did (a watcher is attached). Caller holds the shard write lock, in the critical
// section that sets commitSeq = seq.
func (sh *shard) trackPublishLocked(seq uint64) bool {
	if sh.hub == nil || !sh.hub.active.Load() {
		return false
	}
	o := &sh.pub
	o.mu.Lock()
	// seqs are issued in order under the shard lock, and the stamp is taken here so it
	// orders the same way -- a commit's place in the clock is fixed when the commit is,
	// not when it finishes publishing (which races with every other publisher).
	o.pending = append(o.pending, pendingPub{seq: seq, stamp: o.stampLocked()})
	o.n.Add(1)
	o.mu.Unlock()
	return true
}

// A tracked seq that never retired would hold every later cursor back (resumes then
// over-deliver; nothing is lost), so every commit that tracks a seq must publish it:
// applyOne/applyWrites/applyBatch, Delete, and Txn.Commit via publishTxn. A
// deferred append (commitDeferred) publishes from the sync that makes it durable
// (publishDeferred), or is retired unpublished when Truncate drops it.

// seqPublisher hands one commit's events to the watch hub. It holds back the last
// event so that event can be sent in the same critical section that retires the seq:
// only then may its cursor name the seq itself.
type seqPublisher struct {
	sh    *shard
	seq   uint64
	stamp uint64
	on    bool
	has   bool
	held  rawEvent
}

// beginPublish starts publishing the events committed at seq. Must run after the
// commit's durability sync (watchers never see an event a crash could lose), and every
// beginPublish must be paired with end.
func (sh *shard) beginPublish(seq uint64) seqPublisher {
	on := sh.hub != nil && (sh.hub.active.Load() || sh.pub.n.Load() > 0)
	p := seqPublisher{sh: sh, seq: seq, on: on}
	if on {
		o := &sh.pub
		o.mu.Lock()
		// An untracked commit (the hub went active after it) has no stamp of its own. Its
		// events are dropped by every watcher as at or below their S_reg, so the value
		// only has to be one that cannot pull the clock backwards.
		p.stamp = o.last
		for _, e := range o.pending {
			if e.seq == seq {
				p.stamp = e.stamp
				break
			}
		}
		o.mu.Unlock()
	}
	return p
}

// add queues one event of this commit. key, ad must stay valid until end.
func (p *seqPublisher) add(key, ad []byte, codec Codec, deleted bool) {
	if !p.on {
		return
	}
	if p.has {
		// Not the last event: its seq is still pending, so through stays below it.
		o := &p.sh.pub
		o.mu.Lock()
		p.held.through = o.throughLocked()
		o.mu.Unlock()
		p.sh.hub.send(p.held)
	}
	p.held = rawEvent{shard: p.sh.idx, seq: p.seq, stamp: p.stamp, key: key, ad: ad, codec: codec, deleted: deleted}
	if h := publishMidHook; h != nil {
		h()
	}
	p.has = true
}

// publishMidHook runs inside a commit's publish, after an event has gone to the hub and
// while the commit's seq is still pending. Nil in production.
//
// It exists because the watermark's safety property -- that it never names a version an
// in-flight commit will be stamped at or below -- is invisible from outside: every
// synchronous write has finished publishing by the time it returns, so a test sampling
// between writes finds nothing in flight and cannot tell a correct watermark from one that
// covers the commit it is in the middle of.
var publishMidHook func()

// end sends the last event and retires the seq, atomically with respect to every other
// publisher of the shard: none can compute a through at or above this seq until this
// commit's last event is in every watcher's buffer.
func (p *seqPublisher) end() {
	if !p.on {
		return
	}
	o := &p.sh.pub
	o.mu.Lock()
	for i, e := range o.pending {
		if e.seq == p.seq {
			o.pending = append(o.pending[:i], o.pending[i+1:]...)
			o.n.Add(-1)
			break
		}
	}
	if p.seq > o.done {
		o.done = p.seq
		o.doneStamp = p.stamp
	}
	if p.has {
		p.held.through = o.throughLocked()
		p.sh.hub.send(p.held)
	}
	o.mu.Unlock()
	p.has, p.held = false, rawEvent{}
}

// WatchWatermark is the collection's closed watch clock: every change at or below it has
// been handed to every attached watcher, and no later change will be stamped at or below
// it. It is the version a catch-up has caught up TO, and the one value here that is both
// comparable across shards and non-decreasing in delivery order.
//
// It is the minimum over the shards, because a change is only covered when EVERY shard has
// published past it. That makes an idle shard the limit: one with nothing to publish would
// otherwise pin the whole collection at its last write. A shard with nothing in flight is
// therefore advanced to the current reading first -- safe because its next commit takes a
// stamp past whatever it was advanced to, under the same lock.
//
// Returns 0 when the collection has no watch (Options.WatchHistory == 0).
func (c *Collection) WatchWatermark() uint64 {
	if c.hub == nil {
		return 0
	}
	now := nowMicros()
	wm := uint64(math.MaxUint64)
	for _, sh := range c.shards {
		o := &sh.pub
		o.mu.Lock()
		if len(o.pending) == 0 {
			// Nothing in flight, so every stamp this shard has issued has already gone to
			// the hub and it can vouch for all of them -- which doneStamp alone does not
			// say, because a burst forces stamps past each other and past the clock.
			// Claiming the present on top is what keeps a quiet shard from holding the
			// whole collection back; the next commit here takes a stamp past it.
			if now > o.last {
				o.last = now
			}
			if o.last > o.doneStamp {
				o.doneStamp = o.last
			}
		}
		if st := o.stampThroughLocked(); st < wm {
			wm = st
		}
		o.mu.Unlock()
	}
	if wm == math.MaxUint64 {
		return 0
	}
	return wm
}

// --- live subscription hub ---

type rawEvent struct {
	shard int
	seq   uint64
	// stamp is the publishing shard's clock reading for this commit (see
	// pubOrder.stampLocked). Unlike seq it is comparable across shards.
	stamp uint64
	key   []byte
	// ad is the raw stored bytes: codec-compressed and, for encrypted attributes,
	// sealed with THIS node's data key. It is an internal, node-local representation.
	// INVARIANT: it must be decoded via watchAdAs/decodeAd (which decompresses and
	// decrypts) before it leaves the process. Never ship rawEvent.ad over the wire or
	// to a replica -- a follower has a different data key and could not open it, and it
	// would bypass the private-attribute stripping that adString applies to a decoded ad.
	ad      []byte // nil for a delete
	codec   Codec
	deleted bool
	// through is the highest seq of this shard whose events had all been handed to the
	// hub when this one was (see pubOrder): the furthest a cursor can safely point once
	// the watcher has received this event.
	through uint64
}

type watcher struct {
	ch     chan rawEvent
	lagged atomic.Bool
}

type watchHub struct {
	// epoch is random per process; Open restores it for a persistent append-only
	// collection that was closed cleanly (see watchepoch.go). Fixed once Open returns.
	epoch    uint64
	active   atomic.Bool // lock-free gate: any watchers?
	mu       sync.Mutex
	watchers map[*watcher]struct{}
}

func newWatchHub() *watchHub {
	return &watchHub{epoch: randomEpoch(), watchers: map[*watcher]struct{}{}}
}

func (h *watchHub) register(buf int) *watcher {
	w := &watcher{ch: make(chan rawEvent, buf)}
	h.mu.Lock()
	h.watchers[w] = struct{}{}
	h.active.Store(true)
	h.mu.Unlock()
	return w
}

func (h *watchHub) deregister(w *watcher) {
	h.mu.Lock()
	delete(h.watchers, w)
	if len(h.watchers) == 0 {
		h.active.Store(false)
	}
	h.mu.Unlock()
}

// send fans one committed change out to every active watcher, non-blocking: a
// watcher whose buffer is full is marked lagged (it will be told to resync) rather
// than stalling the commit path. Callers go through seqPublisher.
func (h *watchHub) send(ev rawEvent) {
	if !h.active.Load() {
		return
	}
	h.mu.Lock()
	if len(h.watchers) == 0 {
		h.mu.Unlock()
		return
	}
	ev.key = append([]byte(nil), ev.key...)
	for w := range h.watchers {
		select {
		case w.ch <- ev:
		default:
			w.lagged.Store(true)
		}
	}
	h.mu.Unlock()
}

// kick forces every attached watcher to resync: each is marked lagged, exactly as if its
// buffer had overflowed, and woken with an inert event in case it is idle waiting for one.
// A lagged watcher yields WatchResync instead of anything it receives afterwards, and its
// client reconnects with its last cursor. Truncate calls this under the shard write lock
// (lock order: shard.mu, then h.mu, as publishing takes pub.mu then h.mu).
func (h *watchHub) kick() {
	h.mu.Lock()
	for w := range h.watchers {
		w.lagged.Store(true)
		select {
		// Never processed: lagged is stored before the send, so the receiver sees it and
		// stops first. (A zero event's seq, 0, is at or below every S_reg in any case.)
		case w.ch <- rawEvent{}:
		default: // buffer full: the watcher has events to read and will see lagged then
		}
	}
	h.mu.Unlock()
}

// --- the Watch verb ---

// WatchCursor returns an opaque cursor pointing at the current head of the change log,
// so a subsequent Watch(ctx, cursor) streams only changes from now on -- no initial
// replay of the current contents. Requires Options.WatchHistory > 0. It only snapshots
// each shard's commit sequence (no registration, no replay), so it is cheap.
func (c *Collection) WatchCursor() ([]byte, error) {
	if c.hub == nil {
		return nil, errors.New("collections: Watch requires Options.WatchHistory > 0")
	}
	seqs := make([]uint64, len(c.shards))
	for i, sh := range c.shards {
		sh.mu.RLock()
		seqs[i] = sh.commitSeq
		sh.mu.RUnlock()
		// A cursor names only durable seqs: a deferred append it covers is synced first.
		sh.syncFor(seqs[i])
	}
	return encodeCursor(c.hub.epoch, seqs), nil
}

// Watch replays everything that may have changed since cursor (nil ⇒ a full replay
// from empty), then streams live changes until ctx is cancelled or the consumer
// stops. Requires Options.WatchHistory > 0. See docs/WATCH.md and WatchEvent.
func (c *Collection) Watch(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error) {
	return c.watchAs(ctx, cursor, false)
}

// WatchRedacted is Watch for a watcher NOT entitled to sealed values: every event's ad is decoded with no
// key, so a sealed attribute arrives undefined rather than opened. See Collection.QueryRedacted.
//
// A watch streams ads continuously, so it is the read path with the longest exposure: decoding with the
// collection's key and trusting the serializer to drop private attributes means the secret is opened for
// every event of every unentitled watcher.
func (c *Collection) WatchRedacted(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error) {
	return c.watchAs(ctx, cursor, true)
}

func (c *Collection) watchAs(ctx context.Context, cursor []byte, redact bool) (iter.Seq[WatchEvent], error) {
	if c.hub == nil {
		return nil, errors.New("collections: Watch requires Options.WatchHistory > 0")
	}
	return func(yield func(WatchEvent) bool) {
		w := c.hub.register(c.watchBuf)
		defer c.hub.deregister(w)

		// Decide incremental vs. full replay.
		epoch, seqs, ok := decodeCursor(cursor)
		full := !ok || epoch != c.hub.epoch || len(seqs) != len(c.shards)

		// Snapshot each shard's commit sequence: S_reg, the upper bound of catch-up. The
		// watcher is already registered, so every commit past S_reg reaches it live.
		//
		// Invariant: every event with seq in (cursor, S_reg] is delivered exactly once, in
		// catch-up; every event with seq > S_reg exactly once, live. Everything catch-up
		// reads -- records and the delete journal -- is written under the shard write lock
		// in the same critical section that advances commitSeq, so it is complete through
		// S_reg the moment S_reg is read; catch-up reads only seq <= S_reg; and the live
		// phase drops seq <= S_reg. Publishing runs after unlock and sync, so the live
		// buffer may hold events at or below S_reg -- those are exactly the ones dropped.
		//
		// The fan-out baseline is seeded HERE, from the same snapshot, for the same
		// reason: a parent's live change is emitted only where it differs from what the
		// watcher last saw, so the baseline has to be the state at S_reg exactly. Seeding
		// it later -- after catch-up, which is unbounded -- reads a parent that a live
		// write has already moved, the diff then finds nothing to fan out, and the
		// children of that write are never delivered.
		sReg := make([]uint64, len(c.shards))
		parentSig := map[string]map[string]string{}
		chained := c.parentKeyFor != nil && c.isStructural != nil
		for i, sh := range c.shards {
			if !chained {
				sh.mu.RLock()
				sReg[i] = sh.commitSeq
				sh.mu.RUnlock()
				continue
			}
			// snapshot returns the commit sequence and the windows together, under one
			// lock, which is what makes the two agree.
			s0, wins := sh.snapshot()
			sReg[i] = s0
			c.seedParentSigInto(parentSig, s0, wins, redact)
			releaseWindows(wins)
		}
		// Catch-up reads the store through S_reg, so make it durable first: a deferred
		// append at or below S_reg would otherwise reach this watcher in catch-up before a
		// sync covers it, and a crash could take it back.
		for i, sh := range c.shards {
			sh.syncFor(sReg[i])
		}
		if watchSnapshotHook != nil {
			watchSnapshotHook(c)
		}

		plan := c.planCatchup(seqs, sReg, full)
		if !c.runCatchup(&plan, seqs, sReg, yield, redact) {
			return
		}
		if !yield(WatchEvent{Kind: WatchSynced, Stamp: c.WatchWatermark(), Cursor: encodeCursor(c.hub.epoch, sReg)}) {
			return
		}

		// Live phase: stream buffered + new events, advancing a running cursor vector.
		vec := append([]uint64(nil), sReg...)
		if c.watchCoalesce > 0 {
			c.liveCoalesced(ctx, w, sReg, vec, yield, redact, parentSig)
			return
		}
		for {
			if w.lagged.Load() {
				yield(WatchEvent{Kind: WatchResync})
				return
			}
			select {
			case <-ctx.Done():
				return
			case raw := <-w.ch:
				if w.lagged.Load() {
					// An event was dropped before this one was sent, and this one's
					// through may cover it: never hand out that cursor.
					yield(WatchEvent{Kind: WatchResync})
					return
				}
				if raw.seq <= sReg[raw.shard] {
					continue // already covered by catch-up
				}
				vec[raw.shard] = max(vec[raw.shard], raw.through)
				if !raw.deleted && c.watchHidden(raw.key) {
					// A structural (parent) change: fan out to its children if an
					// inherited attribute changed; the parent itself is not emitted.
					for _, ce := range c.fanoutChildren(raw, parentSig, redact) {
						ad, ok := c.watchAdAs(ce.key, ce.ad, ce.codec, redact)
						if !ok {
							continue
						}
						if !yield(WatchEvent{Kind: WatchUpsert, Key: ce.key, Ad: ad, Stamp: raw.stamp, Cursor: encodeCursor(c.hub.epoch, vec)}) {
							return
						}
					}
					continue
				}
				ev := WatchEvent{Key: raw.key, Stamp: raw.stamp, Cursor: encodeCursor(c.hub.epoch, vec)}
				if raw.deleted {
					if c.watchHidden(raw.key) {
						continue // structural delete: hidden
					}
					ev.Kind = WatchDelete
				} else {
					ad, ok := c.watchAdAs(raw.key, raw.ad, raw.codec, redact)
					if !ok {
						continue // undecodable or a hidden structural ad
					}
					ev.Kind = WatchUpsert
					ev.Ad = ad
				}
				if !yield(ev) {
					return
				}
			}
		}
	}, nil
}

// WatchFilter wraps a Watch event stream to deliver only events for keys whose
// ad satisfies match. It keeps a set of the keys currently matching so a filtered
// view stays correct as ads change:
//
//   - an Upsert whose ad matches is delivered (the key is marked matching);
//   - an Upsert whose ad no longer matches, for a key that was matching, is
//     converted to a Delete so the client drops it from its filtered view;
//   - a Delete is delivered for a key known to be matching, and -- during
//     catch-up (before Synced), where the prior match state of a resumed key is
//     unknown -- forwarded conservatively (the client no-ops an unknown key);
//   - Reset clears the matched set; Synced and Resync pass through.
//
// A nil match returns seq unchanged (no filtering). match is called on each
// Upsert's ad and must be safe for concurrent-free sequential use.
func WatchFilter(seq iter.Seq[WatchEvent], match func(*classad.ClassAd) bool) iter.Seq[WatchEvent] {
	if match == nil {
		return seq
	}
	return func(yield func(WatchEvent) bool) {
		matched := map[string]struct{}{}
		synced := false
		for ev := range seq {
			switch ev.Kind {
			case WatchUpsert:
				k := string(ev.Key)
				if match(ev.Ad) {
					matched[k] = struct{}{}
					if !yield(ev) {
						return
					}
				} else if _, was := matched[k]; was {
					delete(matched, k)
					if !yield(WatchEvent{Kind: WatchDelete, Key: ev.Key, Cursor: ev.Cursor}) {
						return
					}
				}
			case WatchDelete:
				k := string(ev.Key)
				if _, was := matched[k]; was {
					delete(matched, k)
					if !yield(ev) {
						return
					}
				} else if !synced {
					if !yield(ev) {
						return
					}
				}
			case WatchReset:
				matched = map[string]struct{}{}
				synced = false
				if !yield(ev) {
					return
				}
			case WatchSynced:
				synced = true
				if !yield(ev) {
					return
				}
			default: // Resync (and any future kinds) pass through
				if !yield(ev) {
					return
				}
			}
		}
	}
}

// liveCoalesced streams the live phase in windows of c.watchCoalesce, emitting
// only the newest event per key in each window (a key upserted several times, or
// upserted then deleted, collapses to a single event of its settled state). Only
// the last event of a flushed window carries a cursor, so a consumer that crashes
// mid-window resumes from the prior window and re-delivers -- at-least-once is
// preserved. The running cursor vector still advances on every event received.
// liveCoalesced streams coalesced live events; redact is the watcher's entitlement to sealed values (see
// WatchRedacted), threaded because this decodes ads itself.
func (c *Collection) liveCoalesced(ctx context.Context, w *watcher, sReg, vec []uint64, yield func(WatchEvent) bool, redact bool, parentSig map[string]map[string]string) {
	pending := map[string]rawEvent{}
	order := make([]string, 0, 16) // first-seen order; keys are unique per window

	ticker := time.NewTicker(c.watchCoalesce)
	defer ticker.Stop()

	// flush emits the coalesced window. Returns false if the consumer stopped.
	flush := func() bool {
		if len(pending) == 0 {
			return true
		}
		evs := make([]WatchEvent, 0, len(order))
		for _, ks := range order {
			raw := pending[ks]
			ev := WatchEvent{Key: raw.key}
			if raw.deleted {
				if c.watchHidden(raw.key) {
					continue // structural delete: hidden
				}
				ev.Kind = WatchDelete
			} else {
				ad, ok := c.watchAdAs(raw.key, raw.ad, raw.codec, redact)
				if !ok {
					continue // undecodable or a hidden structural ad
				}
				ev.Kind = WatchUpsert
				ev.Ad = ad
			}
			evs = append(evs, ev)
		}
		pending = map[string]rawEvent{}
		order = order[:0]
		if len(evs) == 0 {
			return true
		}
		evs[len(evs)-1].Cursor = encodeCursor(c.hub.epoch, vec)
		for i := range evs {
			if !yield(evs[i]) {
				return false
			}
		}
		return true
	}

	for {
		if w.lagged.Load() {
			if !flush() {
				return
			}
			yield(WatchEvent{Kind: WatchResync})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !flush() {
				return
			}
		case raw := <-w.ch:
			if w.lagged.Load() {
				// Dropped event before this one; see the uncoalesced loop.
				if !flush() {
					return
				}
				yield(WatchEvent{Kind: WatchResync})
				return
			}
			if raw.seq <= sReg[raw.shard] {
				continue // already covered by catch-up
			}
			vec[raw.shard] = max(vec[raw.shard], raw.through)
			if !raw.deleted && c.watchHidden(raw.key) {
				// Structural parent change: coalesce its children's synthetic
				// upserts (the parent itself is not emitted).
				for _, ce := range c.fanoutChildren(raw, parentSig, redact) {
					cks := string(ce.key)
					if _, seen := pending[cks]; !seen {
						order = append(order, cks)
					}
					pending[cks] = ce
				}
				continue
			}
			ks := string(raw.key)
			if _, seen := pending[ks]; !seen {
				order = append(order, ks)
			}
			pending[ks] = raw
		}
	}
}

// watchAdAs decodes a watched key's stored ad for a WatchUpsert event. For a chained collection
// it flattens the ad's parent in (so watch events, like query results, carry inherited
// attributes) and returns ok=false for a structural (parent-only) ad, which is hidden from
// watches. ok=false also means "skip this event" (undecodable or hidden). redact is for a
// watcher that may not be entitled to sealed values (see WatchRedacted).
func (c *Collection) watchAdAs(key, rawAd []byte, codec Codec, redact bool) (*classad.ClassAd, bool) {
	return c.watchAdDictAs(key, rawAd, codec, nil, redact)
}

// watchAdDictAs is watchAdAs for a record of an interned segment, whose attribute ids resolve only
// through the segment's dictionary.
func (c *Collection) watchAdDictAs(key, rawAd []byte, codec Codec, dict *segDictHandle, redact bool) (*classad.ClassAd, bool) {
	if c.isStructural != nil && c.isStructural(key) {
		return nil, false // structural ads are hidden from watches
	}
	ad, err := c.decodeAdDictAs(dict, rawAd, codec, redact)
	if err != nil {
		return nil, false
	}
	if c.parentKeyFor != nil {
		if pk := c.parentKeyFor(key); pk != nil {
			ph := c.h.Hash(pk)
			sh := c.shards[c.shardOf(pk, ph)]
			if pad, pcodec, pdict, _, ok := sh.get(c, ph, pk, mWire); ok {
				if parent, err := c.decodeAdDictAs(pdict, pad, pcodec, redact); err == nil {
					c.mergeParent(ad, parent)
				}
			}
		}
	}
	return ad, true
}

// watchHidden reports whether key is a structural ad (hidden from watches),
// used to suppress structural delete events.
func (c *Collection) watchHidden(key []byte) bool {
	return c.isStructural != nil && c.isStructural(key)
}

// nonPrivateSig renders a parent ad's non-private attributes to a comparable
// signature, so a parent change that touched only parent-private attributes
// (factory bookkeeping) is detected and does not fan out to children.
//
// keyless says the parent was decoded for a watcher not entitled to sealed values (see
// WatchRedacted) from a collection that seals some: each sealed value is then undefined, and
// an undefined value is left out of the signature as if the attribute were absent. Otherwise a
// parent that gained or lost a sealed attribute would fan out to such a watcher while one
// whose sealed value merely changed would not, and the fan-out itself would report a change
// the watcher may not see. Undefined and absent evaluate alike, so the signature still
// changes with everything the watcher can observe of the parent.
func (c *Collection) nonPrivateSig(ad *classad.ClassAd, keyless bool) map[string]string {
	sig := make(map[string]string)
	for _, name := range ad.GetAttributes() {
		lname := strings.ToLower(name)
		if c.parentPrivate != nil {
			if _, priv := c.parentPrivate[lname]; priv {
				continue
			}
		}
		if expr, ok := ad.Lookup(name); ok {
			if keyless {
				if _, undef := expr.AST().(*ast.UndefinedLiteral); undef {
					continue
				}
			}
			sig[lname] = expr.String()
		}
	}
	return sig
}

func sigEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// seedParentSigInto records the non-private signature of every structural ad in ONE shard's
// snapshot, giving the live fan-out a baseline so a parent's first change is diffed (and a change
// to only parent-private attributes does not fan out). redact is the watcher's entitlement (see
// WatchRedacted): the baseline is what that watcher could see of each parent, decoded with no key
// for a watcher without it.
//
// It takes the snapshot rather than the shard BECAUSE OF WHEN it has to be read. The baseline is
// only meaningful against a point in the commit sequence -- it answers "what had this watcher
// seen of this parent at S_reg" -- so it must come from the snapshot that defined S_reg, not from
// whatever the shard holds when the live phase happens to begin. Taking its own snapshot is what
// let a live write slip in between the two and silently cost that write its fan-out.
func (c *Collection) seedParentSigInto(sig map[string]map[string]string, s0 uint64, wins []segWindow, redact bool) {
	keyless := redact && c.SealsValues()
	c.forEachVisibleKeyed(s0, wins, sealedOpened, func(key, ad []byte, codec Codec, dict *segDictHandle) bool {
		if c.isStructural(key) {
			if pad, err := c.decodeAdDictAs(dict, ad, codec, redact); err == nil {
				sig[string(key)] = c.nonPrivateSig(pad, keyless)
			}
		}
		return true
	})
}

// fanoutChildren handles a structural (parent) change during a live watch. If a
// non-private parent attribute changed since sig last recorded this parent, it
// returns synthetic upsert events for the parent's children (whose flattened ads
// changed); otherwise nil. sig caches parents' non-private signatures across the
// watch so parent-private churn (bumped every proc a factory materializes) does
// not fan out. The parent itself is never emitted.
//
// redact is the watcher's entitlement (see WatchRedacted). Whether to fan out is itself
// an answer about the parent: decided with the key, a watcher not entitled to a sealed
// parent attribute would receive its children exactly when that attribute's value
// changed. So for such a watcher the parent is decoded with no key and diffed as that
// watcher sees it, and a change to sealed parent attributes alone fans out no more than
// a parent-private one does.
func (c *Collection) fanoutChildren(parent rawEvent, sig map[string]map[string]string, redact bool) []rawEvent {
	np, err := c.decodeAdDictAs(nil, parent.ad, parent.codec, redact)
	if err != nil {
		return nil
	}
	pkey := string(parent.key)
	newSig := c.nonPrivateSig(np, redact && c.SealsValues())
	old, seen := sig[pkey]
	sig[pkey] = newSig
	if seen && sigEqual(old, newSig) {
		return nil // nothing of the parent this watcher can see changed: no fan-out
	}
	// Collect the parent's children (co-located in the parent's shard) with their
	// current stored ads, as synthetic upserts stamped at the parent's sequence.
	ph := c.h.Hash(parent.key)
	sh := c.shards[c.shardOf(parent.key, ph)]
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	var out []rawEvent
	c.forEachVisibleKeyed(s0, wins, sealedOpened, func(k, ad []byte, codec Codec, dict *segDictHandle) bool {
		if c.isStructural != nil && c.isStructural(k) {
			return true
		}
		if pk := c.parentKeyFor(k); pk != nil && bytes.Equal(pk, parent.key) {
			// This event's ad bytes outlive the scan (decoded later by watchAdAs, when the
			// segment/dict is gone), so make an interned record self-contained now.
			sc := c.toSelfContainedAs(dict, ad, codec, redact)
			out = append(out, rawEvent{
				shard: parent.shard,
				seq:   parent.seq,
				key:   append([]byte(nil), k...),
				ad:    append([]byte(nil), sc...),
				codec: codec,
			})
		}
		return true
	})
	return out
}

// catchupPlan is the input to one watcher's catch-up, captured right after S_reg so
// each retention check is made against exactly what catch-up then reads.
type catchupPlan struct {
	full bool
	dels [][]delEntry  // per shard: journaled deletes in (cursor, S_reg]; incremental only
	wins [][]segWindow // per shard, append-only: windows pinned at S_reg; nil = snapshot on read
}

func (p *catchupPlan) release() {
	for _, w := range p.wins {
		releaseWindows(w)
	}
	p.wins = nil
}

func (p *catchupPlan) shardWins(i int) []segWindow {
	if p.wins == nil {
		return nil
	}
	return p.wins[i]
}

// planCatchup decides incremental vs. full replay for cursor seqs (full already set if
// the cursor is unusable) and captures what catch-up will read.
func (c *Collection) planCatchup(seqs, sReg []uint64, full bool) catchupPlan {
	p := catchupPlan{full: full}
	// Append-only gap: a cursor below the append floor -- records it may hold or never saw
	// were rotated out or truncated -- cannot resume -> full replay from the current floor.
	// Append logs keep no delete journal, so this is their reset trigger. The windows
	// catch-up reads are pinned in the same lock hold that reads the floor; otherwise a
	// Rotate between the check and the read drops records the check counted on. (An
	// append-only collection has one shard, so pinning up front holds no more than
	// catch-up itself would.)
	if c.appendOnly() {
		p.wins = make([][]segWindow, len(c.shards))
		for i, sh := range c.shards {
			var floor uint64
			p.wins[i], floor = sh.appendCatchupView(sReg[i])
			if !p.full && seqs[i] < floor {
				p.full = true
			}
		}
	}
	if p.full {
		return p
	}
	p.dels = make([][]delEntry, len(c.shards))
	for i, sh := range c.shards {
		// A cursor ahead of the head was not issued by this store's log as it stands (e.g.
		// an older copy of a persistent store reopened under the same epoch): its seqs may
		// later be reassigned to records it never saw, so it cannot be resumed.
		if seqs[i] > sReg[i] {
			p.full = true
			break
		}
		// Retention: a cursor below the delete horizon may have missed a trimmed delete,
		// or predates a Truncate (which raises the horizon to its seq).
		var horizon uint64
		p.dels[i], horizon = sh.delLog.window(seqs[i], sReg[i])
		if seqs[i] < horizon {
			// The journal has been trimmed past this cursor, but the deletes are also on
			// disk as markers; serve them from there when the retention window still
			// promises they are all present.
			dels, ok := sh.deleteMarkersSince(seqs[i], sReg[i])
			if !ok {
				p.full = true
				break
			}
			p.dels[i] = dels
		}
	}
	if p.full {
		p.dels = nil
	}
	return p
}

// runCatchup emits the catch-up for plan: a Reset and every record visible at S_reg, or
// per shard the deletes and then the upserts in (cursor, S_reg]. Returns false if the
// consumer stopped. Releases the plan's pins either way.
func (c *Collection) runCatchup(p *catchupPlan, seqs, sReg []uint64, yield func(WatchEvent) bool, redact bool) bool {
	defer p.release()
	if p.full {
		if !yield(WatchEvent{Kind: WatchReset}) {
			return false
		}
		for i := range c.shards {
			if !c.catchupUpserts(i, 0, sReg[i], p.shardWins(i), yield, redact) {
				return false
			}
		}
		return true
	}
	for i := range c.shards {
		// Deletes before upserts: a key deleted then re-added since the cursor must end
		// present (Delete then Upsert), not absent.
		if !c.catchupDeletes(p.dels[i], yield) {
			return false
		}
		if !c.catchupUpserts(i, seqs[i], sReg[i], p.shardWins(i), yield, redact) {
			return false
		}
	}
	return true
}

// catchupUpserts emits an Upsert for every record visible at sReg whose seq is in
// (cursor, sReg] -- the keys whose current version changed since the cursor. wins, if
// non-nil, are windows already pinned at sReg (the caller releases them); otherwise
// they are taken here. redact is the watcher's entitlement (see WatchRedacted),
// threaded because this decodes ads itself.
func (c *Collection) catchupUpserts(i int, cursor, sReg uint64, wins []segWindow, yield func(WatchEvent) bool, redact bool) bool {
	if wins == nil {
		// Windows at sReg, not at the current head: a segment whose records were all
		// superseded after sReg still holds versions catch-up must emit.
		wins = c.shards[i].snapshotAt(sReg)
		defer releaseWindows(wins)
	}
	var wbuf []byte
	for _, wn := range wins {
		for off := 0; off < wn.used; {
			o := uint32(off)
			if end, ok := wn.dmg.skip(o); ok {
				off = int(end)
				continue
			}
			total := recTotalLen(wn.data, o)
			if total == 0 {
				break
			}
			seq := recSeq(wn.data, o)
			if seq > cursor && seq <= sReg && recSuperseded(wn.data, o) > sReg {
				key := recKey(wn.data, o)
				// The FULL ad: a columnarized record carries only what its schema does not
				// cover, and a watch event holding half an ad is indistinguishable from an ad
				// whose attributes really were removed.
				adBytes, adCodec, aok := c.adBytes(recRef{w: wn, off: o, dict: wn.dict()}, sReg, &wbuf, sealedUseFor(redact))
				if !aok {
					off += int(total)
					continue
				}
				// Through the segment's dictionary: an interned record decoded without it fails, and
				// catch-up then dropped every record of an interned segment.
				if ad, ok := c.watchAdDictAs(key, adBytes, adCodec, wn.dict(), redact); ok {
					if !yield(WatchEvent{Kind: WatchUpsert, Key: append([]byte(nil), key...), Ad: ad}) {
						return false
					}
				}
			}
			off += int(total)
		}
	}
	return true
}

// catchupDeletes emits a Delete for each journaled delete in dels.
func (c *Collection) catchupDeletes(dels []delEntry, yield func(WatchEvent) bool) bool {
	for _, e := range dels {
		if c.watchHidden(e.key) {
			continue // structural delete: hidden
		}
		if !yield(WatchEvent{Kind: WatchDelete, Key: e.key}) {
			return false
		}
	}
	return true
}

// watching reports whether any watcher is attached. Callers use it to skip work that only a
// watcher would consume -- publishing already short-circuits on it, but work done to PREPARE
// an event happens before that and would otherwise be paid whether or not anyone is listening.
func (h *watchHub) watching() bool {
	if h == nil || !h.active.Load() {
		return false
	}
	h.mu.Lock()
	n := len(h.watchers)
	h.mu.Unlock()
	return n > 0
}

// watchSnapshotHook, when set (tests only), runs in Watch on c right after the watcher
// has snapshotted S_reg and before catch-up reads anything, so a test can commit inside
// that window deterministically.
var watchSnapshotHook func(c *Collection)
