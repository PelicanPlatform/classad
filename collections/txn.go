package collections

import (
	"bytes"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/classad"
)

// conflictCheckCount counts per-key write-write conflict checks performed by
// transaction commits (observability; the single-writer fast path performs none).
var conflictCheckCount atomic.Int64

// ConflictChecks returns the cumulative number of per-key conflict checks committed
// transactions have performed -- zero while a single writer runs (the fast path).
func ConflictChecks() int64 { return conflictCheckCount.Load() }

// Multi-writer optimistic concurrency control (see docs/MVCC_TRANSACTIONS.md).
//
// A Txn runs against a snapshot and buffers its writes; Commit applies each write
// only if its key was not modified by another committer since the snapshot (a
// write-write conflict under snapshot isolation). Reads are not tracked -- table
// scans and constraint queries impose no bookkeeping. Because HTCondor
// transactions treat each ad independently, writes commit per ad: unaffected keys
// succeed even if others conflict, and the caller retries just the conflicts.
//
// Put/Delete/Update on the Collection remain the unconditional (last-write-wins)
// API; Txn is the opt-in OCC path.

// findVisible returns the record for key that was live at snapshot s0 (seq <= s0 <
// supersededBySeq), walking the bucket chain. Caller holds at least the read lock.
func (sh *shard) findVisible(head loc, key []byte, s0 uint64) (loc, bool) {
	for l := head; l.valid(); {
		seg := sh.segs[l.seg]
		if bytes.Equal(recKey(seg.data, l.off), key) &&
			recSeq(seg.data, l.off) <= s0 && recSuperseded(seg.data, l.off) > s0 {
			return l, true
		}
		l = recNext(seg.data, l.off)
	}
	return noLoc, false
}

// getAt returns a private copy of key's ad bytes as of snapshot s0, or (nil, nil,
// false) if the key had no version live at s0.
func (sh *shard) getAt(h uint64, key []byte, s0 uint64) ([]byte, Codec, bool) {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	l, ok := sh.findVisible(sh.dirGet(h), key, s0)
	if !ok {
		if l, ok = sh.lookupSealedAt(key, h, s0); !ok {
			return nil, nil, false
		}
	}
	seg := sh.segs[l.seg]
	ad := recAd(seg.data, l.off)
	out := make([]byte, len(ad))
	copy(out, ad)
	return out, seg.codec, true
}

// conflictSince reports whether key was modified after snapshot s0 -- the write-
// write conflict test. It walks the bucket chain (superseded versions are retained
// until compaction) and reports a conflict if any record for the key was written
// after s0 (recSeq > s0: an update or insert) or the s0-era version was superseded
// after s0 (a later update or delete; delete leaves no new record, so the
// supersede clause is what catches it). Caller holds at least the read lock.
func (sh *shard) conflictSince(h uint64, key []byte, s0 uint64) bool {
	hasLive, conflict := false, false
	// check applies the conflict test to one record; returns false to stop the scan.
	check := func(seg *segment, off uint32) bool {
		if recSeq(seg.data, off) > s0 {
			conflict = true
			return false
		}
		if sup := recSuperseded(seg.data, off); sup != seqMax && sup > s0 {
			conflict = true
			return false
		}
		if recSuperseded(seg.data, off) == seqMax {
			hasLive = true
		}
		return true
	}
	for l := sh.dirGet(h); l.valid(); {
		seg := sh.segs[l.seg]
		if bytes.Equal(recKey(seg.data, l.off), key) && !check(seg, l.off) {
			return true
		}
		l = recNext(seg.data, l.off)
	}
	// Also scan versions evicted from the directory into the sealed segments. A key's
	// versions live one per segment, so the chain walk above plus this cover them all
	// (an overlap is harmless -- check is an idempotent predicate).
	sh.forEachSealedRecord(key, h, check)
	if conflict {
		return true
	}
	// A currently-absent key whose snapshot predates the last compaction: its delete
	// evidence may have been reclaimed, so we cannot prove it was not deleted after s0.
	// Conservatively conflict (the caller retries with a fresh snapshot). A key with a
	// live record is always decided exactly above, compaction notwithstanding.
	if !hasLive && s0 < sh.gcFloor {
		return true
	}
	return false
}

// txnWrite is one buffered write ready to apply, with its snapshot base for the
// conflict check. ok is set by commitTxn.
type txnWrite struct {
	hash  uint64
	key   []byte
	ad    []byte // compressed bytes (nil for a delete)
	codec Codec
	del   bool
	base  uint64 // snapshot S0: conflict if the key changed after this
	adObj *classad.ClassAd
	ok    bool // committed (true) or conflicted (false)
}

// commitTxn applies a shard's buffered transactional writes with per-write conflict
// detection, all under one shard write lock so the check and apply are atomic with
// respect to other committers (first-committer-wins). Conflicting writes are skipped
// and flagged; the rest commit at one fresh sequence.
func (sh *shard) commitTxn(ws []*txnWrite, durable bool) {
	acq, held := sh.lockWrite()
	seq := sh.commitSeq + 1
	// Single-writer fast path: all of a shard's buffered writes share one snapshot
	// (ws[0].base). If no one has committed to this shard since -- commitSeq is still
	// that snapshot -- then no key can have changed, so every write succeeds without a
	// per-key conflict check. This is the schedd's common single-writer case: zero
	// conflict-detection cost. Under contention it falls to the per-write check.
	fast := len(ws) > 0 && sh.commitSeq == ws[0].base
	changed := false
	for _, w := range ws {
		if !fast {
			conflictCheckCount.Add(1)
			if sh.conflictSince(w.hash, w.key, w.base) {
				w.ok = false
				continue
			}
		}
		w.ok = true
		if w.del {
			if removed, _ := sh.del(w.hash, w.key, seq); removed {
				changed = true
			}
			continue
		}
		sh.put(w.hash, w.key, w.ad, seq, w.codec)
		changed = true
	}
	if changed {
		sh.commitSeq = seq
	}
	sh.unlockWrite(acq, held)
	if changed {
		if durable {
			sh.sync()
		}
		if sh.hub != nil {
			for _, w := range ws {
				if !w.ok {
					continue
				}
				if w.del {
					if sh.delLog != nil {
						sh.delLog.record(w.key, seq)
						sh.hub.publish(sh.idx, seq, w.key, nil, nil, true)
					}
				} else {
					sh.hub.publish(sh.idx, seq, w.key, w.ad, w.codec, false)
				}
			}
		}
	}
}

// Txn is an optimistic, snapshot-isolation transaction over a Collection. Not safe
// for concurrent use by multiple goroutines; each goroutine uses its own Txn.
type Txn struct {
	c       *Collection
	snap    map[int]uint64     // shard index -> snapshot seq, captured lazily on first touch
	writes  map[string]*txnBuf // buffered writes by key (last write wins within the txn)
	durable bool               // Commit runs the durability sync (default true)
}

type txnBuf struct {
	key []byte
	ad  *classad.ClassAd // nil for a delete
	del bool
}

// CommitResult reports a transaction's outcome. Conflicts holds the keys whose
// write lost a write-write race and were not applied; the caller may re-read and
// retry just those. The other buffered writes committed.
type CommitResult struct {
	Committed int
	Conflicts [][]byte
}

// Conflicted reports whether any buffered write lost a conflict.
func (r CommitResult) Conflicted() bool { return len(r.Conflicts) > 0 }

// Begin starts an optimistic transaction. Its snapshot for a shard is captured the
// first time the transaction reads or writes a key in that shard.
func (c *Collection) Begin() *Txn {
	return &Txn{c: c, snap: map[int]uint64{}, writes: map[string]*txnBuf{}, durable: true}
}

// SetDurable controls whether Commit runs the durability sync (default true). A
// nondurable commit is visible immediately (readers and watchers see it) but its
// disk flush is deferred to a later durable commit or flush -- the classad_log.h
// CommitNondurableTransaction batching. No effect on an in-memory collection, whose
// sync is already a no-op.
func (tx *Txn) SetDurable(d bool) { tx.durable = d }

// snapOf returns the transaction's snapshot sequence for the shard holding a key,
// capturing it (the shard's current commit sequence) on first touch.
func (tx *Txn) snapOf(idx int) uint64 {
	if s, ok := tx.snap[idx]; ok {
		return s
	}
	sh := tx.c.shards[idx]
	sh.mu.RLock()
	s := sh.commitSeq
	sh.mu.RUnlock()
	tx.snap[idx] = s
	return s
}

// Get returns the ad for key as the transaction sees it: its own buffered write if
// any (read-your-writes), else the version live at the transaction's snapshot. On a
// chained (parent/child) collection it resolves inherited attributes by merging the
// parent as of the same snapshot -- mirroring Collection.Get, transactionally.
func (tx *Txn) Get(key []byte) (*classad.ClassAd, bool) {
	ad, ok := tx.getOwn(key)
	if !ok {
		return nil, false
	}
	if tx.c.parentKeyFor != nil {
		if pk := tx.c.parentKeyFor(key); pk != nil {
			if parent, ok := tx.getOwn(pk); ok {
				tx.c.mergeParent(ad, parent)
			}
		}
	}
	return ad, true
}

// getOwn reads one key as the transaction sees it (its buffered write, else the
// snapshot version), without parent chaining. Returns a fresh ad the caller may
// mutate (buffered writes are returned as-is -- the caller owns the buffered ad).
func (tx *Txn) getOwn(key []byte) (*classad.ClassAd, bool) {
	if b, ok := tx.writes[string(key)]; ok {
		if b.del {
			return nil, false
		}
		return b.ad, true
	}
	h := tx.c.h.Hash(key)
	idx := tx.c.shardOf(key, h)
	s0 := tx.snapOf(idx)
	stored, codec, ok := tx.c.shards[idx].getAt(h, key, s0)
	if !ok {
		return nil, false
	}
	ad, err := tx.c.decodeAd(stored, codec)
	if err != nil {
		return nil, false
	}
	return ad, true
}

// Put buffers an insert or update of key. Nothing is written until Commit.
func (tx *Txn) Put(key []byte, ad *classad.ClassAd) {
	tx.snapOf(tx.c.shardOf(key, tx.c.h.Hash(key)))
	tx.writes[string(key)] = &txnBuf{key: append([]byte(nil), key...), ad: ad}
}

// Delete buffers a delete of key. Nothing is written until Commit.
func (tx *Txn) Delete(key []byte) {
	tx.snapOf(tx.c.shardOf(key, tx.c.h.Hash(key)))
	tx.writes[string(key)] = &txnBuf{key: append([]byte(nil), key...), del: true}
}

// Commit applies the buffered writes, each independently: a write whose key is
// unchanged since the transaction's snapshot commits; one whose key was modified by
// another committer is reported in CommitResult.Conflicts and not applied (the
// successful writes are not rolled back). The transaction must not be used after
// Commit.
func (tx *Txn) Commit() CommitResult {
	byShard := make(map[int][]*txnWrite)
	for _, b := range tx.writes {
		h := tx.c.h.Hash(b.key)
		idx := tx.c.shardOf(b.key, h)
		w := &txnWrite{hash: h, key: b.key, del: b.del, base: tx.snap[idx], adObj: b.ad}
		if !b.del {
			w.codec = tx.c.currentCodec()
			w.ad = w.codec.Compress(nil, tx.c.encodeAd(b.ad.AST()))
		}
		byShard[idx] = append(byShard[idx], w)
	}
	var res CommitResult
	for idx, ws := range byShard {
		tx.c.shards[idx].commitTxn(ws, tx.durable)
		for _, w := range ws {
			if !w.ok {
				res.Conflicts = append(res.Conflicts, w.key)
				continue
			}
			res.Committed++
			if w.del {
				tx.c.removeOrdered(w.key)
			} else {
				tx.c.maintainOrdered(w.key, w.adObj)
			}
		}
	}
	return res
}
