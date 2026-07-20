// Package collections is an in-memory, sharded, memory-dense store of ClassAds
// with a compiled query engine. Ads are serialized to the compact wire form and
// packed into append-only arena segments; a per-shard directory maps a stable
// key hash to a record location. Updates are MVCC-stamped so table scans see
// every ad exactly once even while background compaction moves bytes.
package collections

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"
)

// crcTable is CRC-32C (Castagnoli), hardware-accelerated on modern CPUs. A
// persistent record ends with a CRC over its immutable bytes so recovery can
// reject a torn/partial tail (a record whose write did not complete before the
// crash).
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Record layout in an arena segment. All multi-byte fields are little-endian and
// each record starts on an 8-byte boundary so the two 8-byte MVCC fields are
// naturally aligned for atomic access.
//
//	off+0   seq             uint64  commitSeq when this version became current (immutable)
//	off+8   supersededBySeq uint64  commitSeq when overwritten/deleted; seqMax while current (atomic)
//	off+16  nextSeg         uint32  hash-bucket chain: segment id of the next record, or noSeg
//	off+20  nextOff         uint32  hash-bucket chain: offset of the next record
//	off+24  totalLen        uint32  total record length from off (multiple of 8)
//	off+28  keyLen          uint32  length of the inline key
//	off+32  key             bytes   the full stable key (for exact match / collision resolution)
//	        adLen           uint32  length of the (possibly codec-compressed) wire ad
//	        ad              bytes   the encoded ad
//	        padding         so totalLen is a multiple of 8
const (
	recSeqOff      = 0
	recSupOff      = 8
	recNextSegOff  = 16
	recNextOffOff  = 20
	recTotalLenOff = 24
	recKeyLenOff   = 28
	recKeyOff      = 32
	recHeaderSize  = 32

	// markerFlag is the high bit of the keyLen field, set on a "time checkpoint"
	// marker entry (see timeseq.go): a non-keyed arena record whose 8-byte payload is
	// a wall-clock unixMillis and whose seq is the shard commitSeq at that instant.
	// Real keys are tiny, so the low 31 bits (keyLenMask) always hold the true key
	// length and existing on-disk records (flag clear) read as ordinary records --
	// the discriminator is backward compatible and needs no header growth.
	markerFlag = uint32(1) << 31
	keyLenMask = markerFlag - 1

	noSeg  = ^uint32(0)
	seqMax = ^uint64(0)

	defaultSegmentSize = 8 * (1 << 20) // 8 MiB
)

// loc identifies a record: its segment id and byte offset. It is all scalars so
// the directory map stays pointer-free.
type loc struct {
	seg uint32
	off uint32
}

func (l loc) valid() bool { return l.seg != noSeg }

var noLoc = loc{seg: noSeg}

// segment is a fixed-capacity append-only arena. The backing array is allocated
// as []uint64 so its start is 8-byte aligned (required for atomic access to the
// supersededBySeq field). data is an immutable slice header over the whole
// backing (len == cap == size); it is never reassigned, so lock-free readers can
// index it without racing a concurrent writer. The write cursor `used` is guarded
// by the shard lock; a scan snapshots it under the read lock at scan start.
type segment struct {
	id   uint32
	raw  []uint64 // RAM backing (nil for mmap segments); retained so it is not collected
	data []byte   // immutable full backing; aliases raw (RAM) or the mmap region (persistent)
	used int      // write cursor (guarded by shard lock)
	dead int64    // bytes belonging to superseded/dead records (updated under shard lock)

	// Time-travel scan pruning (guarded by the shard lock; recomputed on recovery).
	// A scan at snapshot S0 can skip this segment entirely when no record in it is
	// visible at S0 -- either every record was born after S0 (minSeq > S0) or every
	// record was superseded by S0 with none current (dead >= used, so no live record,
	// AND maxSup <= S0). This keeps current-time scans O(live) even when time travel
	// retains a large history in separate (history-only) segments. minSeq is the
	// smallest seq written here (0 = none yet, or a fast-reopened segment not walked --
	// which is then never skipped, conservatively); maxSup is the largest finite
	// supersededBySeq stamped here. "no current record" is read from the byte counters
	// (dead >= used, markers counting as dead) rather than a separate live tally, so it
	// cannot drift out of sync with the arena.
	minSeq uint64
	maxSup uint64

	codec Codec // the codec that compressed this segment's records (immutable)

	// idx is this segment's value/categorical index, or nil if not yet built. It is
	// built by Reindex from immutable bytes and swapped in atomically; each
	// segIndex is immutable once published, so query readers load it lock-free.
	idx atomic.Pointer[segIndex]

	// msidx is a sealed segment's index in its pageable mmap form (the sidecar). When set
	// it supersedes idx (which is then cleared to free the heap copy): readIdx prefers it.
	// Its backing mapping is released via onReap when the segment's scan pins drain.
	msidx atomic.Pointer[mmapSegIndex]

	// Persistent (mmap) segments only; nil/zero for RAM segments. See mmapseg.go.
	// The file name is independent of the logical id (id == array index, reassigned
	// at compaction install and on recovery), so no rename is needed when id changes.
	// persistent marks a file-backed (mmap) segment, independent of whether its fd is
	// currently open. It is the durable-segment predicate (flush/msync, CRC, mapped,
	// reap-unlink) so those keep working after the fd is released. Set once at
	// creation; immutable.
	persistent bool
	file       *os.File // backing fd; released (nil) right after mmap -- see mmapseg.go
	path       string   // backing file path (for unlink on retirement)
	synced     int      // bytes msync'd to disk (the durable length); guarded by shard lock

	// Reclamation of mmap segments (RAM segments are freed by the GC once scans drop
	// their windows). A scan pins the segments it reads; a segment retired by
	// compaction is munmap'd + unlinked only once its pin count drains to zero, so a
	// scan never reads through a mapping torn down under it. Guarded by reapMu.
	reapMu  sync.Mutex
	refs    int  // active scan pins
	retired bool // removed from the live set by compaction; awaiting reap
	reaped  bool // reap()/close-unmap has run (guards against a double munmap)
	closing bool // Archive.Close requested; unmap (no unlink) when pins drain

	// onReap, if set, runs once right after reap() — used by the Archive to unmap a
	// segment's mmap'd sidecar index at the same pin-drained moment its data is
	// reaped, so a query scanning a rotating segment never reads a torn index mapping.
	onReap func()

	// keyIdx is a sealed segment's pageable key-index sidecar (key-hash -> record
	// offset), or nil. Phase 1 of the pageable primary index: built at seal, mmapped
	// on reopen, torn down with the segment (onReapKey, run alongside onReap). It does
	// not yet replace the in-RAM directory -- it is populated and validated beside it.
	keyIdx    atomic.Pointer[mmapKeyIndex]
	onReapKey func() // key-sidecar unmap; run with onReap at reap/close

	// keyBloom is a resident membership filter over this sealed segment's key-hashes
	// (phase 2), built from keyIdx at seal/load. It gates the sealed-segment probe so
	// a Get miss in the active directory only touches segments that might hold the
	// key. Nil until the segment is sealed with a key index.
	keyBloom atomic.Pointer[bloomFilter]

	// pinReap makes a RAM segment (file==nil) participate in pin/reap anyway, because it
	// carries an anonymous mmap sidecar index (in-memory sealing): the anon mapping is not
	// GC-managed, so scans must pin it and compaction/Close must unmap it via onReap, exactly
	// as for a file-backed segment. Set at creation for an in-memory sealing shard; immutable.
	pinReap bool
}

// mapped reports whether the segment holds a non-GC mapping that scans must pin and reap
// must tear down: a file-backed data mmap, or (RAM store) an anon-mmap sealed sidecar. A
// plain RAM segment (neither) relies on the GC and skips pin/reap entirely.
func (s *segment) mapped() bool { return s.persistent || s.pinReap }

// readIdx returns the segment's queryable index behind the readIndex interface: the mmap'd
// sidecar (msidx) once the segment is sealed and converted, else the in-RAM segIndex, else
// nil. A sealed segment's msidx supersedes its (cleared) in-RAM idx; both satisfy readIndex,
// so the query planner dispatches through it without caring which representation it is.
func (s *segment) readIdx() readIndex {
	if m := s.msidx.Load(); m != nil {
		return m
	}
	if i := s.idx.Load(); i != nil {
		return i
	}
	return nil
}

// pin marks the start of a scan's use of a segment. Balanced by unpin. Skipped for a
// plain RAM segment (GC-managed); engaged for a file-backed segment or a RAM segment with
// an anon sidecar (see mapped). Called under the shard read lock.
func (s *segment) pin() {
	if !s.mapped() {
		return
	}
	s.reapMu.Lock()
	s.refs++
	s.reapMu.Unlock()
}

// unpin ends a scan's use of a segment; if the segment was retired while pinned and
// this was the last pin, it is reaped now (munmap + unlink). Called with no lock.
func (s *segment) unpin() {
	if !s.mapped() {
		return
	}
	s.reapMu.Lock()
	s.refs--
	// Last pin drop: reap (munmap + unlink) a compaction-retired segment, or unmap
	// (munmap, no unlink) a segment Archive.Close deferred because we were pinning
	// it. reap wins if both apply. Setting reaped guards against a double free.
	doReap := s.refs == 0 && s.retired && !s.reaped
	doUnmap := s.refs == 0 && s.closing && !s.retired && !s.reaped
	if doReap || doUnmap {
		s.reaped = true
	}
	s.reapMu.Unlock()
	if doReap {
		s.reapAndHook()
	} else if doUnmap {
		_ = s.unmapAndHook()
	}
}

// unmapAndHook unmaps the segment's data (munmap + close file, WITHOUT unlinking
// the file) and then runs onReap once (the sidecar-index unmap). It is the
// close-time counterpart of reapAndHook: the archive files persist across Close.
func (s *segment) unmapAndHook() error {
	err := s.unmap()
	s.runReapHook()
	return err
}

// closeUnmap unmaps the segment for Archive.Close: immediately if no scan pins it,
// otherwise it defers the unmap to the last unpin (so a live watch never reads a
// mapping torn down under it -- the bug the pin count exists to prevent). Unlike
// retire/reap it does not unlink the backing file. Idempotent; caller holds the
// Archive mutex, so no new pin can be taken concurrently. No-op for a plain RAM segment;
// a RAM segment with an anon sidecar unmaps it (via the reap hook) when its pins drain.
func (s *segment) closeUnmap() error {
	if !s.mapped() {
		return nil
	}
	s.reapMu.Lock()
	if s.reaped { // already reaped by compaction or unmapped by a prior Close
		s.reapMu.Unlock()
		return nil
	}
	if s.refs > 0 {
		s.closing = true // a live scan pins it; the last unpin will unmap it
		s.reapMu.Unlock()
		return nil
	}
	s.reaped = true
	s.reapMu.Unlock()
	return s.unmapAndHook()
}

// setOnReap registers a callback run once when the segment is reaped or explicitly
// unhooked (the Archive's sidecar-index unmap). Guarded by reapMu; set while the
// segment is pinned, so it is visible to the last unpin that reaps.
func (s *segment) setOnReap(f func()) {
	s.reapMu.Lock()
	s.onReap = f
	s.reapMu.Unlock()
}

// runReapHook runs and clears onReap (if any) without touching the data mapping.
// Close uses it to unmap a segment's sidecar index without unlinking its files.
func (s *segment) runReapHook() {
	s.reapMu.Lock()
	h, hk := s.onReap, s.onReapKey
	s.onReap, s.onReapKey = nil, nil
	s.reapMu.Unlock()
	if h != nil {
		h()
	}
	if hk != nil {
		hk()
	}
}

// setOnReapKey registers the key-sidecar unmap, run with onReap when the segment is
// reaped or unmapped. Independent of onReap so a segment can carry both an attribute
// sidecar and a key sidecar.
func (s *segment) setOnReapKey(f func()) {
	s.reapMu.Lock()
	s.onReapKey = f
	s.reapMu.Unlock()
}

// reapAndHook reaps the segment (munmap + unlink of its data) and then runs onReap
// once (the sidecar unmap). Both the deferred (unpin) and immediate (retire==true)
// reap paths route through it, so onReap fires exactly once when pins have drained.
func (s *segment) reapAndHook() error {
	err := s.reap()
	s.runReapHook()
	return err
}

// retire removes a compaction source segment from the live set. It returns true if
// the caller should reap() it now (no scan references it); otherwise the last unpin
// reaps it. For a plain RAM segment it is a no-op (the caller nils the slot; the GC frees
// it); a RAM segment with an anon sidecar retires like an mmap one so its sidecar unmaps.
// Called under the shard write lock.
func (s *segment) retire() (reapNow bool) {
	if !s.mapped() {
		return false
	}
	s.reapMu.Lock()
	s.retired = true
	reapNow = s.refs == 0 && !s.reaped
	if reapNow {
		s.reaped = true
	}
	s.reapMu.Unlock()
	return reapNow
}

func newSegment(id uint32, size int, codec Codec) *segment {
	size = recAlign(size)
	if size < recHeaderSize+8 {
		size = recAlign(recHeaderSize + 8)
	}
	raw := make([]uint64, size/8)
	data := unsafe.Slice((*byte)(unsafe.Pointer(&raw[0])), size)
	return &segment{id: id, raw: raw, data: data, codec: codec}
}

func recAlign(n int) int { return (n + 7) &^ 7 }

// recordLen returns the total on-segment length for a record with the given key
// and ad byte lengths, including the 4-byte trailing CRC.
func recordLen(keyLen, adLen int) int {
	return recAlign(recHeaderSize + keyLen + 4 + adLen + 4)
}

// recCRC computes the CRC-32C over a record's immutable bytes: the commit seq and
// the structural/payload region (totalLen, keyLen, key, adLen, ad). It excludes
// the mutable fields (supersededBySeq, the chain pointer), which are rewritten
// after the initial append. b is the record's byte slice, crcOff the offset of the
// trailing CRC within it.
func recCRC(b []byte, crcOff int) uint32 {
	c := crc32.Update(0, crcTable, b[recSeqOff:recSeqOff+8])
	return crc32.Update(c, crcTable, b[recTotalLenOff:crcOff])
}

// append writes a record for (key, ad) at the current end of the segment and
// returns its offset. next is the hash-bucket chain successor. It returns
// (off, true) on success, or (0, false) if the record does not fit. seq is the
// record's commit sequence; supersededBySeq is initialized to seqMax (current).
// The caller holds the shard lock.
func (s *segment) append(seq uint64, next loc, key, ad []byte) (uint32, bool) {
	rl := recordLen(len(key), len(ad))
	off := s.used
	if off+rl > len(s.data) {
		return 0, false
	}
	b := s.data[off : off+rl]
	s.used = off + rl
	// Zero the header + padding region (buffer may be reused across a segment's
	// life only via fresh allocation, so this is defensive for the pad bytes).
	binary.LittleEndian.PutUint64(b[recSeqOff:], seq)
	binary.LittleEndian.PutUint64(b[recSupOff:], seqMax)
	binary.LittleEndian.PutUint32(b[recNextSegOff:], next.seg)
	binary.LittleEndian.PutUint32(b[recNextOffOff:], next.off)
	binary.LittleEndian.PutUint32(b[recTotalLenOff:], uint32(rl))
	binary.LittleEndian.PutUint32(b[recKeyLenOff:], uint32(len(key)))
	copy(b[recKeyOff:], key)
	adLenOff := recKeyOff + len(key)
	binary.LittleEndian.PutUint32(b[adLenOff:], uint32(len(ad)))
	copy(b[adLenOff+4:], ad)
	// Trailing CRC over the immutable bytes (persistent segments only; recovery
	// uses it to detect a torn tail). RAM segments never recover, so skip the cost.
	if s.persistent {
		crcOff := adLenOff + 4 + len(ad)
		binary.LittleEndian.PutUint32(b[crcOff:], recCRC(b, crcOff))
	}
	if s.minSeq == 0 || seq < s.minSeq {
		s.minSeq = seq
	}
	return uint32(off), true
}

// appendMarker writes a time-checkpoint marker (see timeseq.go) at the current end of
// the segment: a non-keyed record whose keyLen field carries markerFlag and whose
// 8-byte payload is unixMillis. Its seq is the shard commitSeq at the checkpoint. It
// reuses the ordinary record framing (empty key, 8-byte "ad") so the linear arena
// walk, CRC, and recovery treat it uniformly; only the flag bit distinguishes it. The
// caller holds the shard lock.
func (s *segment) appendMarker(seq uint64, millis uint64) (uint32, bool) {
	var ad [8]byte
	binary.LittleEndian.PutUint64(ad[:], millis)
	rl := recordLen(0, len(ad))
	off := s.used
	if off+rl > len(s.data) {
		return 0, false
	}
	b := s.data[off : off+rl]
	s.used = off + rl
	binary.LittleEndian.PutUint64(b[recSeqOff:], seq)
	binary.LittleEndian.PutUint64(b[recSupOff:], seqMax)
	binary.LittleEndian.PutUint32(b[recNextSegOff:], noSeg)
	binary.LittleEndian.PutUint32(b[recNextOffOff:], 0)
	binary.LittleEndian.PutUint32(b[recTotalLenOff:], uint32(rl))
	binary.LittleEndian.PutUint32(b[recKeyLenOff:], markerFlag) // flag set, key length 0
	adLenOff := recKeyOff
	binary.LittleEndian.PutUint32(b[adLenOff:], uint32(len(ad)))
	copy(b[adLenOff+4:], ad[:])
	if s.persistent {
		crcOff := adLenOff + 4 + len(ad)
		binary.LittleEndian.PutUint32(b[crcOff:], recCRC(b, crcOff))
	}
	if s.minSeq == 0 || seq < s.minSeq {
		s.minSeq = seq
	}
	// A marker is not live data: count its bytes as dead so a history-only segment that
	// carries only superseded records plus checkpoints still reaches dead >= used (and
	// is thus recognized as fully dead / skippable / reclaimable once aged out).
	s.dead += int64(rl)
	return uint32(off), true
}

// supersedeRec marks the record at off superseded at seq: it stamps the field, adds
// its bytes to the dead total, and advances maxSup. Caller holds the shard write lock.
// This is the one place a record leaves the "current" set, so the scan-pruning
// metadata stays consistent with the arena.
func (s *segment) supersedeRec(off uint32, seq uint64) {
	setRecSuperseded(s.data, off, seq)
	s.dead += int64(recTotalLen(s.data, off))
	if seq > s.maxSup {
		s.maxSup = seq
	}
}

// visibleAt reports whether any record in the segment MIGHT be visible at snapshot s0
// (some record with seq <= s0 < sup). It is a conservative skip test for scans: false
// means definitely nothing visible; true means scan the segment. It reads only the
// byte counters, so it cannot drift out of sync with the arena, and a fast-reopened
// segment whose counters were not rebuilt (minSeq == 0, dead == 0) is never skipped.
//
//   - minSeq > s0: every record here was born after s0 -> nothing visible.
//   - dead >= used (no current record, markers counted as dead) AND maxSup <= s0:
//     every record was superseded at or before s0 -> nothing visible.
func (s *segment) visibleAt(s0 uint64) bool {
	if s.minSeq > s0 {
		return false
	}
	if s.dead >= int64(s.used) && s.maxSup <= s0 {
		return false
	}
	return true
}

// recVerifyCRC reports whether the record at off has a valid trailing CRC (i.e. it
// was fully written). Used by recovery to stop at a torn tail. b is the segment's
// data; the caller has already bounds-checked the record's totalLen.
func recVerifyCRC(b []byte, off uint32) bool {
	kl := recKeyLen(b, off)
	adLenOff := off + recKeyOff + kl
	if int(adLenOff)+4 > len(b) {
		return false
	}
	adLen := binary.LittleEndian.Uint32(b[adLenOff:])
	crcOff := int(adLenOff) + 4 + int(adLen)
	if crcOff+4 > len(b) {
		return false
	}
	rec := b[off : crcOff+4]
	want := binary.LittleEndian.Uint32(b[crcOff:])
	return recCRC(rec, crcOff-int(off)) == want
}

// --- record field accessors (operate on a segment's buf and an offset) ---

func recSeq(b []byte, off uint32) uint64 {
	return binary.LittleEndian.Uint64(b[off+recSeqOff:])
}

// recSuperseded reads the supersededBySeq field atomically (it may be written
// concurrently by an update while a scan reads it lock-free).
func recSuperseded(b []byte, off uint32) uint64 {
	return atomic.LoadUint64((*uint64)(unsafe.Pointer(&b[off+recSupOff])))
}

func setRecSuperseded(b []byte, off uint32, v uint64) {
	atomic.StoreUint64((*uint64)(unsafe.Pointer(&b[off+recSupOff])), v)
}

func recNext(b []byte, off uint32) loc {
	return loc{
		seg: binary.LittleEndian.Uint32(b[off+recNextSegOff:]),
		off: binary.LittleEndian.Uint32(b[off+recNextOffOff:]),
	}
}

// setRecNext rewrites a record's hash-bucket chain successor. Only called while
// the shard write lock is held (compaction rebuilding chains), so no atomicity is
// needed: no concurrent reader walks a chain that is being rebuilt.
func setRecNext(b []byte, off uint32, next loc) {
	binary.LittleEndian.PutUint32(b[off+recNextSegOff:], next.seg)
	binary.LittleEndian.PutUint32(b[off+recNextOffOff:], next.off)
}

func recTotalLen(b []byte, off uint32) uint32 {
	return binary.LittleEndian.Uint32(b[off+recTotalLenOff:])
}

// recKeyLen is the true key length: the low 31 bits of the keyLen field, masking off
// the marker flag (markerFlag) so a marker (flag set, key length 0) reads as keyless.
func recKeyLen(b []byte, off uint32) uint32 {
	return binary.LittleEndian.Uint32(b[off+recKeyLenOff:]) & keyLenMask
}

// recIsMarker reports whether the record at off is a time-checkpoint marker (see
// timeseq.go) rather than a keyed data record.
func recIsMarker(b []byte, off uint32) bool {
	return binary.LittleEndian.Uint32(b[off+recKeyLenOff:])&markerFlag != 0
}

// recMarkerMillis reads a marker's wall-clock payload (unixMillis). Only valid when
// recIsMarker(b, off) is true.
func recMarkerMillis(b []byte, off uint32) uint64 {
	return binary.LittleEndian.Uint64(recAd(b, off))
}

func recKey(b []byte, off uint32) []byte {
	kl := recKeyLen(b, off)
	start := off + recKeyOff
	return b[start : start+kl]
}

func recAd(b []byte, off uint32) []byte {
	kl := recKeyLen(b, off)
	adLenOff := off + recKeyOff + kl
	adLen := binary.LittleEndian.Uint32(b[adLenOff:])
	start := adLenOff + 4
	return b[start : start+adLen]
}
