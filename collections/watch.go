package collections

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	// when a precise incremental resume is impossible (first subscribe, cursor older
	// than the delete-retention window, or a different store generation).
	WatchReset
	// WatchSynced marks the end of the initial catch-up/snapshot: the client is now
	// live. Its Cursor is a durable resume point (and, after a Reset, the point to
	// swap the shadow state live).
	WatchSynced
	// WatchResync tells the client the live stream fell behind and it must reconnect
	// with its last persisted cursor (which re-enters catch-up). No state is implied.
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
// advances to the newest trimmed seq -- a cursor at or below horizon may have missed
// a trimmed delete and must fall back to a full replay.

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

// since returns the deletes with seq > cursor (a copy) and the current horizon.
func (d *deleteLog) since(cursor uint64) []delEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []delEntry
	for _, e := range d.entries {
		if e.seq > cursor {
			out = append(out, e)
		}
	}
	return out
}

func (d *deleteLog) horizonSeq() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.horizon
}

// --- live subscription hub ---

type rawEvent struct {
	shard   int
	seq     uint64
	key     []byte
	ad      []byte // compressed stored bytes; nil for a delete
	codec   Codec
	deleted bool
}

type watcher struct {
	ch     chan rawEvent
	lagged atomic.Bool
}

type watchHub struct {
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

// publish fans one committed change out to every active watcher, non-blocking: a
// watcher whose buffer is full is marked lagged (it will be told to resync) rather
// than stalling the commit path.
func (h *watchHub) publish(shard int, seq uint64, key, ad []byte, codec Codec, deleted bool) {
	if !h.active.Load() {
		return
	}
	h.mu.Lock()
	if len(h.watchers) == 0 {
		h.mu.Unlock()
		return
	}
	ev := rawEvent{shard, seq, append([]byte(nil), key...), ad, codec, deleted}
	for w := range h.watchers {
		select {
		case w.ch <- ev:
		default:
			w.lagged.Store(true)
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
	}
	return encodeCursor(c.hub.epoch, seqs), nil
}

// Watch replays everything that may have changed since cursor (nil ⇒ a full replay
// from empty), then streams live changes until ctx is cancelled or the consumer
// stops. Requires Options.WatchHistory > 0. See docs/WATCH.md and WatchEvent.
func (c *Collection) Watch(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error) {
	if c.hub == nil {
		return nil, errors.New("collections: Watch requires Options.WatchHistory > 0")
	}
	return func(yield func(WatchEvent) bool) {
		w := c.hub.register(c.watchBuf)
		defer c.hub.deregister(w)

		// Decide incremental vs. full replay.
		epoch, seqs, ok := decodeCursor(cursor)
		full := !ok || epoch != c.hub.epoch || len(seqs) != len(c.shards)

		// Snapshot each shard's commit sequence: the upper bound of catch-up. The
		// watcher is already registered, so any commit past this point is buffered.
		sReg := make([]uint64, len(c.shards))
		for i, sh := range c.shards {
			sh.mu.RLock()
			sReg[i] = sh.commitSeq
			sh.mu.RUnlock()
		}
		// Retention: if a shard's cursor predates its delete horizon, a delete may have
		// been trimmed -> full replay.
		if !full {
			for i, sh := range c.shards {
				if seqs[i] < sh.delLog.horizonSeq() {
					full = true
					break
				}
			}
		}

		if full {
			if !yield(WatchEvent{Kind: WatchReset}) {
				return
			}
			for i := range c.shards {
				if !c.catchupUpserts(i, 0, sReg[i], yield) {
					return
				}
			}
		} else {
			for i := range c.shards {
				// Deletes before upserts: a key deleted then re-added since the cursor
				// must end present (Delete then Upsert), not absent.
				if !c.catchupDeletes(i, seqs[i], yield) {
					return
				}
				if !c.catchupUpserts(i, seqs[i], sReg[i], yield) {
					return
				}
			}
		}
		if !yield(WatchEvent{Kind: WatchSynced, Cursor: encodeCursor(c.hub.epoch, sReg)}) {
			return
		}

		// Live phase: stream buffered + new events, advancing a running cursor vector.
		vec := append([]uint64(nil), sReg...)
		if c.watchCoalesce > 0 {
			c.liveCoalesced(ctx, w, sReg, vec, yield)
			return
		}
		parentSig := c.seedParentSig() // parent -> non-private signature (fan-out diff baseline)
		for {
			if w.lagged.Load() {
				yield(WatchEvent{Kind: WatchResync})
				return
			}
			select {
			case <-ctx.Done():
				return
			case raw := <-w.ch:
				if raw.seq <= sReg[raw.shard] {
					continue // already covered by catch-up
				}
				vec[raw.shard] = raw.seq
				if !raw.deleted && c.watchHidden(raw.key) {
					// A structural (parent) change: fan out to its children if an
					// inherited attribute changed; the parent itself is not emitted.
					for _, ce := range c.fanoutChildren(raw, parentSig) {
						ad, ok := c.watchAd(ce.key, ce.ad, ce.codec)
						if !ok {
							continue
						}
						if !yield(WatchEvent{Kind: WatchUpsert, Key: ce.key, Ad: ad, Cursor: encodeCursor(c.hub.epoch, vec)}) {
							return
						}
					}
					continue
				}
				ev := WatchEvent{Key: raw.key, Cursor: encodeCursor(c.hub.epoch, vec)}
				if raw.deleted {
					if c.watchHidden(raw.key) {
						continue // structural delete: hidden
					}
					ev.Kind = WatchDelete
				} else {
					ad, ok := c.watchAd(raw.key, raw.ad, raw.codec)
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
func (c *Collection) liveCoalesced(ctx context.Context, w *watcher, sReg, vec []uint64, yield func(WatchEvent) bool) {
	pending := map[string]rawEvent{}
	order := make([]string, 0, 16) // first-seen order; keys are unique per window
	parentSig := c.seedParentSig()

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
				ad, ok := c.watchAd(raw.key, raw.ad, raw.codec)
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
			if raw.seq <= sReg[raw.shard] {
				continue // already covered by catch-up
			}
			vec[raw.shard] = raw.seq
			if !raw.deleted && c.watchHidden(raw.key) {
				// Structural parent change: coalesce its children's synthetic
				// upserts (the parent itself is not emitted).
				for _, ce := range c.fanoutChildren(raw, parentSig) {
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

// watchAd decodes a watched key's stored ad for a WatchUpsert event. For a
// chained collection it flattens the ad's parent in (so watch events, like query
// results, carry inherited attributes) and returns ok=false for a structural
// (parent-only) ad, which is hidden from watches. For a plain collection it just
// decodes. ok=false also means "skip this event" (undecodable or hidden).
func (c *Collection) watchAd(key, rawAd []byte, codec Codec) (*classad.ClassAd, bool) {
	if c.isStructural != nil && c.isStructural(key) {
		return nil, false // structural ads are hidden from watches
	}
	ad, err := c.decodeAd(rawAd, codec)
	if err != nil {
		return nil, false
	}
	if c.parentKeyFor != nil {
		if pk := c.parentKeyFor(key); pk != nil {
			ph := c.h.Hash(pk)
			sh := c.shards[c.shardOf(pk, ph)]
			if pad, pcodec, ok := sh.get(ph, pk); ok {
				if parent, err := c.decodeAd(pad, pcodec); err == nil {
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
func (c *Collection) nonPrivateSig(ad *classad.ClassAd) map[string]string {
	sig := make(map[string]string)
	for _, name := range ad.GetAttributes() {
		lname := strings.ToLower(name)
		if c.parentPrivate != nil {
			if _, priv := c.parentPrivate[lname]; priv {
				continue
			}
		}
		if expr, ok := ad.Lookup(name); ok {
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

// seedParentSig records the current non-private signature of every structural ad,
// giving the live fan-out a baseline so a parent's first change is diffed (and a
// change to only parent-private attributes does not fan out). Empty for a
// non-chained collection.
func (c *Collection) seedParentSig() map[string]map[string]string {
	sig := map[string]map[string]string{}
	if c.parentKeyFor == nil || c.isStructural == nil {
		return sig
	}
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		forEachVisibleKeyed(s0, wins, func(key, ad []byte, codec Codec) bool {
			if c.isStructural(key) {
				if pad, err := c.decodeAd(ad, codec); err == nil {
					sig[string(key)] = c.nonPrivateSig(pad)
				}
			}
			return true
		})
		releaseWindows(wins)
	}
	return sig
}

// fanoutChildren handles a structural (parent) change during a live watch. If a
// non-private parent attribute changed since sig last recorded this parent, it
// returns synthetic upsert events for the parent's children (whose flattened ads
// changed); otherwise nil. sig caches parents' non-private signatures across the
// watch so parent-private churn (bumped every proc a factory materializes) does
// not fan out. The parent itself is never emitted.
func (c *Collection) fanoutChildren(parent rawEvent, sig map[string]map[string]string) []rawEvent {
	np, err := c.decodeAd(parent.ad, parent.codec)
	if err != nil {
		return nil
	}
	pkey := string(parent.key)
	newSig := c.nonPrivateSig(np)
	old, seen := sig[pkey]
	sig[pkey] = newSig
	if seen && sigEqual(old, newSig) {
		return nil // only parent-private attributes changed: no fan-out
	}
	// Collect the parent's children (co-located in the parent's shard) with their
	// current stored ads, as synthetic upserts stamped at the parent's sequence.
	ph := c.h.Hash(parent.key)
	sh := c.shards[c.shardOf(parent.key, ph)]
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	var out []rawEvent
	forEachVisibleKeyed(s0, wins, func(k, ad []byte, codec Codec) bool {
		if c.isStructural != nil && c.isStructural(k) {
			return true
		}
		if pk := c.parentKeyFor(k); pk != nil && bytes.Equal(pk, parent.key) {
			out = append(out, rawEvent{
				shard: parent.shard,
				seq:   parent.seq,
				key:   append([]byte(nil), k...),
				ad:    append([]byte(nil), ad...),
				codec: codec,
			})
		}
		return true
	})
	return out
}

// catchupUpserts emits an Upsert for every record visible at sReg whose seq is in
// (cursor, sReg] -- the keys whose current version changed since the cursor.
func (c *Collection) catchupUpserts(i int, cursor, sReg uint64, yield func(WatchEvent) bool) bool {
	sh := c.shards[i]
	_, wins := sh.snapshot()
	defer releaseWindows(wins)
	for _, wn := range wins {
		for off := 0; off < wn.used; {
			o := uint32(off)
			total := recTotalLen(wn.data, o)
			if total == 0 {
				break
			}
			seq := recSeq(wn.data, o)
			if seq > cursor && seq <= sReg && recSuperseded(wn.data, o) > sReg {
				key := recKey(wn.data, o)
				if ad, ok := c.watchAd(key, recAd(wn.data, o), wn.codec); ok {
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

// catchupDeletes emits a Delete for each journaled delete with seq > cursor.
func (c *Collection) catchupDeletes(i int, cursor uint64, yield func(WatchEvent) bool) bool {
	for _, e := range c.shards[i].delLog.since(cursor) {
		if c.watchHidden(e.key) {
			continue // structural delete: hidden
		}
		if !yield(WatchEvent{Kind: WatchDelete, Key: e.key}) {
			return false
		}
	}
	return true
}
