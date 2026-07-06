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

	noSeg  = ^uint32(0)
	seqMax = ^uint64(0)

	defaultSegmentSize = 1 << 20 // 1 MiB
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

	codec Codec // the codec that compressed this segment's records (immutable)

	// idx is this segment's value/categorical index, or nil if not yet built. It is
	// built by Reindex from immutable bytes and swapped in atomically; each
	// segIndex is immutable once published, so query readers load it lock-free.
	idx atomic.Pointer[segIndex]

	// Persistent (mmap) segments only; nil/zero for RAM segments. See mmapseg.go.
	// The file name is independent of the logical id (id == array index, reassigned
	// at compaction install and on recovery), so no rename is needed when id changes.
	file   *os.File // backing file (non-nil ⇒ mmap-backed)
	path   string   // backing file path (for unlink on retirement)
	synced int      // bytes msync'd to disk (the durable length); guarded by shard lock

	// Reclamation of mmap segments (RAM segments are freed by the GC once scans drop
	// their windows). A scan pins the segments it reads; a segment retired by
	// compaction is munmap'd + unlinked only once its pin count drains to zero, so a
	// scan never reads through a mapping torn down under it. Guarded by reapMu.
	reapMu  sync.Mutex
	refs    int  // active scan pins
	retired bool // removed from the live set by compaction; awaiting reap
	reaped  bool // reap() has run (guards against a double munmap/unlink)

	// onReap, if set, runs once right after reap() — used by the Archive to unmap a
	// segment's mmap'd sidecar index at the same pin-drained moment its data is
	// reaped, so a query scanning a rotating segment never reads a torn index mapping.
	onReap func()
}

// pin marks the start of a scan's use of a segment (mmap only; RAM segments rely on
// the GC). Balanced by unpin. Called under the shard read lock.
func (s *segment) pin() {
	if s.file == nil {
		return
	}
	s.reapMu.Lock()
	s.refs++
	s.reapMu.Unlock()
}

// unpin ends a scan's use of a segment; if the segment was retired while pinned and
// this was the last pin, it is reaped now (munmap + unlink). Called with no lock.
func (s *segment) unpin() {
	if s.file == nil {
		return
	}
	s.reapMu.Lock()
	s.refs--
	doReap := s.refs == 0 && s.retired && !s.reaped
	if doReap {
		s.reaped = true
	}
	s.reapMu.Unlock()
	if doReap {
		s.reapAndHook()
	}
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
	h := s.onReap
	s.onReap = nil
	s.reapMu.Unlock()
	if h != nil {
		h()
	}
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
// reaps it. For a RAM segment it is a no-op (the caller nils the slot; the GC frees
// it). Called under the shard write lock.
func (s *segment) retire() (reapNow bool) {
	if s.file == nil {
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
	if s.file != nil {
		crcOff := adLenOff + 4 + len(ad)
		binary.LittleEndian.PutUint32(b[crcOff:], recCRC(b, crcOff))
	}
	return uint32(off), true
}

// recVerifyCRC reports whether the record at off has a valid trailing CRC (i.e. it
// was fully written). Used by recovery to stop at a torn tail. b is the segment's
// data; the caller has already bounds-checked the record's totalLen.
func recVerifyCRC(b []byte, off uint32) bool {
	kl := binary.LittleEndian.Uint32(b[off+recKeyLenOff:])
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

func recKey(b []byte, off uint32) []byte {
	kl := binary.LittleEndian.Uint32(b[off+recKeyLenOff:])
	start := off + recKeyOff
	return b[start : start+kl]
}

func recAd(b []byte, off uint32) []byte {
	kl := binary.LittleEndian.Uint32(b[off+recKeyLenOff:])
	adLenOff := off + recKeyOff + kl
	adLen := binary.LittleEndian.Uint32(b[adLenOff:])
	start := adLenOff + 4
	return b[start : start+adLen]
}
