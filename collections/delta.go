package collections

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Delta records store only the attributes one write CHANGED, instead of the whole ad.
//
// The motivating measurement: replaying a real OSPool AP job_queue.log, 117,796 ad-touches
// in 20 MB of log each rewrote a ~7.3 KB ad, handing the encoder and compressor 861 MB --
// a 43x amplification of the log they came from. The attributes one schedd transaction
// actually changes average ~150 bytes (median 70). Storing just those is ~49x less work per
// write.
//
// The cost is read amplification: a point read must walk back along the key's version chain
// collecting deltas until it reaches a full record, then merge them oldest-first. That is
// bounded by writing a fresh full record once a key has accumulated DeltaMax deltas. The
// bound is ADVISORY -- correctness needs only that walking back eventually reaches a full
// record, never that the chain be short -- so an approximate depth counter is sound, and a
// racing second writer can only make a chain longer than intended, never break it.
//
// The hard invariant is elsewhere: anything that can DROP an older version must first
// materialize the deltas that depend on it. Compaction retains superseded versions only
// above a retain floor, so it materializes current deltas as it copies them (see
// compactShard). Sealing into columnar form has the same requirement, which is why delta
// mode and columnarization are not currently enabled together.

// deltaTracker counts, per key, how many delta records have been written since that key's
// last full record. Absence of a key means "no full record has been written by this process",
// which forces a full record -- the conservative direction, and what makes a fresh start or a
// reopen self-healing without reading anything.
type deltaTracker struct {
	mu sync.Mutex
	// depth is keyed by the key's 64-bit hash, not the key itself, for two reasons that both
	// matter at this write rate. A map with string keys holds pointers, so the GC scans every
	// entry on every cycle; a map[uint64]int holds none and is skipped entirely. And passing a
	// key as a string forces an allocation per write -- Go elides the []byte->string copy only
	// for a direct map index, not for a function argument or a map STORE.
	//
	// Collisions are tolerated rather than resolved, which is safe here in a way it would not
	// be in the store: two keys sharing a counter can only make a chain longer or shorter than
	// DeltaMax intended. The bound is advisory -- correctness needs a chain to terminate at a
	// whole record, which keyExists establishes, never a particular length.
	depth map[uint64]int
	// deltas and fulls count the decisions taken. They are the only way to tell a working
	// delta store from one that has quietly fallen back to storing whole ads on every write --
	// which reads back perfectly correctly and saves nothing, so no correctness test can catch
	// it. Also the ratio an operator wants when deciding whether DeltaMax is earning its keep.
	deltas, fulls atomic.Int64
}

func newDeltaTracker() *deltaTracker { return &deltaTracker{depth: make(map[uint64]int)} }

// maxTrackedKeys bounds the tracker's memory. It holds one small entry per key that has been
// written, which for a schedd mirror is one per job -- bounded, but not by anything the store
// controls, and a long-lived process churning through job ids would grow it without limit.
//
// Overflow drops the whole map rather than evicting cleverly: an absent key simply means "no
// full record is known", which forces the next write for it to be a full record. That is the
// conservative direction -- it costs writes, never correctness -- and it self-heals within one
// write per live key.
const maxTrackedKeys = 1 << 20

// next reports whether the write for key should be a delta, and records the decision. A
// caller that cannot use a delta (an attribute was deleted, nothing changed, the key has no
// full record yet, or the bound is reached) gets false and the key's depth resets.
func (t *deltaTracker) next(h uint64, eligible, baseExists bool, max int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.depth) >= maxTrackedKeys {
		clear(t.depth)
	}
	d := t.depth[h]
	// Absence means "depth 0" now, not "no base": collapseSealedChains guarantees every live
	// key has a whole record, so the only question left is whether the key exists at all --
	// which the caller answers with a location lookup, no decode. Treating absence as "no
	// base" instead would force a whole record per key per seal.
	if !eligible || !baseExists || d >= max {
		t.depth[h] = 0 // this write is a full record; the chain restarts here
		t.fulls.Add(1)
		return false
	}
	t.depth[h] = d + 1
	t.deltas.Add(1)
	return true
}

// forget drops a key's depth so its next write is a full record. Used when a key is deleted
// and when compaction materializes, so the tracker never claims a base exists that the store
// no longer holds.
func (t *deltaTracker) forget(h uint64) {
	t.mu.Lock()
	delete(t.depth, h)
	t.mu.Unlock()
}

// deltaAd builds the ad to store in a delta record: the named attributes, taken from the
// full ad the caller already has. Attribute names are case-insensitive, so the wanted set is
// matched folded. Returns nil if any name is missing, which forces a full record rather than
// storing an incomplete delta.
func deltaAd(full *classad.ClassAd, changed []string) *ast.ClassAd {
	if full == nil || len(changed) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(changed))
	for _, n := range changed {
		want[strings.ToLower(n)] = struct{}{}
	}
	src := full.AST()
	if src == nil {
		return nil
	}
	out := &ast.ClassAd{Attributes: make([]*ast.AttributeAssignment, 0, len(changed))}
	for _, a := range src.Attributes {
		if _, ok := want[strings.ToLower(a.Name)]; ok {
			out.Attributes = append(out.Attributes, &ast.AttributeAssignment{Name: a.Name, Value: a.Value})
			delete(want, strings.ToLower(a.Name))
		}
	}
	if len(want) != 0 {
		return nil // a named attribute was not on the ad: store a full record instead
	}
	return out
}

// mergeDelta applies a delta's attributes over base, last write winning. base is mutated.
func mergeDelta(base, delta *classad.ClassAd) {
	if base == nil || delta == nil {
		return
	}
	d := delta.AST()
	if d == nil {
		return
	}
	for _, a := range d.Attributes {
		base.Insert(a.Name, a.Value)
	}
}

// storeReads counts reads of a stored record made to satisfy a write. It exists so a test can
// assert that patching an attribute performs NONE: the removal of that read is the point of
// delta records, it is invisible in the results, and nothing else would notice it coming back.
var storeReads atomic.Int64

// isDeltaRecord reports whether decompressed record bytes are a delta.
func isDeltaRecord(rec []byte) bool { return wire.IsDelta(rec) }

// encodeDelta encodes the record bytes for a write: a delta holding only the changed
// attributes when the collection has delta mode on and this write qualifies, else the whole
// ad exactly as before. The decision is recorded in the tracker, so the caller does not have
// to reason about the chain length.
//
// Delta mode is off unless Options.DeltaMax is set, so every existing caller and store keeps
// byte-identical behavior.
func (c *Collection) encodeDelta(key string, ad *classad.ClassAd, changed []string, removed bool) []byte {
	if c.deltaMax <= 0 || c.deltas == nil || !c.inline {
		return c.encodeAd(ad.AST())
	}
	var d *ast.ClassAd
	eligible := !removed && len(changed) > 0
	if eligible {
		if d = deltaAd(ad, changed); d == nil {
			eligible = false
		}
	}
	// canWriteDelta LAST, and only when a delta is actually wanted: it records the store's
	// delta mode, and asking it on every write recorded it for stores that only ever held whole
	// records -- which is the permanence this lazy marker exists to avoid.
	if eligible && !c.canWriteDelta() {
		eligible = false
	}
	if !c.deltas.next(c.h.Hash([]byte(key)), eligible, c.keyExists([]byte(key)), c.deltaMax) {
		return c.encodeAd(ad.AST())
	}
	// Deltas are encoded with the inline-name form and no hot header: the hot header indexes
	// a whole ad for the match fast path, and a fragment has no business claiming to be one.
	return wire.EncodeInlineDelta(nil, d, nil, c.shouldEncrypt, c.sealer)
}

// materializeAt reconstructs the whole ad for key as of snapshot s0 from a delta chain,
// returning uncompressed wire bytes. The caller must hold at least the shard read lock: this
// reads segment bytes, and a segment's mapping is only guaranteed alive under that lock.
//
// It gathers every version of the key visible at s0 from BOTH resolution routes -- the
// in-directory bucket chain and the sealed key index -- rather than walking one of them,
// because which route holds a given version depends on compaction and reopens, and a delta
// whose base moved to the other route would otherwise be unresolvable. The versions are
// ordered by commit sequence, the newest full record among them is the base, and the deltas
// after it are merged over it in order.
//
// A chain with no full record cannot happen by construction (a key's first write is always
// full, and anything that drops versions materializes first), so it is reported as a miss
// rather than guessed at: returning a partial ad would look like data and be wrong.
// hasFull reports whether any gathered version is a whole record, i.e. whether the chain can
// be resolved from what has been found so far.
func (sh *shard) hasFull(c *Collection, vers []deltaVer) bool {
	for _, v := range vers {
		stored, codec, ok := segStoredOrReassembled(c, v.seg, v.off)
		if !ok {
			continue
		}
		if raw, err := codec.Decompress(nil, stored); err == nil && !isDeltaRecord(raw) {
			return true
		}
	}
	return false
}

func (sh *shard) materializeAt(c *Collection, key []byte, h uint64, s0 uint64) ([]byte, bool) {
	var vers []deltaVer
	add := func(seg *segment, off uint32) {
		if seg == nil {
			return
		}
		if recSeq(seg.data, off) <= s0 && bytes.Equal(recKey(seg.data, off), key) {
			vers = append(vers, deltaVer{recSeq(seg.data, off), seg, off})
		}
	}
	for l := sh.dirGet(h); l.valid(); {
		seg := sh.segForLoc(l)
		if seg == nil {
			break
		}
		add(seg, l.off)
		l = recNext(seg.data, l.off)
	}
	sh.forEachSealedRecord(key, h, func(seg *segment, off uint32) bool {
		add(seg, off)
		return true
	})
	// Neither route is guaranteed to reach a key's OLDER versions. rebuildDir -- which runs on
	// every crash recovery, and on a clean reopen of a time-travel or parent/child collection --
	// relinks each bucket to only each key's CURRENT record, and forEachSealedRecord skips the
	// active segment by construction. A chain living inside the active segment, which is the
	// normal shape, therefore loses its base and the key silently vanishes: Get says not-found
	// while Has says true and a scan yields a fragment.
	//
	// So when the routes above produced no whole record, fall back to reading the active
	// segment directly. Chains never span a seal, so that is the only place the rest of one can
	// be. Bounded to the case that actually needs it: with the directory intact this never runs.
	if !sh.hasFull(c, vers) && sh.act != nil {
		seg := sh.act
		for off := uint32(0); off < uint32(seg.used); {
			tl := recTotalLen(seg.data, off)
			if tl == 0 || off+tl > uint32(seg.used) {
				break
			}
			if recKeyLen(seg.data, off)&markerFlag == 0 {
				add(seg, off)
			}
			off += tl
		}
	}
	if len(vers) == 0 {
		return nil, false
	}
	sort.Slice(vers, func(i, j int) bool { return vers[i].seq < vers[j].seq })

	// Decode from the newest full record forward. Walking backwards to find it first would
	// mean decoding the deltas twice.
	base := -1
	raws := make([][]byte, len(vers))
	for i := len(vers) - 1; i >= 0; i-- {
		stored, codec, ok := segStoredOrReassembled(c, vers[i].seg, vers[i].off)
		if !ok {
			return nil, false
		}
		raw, err := codec.Decompress(nil, stored)
		if err != nil {
			return nil, false
		}
		raws[i] = raw
		if !isDeltaRecord(raw) {
			base = i
			break
		}
	}
	if base < 0 {
		return nil, false // no full record: see above, do not guess
	}
	full, err := c.decodeWire(raws[base])
	if err != nil {
		return nil, false
	}
	merged := classad.FromAST(full)
	for i := base + 1; i < len(vers); i++ {
		d, derr := c.decodeWire(raws[i])
		if derr != nil {
			return nil, false
		}
		mergeDelta(merged, classad.FromAST(d))
	}
	return c.encodeAd(merged.AST()), true
}

// DeltaStats reports how many records this collection has stored as deltas versus in full
// since it was opened. Both zero means delta mode is off (or nothing has been written).
func (c *Collection) DeltaStats() (deltas, fulls int64) {
	if c.deltas == nil {
		return 0, 0
	}
	return c.deltas.deltas.Load(), c.deltas.fulls.Load()
}

// resolveDelta is the shared tail of the point-read paths (shard.get and shard.getAt) once a
// record's stored bytes are in hand. When delta mode is off it reports false and the caller
// returns the bytes untouched, exactly as before. When it is on, the bytes have to be examined
// -- so they are decompressed here and returned under identityCodec, sparing the caller a
// second decompression -- and a delta is replayed into the whole ad.
//
// Callers must hold the shard read lock: materializeAt reads other segments' bytes.
func (sh *shard) resolveDelta(c *Collection, key []byte, h, s0 uint64, stored []byte, codec Codec) ([]byte, Codec, bool, bool) {
	if !c.deltaRead {
		return nil, nil, false, true // not handled here; caller proceeds as before
	}
	raw, err := codec.Decompress(nil, stored)
	if err != nil {
		return nil, nil, true, false
	}
	if !isDeltaRecord(raw) {
		return raw, identityCodec{}, true, true
	}
	merged, ok := sh.materializeAt(c, key, h, s0)
	if !ok {
		return nil, nil, true, false
	}
	return merged, identityCodec{}, true, true
}

// materializeOpenChains writes a full record for every key that currently ends in a delta,
// so that afterwards no surviving delta depends on an older version.
//
// This is how the one hard invariant of delta records is kept: replay needs the base to still
// be there, and compaction is free to DROP superseded versions once they fall below its retain
// floor. Rather than teach compaction to reconstruct fragments -- inside a function that reads
// segment bytes off-lock and already carries two production SIGSEGV scars -- the chains are
// collapsed from outside first, using nothing but ordinary reads and writes. After this every
// key's newest record is self-contained and every delta behind it is superseded, so whatever
// compaction reclaims, what remains is readable.
//
// It is called at the head of Compact, which is rare. The cost is one full write per key with
// an open chain, which is the write that delta mode deferred in the first place.
// collapseBeforeRewrite collapses every live delta chain ahead of anything that REWRITES
// records. It must be called by every route into compactShard -- Compact, RetrainDict, and the
// codec-change path -- not just Compact.
//
// Two separate hazards, either of which is permanent:
//   - compactShard drops superseded versions below the retain floor, so a surviving delta can
//     lose the whole record it merges from and the key becomes unreadable.
//   - its interning re-encode (compact.go, appendInterned) rebuilds a record from its AST and
//     does not carry the wire flags, so a delta comes out looking like a complete ad. That is
//     not a read-time miss that replay can recover; it is corruption written back to disk.
//
// RetrainDict reached compactShard with no collapse at all, which was reproduced as 40 of 40
// keys permanently truncated from 45 attributes to 5.
func (c *Collection) collapseBeforeRewrite() {
	// deltaRead, not deltas/deltaMax: a store REOPENED without DeltaMax still holds delta
	// records, and every guard in this feature used to key on the write-side variables -- so
	// such a store silently stopped collapsing before compaction and re-enabled
	// columnarization, which is precisely where its fragments get rewritten as whole ads.
	if !c.deltaRead {
		return
	}
	if c.deltas == nil {
		c.deltas = newDeltaTracker() // read-only reopen: still needs somewhere to track a collapse
	}
	c.collapseLiveChains()
}

// encodePatchOnly produces the record bytes for a write buffered as a bare patch (see
// Txn.PatchAttrs), where no whole ad was ever read or built.
//
// The fast path encodes the patch as a delta and touches nothing else -- which is the entire
// point: the write costs the encoding of the attributes it changed, not of the ad they belong
// to. The slow path is taken when a delta will not do, and only there is the stored ad read
// and merged: on a key's first write, when the chain has reached its bound, and when the write
// removed an attribute (which a delta of present attributes cannot express). So a read happens
// once per re-materialization rather than once per update.
func (tx *Txn) encodePatchOnly(b *txnBuf) []byte {
	c := tx.c
	eligible := len(b.removed) == 0 && b.patch != nil
	if eligible && c.deltaMax > 0 && c.deltas != nil && c.inline && c.canWriteDelta() {
		if c.deltas.next(c.h.Hash(b.key), true, c.keyExists(b.key), c.deltaMax) {
			return wire.EncodeInlineDelta(nil, b.patch.AST(), nil, c.shouldEncrypt, c.sealer)
		}
	} else if c.deltas != nil {
		// Record the decision so the chain restarts here even when delta mode declined.
		c.deltas.next(c.h.Hash(b.key), false, false, c.deltaMax)
	}
	// A whole record is required: compose it from the stored ad plus this write's changes.
	// b.ad is filled in so the rest of Commit (ordered-index maintenance, watch publication)
	// sees the same object it would have for an ordinary Put.
	storeReads.Add(1)
	ad, ok := tx.readStored(b.key)
	if !ok {
		ad = classad.New()
	}
	mergeDelta(ad, b.patch)
	for _, n := range b.removed {
		ad.Delete(n)
	}
	b.ad = ad
	return c.encodeAd(ad.AST())
}

// deltaModeFile records that a store has written delta records, so a later open knows it must
// replay them whatever DeltaMax it was given. See initDeltaMode.
const deltaModeFile = "deltamode"

// initDeltaMode reconciles the configured DeltaMax with what the store on disk already holds.
//
// Writing deltas is a choice this process makes (deltaMax); READING them is not optional -- a
// store that contains even one delta must replay for the rest of its life, or it silently
// serves fragments. So the marker is written when delta mode is enabled and, once present,
// turns replay on regardless. Turning DeltaMax back to 0 therefore stops NEW deltas being
// written while leaving the existing ones readable, which is what makes the option safe to
// change and safe to roll back.
func (c *Collection) initDeltaMode(dir string) error {
	path := filepath.Join(dir, deltaModeFile)
	if _, err := os.Stat(path); err == nil {
		c.deltaRead = true
		c.deltaMarked.Store(true) // already recorded; do not rewrite it
	} else if !os.IsNotExist(err) {
		return err
	}
	// The marker is NOT written here. It records that the store CONTAINS deltas, and enabling
	// the option is not the same as storing one -- writing it at open meant that setting
	// DeltaMax once, even if no delta was ever stored, turned replay on permanently with no
	// supported way back. It is written on the first actual delta write instead (see
	// markDeltaMode), which makes the option genuinely reversible: set it back to 0 and a store
	// that never wrote a delta is byte-for-byte an ordinary one.
	c.deltaDir = dir
	return nil
}

// markDeltaMode records, once and durably, that this store now contains delta records -- so a
// later open replays them whatever DeltaMax it is given. Called on the first delta write.
//
// Durability matters more here than anywhere else in the feature: this file decides whether the
// store's records are legible at all. Lose it and every delta is served as a whole ad, which is
// the 45-attributes-becomes-1 failure it exists to prevent, so it goes down via
// tmp+fsync+rename+dir-fsync rather than a bare os.WriteFile. (basecodec, which this otherwise
// mirrors, has that gap; it is not a precedent worth copying.)
//
// Ordering: called BEFORE the delta record it describes is committed, so a crash can leave the
// marker without the record -- harmless, a store with no deltas reads identically either way --
// but never the record without the marker.
func (c *Collection) markDeltaMode() {
	if c.deltaDir == "" || c.deltaMarked.Load() {
		return
	}
	c.deltaMarkMu.Lock()
	defer c.deltaMarkMu.Unlock()
	if c.deltaMarked.Load() {
		return
	}
	if err := writeFileDurable(filepath.Join(c.deltaDir, deltaModeFile), []byte("1")); err != nil {
		return // could not record it: the next write tries again rather than storing a delta
	}
	c.deltaMarked.Store(true)
}

// canWriteDelta reports whether a delta may be stored, recording the store's delta mode first.
// A delta must never reach disk before the marker that makes it legible.
func (c *Collection) canWriteDelta() bool {
	if c.deltaDir == "" {
		return true // in-memory: nothing to persist, nothing to recover
	}
	c.markDeltaMode()
	return c.deltaMarked.Load()
}

// TrackedKeys reports how many keys the delta tracker holds. The tracker is the one new
// in-memory structure delta records add -- one small entry per key that has been written -- so
// this is what an operator checks when asking what the feature costs in RAM, and what a test
// checks to confirm the bound is doing something.
func (c *Collection) TrackedKeys() int {
	if c.deltas == nil {
		return 0
	}
	c.deltas.mu.Lock()
	defer c.deltas.mu.Unlock()
	return len(c.deltas.depth)
}

// collapseSealedChains is the heart of the seal-scoped design: when a segment seals, every
// key whose chain is still open gets a whole record written, and the tracker is emptied.
//
// Two things follow, and together they are the reason to do it:
//
//   - No LIVE delta ever exists outside the active segment. Every fragment in a sealed
//     segment is superseded by the whole record written here, so anything that rewrites a
//     sealed segment -- columnarization above all -- can keep assuming each live record is a
//     whole ad. The sealed format is untouched.
//   - A chain can never span a seal, so the tracker only ever describes the ACTIVE segment
//     and can be dropped wholesale. Its size stops being a function of how many keys the
//     table holds and becomes a function of how many are touched between seals.
//
// Emptying the tracker is only safe BECAUSE of the first property: afterwards every live key
// provably has a whole record, so a missing tracker entry means "depth 0, base exists" rather
// than "no base known". Getting that backwards costs the entire optimization -- it forces a
// whole record per key per seal, which measured WORSE than not having deltas at all.
//
// Runs after a commit, never inside one: collapsing reads and writes keys, and the seal that
// triggers it happens under the shard write lock.
func (c *Collection) collapseSealedChains() {
	if c.deltas == nil {
		// Delta mode off: nothing to collapse, but the seal bookkeeping still has to be
		// drained. writeRecord appends to pendingSeal unconditionally, and leaving it to grow
		// retains a pointer to every segment the shard has ever sealed -- including ones
		// compaction has retired and unmapped. That is a leak in the DEFAULT configuration,
		// which is exactly where it must not be.
		for _, sh := range c.shards {
			if sh.sealedPending.Swap(false) {
				sh.mu.Lock()
				sh.pendingSeal = nil
				sh.mu.Unlock()
			}
		}
		return
	}
	pending := false
	for _, sh := range c.shards {
		if sh.sealedPending.Load() {
			pending = true
			break
		}
	}
	if !pending || !c.collapsing.CompareAndSwap(false, true) {
		// Deliberately does NOT clear sealedPending before the CAS. Clearing it first meant a
		// committer that sealed while a collapse was in flight consumed its own flag and then
		// returned, so that seal was never collapsed and a live delta stayed in a sealed
		// segment -- silently breaking the invariant the whole design rests on. The flags are
		// consumed only by the goroutine that wins the CAS, in liveDeltaKeys.
		return
	}
	defer c.collapsing.Store(false)

	c.collapseLiveChains()
}

// collapseLiveChains writes a whole record for every key whose current record is a delta, and
// empties the tracker. It is the one operation behind both the seal collapse and the
// pre-compaction pass: in each case the requirement is the same -- afterwards no live fragment
// depends on anything that is about to be sealed away or reclaimed.
//
// The keys come from the active segments rather than the tracker, which is keyed by hash and
// cannot name them. That is not a workaround: by this function's own invariant the active
// segment is the only place a live fragment can be.
func (c *Collection) collapseLiveChains() {
	open := c.liveDeltaKeys()
	c.deltas.mu.Lock()
	clear(c.deltas.depth) // the tracker describes the active segment, which is now collapsed
	c.deltas.mu.Unlock()

	for _, key := range open {
		// Read and write under ONE transaction, so the snapshot is taken before the read and a
		// commit that lands in between actually conflicts. Reading with c.Get first and opening
		// the transaction afterwards captured the snapshot AFTER the value, so the conflict
		// check could never fire and the collapse wrote a stale whole ad over a newer update --
		// reproduced as 16 of 16 keys losing committed increments.
		tx := c.Begin()
		idx := c.shardOf(key, c.h.Hash(key))
		sh := c.shards[idx]
		// snapOf takes the shard read lock itself, so it must run BEFORE this one: Go's RWMutex
		// is not reentrant for RLock, and a writer arriving between the two acquisitions
		// deadlocks the pair. Caught by the concurrent-writer test, which hung.
		s0 := tx.snapOf(idx)
		sh.mu.RLock()
		merged, ok := sh.materializeAt(c, key, c.h.Hash(key), s0)
		sh.mu.RUnlock()
		if !ok {
			continue // deleted underneath us, or already whole; nothing to collapse
		}
		// The merged WIRE BYTES, written straight through. Going via Get would decode these
		// same bytes into a ClassAd purely so Put could encode them again -- a round trip per
		// chain per seal, which profiled at 57% of an ingest run.
		tx.putWire(key, merged, c.decodeWireAd)
		if r := tx.Commit(); r.Conflicted() {
			// A newer writer won. Its write is the current version; if it was a delta the chain
			// is still open and the next seal collapses it.
			continue
		}
	}
}

// keyExists reports whether key has a live record, resolving its location without decoding
// anything. It is what replaces the tracker's old "have I written a base for this key"
// bookkeeping: after a seal-collapse the tracker is empty, so the question has to be asked of
// the store -- but only the cheap half of it. A key with no record must get a whole record,
// since a delta chained to nothing is unreadable.
func (c *Collection) keyExists(key []byte) bool {
	h := c.h.Hash(key)
	sh := c.shards[c.shardOf(key, h)]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	// findCurrent, not findVisible with seqMax: a LIVE record carries supersededBySeq ==
	// seqMax, and findVisible asks for supersededBySeq > s0, so a seqMax snapshot matches
	// nothing at all. That mistake made every write look like a new key and turned every
	// delta back into a whole record -- silently, since the results stay correct.
	if l, ok := sh.findCurrent(sh.dirGet(h), key); ok {
		return sh.segForLoc(l) != nil
	}
	l, ok := sh.lookupSealed(key, h)
	return ok && sh.segForLoc(l) != nil
}

// liveDeltaKeys returns the keys whose CURRENT record is a delta, read from the active
// segments. The tracker is keyed by hash and so cannot name them; the active segments can,
// and by the seal-collapse invariant they are the only place a live fragment exists.
func (c *Collection) liveDeltaKeys() [][]byte {
	var out [][]byte
	for _, sh := range c.shards {
		// Resolve AND read under one unbroken read lock, with no segment pointer carried
		// across a release. Retirement happens under the write lock and the munmap runs after
		// it is dropped, so a pointer captured in one locked section and dereferenced in the
		// next is a use-after-munmap -- reproduced as a SIGSEGV in recTotalLen. The flags are
		// consumed here, by the collapse that is about to act on them.
		sh.mu.Lock()
		sh.sealedPending.Store(false)
		segs := append([]*segment(nil), sh.pendingSeal...)
		sh.pendingSeal = nil
		if sh.act != nil {
			segs = append(segs, sh.act)
		}
		for _, seg := range segs {
			for off := uint32(0); off < uint32(seg.used); {
				tl := recTotalLen(seg.data, off)
				if tl == 0 || off+tl > uint32(seg.used) {
					break
				}
				if recKeyLen(seg.data, off)&markerFlag == 0 && recSuperseded(seg.data, off) == seqMax {
					if raw, err := seg.codec.Decompress(nil, recAd(seg.data, off)); err == nil && isDeltaRecord(raw) {
						out = append(out, append([]byte(nil), recKey(seg.data, off)...))
					}
				}
				off += tl
			}
		}
		sh.mu.Unlock()
	}
	return out
}

// writeFileDurable writes data to path so that it survives a crash: a temp file in the same
// directory, fsync'd, renamed into place, and the directory fsync'd so the rename itself is
// durable.
func writeFileDurable(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil // the rename landed; an unopenable dir is not worth failing the open over
	}
	defer d.Close()
	return d.Sync()
}

// deltaVer is one version of a key: where it lives and when it was committed.
type deltaVer struct {
	seq uint64
	seg *segment
	off uint32
}

// errNoWireDecoder reports a wire-buffered write whose object was asked for without a decoder
// having been supplied. Only putWire creates such a buffer, and it always supplies one.
var errNoWireDecoder = errors.New("collections: wire-buffered write has no decoder")

// decodeWireAd is decodeWire returning the public ClassAd wrapper, for callers that need the
// object rather than the AST (see Txn.putWire).
func (c *Collection) decodeWireAd(w []byte) (*classad.ClassAd, error) {
	a, err := c.decodeWire(w)
	if err != nil {
		return nil, err
	}
	return classad.FromAST(a), nil
}

// hasOrdered reports whether the collection maintains any ordered index. Callers use it to
// skip preparing input that only ordered-index maintenance consumes.
func (c *Collection) hasOrdered() bool { return len(c.ordered) > 0 }
