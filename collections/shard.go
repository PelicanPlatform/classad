package collections

import (
	"bytes"
	"sync"
)

// shard owns an independent slice of the keyspace: a directory mapping a key hash
// to the head of a bucket chain, the arena segments the records live in, and a
// monotonic commit sequence used for MVCC scan visibility. Each shard is locked
// independently to reduce contention.
type shard struct {
	mu   sync.RWMutex
	dir  map[uint64]loc // key hash -> head of bucket chain (pointer-free value)
	segs []*segment     // indexed by segment id (id == index)
	act  *segment       // current append target

	commitSeq uint64 // bumped once per committed batch; scan snapshots capture it
	count     int    // number of live keys

	segSize int

	// Group-commit state (see commit.go), guarded by cmu.
	cmu      sync.Mutex
	queue    []*commitReq
	flushing bool
	onSync   func() // durability sync run once per committed batch; nil = no-op

	// Persistent segment allocation. alloc is nil for an in-memory shard (RAM
	// segments); for a persistent shard it creates an mmap-backed segment file.
	// writeErr is the first segment-allocation failure (sticky; guarded by mu),
	// surfaced to the caller by Put/Update.
	alloc    func(id uint32, size int, codec Codec) (*segment, error)
	writeErr error
}

func newShard(segSize int, onSync func()) *shard {
	return &shard{
		dir:     make(map[uint64]loc),
		segSize: segSize,
		onSync:  onSync,
	}
}

// allocSeg creates a new segment via the persistent factory if configured, else a
// RAM segment. On a persistent-allocation error it records the sticky writeErr and
// returns nil; the caller must treat the write as failed. Caller holds the write
// lock.
func (sh *shard) allocSeg(id uint32, size int, codec Codec) *segment {
	if sh.alloc == nil {
		return newSegment(id, size, codec)
	}
	seg, err := sh.alloc(id, size, codec)
	if err != nil {
		if sh.writeErr == nil {
			sh.writeErr = err
		}
		return nil
	}
	return seg
}

func (sh *shard) dirGet(h uint64) loc {
	if l, ok := sh.dir[h]; ok {
		return l
	}
	return noLoc
}

// findCurrent walks the bucket chain from head and returns the location of the
// current (non-superseded) record whose key matches, if any. Collisions and
// superseded versions are skipped by comparing the inline key and the atomic
// supersededBySeq field. Caller holds at least the read lock.
func (sh *shard) findCurrent(head loc, key []byte) (loc, bool) {
	for l := head; l.valid(); {
		seg := sh.segs[l.seg]
		if recSuperseded(seg.data, l.off) == seqMax && bytes.Equal(recKey(seg.data, l.off), key) {
			return l, true
		}
		l = recNext(seg.data, l.off)
	}
	return noLoc, false
}

// put inserts or updates key with the given ad bytes (compressed with codec) at
// commit sequence seq. A prior current version of the key (if any) is marked
// superseded at seq; the new record is prepended as the bucket head. Caller holds
// the write lock.
func (sh *shard) put(h uint64, key, ad []byte, seq uint64, codec Codec) {
	head := sh.dirGet(h)
	// Write the new record first: if segment allocation fails (persistent store,
	// disk full), the key is left unchanged rather than superseded-with-no-successor.
	newLoc, ok := sh.writeRecord(seq, head, key, ad, codec)
	if !ok {
		return // sh.writeErr is set; surfaced to the caller
	}
	if old, ok := sh.findCurrent(head, key); ok {
		seg := sh.segs[old.seg]
		setRecSuperseded(seg.data, old.off, seq)
		seg.dead += int64(recTotalLen(seg.data, old.off))
	} else {
		sh.count++
	}
	sh.dir[h] = newLoc
}

// del marks the current version of key superseded at seq (an MVCC tombstone: no
// new record is written). Returns whether a live key was removed. Caller holds
// the write lock.
func (sh *shard) del(h uint64, key []byte, seq uint64) bool {
	old, ok := sh.findCurrent(sh.dirGet(h), key)
	if !ok {
		return false
	}
	seg := sh.segs[old.seg]
	setRecSuperseded(seg.data, old.off, seq)
	seg.dead += int64(recTotalLen(seg.data, old.off))
	sh.count--
	return true
}

// writeRecord appends a record to the active segment and returns its location. A
// new segment is allocated when the active one is full, over-small for the
// record, or was written with a different codec (a segment's records all share
// one codec so reads can decode by segment).
func (sh *shard) writeRecord(seq uint64, next loc, key, ad []byte, codec Codec) (loc, bool) {
	rl := recordLen(len(key), len(ad))
	if sh.act == nil || sh.act.codec != codec || sh.act.used+rl > len(sh.act.data) {
		size := sh.segSize
		if rl > size {
			size = rl
		}
		seg := sh.allocSeg(uint32(len(sh.segs)), size, codec)
		if seg == nil {
			return noLoc, false // allocation failed (sticky writeErr set)
		}
		sh.segs = append(sh.segs, seg)
		sh.act = seg
	}
	off, _ := sh.act.append(seq, next, key, ad)
	return loc{seg: sh.act.id, off: off}, true
}

// get returns a private copy of the current ad bytes for key and the codec they
// were compressed with, or (nil, nil, false).
func (sh *shard) get(h uint64, key []byte) ([]byte, Codec, bool) {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	l, ok := sh.findCurrent(sh.dirGet(h), key)
	if !ok {
		return nil, nil, false
	}
	seg := sh.segs[l.seg]
	ad := recAd(seg.data, l.off)
	out := make([]byte, len(ad))
	copy(out, ad)
	return out, seg.codec, true
}

// segWindow is a frozen view of one segment at scan start: its immutable backing,
// the write watermark captured under the read lock, and the codec its records
// were compressed with.
type segWindow struct {
	data  []byte
	used  int
	codec Codec
	seg   *segment // for reindex-on-demand and the per-segment index (idx)
}

// snapshot captures the shard's scan state under the read lock: the commit
// sequence S0 and a frozen window over each live segment. The windows hold the
// segments' immutable backing arrays directly, so a segment retired by a later
// compaction stays alive (via the garbage collector) for as long as this scan
// references it — no manual reclamation is required. Retired segment slots are
// nil and skipped.
func (sh *shard) snapshot() (s0 uint64, wins []segWindow) {
	sh.mu.RLock()
	s0 = sh.commitSeq
	wins = make([]segWindow, 0, len(sh.segs))
	for _, seg := range sh.segs {
		if seg == nil || seg.used == 0 {
			continue
		}
		wins = append(wins, segWindow{data: seg.data, used: seg.used, codec: seg.codec, seg: seg})
	}
	sh.mu.RUnlock()
	return s0, wins
}

// forEachVisible walks the frozen windows and calls fn with the ad bytes (and the
// codec they were compressed with) of each record visible at S0
// (seq <= S0 < supersededBySeq) — exactly one version per key that existed at S0.
// fn returns false to stop early.
func forEachVisible(s0 uint64, wins []segWindow, fn func(ad []byte, codec Codec) bool) {
	for _, w := range wins {
		for off := 0; off < w.used; {
			o := uint32(off)
			total := recTotalLen(w.data, o)
			if total == 0 {
				break // malformed guard; should not happen
			}
			seq := recSeq(w.data, o)
			sup := recSuperseded(w.data, o)
			if seq <= s0 && sup > s0 {
				if !fn(recAd(w.data, o), w.codec) {
					return
				}
			}
			off += int(total)
		}
	}
}
