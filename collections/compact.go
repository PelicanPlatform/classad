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

// Compact compacts every shard whose dead-byte ratio warrants it, recompressing
// records into the collection's current codec. It is safe to call concurrently
// with reads and writes. Returns the number of shards compacted.
func (c *Collection) Compact() int {
	target := c.currentCodec()
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		do := sh.shouldCompact()
		sh.mu.RUnlock()
		if do {
			c.compactShard(sh, target)
			n++
		}
	}
	if n > 0 {
		c.reindexAfterCompaction()
	}
	return n
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
func (c *Collection) RetrainDict(sampleMax int) (int, error) {
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
	for _, sh := range c.shards {
		c.compactShard(sh, codec) // recompress to the new codec
	}
	c.lastDictBytes.Store(int64(len(dict)))
	c.lastRetrainUnix.Store(time.Now().UnixNano())
	c.reindexAfterCompaction() // rebuild indexes over the recompacted segments
	return len(dict), nil
}

// reindexAfterCompaction rebuilds the segment indexes after compaction replaced the
// segments (their fresh copies carry no index), so queries stay pruned and estimates
// stay populated instead of silently falling back to a full scan until the next Reindex.
func (c *Collection) reindexAfterCompaction() {
	if c.spec.Load().any() {
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
func (c *Collection) compactShard(sh *shard, target Codec) {
	// Phase 1 (lock): snapshot the source segments and seal the active segment so
	// concurrent writes land in fresh post-barrier segments.
	sh.mu.Lock()
	srcSegs := make([]*segment, len(sh.segs))
	copy(srcSegs, sh.segs)
	srcCount := len(srcSegs)
	sh.act = nil
	sh.mu.Unlock()

	// Phase 2 (lock-free): recompress current records into private destination
	// segments. Reads of source records are safe: bytes are immutable and the
	// superseded flag is read atomically.
	var dstSegs []*segment
	var cur *segment
	alloc := func(minSize int, codec Codec) {
		size := sh.segSize
		if minSize > size {
			size = minSize
		}
		// Persistent shards allocate mmap segments so compacted data is durable;
		// real id is assigned at install (file name is id-independent).
		if sh.alloc != nil {
			if seg, err := sh.alloc(0, size, codec); err == nil {
				cur = seg
				dstSegs = append(dstSegs, cur)
				return
			}
			// On allocation error, fall back to a RAM segment (best-effort; P4 makes
			// compaction allocation failure durable/abortable).
		}
		cur = newSegment(0, size, codec)
		cur.pinReap = sh.sealRAM // keep the compacted RAM segment pin/reap-eligible for anon sealing
		dstSegs = append(dstSegs, cur)
	}
	var moved []movedRec
	// Scratch buffers reused across every record's decompress/recompress: append
	// copies the record into the destination segment, so these are transient. This
	// avoids two fresh allocations per record (heavy GC churn at recompaction).
	var decBuf, encBuf []byte
	for _, seg := range srcSegs {
		if seg == nil {
			continue
		}
		for off := 0; off < seg.used; {
			o := uint32(off)
			total := recTotalLen(seg.data, o)
			if total == 0 {
				break
			}
			if recSuperseded(seg.data, o) == seqMax {
				key := recKey(seg.data, o)
				ad := recAd(seg.data, o)
				seq := recSeq(seg.data, o)
				outAd, outCodec := ad, seg.codec
				if seg.codec != target {
					if w, err := seg.codec.Decompress(decBuf[:0], ad); err == nil {
						decBuf = w
						outAd = target.Compress(encBuf[:0], w)
						encBuf = outAd
						outCodec = target
					}
				}
				rl := recordLen(len(key), len(outAd))
				if cur == nil || cur.codec != outCodec || cur.used+rl > len(cur.data) {
					alloc(rl, outCodec)
				}
				dstOff, _ := cur.append(seq, noLoc, key, outAd)
				moved = append(moved, movedRec{
					srcSeg: seg, srcOff: o,
					dstSeg: cur, dstOff: dstOff,
					key:  append([]byte(nil), key...),
					hash: c.h.Hash(key),
				})
			}
			off += int(total)
		}
	}

	// Make the compacted (destination) records durable BEFORE any source segment is
	// retired/unlinked, so a crash cannot lose them (the source still holds a copy
	// until it is reaped, and recovery dedups if both survive). No-op for RAM.
	for _, seg := range dstSegs {
		_ = seg.flush()
	}

	// Phase 3 (lock): finalize, rebuild the directory, and install.
	sh.mu.Lock()
	baseID := uint32(len(sh.segs))
	for i, s := range dstSegs {
		s.id = baseID + uint32(i)
	}
	// Transfer any supersession that happened during Phase 2 onto the destination
	// copies, so a stale copy is not seen as current by a later scan.
	for i := range moved {
		if sup := recSuperseded(moved[i].srcSeg.data, moved[i].srcOff); sup != seqMax {
			setRecSuperseded(moved[i].dstSeg.data, moved[i].dstOff, sup)
		}
	}
	// Rebuild the directory from every current record: destination copies still
	// current, plus current records in post-barrier segments (written during the
	// copy). Chains are rebuilt fresh, so no entry references a retired segment.
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
	for id := srcCount; id < int(baseID); id++ { // post-barrier segments
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
			if recSuperseded(seg.data, o) == seqMax {
				h := c.h.Hash(recKey(seg.data, o))
				setRecNext(seg.data, o, dirGetOr(newDir, h))
				newDir[h] = loc{seg: seg.id, off: o}
				count++
			}
			off += int(total)
		}
	}

	sh.segs = append(sh.segs, dstSegs...)
	// Retire the source segments. RAM segments are just dropped (the GC frees them
	// once in-flight scans release their windows). mmap segments are munmap'd +
	// unlinked, but only once no scan references them: retire() reaps immediately if
	// unpinned, else the last unpin reaps. Defer the actual reap (syscalls) until
	// after the lock is dropped.
	var toReap []*segment
	retired := make(map[*segment]struct{})
	for i := 0; i < srcCount; i++ {
		if seg := sh.segs[i]; seg != nil {
			retired[seg] = struct{}{}
			if seg.retire() {
				toReap = append(toReap, seg)
			}
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
	// Superseded records (delete evidence) at or below the current sequence were just
	// dropped; raise the transaction GC floor so a snapshot older than this can no
	// longer trust a bucket-chain walk for a currently-absent key (see conflictSince).
	if sh.commitSeq > sh.gcFloor {
		sh.gcFloor = sh.commitSeq
	}
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
