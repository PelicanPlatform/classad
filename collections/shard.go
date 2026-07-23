package collections

import (
	"bytes"
	"sync"
	"sync/atomic"
	"time"
)

// shard owns an independent slice of the keyspace: a directory mapping a key hash
// to the head of a bucket chain, the arena segments the records live in, and a
// monotonic commit sequence used for MVCC scan visibility. Each shard is locked
// independently to reduce contention.
type shard struct {
	mu   sync.RWMutex
	dir  map[uint64]loc // key hash -> head of bucket chain (pointer-free value)
	segs []*segment     // indexed by segment id (id == index)
	act  *segment       // current append target

	commitSeq uint64 // bumped once per committed batch; scan snapshots capture it
	count     int    // number of live keys

	// gcFloor is the commit sequence at the most recent compaction: superseded/delete
	// evidence at or below it may have been reclaimed. A transaction whose snapshot
	// predates it cannot prove a currently-absent key was not deleted after its
	// snapshot, so conflictSince conservatively treats such a write as a conflict.
	// Guarded by mu.
	gcFloor uint64

	segSize int

	// Group-commit state (see commit.go), guarded by cmu.
	cmu      sync.Mutex
	queue    []*commitReq
	flushing bool
	onSync   func() // durability sync run once per committed batch; nil = no-op

	// Sync coalescing (group commit for the durability msync; see shard.syncFor).
	// syncedSeq is the highest commitSeq a COMPLETED msync pass covers; syncing marks a
	// pass in flight. A syncFor caller already covered rides free; others wait for or
	// lead a pass whose dirty capture postdates their writes. Guarded by smu/scond.
	smu       sync.Mutex
	scond     *sync.Cond
	syncing   bool
	syncedSeq uint64

	// Persistent segment allocation. alloc is nil for an in-memory shard (RAM
	// segments); for a persistent shard it creates an mmap-backed segment file.
	// writeErr is the first segment-allocation failure (sticky; guarded by mu),
	// surfaced to the caller by Put/Update.
	alloc    func(id uint32, size int, codec Codec) (*segment, error)
	writeErr error
	// sealRAM, when true, makes this (in-memory) shard's RAM segments seal their sealed
	// index to an anonymous mmap sidecar rather than keep it on the Go heap. It also makes
	// those RAM segments participate in pin/reap (see segment.mapped): the anon mapping is
	// not GC-managed, so scans must pin it and compaction/Close must unmap it. Set once at
	// construction (in-memory + mmap-supported + indexes configured); never mutated.
	sealRAM bool
	dirty   []*segment // segments with unsynced writes since the last sync (persistent)
	// dirtySup lists supersededBySeq fields tombstoned by a delete since the last
	// sync; their pages must be msync'd for the delete to be durable (unlike an
	// overwrite, a delete writes no new record, so recovery's max-seq rule cannot
	// re-derive it — the tombstone itself must reach disk). Guarded by mu.
	dirtySup []supRef

	// Watch support (see watch.go); nil/zero unless WatchHistory > 0. idx is the
	// shard's index (used to tag events for the cursor). hub fans committed changes
	// to live watchers; delLog retains recent deletes for resuming watchers.
	idx    int
	hub    *watchHub
	delLog *deleteLog

	// Chained-parent child counting (see store.go). childParentHash, if set, maps a
	// key to its parent's dir-hash and reports whether the key is a chained child;
	// it is nil unless the collection is configured with ParentKeyFor. childCount
	// tracks, per parent dir-hash, how many live children chain to it, so Delete can
	// tell in O(1) when a structural parent's last child has left (auto-delete)
	// without scanning the shard. The parent is co-located in this shard, so the
	// count is complete here. Keyed by hash (pointer-free) rather than the parent
	// key: a hash collision can only mask an auto-delete (a lingering empty parent),
	// never trigger a wrong one -- childCount[h] reaching zero means every parent
	// hashing to h has zero children, so this parent does too. Guarded by mu.
	childParentHash func(key []byte) (uint64, bool)
	childCount      map[uint64]int

	// tseq is this shard's time->seq checkpoint index for point-in-time queries
	// (see timeseq.go). Empty and unused unless the collection has TimeTravel enabled.
	tseq shardTimeIndex
	// tt points at the collection's time-travel config (shared, swappable at runtime);
	// nil-config means disabled. Set once at construction.
	tt *atomic.Pointer[ttConfig]

	// metrics accumulates this shard's operational timings (write-lock wait/hold,
	// segment allocation, durability sync). See opstats.go.
	metrics shardMetrics
}

// lockWrite acquires the shard write lock, returning the timestamps unlockWrite needs
// to attribute time to the writeWait (blocked acquiring) and writeHold (blocking the
// world) counters. Every lockWrite must be paired with an unlockWrite.
func (sh *shard) lockWrite() (acq, held time.Time) {
	acq = time.Now()
	sh.mu.Lock()
	held = time.Now()
	return
}

// unlockWrite releases the shard write lock and records the wait/hold timings. The
// counter updates run after Unlock so they never extend the critical section.
func (sh *shard) unlockWrite(acq, held time.Time) {
	hold := time.Since(held)
	sh.mu.Unlock()
	sh.metrics.writeWait.observe(held.Sub(acq))
	sh.metrics.writeHold.observe(hold)
}

// supRef identifies a supersededBySeq field (a record's tombstone) that must be
// flushed to disk for a persistent shard.
type supRef struct {
	seg *segment
	off uint32
}

func newShard(segSize int, onSync func()) *shard {
	sh := &shard{
		dir:     make(map[uint64]loc),
		segSize: segSize,
		onSync:  onSync,
	}
	sh.scond = sync.NewCond(&sh.smu)
	return sh
}

// allocSeg creates a new segment via the persistent factory if configured, else a
// RAM segment. On a persistent-allocation error it records the sticky writeErr and
// returns nil; the caller must treat the write as failed. Caller holds the write
// lock.
func (sh *shard) allocSeg(id uint32, size int, codec Codec) *segment {
	start := time.Now()
	defer func() { sh.metrics.segAlloc.observe(time.Since(start)) }()
	if sh.alloc == nil {
		s := newSegment(id, size, codec)
		s.pinReap = sh.sealRAM // pin/reap-eligible so its anon sidecar tears down safely
		return s
	}
	seg, err := sh.alloc(id, size, codec)
	if err != nil {
		if sh.writeErr == nil {
			sh.writeErr = err
		}
		return nil
	}
	return seg
}

func (sh *shard) dirGet(h uint64) loc {
	if l, ok := sh.dir[h]; ok {
		return l
	}
	return noLoc
}

// findCurrent walks the bucket chain from head and returns the location of the
// current (non-superseded) record whose key matches, if any. Collisions and
// superseded versions are skipped by comparing the inline key and the atomic
// supersededBySeq field. Caller holds at least the read lock.
func (sh *shard) findCurrent(head loc, key []byte) (loc, bool) {
	for l := head; l.valid(); {
		seg := sh.segs[l.seg]
		if recSuperseded(seg.data, l.off) == seqMax && bytes.Equal(recKey(seg.data, l.off), key) {
			return l, true
		}
		l = recNext(seg.data, l.off)
	}
	return noLoc, false
}

// put inserts or updates key with the given ad bytes (compressed with codec) at
// commit sequence seq. A prior current version of the key (if any) is marked
// superseded at seq; the new record is prepended as the bucket head. Caller holds
// the write lock.
func (sh *shard) put(h uint64, key, ad []byte, seq uint64, codec Codec) {
	head := sh.dirGet(h)
	// Write the new record first: if segment allocation fails (persistent store,
	// disk full), the key is left unchanged rather than superseded-with-no-successor.
	newLoc, ok := sh.writeRecord(seq, head, key, ad, codec)
	if !ok {
		return // sh.writeErr is set; surfaced to the caller
	}
	if old, ok := sh.findCurrent(head, key); ok {
		sh.segs[old.seg].supersedeRec(old.off, seq)
	} else if old, ok := sh.lookupSealed(key, h); ok {
		// The key's current version lives in a sealed segment (evicted from the
		// directory). Supersede it there so it does not remain a second live record,
		// and flush the supersession (it lands in an already-synced region).
		seg := sh.segs[old.seg]
		seg.supersedeRec(old.off, seq)
		if sh.alloc != nil {
			sh.dirtySup = append(sh.dirtySup, supRef{seg, old.off})
		}
	} else {
		sh.count++
		// A newly-inserted chained child bumps its parent's live-child count. Re-puts
		// (updates) take the branch above and do not double-count.
		if sh.childParentHash != nil {
			if ph, isChild := sh.childParentHash(key); isChild {
				if sh.childCount == nil {
					sh.childCount = make(map[uint64]int)
				}
				sh.childCount[ph]++
			}
		}
	}
	sh.dir[h] = newLoc
}

// del marks the current version of key superseded at seq (an MVCC tombstone: no
// new record is written). It returns whether a live key was removed and, for a
// chained child, whether that removal dropped its parent's live-child count to
// zero (parentEmptied) -- the signal Delete uses to auto-delete an orphaned
// structural parent. Caller holds the write lock.
func (sh *shard) del(h uint64, key []byte, seq uint64) (removed, parentEmptied bool) {
	old, ok := sh.findCurrent(sh.dirGet(h), key)
	if !ok {
		// Not in the active directory; probe the sealed segments (evicted keys).
		old, ok = sh.lookupSealed(key, h)
		if !ok {
			return false, false
		}
	}
	seg := sh.segs[old.seg]
	seg.supersedeRec(old.off, seq)
	if sh.alloc != nil {
		sh.dirtySup = append(sh.dirtySup, supRef{seg, old.off}) // flush the tombstone
	}
	sh.count--
	if sh.childParentHash != nil {
		if ph, isChild := sh.childParentHash(key); isChild {
			if n := sh.childCount[ph] - 1; n <= 0 {
				delete(sh.childCount, ph) // keep the map bounded by parents-with-children
				parentEmptied = true
			} else {
				sh.childCount[ph] = n
			}
		}
	}
	return true, parentEmptied
}

// writeRecord appends a record to the active segment and returns its location. A
// new segment is allocated when the active one is full, over-small for the
// record, or was written with a different codec (a segment's records all share
// one codec so reads can decode by segment).
func (sh *shard) writeRecord(seq uint64, next loc, key, ad []byte, codec Codec) (loc, bool) {
	rl := recordLen(len(key), len(ad))
	if sh.act == nil || sh.act.codec != codec || sh.act.used+rl > len(sh.act.data) {
		size := sh.segSize
		if rl > size {
			size = rl
		}
		seg := sh.allocSeg(uint32(len(sh.segs)), size, codec)
		if seg == nil {
			return noLoc, false // allocation failed (sticky writeErr set)
		}
		sh.segs = append(sh.segs, seg)
		sh.act = seg
	}
	off, _ := sh.act.append(seq, next, key, ad)
	if sh.alloc != nil && (len(sh.dirty) == 0 || sh.dirty[len(sh.dirty)-1] != sh.act) {
		sh.dirty = append(sh.dirty, sh.act) // track for msync (persistent)
	}
	return loc{seg: sh.act.id, off: off}, true
}

// maybeCheckpoint records a time->seq checkpoint (see timeseq.go) if time travel is
// enabled and the checkpoint interval has elapsed. Caller holds the write lock and
// calls this after the batch's records are written and sh.commitSeq is bumped, so the
// marker lands in the active segment and rides the batch's msync.
func (sh *shard) maybeCheckpoint(seq uint64) {
	if sh.tt == nil {
		return
	}
	cfg := sh.tt.Load()
	if cfg == nil {
		return // time travel disabled
	}
	nowMs := nowMillis()
	if !sh.tseq.due(nowMs, cfg.interval) {
		return
	}
	if sh.writeMarker(seq, nowMs) {
		sh.tseq.record(seq, nowMs)
	}
}

// writeMarker appends a time-checkpoint marker to the active segment if there is room,
// tracking it for msync like a record. It returns false (checkpoint skipped, retried
// next commit) when the active segment is absent or full -- rather than allocating a
// whole segment just for a 48-byte marker. Caller holds the write lock.
func (sh *shard) writeMarker(seq, millis uint64) bool {
	if sh.act == nil {
		return false
	}
	if _, ok := sh.act.appendMarker(seq, millis); !ok {
		return false
	}
	if sh.alloc != nil && (len(sh.dirty) == 0 || sh.dirty[len(sh.dirty)-1] != sh.act) {
		sh.dirty = append(sh.dirty, sh.act)
	}
	return true
}

// get returns a private copy of the current ad bytes for key and the codec they
// were compressed with, or (nil, nil, false).
func (sh *shard) get(h uint64, key []byte) ([]byte, Codec, bool) {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	l, ok := sh.findCurrent(sh.dirGet(h), key)
	if !ok {
		if l, ok = sh.lookupSealed(key, h); !ok {
			return nil, nil, false
		}
	}
	seg := sh.segs[l.seg]
	ad := recAd(seg.data, l.off)
	out := make([]byte, len(ad))
	copy(out, ad)
	return out, seg.codec, true
}

// forEachSealedRecord calls fn for every record in this shard's SEALED, indexed
// segments whose key-hash is h and whose inline key equals key (Bloom-gated). It
// stops early if fn returns false. This is the shared access path the by-key MVCC
// operations use for keys that have been evicted from the directory: a key's versions
// live one per segment, so dir-chain walking plus this cover every version (some
// possibly twice, which is harmless -- every consumer here is find-first or an
// idempotent predicate). Caller holds at least the shard read lock.
func (sh *shard) forEachSealedRecord(key []byte, h uint64, fn func(seg *segment, off uint32) bool) {
	for _, seg := range sh.segs {
		if seg == nil || seg == sh.act {
			continue
		}
		bf := seg.keyBloom.Load()
		ki := seg.keyIdx.Load()
		if bf == nil || ki == nil || !bf.mayContain(h) {
			continue
		}
		for _, off := range ki.lookup(h) {
			if bytes.Equal(recKey(seg.data, off), key) {
				if !fn(seg, off) {
					return
				}
			}
		}
	}
}

// lookupSealed returns the location of key's current (non-superseded) record among
// this shard's sealed segments, or (noLoc, false). Currency is a local property
// (supersededBySeq == seqMax), so no cross-segment version ordering is needed.
func (sh *shard) lookupSealed(key []byte, h uint64) (loc, bool) {
	var found loc
	ok := false
	sh.forEachSealedRecord(key, h, func(seg *segment, off uint32) bool {
		if recSuperseded(seg.data, off) == seqMax {
			found, ok = loc{seg: seg.id, off: off}, true
			return false
		}
		return true
	})
	return found, ok
}

// lookupSealedAt returns the location of key's record live at snapshot s0
// (seq <= s0 < supersededBySeq) among this shard's sealed segments, or (noLoc, false).
func (sh *shard) lookupSealedAt(key []byte, h uint64, s0 uint64) (loc, bool) {
	var found loc
	ok := false
	sh.forEachSealedRecord(key, h, func(seg *segment, off uint32) bool {
		if recSeq(seg.data, off) <= s0 && recSuperseded(seg.data, off) > s0 {
			found, ok = loc{seg: seg.id, off: off}, true
			return false
		}
		return true
	})
	return found, ok
}

// spliceDeadFromChain removes every record that lives in a reclaimed (dead) segment from
// hash bucket h's recNext chain, so no surviving link dangles into the reaped mapping. The
// bucket head is a current record and a fully-dead segment holds none, so the head is
// normally not dead; the leading loop still advances (or clears) it defensively, since after
// the reap the directory must not reference a removed segment. Caller holds the write lock.
func (sh *shard) spliceDeadFromChain(h uint64, deadSet map[uint32]struct{}) {
	isDead := func(l loc) bool { _, d := deadSet[l.seg]; return l.valid() && d }
	head := sh.dir[h]
	for isDead(head) {
		head = recNext(sh.segs[head.seg].data, head.off)
	}
	if head != sh.dir[h] {
		if head.valid() {
			sh.dir[h] = head
		} else {
			delete(sh.dir, h)
		}
	}
	for l := head; l.valid(); {
		seg := sh.segs[l.seg]
		nxt := recNext(seg.data, l.off)
		orig := nxt
		for isDead(nxt) {
			nxt = recNext(sh.segs[nxt.seg].data, nxt.off)
		}
		if nxt != orig {
			setRecNext(seg.data, l.off, nxt)
		}
		l = nxt
	}
}

// dropDirtySegs removes the given segments from the pending-sync lists: their bytes are
// being unlinked, so a later sync must not msync one. Mirrors compaction's list pruning.
// Caller holds the write lock.
func (sh *shard) dropDirtySegs(deadSet map[uint32]struct{}) {
	if len(sh.dirty) > 0 {
		kept := sh.dirty[:0]
		for _, s := range sh.dirty {
			if _, d := deadSet[s.id]; !d {
				kept = append(kept, s)
			}
		}
		sh.dirty = kept
	}
	if len(sh.dirtySup) > 0 {
		kept := sh.dirtySup[:0]
		for _, s := range sh.dirtySup {
			if _, d := deadSet[s.seg.id]; !d {
				kept = append(kept, s)
			}
		}
		sh.dirtySup = kept
	}
}

// evictSegKeys removes from the directory the entries whose current record lives in
// seg (a just-indexed sealed segment): those keys are now reachable through the
// sealed probe, so dropping them from the directory is the RAM win. Caller holds the
// write lock. Safe only once seg carries a key index (the probe can find the keys).
func (sh *shard) evictSegKeys(seg *segment) {
	ki := seg.keyIdx.Load()
	if ki == nil {
		return
	}
	for i := uint32(0); i < ki.count; i++ {
		h := ki.hashAt(i)
		if l, ok := sh.dir[h]; ok && l.seg == seg.id {
			delete(sh.dir, h)
		}
	}
}

// segWindow is a frozen view of one segment at scan start: its immutable backing,
// the write watermark captured under the read lock, and the codec its records
// were compressed with.
type segWindow struct {
	data  []byte
	used  int
	codec Codec
	seg   *segment // for reindex-on-demand and the per-segment index (idx)
}

// snapshot captures the shard's scan state under the read lock: the commit
// sequence S0 and a frozen window over each live segment. The windows hold the
// segments' immutable backing arrays directly. A RAM segment retired by a later
// compaction stays alive (via the garbage collector) for as long as this scan
// references it; an mmap segment is pinned here (see segment.pin) so a compaction
// that retires it defers the munmap+unlink until releaseWindows drops the pin.
// Retired segment slots are nil and skipped. The caller MUST releaseWindows(wins)
// when the scan is done.
func (sh *shard) snapshot() (s0 uint64, wins []segWindow) {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	s0 = sh.commitSeq
	return s0, sh.buildWindowsLocked(s0)
}

// snapshotAt is snapshot for a historical snapshot sequence s0 (point-in-time / AS OF
// queries): it freezes the same window set but for an arbitrary caller-supplied s0
// instead of the current commit sequence. The caller MUST releaseWindows(wins).
func (sh *shard) snapshotAt(s0 uint64) []segWindow {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.buildWindowsLocked(s0)
}

// buildWindowsLocked pins and returns a frozen window over each segment that holds at
// least one record visible at s0, skipping segments with nothing visible (all records
// born after s0, or all superseded by s0 -- e.g. history-only segments a current-time
// scan never needs). This keeps a scan's cost proportional to the versions actually
// visible at s0, so retained time-travel history does not slow current-time scans.
// Caller holds the read lock.
func (sh *shard) buildWindowsLocked(s0 uint64) []segWindow {
	wins := make([]segWindow, 0, len(sh.segs))
	for _, seg := range sh.segs {
		if seg == nil || seg.used == 0 || !seg.visibleAt(s0) {
			continue
		}
		seg.pin() // no-op for RAM segments
		wins = append(wins, segWindow{data: seg.data, used: seg.used, codec: seg.codec, seg: seg})
	}
	return wins
}

// releaseWindows drops the pins snapshot took, allowing any mmap segment retired
// during the scan to be reaped. Balances snapshot; call it exactly once per
// snapshot (typically deferred). No-op for RAM segments.
func releaseWindows(wins []segWindow) {
	for i := range wins {
		wins[i].seg.unpin()
	}
}

// forEachVisible walks the frozen windows and calls fn with the ad bytes (and the
// codec they were compressed with) of each record visible at S0
// (seq <= S0 < supersededBySeq) — exactly one version per key that existed at S0.
// fn returns false to stop early.
func forEachVisible(s0 uint64, wins []segWindow, fn func(ad []byte, codec Codec) bool) {
	for _, w := range wins {
		for off := 0; off < w.used; {
			o := uint32(off)
			total := recTotalLen(w.data, o)
			if total == 0 {
				break // malformed guard; should not happen
			}
			if recIsMarker(w.data, o) {
				off += int(total) // time-checkpoint marker, not a data record
				continue
			}
			seq := recSeq(w.data, o)
			sup := recSuperseded(w.data, o)
			if seq <= s0 && sup > s0 {
				if !fn(recAd(w.data, o), w.codec) {
					return
				}
			}
			off += int(total)
		}
	}
}

// forEachVisibleKeyed is forEachVisible that also passes each record's key (a
// view into the frozen window; the callback must not retain it). Used by the
// chained scan, which needs a record's key to find its parent.
func forEachVisibleKeyed(s0 uint64, wins []segWindow, fn func(key, ad []byte, codec Codec) bool) {
	for _, w := range wins {
		for off := 0; off < w.used; {
			o := uint32(off)
			total := recTotalLen(w.data, o)
			if total == 0 {
				break
			}
			if recIsMarker(w.data, o) {
				off += int(total) // time-checkpoint marker, not a data record
				continue
			}
			seq := recSeq(w.data, o)
			sup := recSuperseded(w.data, o)
			if seq <= s0 && sup > s0 {
				if !fn(recKey(w.data, o), recAd(w.data, o), w.codec) {
					return
				}
			}
			off += int(total)
		}
	}
}
