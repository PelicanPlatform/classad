package collections

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
	c.codec.Store(&codecHolder{codec}) // new writes use the new codec
	for _, sh := range c.shards {
		c.compactShard(sh, codec) // recompress to the new codec
	}
	return len(dict), nil
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
		dstSegs = append(dstSegs, cur)
	}
	var moved []movedRec
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
					if w, err := seg.codec.Decompress(nil, ad); err == nil {
						outAd, outCodec = target.Compress(nil, w), target
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
	for i := 0; i < srcCount; i++ {
		sh.segs[i] = nil // retire source segments (kept alive for scans via GC)
	}
	sh.dir = newDir
	sh.count = count
	if len(dstSegs) > 0 {
		sh.act = dstSegs[len(dstSegs)-1]
	}
	sh.mu.Unlock()
}

// dirGetOr returns the directory head for hash h, or noLoc.
func dirGetOr(dir map[uint64]loc, h uint64) loc {
	if l, ok := dir[h]; ok {
		return l
	}
	return noLoc
}
