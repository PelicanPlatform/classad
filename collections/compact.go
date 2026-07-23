package collections

import "time"

// Compaction reclaims space consumed by superseded/deleted records.
//
// It is driven by the per-shard dead-byte ratio (never by age: ClassAds are
// rewritten by daemons every few seconds to minutes, so a long-lived ad can be
// the hottest one). When a shard's dead fraction crosses compactThreshold, its
// live (current) records are copied forward into fresh segments and the old
// segments are retired.
//
// Compaction is *concurrent*: the expensive work (walking records and
// recompressing them into new segments) runs WITHOUT the shard lock, so writers
// and scanners are not blocked while it runs. Only two brief critical sections
// take the lock — one to snapshot the source segments and seal the active
// segment, and one to finalize MVCC stamps, rebuild the directory, and swap the
// segments in.
//
// Correctness rests on the MVCC design:
//   - After sealing (sh.act=nil), concurrent writes go to fresh "post-barrier"
//     segments, never the source segments, so the lock-free copy reads immutable
//     bytes (superseded flags are read atomically).
//   - A source record superseded during the copy has its destination copy marked
//     superseded in the final critical section, so a scan never sees both the
//     stale copy and the post-barrier version (no duplicates).
//   - Scans hold their source-segment windows, so retired segments stay alive via
//     the GC until in-flight scans finish (no manual epochs).

const (
	// compactThreshold is the dead/used fraction at which a shard compacts.
	compactThreshold = 0.5
	// compactMinBytes avoids compacting tiny shards.
	compactMinBytes = 1 << 16
)

// Compact reclaims space from superseded/deleted records. For each shard it first
// unlinks any fully-dead segment cheaply (no rewrite), then, if the shard's dead-byte
// ratio still warrants it, recompresses the live records into fresh segments. It is
// safe to call concurrently with reads and writes. Returns the number of shards where
// space was reclaimed (by either mechanism).
func (c *Collection) Compact() int {
	c.maintMu.Lock()
	defer c.maintMu.Unlock()
	start := time.Now()
	defer func() { c.opm.compact.observe(time.Since(start)) }()
	target := c.currentCodec()
	n := 0
	for _, sh := range c.shards {
		// First drop any fully-dead segment cheaply (unlink, no rewrite). This reclaims a
		// segment whose records are all superseded without copying the shard's live data,
		// and by removing those dead bytes it can pull the shard back under the compaction
		// threshold -- so concentrated garbage (e.g. a batch of keys all deleted) costs an
		// unlink, and only genuinely fragmented garbage triggers the full rewrite below.
		reclaimed := c.reclaimDeadShard(sh) > 0
		sh.mu.RLock()
		do := sh.shouldCompact()
		sh.mu.RUnlock()
		if do {
			c.compactShard(sh, target)
			n++
		} else if reclaimed {
			n++ // space was freed by unlinking dead segments, though no rewrite was needed
		}
	}
	if n > 0 {
		c.reindexAfterCompaction()
	}
	return n
}

// reclaimDeadShard unlinks every sealed (non-active) segment whose records are all
// superseded (seg.dead >= seg.used), reclaiming its file/mmap/VMA/sidecar without the
// full-shard rewrite compactShard performs. Returns the number of segments reclaimed.
//
// A fully-dead segment holds no current record, so nothing in the directory or the sealed
// probe points at it as live. But its superseded records may still sit MID-CHAIN in a
// shared hash bucket -- recNext chains are per-bucket, not per-key (shard.put links a new
// record to the previous bucket head), so a colliding key's live record can live deeper in
// the chain. Each dead record is therefore spliced out of its bucket chain before the
// segment is dropped, so no surviving recNext dangles into the reaped mapping. The pageable
// probe (forEachSealedRecord) locates records by key index, never following recNext, so
// evicted buckets (absent from the directory) have no chain to repair.
//
// As with compaction, dropping superseded records raises the GC floor so an older snapshot
// that can no longer find a key conservatively conflicts (see conflictSince), and the reap
// is pin-aware so an in-flight scan keeps the mapping mapped until it unpins.
func (c *Collection) reclaimDeadShard(sh *shard) int {
	sh.mu.Lock()
	retain := sh.retainFloorLocked()
	var dead []*segment
	for _, seg := range sh.segs {
		if seg == nil || seg == sh.act || seg.used == 0 {
			continue
		}
		// A segment is fully reclaimable when it holds no current record and every
		// version in it was superseded at or below the retain floor (so no AS OF query
		// within the window needs it). This is marker-aware, unlike a dead>=used byte
		// check: a history segment carrying checkpoint markers still reclaims once its
		// data has aged out. With time travel off, retain == commitSeq, so any
		// all-superseded segment qualifies -- exactly the previous behavior.
		if seg.dead >= int64(seg.used) && seg.maxSup <= retain {
			dead = append(dead, seg)
		}
	}
	if len(dead) == 0 {
		sh.mu.Unlock()
		return 0
	}
	deadSet := make(map[uint32]struct{}, len(dead))
	for _, seg := range dead {
		deadSet[seg.id] = struct{}{}
	}
	// Splice every dead-segment record out of its (shared) hash-bucket chain. Only buckets
	// present in the directory are ever walked by recNext, so repair just those, once each.
	repaired := make(map[uint64]struct{})
	for _, seg := range dead {
		for off := 0; off < seg.used; {
			o := uint32(off)
			total := recTotalLen(seg.data, o)
			if total == 0 {
				break
			}
			h := c.h.Hash(recKey(seg.data, o))
			if _, done := repaired[h]; !done {
				repaired[h] = struct{}{}
				if _, ok := sh.dir[h]; ok {
					sh.spliceDeadFromChain(h, deadSet)
				}
			}
			off += int(total)
		}
	}
	// Remove the dead segments from the live set and the pending-sync lists.
	var toReap []*segment
	for i, seg := range sh.segs {
		if seg == nil {
			continue
		}
		if _, isDead := deadSet[seg.id]; isDead {
			if seg.retire() {
				toReap = append(toReap, seg)
			}
			sh.segs[i] = nil
		}
	}
	sh.dropDirtySegs(deadSet)
	// Superseded records at or below the retain floor were dropped: raise the GC floor
	// to it so a snapshot older than the floor conservatively conflicts rather than
	// trusting a truncated chain. With time travel off, retain == commitSeq (as before).
	if retain > sh.gcFloor {
		sh.gcFloor = retain
	}
	sh.tseq.trim(retain)
	sh.mu.Unlock()

	for _, seg := range toReap {
		_ = seg.reapAndHook()
	}
	return len(dead)
}

// Rewrite re-encodes every live ad with the current hot set (and match closure)
// so a changed hot set takes effect on existing ads, not just future writes,
// then force-compacts every shard to reclaim the superseded pre-rewrite records.
// Returns the number of ads rewritten.
//
// It re-Puts ads on the normal write path, so it is a maintenance operation: an
// update to a key that races the rewrite may be overwritten by the pre-rewrite
// value. Run it during low write activity (or, in an HA deployment, on the sole
// writer).
func (c *Collection) Rewrite() int {
	c.maintMu.Lock()
	defer c.maintMu.Unlock()
	n := 0
	for _, k := range c.Keys() {
		kb := []byte(k)
		ad, ok := c.Get(kb)
		if !ok {
			continue
		}
		if c.Put(kb, ad) == nil {
			n++
		}
	}
	target := c.currentCodec()
	for _, sh := range c.shards {
		c.compactShard(sh, target)
	}
	c.reindexAfterCompaction()
	return n
}

// RetrainDict samples the live ads, trains a fresh ZSTD dictionary from them,
// switches new writes to a codec using that dictionary, and recompacts every
// shard so existing records are recompressed under the new dictionary. In-flight
// scans are unaffected: they decode retired segments with the codec those
// segments were written under (recorded per segment). Returns the dictionary size.
// retrainStallHook, when non-nil, is invoked once mid-retrain (after the codec swap,
// before recompaction) while holding NO collection lock. It is an unexported test seam
// (see retrain_stall_test.go) for observing how a long-running retrain affects concurrent
// transactions; production leaves it nil.
var retrainStallHook func()

func (c *Collection) RetrainDict(sampleMax int) (int, error) {
	c.maintMu.Lock()
	defer c.maintMu.Unlock()
	start := time.Now()
	defer func() { c.opm.retrain.observe(time.Since(start)) }()
	dict, err := TrainDict(c.CollectSamples(sampleMax))
	if err != nil {
		return 0, err
	}
	codec, err := NewZSTDCodec(dict)
	if err != nil {
		return 0, err
	}
	// Register the dictionary so segments can be tagged with its id and, for a
	// persistent collection, its bytes are written durably before any segment
	// references it (recovery reconstructs the codec from them).
	if _, err := c.dicts.register(codec, dict); err != nil {
		return 0, err
	}
	c.codec.Store(&codecHolder{codec}) // new writes use the new codec
	if retrainStallHook != nil {
		retrainStallHook() // test seam: observe concurrent access during a long retrain
	}
	for _, sh := range c.shards {
		c.compactShard(sh, codec) // recompress to the new codec
	}
	c.lastDictBytes.Store(int64(len(dict)))
	c.lastRetrainUnix.Store(time.Now().UnixNano())
	c.reindexAfterCompaction() // rebuild indexes over the recompacted segments
	// The recompaction re-encoded (or left in place) every live segment; dictionaries no
	// segment references anymore are dead weight -- drop them so the registry does not
	// grow by one inflated codec per retrain for the life of the process.
	c.pruneDicts()
	return len(dict), nil
}

// pruneDicts drops registered dictionaries that no live segment references (keeping the
// current write codec), unlinking their on-disk .zst files. Called after a retrain's
// recompaction (which retires old-codec segments en masse) and at Open (loadDicts loads
// the full on-disk history; recovery then references only the ids live segments carry).
func (c *Collection) pruneDicts() {
	live := map[Codec]bool{c.currentCodec(): true}
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil {
				live[seg.codec] = true
			}
		}
		sh.mu.RUnlock()
	}
	c.dicts.prune(func(cd Codec) bool { return live[cd] })
}

// reindexAfterCompaction rebuilds the segment indexes after compaction replaced the
// segments (their fresh copies carry no index), so queries stay pruned and estimates
// stay populated instead of silently falling back to a full scan until the next Reindex.
func (c *Collection) reindexAfterCompaction() {
	// Reindex rebuilds the attribute indexes and, for a persistent collection, seals each
	// compacted segment's key sidecar and evicts its keys from the directory (phase 3), so
	// compaction re-bounds the resident directory to O(active-segment) instead of leaving
	// the full directory it just rebuilt. Run it whenever either applies.
	if c.spec.Load().any() || c.dir != "" {
		c.Reindex()
	}
}

// shouldCompact reports whether the shard's garbage ratio warrants compaction.
// Caller holds at least the read lock.
func (sh *shard) shouldCompact() bool {
	var used, dead int64
	for _, seg := range sh.segs {
		if seg == nil {
			continue
		}
		// Exclude pure-history segments (no current record) from the trigger: their
		// dead bytes are retained time-travel history, not reclaimable garbage, so they
		// must not drive compaction of the live working set (which would recopy history
		// every pass). With time travel off there are none (reclaimDeadShard drops them
		// first), so this is a no-op and the ratio is over the whole shard, as before.
		if seg.dead >= int64(seg.used) {
			continue
		}
		used += int64(seg.used)
		dead += seg.dead
	}
	return used >= compactMinBytes && float64(dead) >= compactThreshold*float64(used)
}

// movedRec records a live source record copied to a destination segment during
// the lock-free phase, so the final critical section can finalize its superseded
// stamp and place it in the rebuilt directory.
type movedRec struct {
	srcSeg *segment
	srcOff uint32
	dstSeg *segment
	dstOff uint32
	key    []byte
	hash   uint64
}

// compactShard performs concurrent compaction of one shard (see the package
// comment). target is the codec destination records are (re)compressed into.
//
// With time travel enabled, compaction is retention- and segregation-aware. It only
// rewrites the working-set segments (those with a current record); pure-history
// segments (all versions superseded, none current) are left untouched so they are not
// recopied on every pass (they are dropped by reclaimDeadShard once they age past the
// retain floor). From each working-set segment it splits records into two destination
// streams: CURRENT versions go to fresh live segments (rebuilt into the directory), and
// superseded versions still newer than the retain floor go to separate HISTORY segments
// (no current record, so scans skip them at the current time but read them for an AS OF
// query). Versions superseded at or below the retain floor -- and checkpoint markers
// that old -- are dropped. With time travel off the retain floor is the current commit
// sequence, so no version is retained and this behaves exactly as before.
func (c *Collection) compactShard(sh *shard, target Codec) {
	// Phase 1 (lock): choose the working-set sources (segments with a current record),
	// capture the retain floor, and seal the active segment so concurrent writes land
	// in fresh post-barrier segments. Pure-history segments are not sources.
	sh.mu.Lock()
	retain := sh.retainFloorLocked()
	origLen := len(sh.segs)
	sources := make([]*segment, 0, origLen)
	sourceSet := make(map[*segment]struct{}, origLen)
	for _, seg := range sh.segs {
		if seg == nil || seg.used == 0 {
			continue
		}
		if seg.dead >= int64(seg.used) && seg.maxSup > retain {
			continue // in-window history segment: leave it in place, don't recopy
		}
		sources = append(sources, seg)
		sourceSet[seg] = struct{}{}
	}
	sh.act = nil
	sh.mu.Unlock()

	// Phase 2 (lock-free): copy into private destination segments -- current versions to
	// the live stream, retained superseded versions to the history stream. Reads of
	// source records are safe: bytes are immutable and the superseded flag is atomic.
	var dstSegs, histSegs []*segment
	var cur, hcur *segment
	newDst := func(streamSegs *[]*segment, minSize int, codec Codec) *segment {
		size := sh.segSize
		if minSize > size {
			size = minSize
		}
		if sh.alloc != nil {
			if seg, err := sh.alloc(0, size, codec); err == nil {
				*streamSegs = append(*streamSegs, seg)
				return seg
			}
			// On allocation error, fall back to a RAM segment (best-effort).
		}
		s := newSegment(0, size, codec)
		s.pinReap = sh.sealRAM
		*streamSegs = append(*streamSegs, s)
		return s
	}
	var moved []movedRec
	// Scratch buffers reused across every record's decompress/recompress.
	var decBuf, encBuf []byte
	recompress := func(seg *segment, ad []byte) ([]byte, Codec) {
		if seg.codec == target {
			return ad, seg.codec
		}
		if w, err := seg.codec.Decompress(decBuf[:0], ad); err == nil {
			decBuf = w
			out := target.Compress(encBuf[:0], w)
			encBuf = out
			return out, target
		}
		return ad, seg.codec
	}
	for _, seg := range sources {
		for off := 0; off < seg.used; {
			o := uint32(off)
			total := recTotalLen(seg.data, o)
			if total == 0 {
				break
			}
			seq := recSeq(seg.data, o)
			if recIsMarker(seg.data, o) {
				if seq > retain { // carry an in-window checkpoint forward (to history)
					if hcur == nil || hcur.used+recordLen(0, 8) > len(hcur.data) {
						hcur = newDst(&histSegs, recordLen(0, 8), target)
					}
					hcur.appendMarker(seq, recMarkerMillis(seg.data, o))
				}
				off += int(total)
				continue
			}
			sup := recSuperseded(seg.data, o)
			if sup == seqMax {
				// Current version -> live stream (rebuilt into the directory).
				key := recKey(seg.data, o)
				outAd, outCodec := recompress(seg, recAd(seg.data, o))
				rl := recordLen(len(key), len(outAd))
				if cur == nil || cur.codec != outCodec || cur.used+rl > len(cur.data) {
					cur = newDst(&dstSegs, rl, outCodec)
				}
				dstOff, _ := cur.append(seq, noLoc, key, outAd)
				moved = append(moved, movedRec{
					srcSeg: seg, srcOff: o,
					dstSeg: cur, dstOff: dstOff,
					key:  append([]byte(nil), key...),
					hash: c.h.Hash(key),
				})
			} else if sup > retain {
				// Superseded but still within the travel window -> history stream,
				// preserving its original supersededBySeq (supersedeRec keeps the
				// segment's live/dead counters right: appended live, then retired).
				key := recKey(seg.data, o)
				outAd, outCodec := recompress(seg, recAd(seg.data, o))
				rl := recordLen(len(key), len(outAd))
				if hcur == nil || hcur.codec != outCodec || hcur.used+rl > len(hcur.data) {
					hcur = newDst(&histSegs, rl, outCodec)
				}
				dstOff, _ := hcur.append(seq, noLoc, key, outAd)
				hcur.supersedeRec(dstOff, sup)
			}
			// else: superseded at or below the retain floor -> reclaimed (dropped).
			off += int(total)
		}
	}

	// Make destination records durable BEFORE any source is retired (crash safety).
	for _, seg := range dstSegs {
		_ = seg.flush()
	}
	for _, seg := range histSegs {
		_ = seg.flush()
	}

	// Phase 3 (lock): finalize, rebuild the directory, and install.
	sh.mu.Lock()
	allDst := append(append([]*segment{}, dstSegs...), histSegs...)
	baseID := uint32(len(sh.segs))
	for i, s := range allDst {
		s.id = baseID + uint32(i)
	}
	// Transfer any supersession that happened during Phase 2 onto the destination live
	// copies, so a stale copy is not seen as current (supersedeRec keeps counters right).
	for i := range moved {
		if sup := recSuperseded(moved[i].srcSeg.data, moved[i].srcOff); sup != seqMax {
			moved[i].dstSeg.supersedeRec(moved[i].dstOff, sup)
		}
	}
	// Rebuild the directory from every current record: destination live copies still
	// current, plus current records in post-barrier segments (written during the copy).
	newDir := make(map[uint64]loc, sh.count)
	count := 0
	for i := range moved {
		m := &moved[i]
		if recSuperseded(m.dstSeg.data, m.dstOff) != seqMax {
			continue // stale copy of a key updated during Phase 2
		}
		setRecNext(m.dstSeg.data, m.dstOff, dirGetOr(newDir, m.hash))
		newDir[m.hash] = loc{seg: m.dstSeg.id, off: m.dstOff}
		count++
	}
	for id := origLen; id < int(baseID); id++ { // post-barrier segments
		seg := sh.segs[id]
		if seg == nil {
			continue
		}
		for off := 0; off < seg.used; {
			o := uint32(off)
			total := recTotalLen(seg.data, o)
			if total == 0 {
				break
			}
			if !recIsMarker(seg.data, o) && recSuperseded(seg.data, o) == seqMax {
				h := c.h.Hash(recKey(seg.data, o))
				setRecNext(seg.data, o, dirGetOr(newDir, h))
				newDir[h] = loc{seg: seg.id, off: o}
				count++
			}
			off += int(total)
		}
	}

	sh.segs = append(sh.segs, allDst...)
	// Retire only the source (working-set) segments; pure-history segments left in
	// place above are kept. RAM segments are dropped (GC frees them once scans release
	// their windows); mmap segments munmap+unlink once unpinned.
	var toReap []*segment
	retired := make(map[*segment]struct{})
	for i := 0; i < origLen; i++ {
		seg := sh.segs[i]
		if seg == nil {
			continue
		}
		if _, isSrc := sourceSet[seg]; !isSrc {
			continue // pure-history segment kept in place
		}
		retired[seg] = struct{}{}
		if seg.retire() {
			toReap = append(toReap, seg)
		}
		sh.segs[i] = nil
	}
	// Drop retired segments from the pending-sync lists: their live records were copied
	// forward and the segments are being unlinked, so a later sync must not msync one (a
	// segment concurrently being reaped). An in-flight sync that already captured one pins
	// it (see shard.sync), which deferred that reap above.
	if len(retired) > 0 {
		if kept := sh.dirty[:0]; len(sh.dirty) > 0 {
			for _, s := range sh.dirty {
				if _, gone := retired[s]; !gone {
					kept = append(kept, s)
				}
			}
			sh.dirty = kept
		}
		if kept := sh.dirtySup[:0]; len(sh.dirtySup) > 0 {
			for _, s := range sh.dirtySup {
				if _, gone := retired[s.seg]; !gone {
					kept = append(kept, s)
				}
			}
			sh.dirtySup = kept
		}
	}
	sh.dir = newDir
	sh.count = count
	if len(dstSegs) > 0 {
		sh.act = dstSegs[len(dstSegs)-1]
	}
	// Superseded/delete evidence at or below the retain floor was just dropped; raise
	// the transaction GC floor to it so a snapshot older than the floor can no longer
	// trust a bucket-chain walk for a currently-absent key (see conflictSince). With
	// time travel off the floor is the current commit sequence, exactly as before.
	if retain > sh.gcFloor {
		sh.gcFloor = retain
	}
	// Checkpoints for versions that just aged out of the window are no longer needed.
	sh.tseq.trim(retain)
	sh.mu.Unlock()

	for _, seg := range toReap {
		// reapAndHook (not bare reap) so a sealed segment's mmap sidecar index is unmapped
		// with its data (onReap); a no-op for segments without a sidecar. toReap holds only
		// unpinned segments, so unmapping now is safe.
		_ = seg.reapAndHook()
	}
}

// dirGetOr returns the directory head for hash h, or noLoc.
func dirGetOr(dir map[uint64]loc, h uint64) loc {
	if l, ok := dir[h]; ok {
		return l
	}
	return noLoc
}
