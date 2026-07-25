package collections

// Append-only recompression. The mutable store recompresses (RetrainDict) and repacks
// (Rewrite) via compaction -- a live-copy that supersedes old versions, renumbers
// segments, and rebuilds the key directory. None of that is legal for an append log,
// where every record is permanently live and order is meaningful. Instead each sealed
// segment is *resealed in place*: its records are decoded, re-encoded with the current
// hot set, recompressed under a target codec, and written to a fresh same-id segment
// file, preserving append order (seq, key, and time markers). The new segment is swapped
// for the old under the shard lock; the old file is unlinked once in-flight scans drop
// their pins (the standard retire/reap path). Indexes are rebuilt afterward by Reindex.

// resealAppendOnly rebuilds every segment of an append-only collection under targetCodec
// and the current hot set, preserving append order, then rebuilds indexes. It is the
// shared engine for append-only RetrainDict (targetCodec = the new-dictionary codec) and
// Rewrite (targetCodec = the current codec, to apply a hot-set change or repack). The
// caller holds maintMu.
func (c *Collection) resealAppendOnly(targetCodec Codec) {
	for _, sh := range c.shards {
		// Retire the active segment from the write path so it becomes immutable and is
		// resealed too; the next write allocates a fresh active segment under the current
		// codec. Snapshot the segments to reseal under the same lock.
		sh.mu.Lock()
		var srcs []*segment
		for _, s := range sh.segs {
			if s != nil {
				srcs = append(srcs, s)
			}
		}
		sh.act = nil
		sh.mu.Unlock()

		var toReap []*segment
		for _, src := range srcs {
			newseg := c.resealOneSegment(sh, src, targetCodec)
			if newseg == nil {
				continue // reseal failed: leave the original in place (best-effort)
			}
			sh.mu.Lock()
			// Swap only if the source still occupies its slot (append-only segments are
			// not otherwise mutated, but stay defensive against a concurrent reseal).
			if int(src.id) < len(sh.segs) && sh.segs[src.id] == src {
				sh.segs[src.id] = newseg
				if src.retire() {
					toReap = append(toReap, src)
				}
			} else {
				// Slot moved under us: drop the freshly built segment's file rather than
				// leak it.
				newseg.retire()
				newseg.reapAndHook()
			}
			sh.mu.Unlock()
		}
		for _, seg := range toReap {
			seg.reapAndHook() // munmap + unlink the old segment file, off-lock
		}
	}
	// Rebuild the per-segment indexes (and seal sidecars) over the fresh segments, and
	// drop any dictionary no live segment references anymore.
	c.reindexAfterCompaction()
	c.pruneDicts()
}

// resealEntry is one record staged for reseal: a data record (its re-encoded, recompressed
// stored bytes and key) or a time-checkpoint marker (marker=true, millis set).
type resealEntry struct {
	seq    uint64
	key    []byte
	stored []byte
	marker bool
	millis uint64
}

// resealOneSegment builds a new segment holding src's records re-encoded with the current
// hot set and recompressed under targetCodec, preserving order. It returns the new segment
// (with the same logical id as src) or nil on any error. The source is immutable (sealed
// or retired from the write path), so it is read without the shard lock.
func (c *Collection) resealOneSegment(sh *shard, src *segment, targetCodec Codec) *segment {
	if src == nil || src.used == 0 {
		return nil
	}
	// Pass 1: decode + re-encode + recompress every record, computing the exact size the
	// new segment needs so it fits in a single segment (append order is 1:1).
	var entries []resealEntry
	total := 0
	for off := 0; off < src.used; {
		o := uint32(off)
		rl := recTotalLen(src.data, o)
		if rl == 0 {
			break
		}
		if recIsMarker(src.data, o) {
			seq := recSeq(src.data, o)
			millis := recMarkerMillis(src.data, o)
			entries = append(entries, resealEntry{seq: seq, marker: true, millis: millis})
			total += recordLen(0, 8)
			off += int(rl)
			continue
		}
		seq := recSeq(src.data, o)
		key := append([]byte(nil), recKey(src.data, o)...)
		wireBytes, err := src.codec.Decompress(nil, recAd(src.data, o))
		if err != nil {
			return nil
		}
		ad, err := c.decodeWire(wireBytes)
		if err != nil {
			return nil
		}
		stored := targetCodec.Compress(nil, c.encodeAd(ad))
		entries = append(entries, resealEntry{seq: seq, key: key, stored: stored})
		total += recordLen(len(key), len(stored))
		off += int(rl)
	}
	if total == 0 {
		return nil
	}

	// Allocate a new same-id segment sized to the recompressed data (rounded up by the
	// allocator). sh.alloc names the file by the codec's dictionary id; a nil alloc
	// (in-memory collection) yields a RAM segment.
	size := recAlign(total)
	var newseg *segment
	if sh.alloc == nil {
		newseg = newSegment(src.id, size, targetCodec)
		newseg.pinReap = sh.sealRAM
	} else {
		s, err := sh.alloc(src.id, size, targetCodec)
		if err != nil {
			return nil
		}
		newseg = s
	}

	// Pass 2: append the staged records in order.
	for _, e := range entries {
		if e.marker {
			if _, ok := newseg.appendMarker(e.seq, e.millis); !ok {
				return nil
			}
			continue
		}
		if _, ok := newseg.append(e.seq, noLoc, e.key, e.stored); !ok {
			return nil
		}
	}
	// Persistent segments: flush the written extent so recovery sees a complete segment.
	if newseg.persistent {
		newseg.synced = newseg.used
		if err := newseg.flush(); err != nil {
			return nil
		}
	}
	return newseg
}
