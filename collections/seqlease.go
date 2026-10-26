package collections

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Keeping a paged read's snapshot readable.
//
// A paged read (QueryRawFromSeq) freezes each shard at the sequence it had when the shard's first
// page was taken, and later pages read the versions visible at that snapshot. Those versions are
// only kept as long as maintenance keeps them: compaction and the dead-segment reclaim drop every
// version superseded at or below the retain floor, which without time travel is the current commit
// sequence. A key rewritten after a pagination's snapshot and then compacted has lost the version
// the snapshot sees, and its new version is too new for the snapshot -- so later pages silently
// skip it.
//
// The cursor is stateless: the store does not know a pagination is in progress unless the read
// tells it. Two mechanisms close the hole between them.
//
// Leases. Taking a shard's snapshot leases it for Options.SeqCursorLease. While a lease is held the
// retain floor stays at or below the leased snapshot, so compaction keeps every version the
// snapshot can see -- the retention time travel already uses, moved down by the oldest lease
// rather than by a clock window. A collection with delta records cannot keep versions that way:
// a retained delta can outlive the whole record it merges from (the reason delta records and time
// travel are refused together), so there a lease DEFERS the shard's reclamation instead, for at
// most one lease period. A shard whose reclamation has been deferred that long has its leases
// revoked and is reclaimed, so a steady stream of paginations cannot keep a shard's garbage
// forever.
//
// Detection. Whatever reclaims versions raises the shard's gcFloor to the retain floor it used, and
// Open starts it at the recovered commit sequence. A version at or below the floor may be gone, so
// a cursor whose snapshot is below it cannot be resumed faithfully: the read refuses it with
// ErrSeqCursorExpired before yielding anything, and the caller restarts the listing. An expired
// lease therefore costs a restart, never a silently short listing.

// ErrSeqCursorExpired reports that a paged read's cursor names a snapshot older than versions the
// store has since reclaimed, so the rest of the listing can no longer be read as of that snapshot.
// It is returned in SeqPage.Err before any row of the page. Resuming the same cursor fails the same
// way: restart the listing from the zero cursor.
var ErrSeqCursorExpired = errors.New("collections: the cursor's snapshot has been reclaimed; restart the listing from the beginning")

// defaultSeqCursorLease is how long a paged read's snapshot of a shard is protected when
// Options.SeqCursorLease is zero.
const defaultSeqCursorLease = 10 * time.Minute

// seqLeases is one shard's set of leased paged-read snapshots.
type seqLeases struct {
	mu sync.Mutex
	// until maps a leased snapshot to when its protection ends.
	until map[uint64]time.Time
	// deferredSince is when reclamation of the shard was first deferred for a lease (delta
	// stores only); zero while it is not being deferred.
	deferredSince time.Time
}

// What the leases have done, for operators: reclamation passes a lease deferred, leases revoked
// because their shard had been deferred for a whole lease period, and cursors refused as expired.
var (
	seqLeaseDeferrals   atomic.Int64
	seqLeaseRevocations atomic.Int64
	seqCursorsExpired   atomic.Int64
)

// SeqCursorStats reports, process-wide, how many reclamation passes a paged read's lease deferred,
// how many leases were revoked so a deferred shard could be reclaimed, and how many cursors were
// refused with ErrSeqCursorExpired.
func SeqCursorStats() (deferrals, revocations, expired int64) {
	return seqLeaseDeferrals.Load(), seqLeaseRevocations.Load(), seqCursorsExpired.Load()
}

// leaseSnapshotLocked leases snapshot s0 of sh until now plus the lease period. Caller holds sh.mu
// (read or write), which is what orders the lease against a reclamation computing its floor: that
// runs under the write lock, so it either sees this lease or ran before s0 was taken, when its
// floor was at most s0 anyway.
func (sh *shard) leaseSnapshotLocked(s0 uint64) {
	if sh.leaseFor <= 0 {
		return
	}
	until := sh.leaseNow().Add(sh.leaseFor)
	sh.leases.mu.Lock()
	if sh.leases.until == nil {
		sh.leases.until = map[uint64]time.Time{}
	}
	if until.After(sh.leases.until[s0]) {
		sh.leases.until[s0] = until
	}
	sh.leases.mu.Unlock()
}

// leaseFloorLocked returns the oldest snapshot still under lease, dropping expired leases, and
// whether there is one. Caller holds sh.mu (read or write).
func (sh *shard) leaseFloorLocked() (uint64, bool) {
	sh.leases.mu.Lock()
	defer sh.leases.mu.Unlock()
	if len(sh.leases.until) == 0 {
		return 0, false
	}
	now := sh.leaseNow()
	var floor uint64
	held := false
	for s0, until := range sh.leases.until {
		if !now.Before(until) {
			delete(sh.leases.until, s0)
			continue
		}
		if !held || s0 < floor {
			floor, held = s0, true
		}
	}
	return floor, held
}

// reclaimDeferredLocked reports whether reclamation of sh -- the dead-segment reclaim and the
// compacting rewrite -- must wait for a lease, which is only ever the case in a collection with
// delta records (deltas). Caller holds sh.mu for writing.
//
// A shard is deferred for at most one lease period, counted from the first pass that deferred it.
// Once that has passed its leases are revoked and the pass proceeds, raising gcFloor past their
// snapshots so their cursors fail loudly rather than read what the pass removed.
func (sh *shard) reclaimDeferredLocked(deltas bool) bool {
	if !deltas || sh.leaseFor <= 0 {
		return false
	}
	floor, held := sh.leaseFloorLocked()
	sh.leases.mu.Lock()
	defer sh.leases.mu.Unlock()
	if !held || floor >= sh.commitSeq {
		// No lease predates a write, so nothing a pass could drop is visible to one.
		sh.leases.deferredSince = time.Time{}
		return false
	}
	now := sh.leaseNow()
	if sh.leases.deferredSince.IsZero() {
		sh.leases.deferredSince = now
	}
	if now.Sub(sh.leases.deferredSince) < sh.leaseFor {
		seqLeaseDeferrals.Add(1)
		return true
	}
	seqLeaseRevocations.Add(int64(len(sh.leases.until)))
	clear(sh.leases.until)
	sh.leases.deferredSince = time.Time{}
	return false
}

// seqCursorExpiredLocked reports whether a cursor's snapshot s0 of sh may have lost versions it
// sees: something reclaimed versions superseded above it. Caller holds sh.mu (read or write).
func (sh *shard) seqCursorExpiredLocked(s0 uint64) bool {
	return s0 < sh.gcFloor
}
