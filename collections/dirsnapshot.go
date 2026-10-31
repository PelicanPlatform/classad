package collections

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
)

// A directory snapshot is a per-shard fast-reopen optimization. On a clean Close the
// shard writes its directory (key-hash -> record location), commit sequence, and live
// count to <shardDir>/dir.snap; a subsequent Open restores the directory from that
// file instead of scanning every record to rebuild it (rebuildDir, which is O(DB)).
//
// The snapshot is a pure optimization and never the source of truth: it is validated
// exhaustively (CRC, plus the exact segment set it was taken over -- basename and
// durable length, in load order) and on ANY discrepancy the reopen falls back to the
// full scan, so a stale or corrupt snapshot can never yield incorrect state. It is
// also consume-once (removed as it is read), so it is trusted only for the one reopen
// immediately following the clean Close that wrote it; a crash mid-life leaves no
// snapshot and forces the scan.
//
// Correctness rests on one invariant: the snapshot exists only after a clean Close,
// and a clean Close flushes every segment (so all supersession flags are durable and
// no compaction is mid-flight). That is exactly the state rebuildDir's crash-dedup
// pass exists to repair, so a clean reopen can safely skip both the rebuild and the
// dedup.
//
// Version 2 also carries each segment's scan-pruning counters (minSeq, maxSeq, maxSup,
// dead), which rebuildDir otherwise derives by walking every record. Without them a
// restored segment knows none of its bounds: no scan skips it by sequence, and the
// garbage it held before the restart is invisible to the compaction trigger and to dead-
// segment reclaim until new garbage accumulates. A version 1 snapshot is still read; its
// segments' counters are then derived by a header-only walk (walkSegBounds).
const (
	dirSnapName      = "dir.snap"
	dirSnapMagic     = 0x53524944 // "DIRS"
	dirSnapVersionV1 = 1          // directory only
	dirSnapVersion   = 2          // + per-segment scan-pruning counters

	// dirSnapEntryLen is one serialized directory entry: hash u64, slot u32, offset u32.
	dirSnapEntryLen = 16
)

// dirSnapVersionKnown reports whether v is a snapshot version this build reads. The header
// (magic, version, commit sequence) is laid out identically in every version.
func dirSnapVersionKnown(v uint32) bool {
	return v == dirSnapVersionV1 || v == dirSnapVersion
}

// dirEntryValid reports whether the directory entry h -> (seg, off) names a record that can
// head h's bucket chain: a whole keyed record inside the segment's durable extent whose key
// hashes to h. A directory can hold entries that do not: a delete leaves its key's superseded
// record as the bucket head, and a segment rewrite that drops that record (columnarizing,
// resealing) moves every offset in the segment without evicting it, so the head names bytes
// that are no longer that record. Bucket-chain walks tolerate such a head (segForLoc), and a
// lookup that misses through it falls through to the sealed-segment probe -- every record
// still in that bucket lives in a sealed segment, because the head was the newest of them and
// its segment was sealed before it was rewritten.
func dirEntryValid(seg *segment, off uint32, h uint64, hasher Hasher) bool {
	if seg == nil || int(off)+recHeaderSize > seg.used || seg.damaged().covers(off) {
		return false
	}
	total := recTotalLen(seg.data, off)
	if total == 0 || total%8 != 0 || int(off)+int(total) > seg.used || recIsMarker(seg.data, off) {
		return false
	}
	if !recFits(seg.data[:int(off)+int(total)], off) {
		return false // key or body length runs past the record's own extent
	}
	return hasher.Hash(recKey(seg.data, off)) == h
}

// dirSnapEnt is one directory entry while it is being ordered: the key hash, the
// snapshot slot it is written under, and the segment array index + offset it names.
// Both the writer and the reader sort by (seg, off) so that validating the entries --
// which reads each record -- walks every segment forwards instead of at random.
type dirSnapEnt struct {
	hash uint64
	slot uint32
	seg  uint32
	off  uint32
}

// sortDirSnapEnts orders entries by the record they name, so a pass that reads those
// records runs in offset order within each segment. Sorting by slot rather than by
// array index is the same order -- slots are handed out in increasing array index --
// and it is the order the file is written in, which is what the reader sorts back to.
func sortDirSnapEnts(ents []dirSnapEnt) {
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].slot != ents[j].slot {
			return ents[i].slot < ents[j].slot
		}
		return ents[i].off < ents[j].off
	})
}

// dirSnapEntryBytes sorts a serialized entry array in place. It is sortDirSnapEnts over
// the on-disk form: each element is one dirSnapEntryLen-byte entry, ordered by the
// (slot, offset) it names, so a reader validating them walks each segment forwards.
type dirSnapEntryBytes []byte

func (e dirSnapEntryBytes) Len() int { return len(e) / dirSnapEntryLen }

func (e dirSnapEntryBytes) at(i int) []byte {
	return e[i*dirSnapEntryLen : (i+1)*dirSnapEntryLen]
}

func (e dirSnapEntryBytes) Less(i, j int) bool {
	a, b := e.at(i), e.at(j)
	as, bs := binary.LittleEndian.Uint32(a[8:]), binary.LittleEndian.Uint32(b[8:])
	if as != bs {
		return as < bs
	}
	return binary.LittleEndian.Uint32(a[12:]) < binary.LittleEndian.Uint32(b[12:])
}

func (e dirSnapEntryBytes) Swap(i, j int) {
	var tmp [dirSnapEntryLen]byte
	a, b := e.at(i), e.at(j)
	copy(tmp[:], a)
	copy(a, b)
	copy(b, tmp[:])
}

// sortDirSnapEntryBytes orders a serialized entry array in place; see dirSnapEntryBytes.
func sortDirSnapEntryBytes(b []byte) { sort.Sort(dirSnapEntryBytes(b)) }

// writeDirSnapshot persists shard sh's directory to <shardDir>/dir.snap. The caller
// holds sh.mu and must have flushed every segment durable first, so the snapshot
// never references not-yet-durable bytes. Best effort, so it reports nothing: a snapshot
// that is not written just means the next open falls back to a full scan.
//
// Directory entries that no longer name their bucket's record (dirEntryValid) are left
// out rather than written. The reopen checks every entry the same way and refuses the
// whole snapshot on one that fails, so writing a stale head made every clean reopen after
// a segment rewrite fall back to the full scan. Leaving it out restores exactly what a
// lookup through it finds today: nothing, then the sealed probe.
func writeDirSnapshot(sh *shard, shardDir string, hasher Hasher) {
	if writeFaultHook != nil {
		if err := writeFaultHook(filepath.Join(shardDir, dirSnapName)); err != nil {
			return
		}
	}
	// A segment still in the shard but already unmapped means the collection was closed before.
	// Its state is then not the store's: a later Open may have consumed the snapshot and written a
	// newer one, which this would replace. The directory checks below read the segments, too.
	for _, seg := range sh.segs {
		if seg != nil && seg.isReaped() {
			return
		}
	}
	// Segments are referenced by their file basename, which is stable across reopen;
	// array indices are not (compaction leaves nil holes and reopen reloads without
	// them). Assign each live (non-nil) segment a snapshot slot and record its
	// basename + durable length; directory entries then reference the slot.
	slotOf := make(map[uint32]uint32, len(sh.segs)) // array index -> snapshot slot
	buf := make([]byte, 0, 32+len(sh.segs)*56+len(sh.dir)*16)
	buf = binary.LittleEndian.AppendUint32(buf, dirSnapMagic)
	buf = binary.LittleEndian.AppendUint32(buf, dirSnapVersion)
	buf = binary.LittleEndian.AppendUint64(buf, sh.commitSeq)
	buf = binary.LittleEndian.AppendUint64(buf, uint64(sh.count))
	nSegOff := len(buf)
	buf = binary.LittleEndian.AppendUint32(buf, 0) // segment count, back-patched below
	var nSegs uint32
	for ai, seg := range sh.segs {
		if seg == nil {
			continue // a retired (compacted-away) slot
		}
		slotOf[uint32(ai)] = nSegs
		nSegs++
		name := filepath.Base(seg.path)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(seg.used))
		buf = binary.LittleEndian.AppendUint16(buf, uint16(len(name)))
		buf = append(buf, name...)
		// The counters as this process kept them. Every segment is flushed and no rewrite is in
		// flight, so they describe exactly the bytes the reopen maps (it checks the extent). They
		// are the values every scan-skip, compaction and reclaim decision of this process ran on;
		// the reopen resumes from them rather than from zero.
		//
		// Soundness: a skip drops records only if minSeq is too HIGH, maxSeq too LOW, or dead or
		// maxSup wrong in the direction that makes a segment look emptier or older than it is. The
		// runtime keeps all four exact -- every append and every supersession updates them under
		// the shard lock (appendFlagged, appendMarker, the keyless appends, supersedeRec,
		// appendRawRecordFlags) -- and a supersession only ever stamps a current record, so no
		// byte is counted dead twice. They therefore equal what rebuildDir's walk derives from the
		// same bytes (TestDirSnapshotReopenMatchesWalk), and nothing between this Close and the
		// next Open changes a record header without also changing a segment's extent or name,
		// which the reopen checks.
		buf = binary.LittleEndian.AppendUint64(buf, seg.minSeq)
		buf = binary.LittleEndian.AppendUint64(buf, seg.maxSeq)
		buf = binary.LittleEndian.AppendUint64(buf, seg.maxSup)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(seg.dead))
	}
	binary.LittleEndian.PutUint32(buf[nSegOff:], nSegs)
	nEntOff := len(buf)
	buf = binary.LittleEndian.AppendUint64(buf, 0) // entry count, back-patched below
	var nEntries uint64
	// Collect and order the entries by the record they name BEFORE validating any of
	// them, because dirEntryValid reads the record -- its header, and its key, which it
	// re-hashes. Ranging over sh.dir hands those out in map order, so validating as we
	// went touched one page per live key in random order, with nothing for readahead to
	// work with. On a large table that is the dominant cost of both Close and the reopen
	// that reads this file back, and it can exceed the full sequential walk the snapshot
	// exists to avoid -- invisible until a store outgrows the page cache.
	//
	// Sorted, each segment is read front to back instead. The order is also what the
	// reader sees, so the same win lands on the Open side; see loadDirSnapshot, which
	// sorts too rather than trusting the file to be ordered.
	ents := make([]dirSnapEnt, 0, len(sh.dir))
	for h, l := range sh.dir {
		slot, ok := slotOf[l.seg]
		if !ok {
			// A live directory entry pointing at a retired segment should never
			// happen on a clean shutdown; if it does, skip the snapshot entirely so
			// the reopen falls back to the authoritative scan.
			return
		}
		ents = append(ents, dirSnapEnt{hash: h, slot: slot, seg: l.seg, off: l.off})
	}
	sortDirSnapEnts(ents)
	for _, e := range ents {
		if !dirEntryValid(sh.segs[e.seg], e.off, e.hash, hasher) {
			continue
		}
		buf = binary.LittleEndian.AppendUint64(buf, e.hash)
		buf = binary.LittleEndian.AppendUint32(buf, e.slot)
		buf = binary.LittleEndian.AppendUint32(buf, e.off)
		nEntries++
	}
	binary.LittleEndian.PutUint64(buf[nEntOff:], nEntries)
	buf = binary.LittleEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))

	tmp := filepath.Join(shardDir, dirSnapName+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	_, werr := f.Write(buf)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, filepath.Join(shardDir, dirSnapName))
	}
	if werr != nil {
		os.Remove(tmp)
	}
}

// onDirSnapLoad, if set, is notified whether a shard's directory was restored from a
// snapshot (true) or fell back to a full rebuild (false). Test-only; nil in production.
var onDirSnapLoad func(loaded bool)

// tryLoadDirSnapshot restores shard sh's directory from a clean Close's dir.snap,
// returning whether it succeeded. On any error/mismatch it returns false (the caller
// runs rebuildDir). It always removes the file (consume-once). Caller holds sh.mu.
func (c *Collection) tryLoadDirSnapshot(sh *shard, shardDir string) bool {
	ok := c.loadDirSnapshot(sh, shardDir)
	if onDirSnapLoad != nil {
		onDirSnapLoad(ok)
	}
	return ok
}

func (c *Collection) loadDirSnapshot(sh *shard, shardDir string) bool {
	path := filepath.Join(shardDir, dirSnapName)
	data, err := os.ReadFile(path)
	if err != nil {
		return false // no snapshot: first open, or a prior crash
	}
	_ = os.Remove(path) // consume-once: trusted only for this immediate reopen

	// Verify the trailing CRC before trusting any length field (so a corrupt file
	// cannot drive a huge allocation or an out-of-bounds read).
	if len(data) < 4 {
		return false
	}
	body := data[:len(data)-4]
	if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(data[len(data)-4:]) {
		return false
	}

	// Map each loaded segment's basename to its array index (the reopen loads only
	// live files, in counter order, with no nil holes).
	byName := make(map[string]uint32, len(sh.segs))
	for ai, seg := range sh.segs {
		if seg != nil {
			byName[filepath.Base(seg.path)] = uint32(ai)
		}
	}

	r := snapReader{b: body}
	if r.u32() != dirSnapMagic {
		return false
	}
	version := r.u32()
	if !dirSnapVersionKnown(version) {
		return false
	}
	commitSeq := r.u64()
	count := int(r.u64())
	nSegs := int(r.u32())
	if r.err || nSegs != len(byName) {
		return false // the live segment set changed since the snapshot
	}
	// Resolve each snapshot slot to a loaded segment by basename, verifying its
	// durable length is unchanged.
	slotToArray := make([]uint32, nSegs)
	var bounds []segBounds // by array index; v2 only
	if version >= 2 {
		bounds = make([]segBounds, len(sh.segs))
	}
	for j := 0; j < nSegs; j++ {
		used := int(r.u64())
		name := r.str16()
		if r.err {
			return false
		}
		ai, ok := byName[name]
		if !ok || used != sh.segs[ai].used {
			return false
		}
		slotToArray[j] = ai
		if bounds != nil {
			b := segBounds{minSeq: r.u64(), maxSeq: r.u64(), maxSup: r.u64(), dead: int64(r.u64())}
			if r.err || !b.plausible(used, commitSeq) {
				return false
			}
			bounds[ai] = b
		}
	}
	nEntries := int(r.u64())
	if r.err {
		return false
	}
	entryStart := r.pos
	// Order the entries by the record they name before validating any of them, because
	// dirEntryValid reads that record -- its header, and its key, which it re-hashes.
	// Taken in file order those are one page touch per live key wherever the writer
	// happened to emit them, which for a map range is random, with nothing for readahead
	// to work with. On a store whose segments no longer fit in page cache that is the
	// dominant cost of the fast reopen, and it can exceed the full sequential walk the
	// snapshot exists to avoid.
	//
	// The sort is over the file's own bytes, in place, rather than over decoded entries:
	// a decoded slice would add 24 bytes per live key to the peak of the path being made
	// cheaper. A current writer has already sorted them, but the file is not trusted to
	// be ordered -- an older snapshot is still read, and re-sorting a sorted slice costs
	// almost nothing next to the faults it saves.
	//
	// Validation order does not affect the outcome: a single failure refuses the whole
	// snapshot, so it does not matter which entry is reached first.
	if nEntries < 0 || entryStart+nEntries*dirSnapEntryLen != len(body) {
		return false // entry count does not match the remaining bytes
	}
	sortDirSnapEntryBytes(body[entryStart:])
	dir := make(map[uint64]loc, nEntries)
	for i := 0; i < nEntries; i++ {
		e := body[entryStart+i*dirSnapEntryLen:]
		h := binary.LittleEndian.Uint64(e)
		slot := int(binary.LittleEndian.Uint32(e[8:]))
		off := binary.LittleEndian.Uint32(e[12:])
		if slot >= nSegs {
			return false
		}
		l := loc{seg: slotToArray[slot], off: off}
		if !dirEntryValid(sh.segs[l.seg], l.off, h, c.h) {
			return false // not a record of this bucket: the snapshot does not describe these segments
		}
		dir[h] = l
	}

	sh.dir = dir
	sh.commitSeq = commitSeq
	sh.count = count
	if bounds != nil {
		for ai, seg := range sh.segs {
			if seg != nil {
				bounds[ai].apply(seg)
			}
		}
	} else {
		// A version 1 snapshot carries no counters. Derive them from the record headers: the
		// same values rebuildDir would, without the per-key replay that makes it expensive.
		for _, seg := range sh.segs {
			if seg != nil {
				walkSegBounds(seg, sh.appendOnly)
			}
		}
	}
	if len(sh.segs) > 0 {
		sh.act = sh.segs[len(sh.segs)-1]
	}
	return true
}

// segBounds is a segment's scan-pruning counters as a snapshot carries them.
type segBounds struct {
	minSeq, maxSeq, maxSup uint64
	dead                   int64
}

// plausible checks the counters against what they describe, so a snapshot that does not belong
// to these segments is refused rather than trusted. Each check is a bound the counters cannot
// exceed when they are right: dead bytes are bytes of the segment; no sequence written or
// stamped in it is past the shard's commit sequence; a segment's smallest sequence is not past
// its largest. The CRC and the segment extents are what make a snapshot believable; this only
// keeps a wrong one from being believed.
func (b segBounds) plausible(used int, commitSeq uint64) bool {
	return b.dead >= 0 && b.dead <= int64(used) &&
		b.maxSeq <= commitSeq && b.maxSup <= commitSeq && b.minSeq <= b.maxSeq
}

func (b segBounds) apply(seg *segment) {
	seg.minSeq, seg.maxSeq, seg.maxSup, seg.dead = b.minSeq, b.maxSeq, b.maxSup, b.dead
}

// snapReader is a bounds-checked little-endian reader over a snapshot body; any
// short read latches err so the caller can bail without a panic.
type snapReader struct {
	b   []byte
	pos int
	err bool
}

func (r *snapReader) need(n int) bool {
	if r.err || r.pos+n > len(r.b) {
		r.err = true
		return false
	}
	return true
}

func (r *snapReader) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v
}

func (r *snapReader) u64() uint64 {
	if !r.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(r.b[r.pos:])
	r.pos += 8
	return v
}

func (r *snapReader) str16() string {
	if !r.need(2) {
		return ""
	}
	n := int(binary.LittleEndian.Uint16(r.b[r.pos:]))
	r.pos += 2
	if !r.need(n) {
		return ""
	}
	s := string(r.b[r.pos : r.pos+n])
	r.pos += n
	return s
}
