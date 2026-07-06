package collections

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
	sh.mu.Lock()
	seq := sh.commitSeq + 1
	for i := range writes {
		sh.put(writes[i].hash, writes[i].key, writes[i].ad, seq, writes[i].codec)
	}
	sh.commitSeq = seq
	sh.mu.Unlock()
	sh.sync()
}

// applyOne commits a single write under the shard lock at a fresh commit
// sequence, then runs the durability sync once.
func (sh *shard) applyOne(p pendingPut) {
	sh.mu.Lock()
	seq := sh.commitSeq + 1
	sh.put(p.hash, p.key, p.ad, seq, p.codec)
	sh.commitSeq = seq
	sh.mu.Unlock()
	sh.sync()
}

// applyBatch commits a coalesced batch of requests under the shard lock at a
// single fresh commit sequence, then runs the durability sync once for the batch.
func (sh *shard) applyBatch(batch []*commitReq) {
	sh.mu.Lock()
	seq := sh.commitSeq + 1
	for _, r := range batch {
		for i := range r.writes {
			sh.put(r.writes[i].hash, r.writes[i].key, r.writes[i].ad, seq, r.writes[i].codec)
		}
	}
	sh.commitSeq = seq
	sh.mu.Unlock()
	sh.sync()
}

// sync is the durability point: where a future durable collection would fsync or
// serialize the just-committed batch. It runs once per (possibly coalesced)
// batch. Today it is a no-op unless a CommitSync hook is configured.
func (sh *shard) sync() {
	if sh.onSync != nil {
		sh.onSync()
	}
	if sh.alloc == nil {
		return // in-memory shard
	}
	// Persistent shard: msync the pages written since the last sync so the commit
	// is durable before this returns. Capture the ranges and advance the durable
	// mark under the lock (dirty is guarded by mu), then msync lock-free — only the
	// current flusher runs sync() per shard, so no concurrent sync races here.
	sh.mu.Lock()
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
	sh.mu.Unlock()
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
}
