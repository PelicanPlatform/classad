package collections

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
)

// errStaleDirEntry aborts a snapshot write when a directory entry points at a
// retired segment (an unexpected state); the reopen then falls back to a scan.
var errStaleDirEntry = errors.New("collections: directory entry references a retired segment")

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
const (
	dirSnapName    = "dir.snap"
	dirSnapMagic   = 0x53524944 // "DIRS"
	dirSnapVersion = 1
)

// writeDirSnapshot persists shard sh's directory to <shardDir>/dir.snap. The caller
// holds sh.mu and must have flushed every segment durable first, so the snapshot
// never references not-yet-durable bytes. Best effort: an error just means the next
// open falls back to a full scan.
func writeDirSnapshot(sh *shard, shardDir string) error {
	// Segments are referenced by their file basename, which is stable across reopen;
	// array indices are not (compaction leaves nil holes and reopen reloads without
	// them). Assign each live (non-nil) segment a snapshot slot and record its
	// basename + durable length; directory entries then reference the slot.
	slotOf := make(map[uint32]uint32, len(sh.segs)) // array index -> snapshot slot
	buf := make([]byte, 0, 32+len(sh.segs)*24+len(sh.dir)*16)
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
	}
	binary.LittleEndian.PutUint32(buf[nSegOff:], nSegs)
	nEntOff := len(buf)
	buf = binary.LittleEndian.AppendUint64(buf, 0) // entry count, back-patched below
	var nEntries uint64
	for h, l := range sh.dir {
		slot, ok := slotOf[l.seg]
		if !ok {
			// A live directory entry pointing at a retired segment should never
			// happen on a clean shutdown; if it does, skip the snapshot entirely so
			// the reopen falls back to the authoritative scan.
			return errStaleDirEntry
		}
		buf = binary.LittleEndian.AppendUint64(buf, h)
		buf = binary.LittleEndian.AppendUint32(buf, slot)
		buf = binary.LittleEndian.AppendUint32(buf, l.off)
		nEntries++
	}
	binary.LittleEndian.PutUint64(buf[nEntOff:], nEntries)
	buf = binary.LittleEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))

	tmp := filepath.Join(shardDir, dirSnapName+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, filepath.Join(shardDir, dirSnapName))
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
	if r.u32() != dirSnapMagic || r.u32() != dirSnapVersion {
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
	}
	nEntries := int(r.u64())
	if r.err {
		return false
	}
	dir := make(map[uint64]loc, nEntries)
	for i := 0; i < nEntries; i++ {
		h := r.u64()
		slot := int(r.u32())
		off := r.u32()
		if r.err || slot >= nSegs {
			return false
		}
		l := loc{seg: slotToArray[slot], off: off}
		s := sh.segs[l.seg]
		if int(l.off)+recHeaderSize > s.used || recTotalLen(s.data, l.off) == 0 {
			return false // location out of the durable region / unwritten
		}
		dir[h] = l
	}
	if r.err || r.pos != len(body) {
		return false // trailing garbage
	}

	sh.dir = dir
	sh.commitSeq = commitSeq
	sh.count = count
	if len(sh.segs) > 0 {
		sh.act = sh.segs[len(sh.segs)-1]
	}
	return true
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
