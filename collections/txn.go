package collections

import (
	"bytes"
	"sync"
	"sync/atomic"
	"time"

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

// corruptChainLinks counts bucket-chain links that named a segment the shard does not have. Always zero on a
// healthy store; see findVisible for why a nonzero value is a lifetime bug and not a data bug.
var corruptChainLinks atomic.Int64

// CorruptChainLinks reports how many times a bucket-chain walk found a link naming a segment that does not
// exist, across every collection in this process.
//
// It is exported so an operator can see it without a debugger. Nonzero means some reader walked a segment
// whose mapping it did not hold alive, and the walk read whatever replaced it; the records themselves are
// not corrupt on disk.
func CorruptChainLinks() int64 { return corruptChainLinks.Load() }

// findVisible returns the record for key that was live at snapshot s0 (seq <= s0 <
// supersededBySeq), walking the bucket chain. Caller holds at least the read lock.
func (sh *shard) findVisible(head loc, key []byte, s0 uint64) (loc, bool) {
	for l := head; l.valid(); {
		// BOUNDS-CHECKED because a chain link can be garbage rather than merely stale, and the difference
		// matters: this crashed production with
		//
		//	panic: runtime error: index out of range [1765] with length 2
		//
		// A shard never held 1766 segments, so that loc was not a once-valid index into a slice that shrank --
		// it was decoded by recNext out of bytes that are no longer a record header. A mapping whose address
		// space was reused parses as whatever now lives there.
		//
		// Treating it as end-of-chain rather than panicking is deliberately NOT presented as a fix. The caller
		// falls through to the sealed index, so a lookup degrades to "not found here" instead of taking the
		// daemon down, and corruptChainLinks counts it so the anomaly is visible rather than silent. A count
		// that climbs says a reader is walking a segment whose lifetime it does not hold -- getAt takes the
		// shard read lock but no PIN, which is the next thing to look at if this fires.
		seg := sh.segAt(l.seg)
		if seg == nil {
			return noLoc, false
		}
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
func (sh *shard) getAt(h uint64, key []byte, s0 uint64) ([]byte, Codec, *segDictHandle, bool) {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	l, ok := sh.findVisible(sh.dirGet(h), key, s0)
	if !ok {
		if l, ok = sh.lookupSealedAt(key, h, s0); !ok {
			return nil, nil, nil, false
		}
	}
	seg := sh.segAt(l.seg)
	if seg == nil {
		return nil, nil, nil, false
	}
	ad := recAd(seg.data, l.off)
	out := make([]byte, len(ad))
	copy(out, ad)
	// The record bytes are COPIED above and seg.codec's dictionary is Go-heap (TrainDict /
	// os.ReadFile), so neither outlives this lock. The dict HANDLE does not have that property: it
	// holds `data: seg.data`, the segment arena, which for a persistent segment is an mmap that
	// compaction unmaps -- and it unmaps AFTER dropping the shard write lock, while this reader holds
	// no pin. So the caller decoding with the handle after this function returns could resolve
	// attribute names out of unmapped memory: a SIGSEGV, or garbage that parses, which is what the two
	// production crashes looked like.
	//
	// Rather than hand the caller a pin to release, make the handle stop depending on the mapping:
	// building the id->name cache copies every name to the Go heap, and resolve -- the only thing a
	// decode calls -- reads only that cache once it exists. Built here, while the read lock still
	// guarantees the segment is alive. It costs one atomic load per Get after the first touch of a
	// segment, and it is exactly the cache the decode path would have built anyway.
	dict := seg.dict.Load()
	if dict != nil {
		dict.ensureNames()
	}
	return out, seg.codec, dict, true
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
		seg := sh.segAt(l.seg) // same guard as findVisible: this walk follows the same links
		if seg == nil {
			break
		}
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
	// buf is the originating buffered write, so ordered-index maintenance can materialize
	// a wire-ingested ad on demand -- a collection with no ordered index never does.
	buf *txnBuf
	ok  bool // committed (true) or conflicted (false)
}

// commitTxn applies a shard's buffered transactional writes with per-write conflict
// detection, all under one shard write lock so the check and apply are atomic with
// respect to other committers (first-committer-wins). Conflicting writes are skipped
// and flagged; the rest commit at one fresh sequence.
func (sh *shard) commitTxn(ws []*txnWrite, durable bool) {
	changed, seq := sh.applyTxn(ws)
	if !changed {
		return
	}
	if durable {
		sh.sync()
	}
	sh.publishTxn(ws, seq)
}

// applyTxn applies a shard's buffered writes under the write lock, advancing the shard's
// commit sequence, and sets each write's ok flag. It returns whether anything changed and
// the sequence used. The durability sync (sh.sync) and the watch-hub publish (publishTxn)
// are DELIBERATELY left to the caller so a multi-shard commit can sync its shards
// concurrently -- the msync is a commit's slow part, and distinct shards sync independently
// (disjoint locks and segments), so Txn.Commit overlaps them instead of paying them in
// series. See Txn.Commit.
func (sh *shard) applyTxn(ws []*txnWrite) (changed bool, seq uint64) {
	acq, held := sh.lockWrite()
	seq = sh.commitSeq + 1
	// Single-writer fast path: all of a shard's buffered writes share one snapshot
	// (ws[0].base). If no one has committed to this shard since -- commitSeq is still
	// that snapshot -- then no key can have changed, so every write succeeds without a
	// per-key conflict check. This is the schedd's common single-writer case: zero
	// conflict-detection cost. Under contention it falls to the per-write check.
	fast := len(ws) > 0 && sh.commitSeq == ws[0].base
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
		sh.maybeCheckpoint(seq)
	}
	sh.unlockWrite(acq, held)
	return changed, seq
}

// publishTxn notifies the watch hub of a committed batch. It must run only after the batch
// is durable (sh.sync has returned), so watchers never observe an event that a crash could
// lose.
func (sh *shard) publishTxn(ws []*txnWrite, seq uint64) {
	if sh.hub == nil {
		return
	}
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
	ad  *classad.ClassAd // nil for a delete, and for a wire-encoded put until materialized
	// wire holds the uncompressed wire form of an ad ingested by PutOld, encoded from
	// old-ClassAd text at Put time with no intermediate ast.ClassAd. Commit stores these
	// bytes directly; ad stays nil unless something needs the object (see materialize).
	wire []byte
	// text is retained beside wire only so materialize can rebuild the object faithfully
	// through the reference parser rather than round-tripping the encoding.
	text string
	del  bool
}

// live reports whether this buffer holds an ad -- as an object OR as wire bytes not yet
// materialized. Anything scanning the buffered writes must ask this rather than testing
// ad != nil, which silently skips every wire-ingested write.
func (b *txnBuf) live() bool { return !b.del && (b.ad != nil || b.wire != nil) }

// materialize returns the buffered ad as an object, decoding a wire-ingested one on
// first use. Only two things need it -- a read-your-writes Get and ordered-index
// maintenance -- so the common insert path never pays for it.
func (b *txnBuf) materialize() (*classad.ClassAd, bool) {
	if b.del {
		return nil, false
	}
	if b.ad == nil && b.wire != nil {
		ad, err := classad.ParseOld(b.text)
		if err != nil {
			return nil, false
		}
		b.ad = ad
	}
	return b.ad, b.ad != nil
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
		return b.materialize()
	}
	h := tx.c.h.Hash(key)
	idx := tx.c.shardOf(key, h)
	s0 := tx.snapOf(idx)
	stored, codec, dict, ok := tx.c.shards[idx].getAt(h, key, s0)
	if !ok {
		return nil, false
	}
	ad, err := tx.c.decodeAdDict(dict, stored, codec)
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

// PutOld buffers an insert or update of key whose ad arrives as old-ClassAd text,
// encoding it straight to the stored wire form here rather than building an
// ast.ClassAd for Commit to encode. Every transactional guarantee is unchanged --
// the write is buffered, conflict-checked against the same snapshot, and committed
// identically; only the encoding path differs.
//
// It reports whether the fast path was taken. False means the caller should parse the
// text and use Put: an encrypted collection stores sealed values, and the streaming
// encoder does not seal.
func (tx *Txn) PutOld(key []byte, text string) bool {
	if tx.c.EncryptionEnabled() {
		return false
	}
	enc := tx.c.newStreamEncoder()
	seen := make(map[uint32]struct{}, 64)
	var unesc []byte
	w, err := tx.c.encodeOld(text, enc, seen, &unesc)
	if err != nil {
		return false // malformed, or a shape the streaming encoder defers: let the caller parse
	}
	tx.snapOf(tx.c.shardOf(key, tx.c.h.Hash(key)))
	tx.writes[string(key)] = &txnBuf{key: append([]byte(nil), key...), wire: w, text: text}
	return true
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
		w := &txnWrite{hash: h, key: b.key, del: b.del, base: tx.snap[idx], adObj: b.ad, buf: b}
		if !b.del {
			w.codec = tx.c.currentCodec()
			// A wire-ingested put is already encoded; only an object put encodes here.
			raw := b.wire
			if raw == nil {
				raw = tx.c.encodeAd(b.ad.AST())
			}
			w.ad = w.codec.Compress(nil, raw)
		}
		byShard[idx] = append(byShard[idx], w)
	}
	// Phase 1: apply each touched shard's writes under its own lock (fast; disjoint locks).
	type shardCommit struct {
		idx     int
		ws      []*txnWrite
		seq     uint64
		changed bool
	}
	commits := make([]shardCommit, 0, len(byShard))
	for idx, ws := range byShard {
		changed, seq := tx.c.shards[idx].applyTxn(ws)
		commits = append(commits, shardCommit{idx, ws, seq, changed})
	}

	// Phase 2: sync the changed shards CONCURRENTLY. The durability msync is a commit's
	// slow part; distinct shards sync independently, so a commit touching N shards pays
	// ~one msync latency instead of N in series. The parallelism is inherently sized to
	// the commit -- a small commit touches one shard and syncs inline with no goroutines;
	// only a large, many-shard commit fans out.
	if tx.durable {
		var toSync []shardCommit
		for _, c := range commits {
			if c.changed {
				toSync = append(toSync, c)
			}
		}
		if len(toSync) > 0 {
			// Time the whole durability phase as ONE observation: with the shard syncs
			// running in parallel this is the commit's critical path (≈ the slowest shard),
			// the true commit durability latency. The per-shard Sync counter still records
			// each msync, but its total is now fsync WORK, not this latency -- so measure
			// the wall time here explicitly (commitSync).
			//
			// syncFor(seq) group-commits: concurrent transactions hitting the same shard
			// share msync passes (and none returns before the pass covering ITS writes
			// completes), so under commit fan-out N transactions pay ~2 passes, not N.
			syncStart := time.Now()
			if len(toSync) == 1 {
				tx.c.shards[toSync[0].idx].syncFor(toSync[0].seq)
			} else {
				var wg sync.WaitGroup
				wg.Add(len(toSync))
				for _, c := range toSync {
					go func(sh *shard, seq uint64) { defer wg.Done(); sh.syncFor(seq) }(tx.c.shards[c.idx], c.seq)
				}
				wg.Wait()
			}
			tx.c.opm.commitSync.observe(time.Since(syncStart))
		}
	}

	// Phase 3: publish (now durable) and aggregate the result + ordered-index maintenance.
	// Kept sequential: publishing and the ordered index touch collection-shared state.
	var res CommitResult
	for _, c := range commits {
		if c.changed {
			tx.c.shards[c.idx].publishTxn(c.ws, c.seq)
		}
		for _, w := range c.ws {
			if !w.ok {
				res.Conflicts = append(res.Conflicts, w.key)
				continue
			}
			res.Committed++
			if w.del {
				tx.c.removeOrdered(w.key)
			} else {
				ad := w.adObj
				if ad == nil && w.buf != nil {
					ad, _ = w.buf.materialize()
				}
				tx.c.maintainOrdered(w.key, ad)
			}
		}
	}
	return res
}
