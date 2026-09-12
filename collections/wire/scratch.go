package wire

import (
	"encoding/binary"
	"sync"
)

// The hot-header encoders build the entries region in a scratch buffer before framing it
// into the returned record, because the header's size is not known until every entry's
// offset is. That buffer started nil on every call, so encoding one ad re-grew it from
// zero -- a dozen doublings of allocate-zero-copy for a job ad, on every write of every
// ad. It is pure scratch (copied into the result and then dead), so it pools.

var scratchPool = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}

// getScratch returns an empty pooled buffer for an encoder's entries region.
func getScratch() []byte {
	b, ok := scratchPool.Get().(*[]byte)
	if !ok {
		return make([]byte, 0, 4096)
	}
	return (*b)[:0]
}

// putScratch returns a scratch buffer to the pool. Oversized buffers are dropped rather
// than pooled so one pathological ad does not pin a large allocation forever.
func putScratch(b []byte) {
	if cap(b) > 1<<20 {
		return
	}
	scratchPool.Put(&b)
}

// frame appends the record header -- magic, version, flags, the hot index, and the
// attribute count -- to dst and then the entries region, sizing the result exactly once
// instead of letting append rediscover it. entries is the scratch region; it is not
// retained.
func frame(dst []byte, flags byte, hots []hotPair, attrCount int, entries []byte) []byte {
	need := 3 + uvarintLen(uint64(len(hots))) + uvarintLen(uint64(attrCount)) + len(entries)
	for _, h := range hots {
		need += uvarintLen(uint64(h.id)) + uvarintLen(uint64(h.off))
	}
	if cap(dst)-len(dst) < need {
		grown := make([]byte, len(dst), len(dst)+need)
		copy(grown, dst)
		dst = grown
	}
	dst = append(dst, magicByte, formatVer, flags)
	dst = binary.AppendUvarint(dst, uint64(len(hots)))
	for _, h := range hots {
		dst = binary.AppendUvarint(dst, uint64(h.id))
		dst = binary.AppendUvarint(dst, uint64(h.off))
	}
	dst = binary.AppendUvarint(dst, uint64(attrCount))
	return append(dst, entries...)
}

// uvarintLen is the encoded width of x as a uvarint. frame sums the real widths rather
// than reserving binary.MaxVarintLen64 apiece: the slack from a worst-case reservation is
// memory the allocator still has to zero before the record is written over it.
func uvarintLen(x uint64) int {
	n := 1
	for x >= 0x80 {
		x >>= 7
		n++
	}
	return n
}
