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
		seg := sh.segForLoc(l)
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
// The fourth result is the ad as an object, on the same terms as shard.get: non-nil only when a
// delta chain had to be merged, in which case the bytes are just that object encoded.
func (sh *shard) getAt(c *Collection, h uint64, key []byte, s0 uint64, want materializeWant) ([]byte, Codec, *segDictHandle, *classad.ClassAd, bool) {
	raw, codec, dict, obj, ok, _ := sh.getAtWhy(c, h, key, s0, want)
	return raw, codec, dict, obj, ok
}

// getAtWhy is getAt carrying the reason for a miss. getAt's bare false covers conditions with
// unrelated causes -- an MVCC miss, a reaped segment, a missing columnar payload, an
// unresolvable delta chain -- and a caller that must explain the failure needs to tell them
// apart. See readFail.
func (sh *shard) getAtWhy(c *Collection, h uint64, key []byte, s0 uint64, want materializeWant) ([]byte, Codec, *segDictHandle, *classad.ClassAd, bool, readFail) {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	l, ok := sh.findVisible(sh.dirGet(h), key, s0)
	if !ok {
		if l, ok = sh.lookupSealedAt(key, h, s0); !ok {
			return nil, nil, nil, nil, false, failNotVisible
		}
	}
	// segForLoc, not segAt: l can come from the sealed KEY INDEX as well as the chain, and a sidecar
	// describing a segment that has since been rewritten names offsets that no longer hold records.
	seg := sh.segForLoc(l)
	if seg == nil {
		return nil, nil, nil, nil, false, failSegGone
	}
	ad, adCodec, ok2, why := segStoredOrReassembledWhy(c, seg, l.off)
	if !ok2 {
		return nil, nil, nil, nil, false, why
	}
	if raw, rc, obj, handled, ok3, why3 := sh.resolveDelta(c, key, h, s0, recIsDelta(seg.data, l.off), ad, adCodec, want); handled {
		if !ok3 {
			return nil, nil, nil, nil, false, why3
		}
		dict := seg.dict.Load()
		if dict != nil {
			dict.ensureNames()
		}
		return raw, rc, dict, obj, true, failNone
	}
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
	return out, adCodec, dict, nil, true, failNone
}

// hasAt reports whether key had a version live at snapshot s0. It is getAt's resolution
// half with none of its payload work -- no record copy, no dictionary name cache, no
// decode -- because presence is decided entirely by finding the location.
func (sh *shard) hasAt(h uint64, key []byte, s0 uint64) bool {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	l, ok := sh.findVisible(sh.dirGet(h), key, s0)
	if !ok {
		if l, ok = sh.lookupSealedAt(key, h, s0); !ok {
			return false
		}
	}
	// Same guard getAt applies: a location from the sealed key index can name a segment
	// that has since been rewritten, in which case the key is not readable here.
	return sh.segForLoc(l) != nil
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
		seg := sh.segForLoc(l) // same guard as findVisible: this walk follows the same links
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
	// delta records that ad holds only what this write CHANGED, so the record's header can say
	// so and readers can tell without decompressing it (see segment.deltaFlag).
	delta bool
	base  uint64 // snapshot S0: conflict if the key changed after this
	adObj *classad.ClassAd
	// buf is the originating buffered write, so ordered-index maintenance can materialize
	// a wire-ingested ad on demand -- a collection with no ordered index never does.
	buf *txnBuf
	ok  bool // committed (true) or conflicted (false)
}

// hdrFlags returns the record header flag bits for this write.
func (w *txnWrite) hdrFlags() uint32 {
	if w.delta {
		return deltaFlag
	}
	return 0
}

// commitTxn applies a shard's buffered transactional writes with per-write conflict
// detection, all under one shard write lock so the check and apply are atomic with
// respect to other committers (first-committer-wins). Conflicting writes are skipped
// and flagged; the rest commit at one fresh sequence.
func (sh *shard) commitTxn(c *Collection, ws []*txnWrite, durable bool) {
	changed, seq := sh.applyTxn(ws)
	if !changed {
		return
	}
	if durable {
		sh.sync()
	}
	sh.publishTxn(c, ws, seq)
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
		sh.put(w.hash, w.key, w.ad, seq, w.codec, w.hdrFlags())
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
func (sh *shard) publishTxn(c *Collection, ws []*txnWrite, seq uint64) {
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
			ad, codec := w.ad, w.codec
			// Gated on deltaRead AND on somebody actually watching. Neither guard was here, and
			// since every SetAttribute routes through PatchAttrs the effect was that EVERY write
			// -- in a store with no deltas and no watchers -- paid a full chain materialization
			// at publish time. It was 24% of the delta-mode-OFF run.
			if c.deltaRead && w.buf != nil && w.buf.patch != nil && sh.hub.watching() {
				// A delta write's stored bytes are only the attributes that changed. Publishing
				// them hands every watcher a fragment that looks like the whole ad -- a
				// changefeed exporter replacing its destination document would delete the rest
				// of the record downstream. Publish the merged form instead.
				// materializeAt reads other segments' bytes, so it needs the shard read lock;
				// publishTxn runs after the write lock is dropped and holds none of its own.
				sh.mu.RLock()
				merged, _, ok := sh.materializeAt(c, w.key, w.hash, seq, mWire, nil)
				sh.mu.RUnlock()
				if ok {
					ad, codec = merged, identityCodec{}
				}
			}
			sh.hub.publish(sh.idx, seq, w.key, ad, codec, false)
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
	// redact is this transaction's entitlement to sealed values: a redacted transaction reads with no
	// key, so a sealed attribute comes back undefined (see Collection.QueryRedacted). Set once at Begin
	// and never changed, so every read through the transaction inherits it -- a per-read flag would let
	// one call site forget.
	redact bool
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
	// changed names the attributes this write modified, when the caller knows (PutPatch).
	// It is what lets Commit store a delta instead of the whole ad. Empty means "unknown",
	// which forces a full record -- Put cannot tell what changed, so it says nothing.
	changed []string
	// noDelta forces a full record even when changed is populated: set when the write also
	// REMOVED an attribute, which a delta of present attributes cannot express.
	noDelta bool
	// decodeWireFn rebuilds the object from wire when there is no source text; see putWire.
	decodeWireFn func([]byte) (*classad.ClassAd, error)
	// patch holds ONLY the attributes this write changed, with no full ad beside it -- the
	// point being that the writer never had to read the stored ad to produce it. removed names
	// attributes the write deleted. Commit turns this into a delta record directly; when a
	// delta is not permissible it is here, once, that the stored ad is read and merged.
	patch   *classad.ClassAd
	removed []string
}

// live reports whether this buffer holds an ad -- as an object OR as wire bytes not yet
// materialized. Anything scanning the buffered writes must ask this rather than testing
// ad != nil, which silently skips every wire-ingested write.
func (b *txnBuf) live() bool {
	return !b.del && (b.ad != nil || b.wire != nil || b.patch != nil)
}

// materialize returns the buffered ad as an object, decoding a wire-ingested one on
// first use. Only two things need it -- a read-your-writes Get and ordered-index
// maintenance -- so the common insert path never pays for it.
func (b *txnBuf) materialize() (*classad.ClassAd, bool) {
	if b.del {
		return nil, false
	}
	if b.ad == nil && b.wire != nil {
		if b.text == "" {
			// Wire bytes with no source text: a whole record the caller already had encoded
			// (see Txn.putWire). Decode them only if something actually needs the object --
			// which for most tables is nothing, so the common path never pays it.
			ad, err := b.decodeWire()
			if err != nil {
				return nil, false
			}
			b.ad = ad
			return b.ad, true
		}
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
//
// Unapplied is the OTHER kind of not-applied, and the distinction is the point: those writes
// could not be composed at all, and re-applying the identical write cannot change that. A patch
// whose stored base is unreadable is the case that exists today -- the key is present but the
// record behind it will not come back (a delta chain that no longer materializes, a segment that
// went away), so there is nothing to merge the patch onto.
//
// Reporting those as Conflicts made a caller that retries conflicts loop forever. On a production
// mirror each one rewound the tailer, re-applied, failed identically, and after three attempts
// escalated to a full 460-second replay of a 923 MB log that wrote nothing -- 285 times, which is
// why the mirror could never catch up. A caller must be able to tell "try again" from "this will
// never work"; nothing else about the commit changes.
type CommitResult struct {
	Committed int
	Conflicts [][]byte
	Unapplied [][]byte
}

// Conflicted reports whether any buffered write lost a conflict.
func (r CommitResult) Conflicted() bool { return len(r.Conflicts) > 0 }

// HasUnapplied reports whether any buffered write could not be composed at all. Retrying it is
// futile; the caller should record it and make progress.
func (r CommitResult) HasUnapplied() bool { return len(r.Unapplied) > 0 }

// Begin starts an optimistic transaction. Its snapshot for a shard is captured the
// first time the transaction reads or writes a key in that shard.
func (c *Collection) Begin() *Txn {
	return &Txn{c: c, snap: map[int]uint64{}, writes: map[string]*txnBuf{}, durable: true}
}

// BeginRedacted is Begin for a caller NOT entitled to sealed values: reads through the transaction decode
// with no key, so a sealed attribute comes back undefined. Writes are unaffected -- a redacted transaction
// can still store an ad, and encodeAd seals what the collection's policy says to seal.
//
// The entitlement lives on the transaction rather than on each read, so a new read method inherits it
// instead of having to remember it.
func (c *Collection) BeginRedacted() *Txn {
	t := c.Begin()
	t.redact = true
	return t
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
		if b.patch != nil && b.ad == nil && b.wire == nil {
			// A patch-only buffer holds no whole ad, so read-your-writes composes one here:
			// the stored view with this transaction's changes applied over it. Only an actual
			// READ pays this; the write that buffered the patch did not.
			ad, sok := tx.readStored(key)
			if !sok {
				ad = classad.New()
			}
			mergeDelta(ad, b.patch)
			for _, n := range b.removed {
				ad.Delete(n)
			}
			return ad, true
		}
		return b.materialize()
	}
	return tx.readStored(key)
}

// readStored reads key as of the transaction's snapshot, ignoring its own buffered writes.
// readStoredMissHook, when non-nil and returning true for a key, makes readStored report a miss
// for it. Test-only (nil in production): a resolve miss on a key the store HOLDS is the condition
// that produced identity-less fragment rows in production, and it is not reachable on demand --
// it needs a key index and directory that disagree with the records. The behaviour it guards
// (refuse and report a conflict, never store the patch against an empty ad) is worth a test that
// does not depend on reproducing the miss.
var readStoredMissHook func(key []byte) bool

func (tx *Txn) readStored(key []byte) (*classad.ClassAd, bool) {
	ad, ok, _ := tx.readStoredWhy(key)
	return ad, ok
}

// readStoredWhy is readStored carrying the reason for a miss. See readFail.
func (tx *Txn) readStoredWhy(key []byte) (*classad.ClassAd, bool, readFail) {
	if readStoredMissHook != nil && readStoredMissHook(key) {
		return nil, false, failNotVisible // test-only: see readStoredMissHook
	}
	h := tx.c.h.Hash(key)
	idx := tx.c.shardOf(key, h)
	s0 := tx.snapOf(idx)
	stored, codec, dict, obj, ok, why := tx.c.shards[idx].getAtWhy(tx.c, h, key, s0, readWant(tx.redact))
	if !ok {
		return nil, false, why
	}
	// A merged delta chain arrives as the object it was encoded from; decoding the bytes back
	// would rebuild what we are holding. Not for a redacting read -- redaction is applied by the
	// decode, and the object has every sealed value open.
	if obj != nil && !tx.redact {
		return obj, true, failNone
	}
	ad, err := tx.c.decodeAdDictAs(dict, stored, codec, tx.redact)
	if err != nil {
		return nil, false, failDecode
	}
	return ad, true, failNone
}

// Has reports whether key exists as the transaction sees it, without reading the stored
// record: it resolves the key to a live record location and stops there. Get is the wrong
// tool for a presence question -- it copies the record bytes out from under the shard lock
// and decodes them, and for a wide job ad that decode is the most expensive thing in an
// ingest -- so a caller that only needs presence (a diagnostic counter, an upsert-vs-insert
// branch) should ask here.
func (tx *Txn) Has(key []byte) bool {
	if b, ok := tx.writes[string(key)]; ok {
		return b.live()
	}
	h := tx.c.h.Hash(key)
	idx := tx.c.shardOf(key, h)
	return tx.c.shards[idx].hasAt(h, key, tx.snapOf(idx))
}

// putWire buffers a whole record the caller already holds in encoded form, skipping the
// decode-and-re-encode round trip a Put would make of it.
//
// It exists for the seal collapse, whose whole job is to turn a resolved chain into one whole
// record: materializeAt hands back exactly those bytes, and routing them through a ClassAd and
// back was 57% of the ingest run. The bytes must be a complete record in this collection's
// encoding -- nothing validates that, which is why this is unexported and has one caller.
func (tx *Txn) putWire(key, wire []byte, decode func([]byte) (*classad.ClassAd, error)) {
	tx.putWireAd(key, wire, nil, decode)
}

// putWireAd is putWire for a caller that already holds the ad these bytes encode. The object is
// buffered beside the bytes, so anything in Commit that wants it (ordered-index maintenance,
// watch publication) gets it for free instead of decoding the bytes back into the object they
// were just encoded from. decode is still required: ad may be nil, and a later caller may need
// to materialize from bytes alone.
func (tx *Txn) putWireAd(key, wire []byte, ad *classad.ClassAd, decode func([]byte) (*classad.ClassAd, error)) {
	tx.snapOf(tx.c.shardOf(key, tx.c.h.Hash(key)))
	tx.writes[string(key)] = &txnBuf{
		key: append([]byte(nil), key...), wire: wire, ad: ad, decodeWireFn: decode,
	}
}

// PatchAttrs buffers a change to key expressed ONLY as the attributes it changes -- the whole
// ad is never read, built, or buffered. That is the point: a mirror applying a schedd's
// attribute updates spends most of its time reading an ad back just to hand a copy of it to
// the encoder, and the update itself does not need it.
//
// Commit stores this as a delta record. When it cannot -- the key has no full record yet, the
// chain has reached its bound, or the write removed an attribute -- Commit reads the stored ad
// and merges there, so the read happens once per re-materialization instead of once per write.
//
// removed names attributes the write deletes. Reads through this transaction still see the
// merged view (Get applies the patch over the stored ad), so read-your-writes is unaffected.
func (tx *Txn) PatchAttrs(key []byte, patch *classad.ClassAd, removed []string) {
	tx.snapOf(tx.c.shardOf(key, tx.c.h.Hash(key)))
	// Reuse an existing patch buffer for this key instead of replacing it. A caller applying a
	// schedd transaction calls this once per attribute it changed, accumulating into one patch
	// object, so a fresh buffer and a fresh copy of the key per call was N-1 of each thrown away
	// per transaction. A buffer holding anything else -- a whole ad, wire bytes, a delete -- is
	// still REPLACED, which is what it meant before: the last write in a transaction wins.
	if b, ok := tx.writes[string(key)]; ok && b.patch != nil && b.ad == nil && b.wire == nil && !b.del {
		b.patch, b.removed = patch, removed
		return
	}
	tx.writes[string(key)] = &txnBuf{
		key: append([]byte(nil), key...), patch: patch, removed: removed,
	}
}

// PutPatch is Put for a caller that knows which attributes it changed. The full ad is still
// buffered -- read-your-writes and the value lookup both need it -- but Commit may store only
// the named attributes as a delta record, which for a wide ad is the difference between
// encoding and compressing ~7.3 KB and ~150 B. removed reports that the write also deleted an
// attribute, which a delta cannot express, so the whole ad is stored instead.
//
// It is always safe to call Put instead; a caller that does loses only the optimization.
func (tx *Txn) PutPatch(key []byte, ad *classad.ClassAd, changed []string, removed bool) {
	tx.snapOf(tx.c.shardOf(key, tx.c.h.Hash(key)))
	tx.writes[string(key)] = &txnBuf{
		key: append([]byte(nil), key...), ad: ad, changed: changed, noDelta: removed,
	}
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
// It reports whether the fast path was taken. False means the caller should parse the text and use Put.
//
// An encrypted collection no longer refuses this path wholesale. The streaming encoder cannot seal, so
// encodeOld defers to the sealing path for an ad that HAS something to seal, and streams the rest -- which
// for job and history ads is nearly all of them. Refusing wholesale made encryption cost every ingest.
func (tx *Txn) PutOld(key []byte, text string) bool {
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
	// Collapsing sealed delta chains happens on the way OUT of a commit: the seal that
	// triggers it occurs under a shard write lock deep inside this call, and collapsing means
	// ordinary reads and writes. No-op unless a segment actually sealed.
	defer tx.c.collapseSealedChains()
	// One scratch buffer for every encode in this commit: each write's uncompressed wire bytes
	// are alive only until the line that compresses them, so the next write can have the same
	// buffer. It grows once to the widest ad in the batch instead of being reallocated per write.
	var encScratch []byte
	byShard := make(map[int][]*txnWrite)
	// Keys whose patch could not be composed because the store holds the key but could not read
	// its current record. They are reported as UNAPPLIED, not as conflicts: storing them against
	// an empty ad is what turned a resolve miss into a fragment row, but calling them conflicts
	// made every caller that retries conflicts retry something that cannot succeed. See
	// CommitResult.Unapplied.
	var unreadable [][]byte
	for _, b := range tx.writes {
		if b.del && tx.c.deltas != nil {
			// The key's records are going away, so the tracker must stop believing a full
			// record exists for it -- otherwise a later re-creation could be stored as a
			// delta chained to a base that was deleted.
			tx.c.deltas.forget(tx.c.h.Hash(b.key))
		}
		h := tx.c.h.Hash(b.key)
		idx := tx.c.shardOf(b.key, h)
		w := &txnWrite{hash: h, key: b.key, del: b.del, base: tx.snap[idx], adObj: b.ad, buf: b}
		if !b.del {
			w.codec = tx.c.currentCodec()
			// A wire-ingested put is already encoded; only an object put encodes here.
			raw := b.wire
			switch {
			case raw != nil:
				// Bytes the caller buffered (Txn.putWire); they are not ours to reuse.
			case b.patch != nil && b.ad == nil:
				var ok bool
				raw, w.delta, ok = tx.encodePatchOnly(encScratch[:0], b, h)
				if !ok {
					unreadable = append(unreadable, b.key)
					continue
				}
				encScratch = raw
			default:
				raw, w.delta = tx.c.encodeDelta(encScratch[:0], b.key, h, b.ad, b.changed, b.noDelta)
				encScratch = raw
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
	res.Unapplied = append(res.Unapplied, unreadable...)
	for _, c := range commits {
		if c.changed {
			tx.c.shards[c.idx].publishTxn(tx.c, c.ws, c.seq)
		}
		for _, w := range c.ws {
			if !w.ok {
				res.Conflicts = append(res.Conflicts, w.key)
				continue
			}
			res.Committed++
			if w.del {
				tx.c.removeOrdered(w.key)
			} else if tx.c.hasOrdered() {
				// The WHOLE block is gated on the collection having an ordered index, because
				// maintainOrdered is the only thing that consumes the ad and it returns
				// immediately without one. The gate used to sit on the inner Get alone, which
				// left materialize() below running unconditionally -- and for a write buffered as
				// wire bytes with no object (every spliced delta collapse) that is a full decode
				// of the bytes the collapse had just produced, thrown away. On a real queue
				// replay it was 142 MB, 14% of the tail's allocation, feeding a consumer that did
				// not exist. Same mistake as the one the comment below describes, one line up.
				ad := w.adObj
				if ad == nil && w.buf != nil {
					ad, _ = w.buf.materialize()
				}
				if ad == nil && w.buf != nil && w.buf.patch != nil {
					// A delta write buffers only the changed attributes, so materialize() has
					// no object to return. Handing nil to maintainOrdered evaluated the index
					// predicate against nothing, which is never a member -- so every delta
					// write EVICTED its key from every ordered index while the stored ad stayed
					// correct. (On a chained collection it is worse: ordered.go dereferences
					// the nil to read its parent.) Read the merged ad back instead.
					if merged, ok := tx.c.Get(w.key); ok {
						ad = merged
					}
				}
				if ad != nil {
					tx.c.maintainOrdered(w.key, ad)
				}
			}
		}
	}
	return res
}

// decodeWire rebuilds this buffer's ad from its wire bytes.
func (b *txnBuf) decodeWire() (*classad.ClassAd, error) {
	if b.decodeWireFn == nil {
		return nil, errNoWireDecoder
	}
	return b.decodeWireFn(b.wire)
}
