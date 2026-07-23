package collections

import "time"

// Group commit. Concurrent writers to a shard are coalesced so that a batch of
// writes is applied under a single commitSeq bump and a single durability sync.
// Today the store is in-memory and the sync is a no-op, so coalescing is roughly
// a wash; it exists so that a future durable-serialization feature (fsync/write
// of the batch) is amortized across all writers that commit together, instead of
// paying the sync once per writer.
//
// The protocol is leader/handoff:
//   - A writer with no commit in progress becomes the flusher and applies its
//     writes directly (the uncontended fast path — no queue, no channels).
//   - Writers arriving while a flush is in progress enqueue and block.
//   - When a flusher finishes, if others are queued it drains and applies them all
//     as one batch (coalescing = everyone who arrived during the previous sync),
//     then hands the flusher role to one of them. Each flusher applies at most one
//     batch, so no writer is trapped committing for others indefinitely.

// pendingPut is one encoded, compressed write awaiting commit.
type pendingPut struct {
	hash  uint64
	key   []byte
	ad    []byte
	codec Codec
}

// commitReq is a queued writer's request (one Update's writes for one shard).
type commitReq struct {
	writes []pendingPut
	done   chan struct{} // closed when these writes are committed
	lead   chan struct{} // closed to appoint this request's waiter as the flusher
}

// commit applies writes to the shard, returning only once they are committed and
// (in the future) durably synced. Writes are coalesced with any concurrent
// writers' commits.
func (sh *shard) commit(writes []pendingPut) {
	sh.cmu.Lock()
	if !sh.flushing {
		// Fast path: no commit in progress. Become the flusher and apply directly.
		sh.flushing = true
		sh.cmu.Unlock()
		sh.applyWrites(writes)
		sh.finishFlush()
		return
	}
	// Slow path: a flush is in progress. Enqueue and wait to be committed or
	// appointed as the next flusher.
	req := &commitReq{writes: writes, done: make(chan struct{}), lead: make(chan struct{})}
	sh.queue = append(sh.queue, req)
	sh.cmu.Unlock()
	select {
	case <-req.done:
		return
	case <-req.lead:
		sh.flushAsLeader(req)
	}
}

// commitOne is commit for a single write, avoiding the pendingPut slice
// allocation on the uncontended fast path (the common one-ad Put).
func (sh *shard) commitOne(p pendingPut) {
	sh.cmu.Lock()
	if !sh.flushing {
		sh.flushing = true
		sh.cmu.Unlock()
		sh.applyOne(p)
		sh.finishFlush()
		return
	}
	req := &commitReq{writes: []pendingPut{p}, done: make(chan struct{}), lead: make(chan struct{})}
	sh.queue = append(sh.queue, req)
	sh.cmu.Unlock()
	select {
	case <-req.done:
		return
	case <-req.lead:
		sh.flushAsLeader(req)
	}
}

// flushAsLeader drains and commits the whole queue as one batch (coalescing),
// signals the other waiters, and hands off. self is this flusher's own request,
// which is committed but not signaled (the caller returns normally).
func (sh *shard) flushAsLeader(self *commitReq) {
	sh.cmu.Lock()
	batch := sh.queue
	sh.queue = nil
	sh.cmu.Unlock()

	sh.applyBatch(batch)

	for _, r := range batch {
		if r != self {
			close(r.done)
		}
	}
	sh.finishFlush()
}

// finishFlush clears the flusher role if the queue is empty, or appoints the
// first queued waiter as the next flusher. Caller must hold no locks.
func (sh *shard) finishFlush() {
	sh.cmu.Lock()
	if len(sh.queue) == 0 {
		sh.flushing = false
		sh.cmu.Unlock()
		return
	}
	next := sh.queue[0]
	sh.cmu.Unlock()
	close(next.lead)
}

// applyWrites commits a single writer's writes under the shard lock at a fresh
// commit sequence, then runs the durability sync once.
func (sh *shard) applyWrites(writes []pendingPut) {
	acq, held := sh.lockWrite()
	seq := sh.commitSeq + 1
	for i := range writes {
		sh.put(writes[i].hash, writes[i].key, writes[i].ad, seq, writes[i].codec)
	}
	sh.commitSeq = seq
	sh.maybeCheckpoint(seq)
	sh.unlockWrite(acq, held)
	sh.syncFor(seq)
	if sh.hub != nil {
		for i := range writes {
			sh.hub.publish(sh.idx, seq, writes[i].key, writes[i].ad, writes[i].codec, false)
		}
	}
}

// applyOne commits a single write under the shard lock at a fresh commit
// sequence, then runs the durability sync once.
func (sh *shard) applyOne(p pendingPut) {
	acq, held := sh.lockWrite()
	seq := sh.commitSeq + 1
	sh.put(p.hash, p.key, p.ad, seq, p.codec)
	sh.commitSeq = seq
	sh.maybeCheckpoint(seq)
	sh.unlockWrite(acq, held)
	sh.syncFor(seq)
	if sh.hub != nil {
		sh.hub.publish(sh.idx, seq, p.key, p.ad, p.codec, false)
	}
}

// applyBatch commits a coalesced batch of requests under the shard lock at a
// single fresh commit sequence, then runs the durability sync once for the batch.
func (sh *shard) applyBatch(batch []*commitReq) {
	acq, held := sh.lockWrite()
	seq := sh.commitSeq + 1
	for _, r := range batch {
		for i := range r.writes {
			sh.put(r.writes[i].hash, r.writes[i].key, r.writes[i].ad, seq, r.writes[i].codec)
		}
	}
	sh.commitSeq = seq
	sh.maybeCheckpoint(seq)
	sh.unlockWrite(acq, held)
	sh.syncFor(seq)
	if sh.hub != nil {
		for _, r := range batch {
			for i := range r.writes {
				sh.hub.publish(sh.idx, seq, r.writes[i].key, r.writes[i].ad, r.writes[i].codec, false)
			}
		}
	}
}

// sync is the durability point for callers that do not know their commit sequence:
// it makes everything applied so far durable. Callers that do know (Txn.Commit,
// applyBatch) use syncFor directly for precise coalescing.
func (sh *shard) sync() {
	sh.mu.RLock()
	need := sh.commitSeq
	sh.mu.RUnlock()
	sh.syncFor(need)
}

// syncFor makes every write with commit sequence <= need durable before returning.
// It group-commits: concurrent committers to one shard share msync passes instead of
// each running (or worse, skipping) one. A caller whose need is already covered by a
// COMPLETED pass returns immediately; if a pass is in flight it waits -- the pass may
// cover it (its dirty capture may postdate this caller's apply), and if not the loop
// elects a waiter to lead the next pass, which then covers every waiter at once. N
// concurrent commits thus pay ~2 msync passes.
//
// This also closes a durability hole in the old code, which swapped the dirty list and
// msync'd with no coordination: a racing sync() could observe an empty dirty list and
// return -- acking its commit durable -- while the pass covering its pages was still in
// flight. Here nobody returns before a pass whose capture postdates their writes has
// COMPLETED.
func (sh *shard) syncFor(need uint64) {
	if sh.onSync != nil {
		sh.onSync()
	}
	if sh.alloc == nil {
		return // in-memory shard
	}
	sh.smu.Lock()
	for sh.syncedSeq < need {
		if sh.syncing {
			sh.scond.Wait() // an in-flight pass may cover us; re-check when it lands
			continue
		}
		sh.syncing = true
		sh.smu.Unlock()
		covered := sh.syncPass()
		sh.smu.Lock()
		if covered > sh.syncedSeq {
			sh.syncedSeq = covered
		}
		sh.syncing = false
		sh.scond.Broadcast()
	}
	sh.smu.Unlock()
}

// syncPass runs one durability pass: msync the pages written since the last pass.
// It returns the commit sequence the pass covers -- captured under the same lock
// acquisition as the dirty list, so every write applied at or before that sequence
// is either in the captured ranges or was covered by an earlier pass.
func (sh *shard) syncPass() uint64 {
	sh.mu.Lock()
	covered := sh.commitSeq
	dirty := sh.dirty
	sh.dirty = nil
	sup := sh.dirtySup
	sh.dirtySup = nil
	type flushRange struct {
		seg      *segment
		from, to int
	}
	var ranges []flushRange
	for _, seg := range dirty {
		if seg.used > seg.synced {
			ranges = append(ranges, flushRange{seg, seg.synced, seg.used})
			seg.synced = seg.used
		}
	}
	// Pin the segments we are about to msync lock-free (still under the lock, so it is
	// atomic with capturing them). A concurrent compaction that retires one of these
	// segments then sees a non-zero pin and defers its reap (munmap+unlink) to our unpin,
	// so msyncRange never reads a mapping torn down under it. compactShard also drops
	// retired segments from sh.dirty/dirtySup under the lock, so a *later* sync cannot pick
	// up a segment already being reaped.
	for i := range ranges {
		ranges[i].seg.pin()
	}
	for _, s := range sup {
		s.seg.pin()
	}
	sh.mu.Unlock()
	// Time the actual durability flush -- the msync syscalls -- so an operator can see
	// whether stalls are "just an fsync thing". The unpin bookkeeping below is cheap and
	// excluded.
	syncStart := time.Now()
	for _, r := range ranges {
		_ = r.seg.msyncRange(r.from, r.to)
	}
	// Flush delete tombstones: the supersededBySeq field a delete wrote may sit in an
	// already-synced region (so it is not covered by the append ranges above); msync
	// its page so the delete survives a crash. Overwrites need no such flush — their
	// new record is in an append range and recovery's max-seq rule supersedes the old
	// version regardless.
	for _, s := range sup {
		off := int(s.off) + recSupOff
		_ = s.seg.msyncRange(off, off+8)
	}
	sh.metrics.sync.observe(time.Since(syncStart))
	for i := range ranges {
		ranges[i].seg.unpin()
	}
	for _, s := range sup {
		s.seg.unpin()
	}
	return covered
}
