package collections

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
)

// Crash recovery and supersession stamps.
//
// A commit that overwrites a key appends the new record to the write target and stamps the old
// record's supersededBySeq in place. Both are writes to a shared mapping, and the kernel writes
// dirty pages back in any order: until the commit's msync completes -- and for a nondurable
// commit, until some later sync -- the stamp can reach the disk while the new record does not. A
// recovery that believed the stamp found the key's old version superseded and no newer one, and
// read the key as deleted. A delete is the same stamp with no record behind it at all.
//
// Recovery therefore decides, for each stamp it finds, whether the commit that wrote it survived,
// and undoes the stamp when it did not (supersededBySeq back to "current"). Three facts make that
// decidable from what is on disk:
//
//   - Every commit leaves a record at its sequence in the shard's write target: an overwrite or
//     insert its new record, a delete a delete marker naming the key (writeDeleteMarker). A
//     commit made of nothing but deletes is no longer invisible in the record stream.
//   - Within a segment, records are appended in commit order, and recovery loads a segment only up
//     to its first torn or unwritten record. A write target is msynced whole before the next one
//     is first written to (shard.syncOutgoingLocked), so a crash can only take records off the end
//     of the last one. Every commit below the newest surviving record's sequence (lastSeq)
//     therefore survived whole; a commit above it left nothing; the commit AT it may be partial,
//     so its stamps are checked key by key against the records it left.
//   - A durable horizon per shard (supersede.horizon, in the collection directory) names a
//     sequence every commit at or below which is durable, records and delete stamps alike. Stamps at or below it are believed outright. It is
//     raised, after a sync, before anything rewrites or drops segments (compaction, reclaim,
//     columnarizing, seal migration), because those can remove the newest records a lastSeq
//     would otherwise be read from, and at every Open. A store with no horizon file was last
//     written by a build that wrote no delete markers; its recovery believes every stamp, as
//     before.
//
// A delete marker whose stamp did not reach the disk is replayed (the key's surviving version is
// stamped at the marker's sequence). That makes the marker, not the stamp, what makes a delete
// durable: a sync pass flushes a delete's record range and leaves its stamp for later
// (deferSupFlushLocked), so a durable delete costs one msync, as it did before it had a marker. The
// stamps are flushed before anything relies on them being on disk -- before a horizon covering them
// is written (a stamp at or below the horizon is believed and its marker is no longer read), and at
// Close, before the directory snapshot whose reopen reads no markers.
//
// What this does not provide is atomicity across keys for a commit that was not yet durable: each
// key of such a commit comes back either before or after it, independently, and in a shard's
// partial last commit some may be applied and others not. A durable commit (one whose sync
// returned) is recovered whole.

const (
	horizonName    = "supersede.horizon"
	horizonMagic   = 0x5a5a4f48 // "HOZZ"
	horizonVersion = 1
	horizonUnknown = seqMax // a shard with no horizon: recovery believes every stamp
)

// readHorizons reads the collection's horizon file, one value per shard. A shard whose value is
// horizonUnknown, or every shard when the file is absent, does not verify, or was written for a
// different shard count, has none: recovery then believes every stamp it finds, which is what
// every earlier build did.
func readHorizons(dir string, shards int) []uint64 {
	out := make([]uint64, shards)
	for i := range out {
		out[i] = horizonUnknown
	}
	b, err := os.ReadFile(filepath.Join(dir, horizonName))
	if err != nil || len(b) != 16+8*shards {
		return out
	}
	body := b[:len(b)-4]
	if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(b[len(b)-4:]) ||
		binary.LittleEndian.Uint32(b[0:]) != horizonMagic || binary.LittleEndian.Uint32(b[4:]) != horizonVersion ||
		int(binary.LittleEndian.Uint32(b[8:])) != shards {
		return out
	}
	for i := range out {
		out[i] = binary.LittleEndian.Uint64(b[12+8*i:])
	}
	return out
}

// writeHorizons replaces the horizon file: written to a temporary name, synced, and renamed into
// place. The directory is not synced: a crash that loses the rename leaves the previous file,
// whose horizons are lower, which only makes recovery check more stamps.
func writeHorizons(dir string, hs []uint64) error {
	path := filepath.Join(dir, horizonName)
	if writeFaultHook != nil {
		if err := writeFaultHook(path); err != nil {
			return err
		}
	}
	b := make([]byte, 0, 16+8*len(hs))
	b = binary.LittleEndian.AppendUint32(b, horizonMagic)
	b = binary.LittleEndian.AppendUint32(b, horizonVersion)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(hs)))
	for _, h := range hs {
		b = binary.LittleEndian.AppendUint64(b, h)
	}
	b = binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// writeHorizonsLocked writes every shard's current horizon. Caller holds c.hzMu.
func (c *Collection) writeHorizonsLocked() error {
	hs := make([]uint64, len(c.shards))
	for i, sh := range c.shards {
		hs[i] = sh.horizon.Load()
	}
	return writeHorizons(c.dir, hs)
}

// raiseHorizon makes every commit at or below need durable and records need as the shard's
// horizon, unless it already covers need. It returns false when that could not be done -- the
// sync or the file write failed -- and the caller must then not drop or rewrite any record above
// the horizon it had. A no-op returning true for a shard that keeps no stamps (in-memory,
// append-only). Must not be called with the shard lock held: the sync takes it.
func (c *Collection) raiseHorizon(sh *shard, need uint64) bool {
	if sh.appendOnly || sh.segDir == "" {
		return true
	}
	sh.hzMu.Lock() // one raise per shard at a time: the sync must precede the value it justifies
	defer sh.hzMu.Unlock()
	if h := sh.horizon.Load(); h != horizonUnknown && h >= need {
		return true
	}
	sh.syncFor(need)
	if err := sh.flushDeferredSups(); err != nil && isDurabilityFailure(err) {
		sh.syncErr.CompareAndSwap(nil, &syncFail{err})
	}
	if sh.syncErr.Load() != nil {
		return false
	}
	c.hzMu.Lock()
	defer c.hzMu.Unlock()
	prev := sh.horizon.Load()
	sh.horizon.Store(need)
	if err := c.writeHorizonsLocked(); err != nil {
		sh.horizon.Store(prev)
		return false
	}
	return true
}

// persistOpenHorizons makes a freshly opened mutable store's recovered state durable and records
// each shard's commit sequence as its horizon. Everything recovery saw is in the files, but after
// a process crash much of it may still be only in the page cache, and recovery itself rewrites
// stamps; both are flushed before the horizon says they are durable.
func (c *Collection) persistOpenHorizons() error {
	if c.appendOnly() {
		return nil
	}
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg != nil {
				if err := seg.msyncRange(0, seg.used); err != nil && isDurabilityFailure(err) {
					return err
				}
				seg.synced = seg.used
			}
		}
		sh.syncedSeq = sh.commitSeq
	}
	c.hzMu.Lock()
	defer c.hzMu.Unlock()
	prev := make([]uint64, len(c.shards))
	for i, sh := range c.shards {
		prev[i] = sh.horizon.Load()
		sh.horizon.Store(sh.commitSeq)
	}
	// Best effort: a store whose horizon cannot be written still opens. Its next recovery works
	// from the horizon it had (or none), and every rewrite first raises it, refusing to run if
	// that fails.
	if c.writeHorizonsLocked() != nil {
		for i, sh := range c.shards {
			sh.horizon.Store(prev[i])
		}
	}
	return nil
}

// maxDeferredSups bounds the delete stamps a shard leaves unflushed between horizons, so the list
// (and the flush that eventually empties it) stays small on a store that deletes a lot and rarely
// runs maintenance.
const maxDeferredSups = 4096

// deferSupFlushLocked reports whether a sync pass may leave the shard's pending delete stamps
// unflushed: their delete markers are in the record ranges the pass does flush, and recovery
// replays a marker whose stamp is missing. Not when the shard has no horizon -- its recovery
// believes stamps and reads no markers -- nor once enough have piled up. Caller holds sh.mu.
func (sh *shard) deferSupFlushLocked() bool {
	return sh.horizon.Load() != horizonUnknown && len(sh.dirtySup) < maxDeferredSups
}

// flushDeferredSups flushes the shard's pending delete stamps. Must not be called with the shard
// lock held.
func (sh *shard) flushDeferredSups() error {
	sh.mu.Lock()
	sup := sh.dirtySup
	sh.dirtySup = nil
	for _, s := range sup {
		s.seg.pin() // as syncPass does: a concurrent compaction may retire the segment
	}
	sh.mu.Unlock()
	err := msyncSups(sup)
	for _, s := range sup {
		s.seg.unpin()
	}
	return err
}

// msyncSups flushes the supersession fields named, one msync per segment over the span they
// occupy rather than one per field.
func msyncSups(sup []supRef) error {
	type span struct{ from, to int }
	spans := map[*segment]span{}
	for _, s := range sup {
		off := int(s.off) + recSupOff
		sp, ok := spans[s.seg]
		if !ok {
			sp = span{off, off + 8}
		}
		sp.from, sp.to = min(sp.from, off), max(sp.to, off+8)
		spans[s.seg] = sp
	}
	var first error
	for seg, sp := range spans {
		if err := seg.msyncRange(sp.from, sp.to); err != nil && first == nil {
			first = err
		}
	}
	return first
}
