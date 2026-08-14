package collections

import (
	"encoding/binary"
	"fmt"
	"testing"
)

// A chain link naming a live segment and an offset past the end of it must end the walk, not panic. That
// is the difference between a daemon that returns a wrong "not found" -- visible in CorruptChainLinks --
// and one that dies:
//
//	panic: runtime error: slice bounds out of range [:67269717] with capacity 148816
//
// The offset is bounds-checked as deliberately as the segment index was, and for the same reason: a loc
// that is no longer a record's address will parse as whatever now lives there.
func TestFindVisibleSurvivesAnOutOfRangeOffset(t *testing.T) {
	c, err := Open(Options{Shards: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 8; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[Owner="alice"; Cpus=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	key := []byte("k3")
	h := c.h.Hash(key)
	sh := c.shards[c.shardOf(key, h)]

	before := CorruptChainLinks()
	sh.mu.Lock()
	head := sh.dirGet(h)
	if !head.valid() {
		sh.mu.Unlock()
		t.Fatal("the key is not in the directory, so this cannot exercise the chain walk")
	}
	seg := sh.segAt(head.seg)
	if seg == nil {
		sh.mu.Unlock()
		t.Fatal("the directory names a segment the shard does not have")
	}
	// Point the bucket at an offset past the end of a segment that really exists -- the shape a rewritten
	// segment leaves behind, and the shape the segment-index check does not catch.
	bad := head
	bad.off = uint32(len(seg.data)) + 4096
	sh.dir[h] = bad
	sh.mu.Unlock()

	if _, ok := c.Get(key); ok {
		t.Error("Get answered through a link that cannot be a record")
	}
	if got := CorruptChainLinks() - before; got == 0 {
		t.Error("an unusable link was skipped without being counted; a silent wrong answer is exactly " +
			"what the counter exists to prevent")
	}
	// The rest of the store must be unaffected: one bad bucket is not a reason to stop answering.
	for i := 0; i < 8; i++ {
		if i == 3 {
			continue
		}
		if _, ok := c.Get([]byte(fmt.Sprintf("k%d", i))); !ok {
			t.Errorf("k%d became unreadable after one bucket was corrupted", i)
		}
	}
}

// recFits is the guard under that check, and it has to reject a length that would WRAP as well as one that
// simply overruns: a garbage uint32 must not come back inside the slice.
func TestRecFitsRejectsOverrunAndWrap(t *testing.T) {
	buf := make([]byte, 128)
	if recFits(buf, 200) {
		t.Error("an offset past the buffer reported as fitting")
	}
	if recFits(buf, uint32(len(buf))-4) {
		t.Error("an offset with no room for the fixed header reported as fitting")
	}
	// A plausible header whose ad length overruns the buffer.
	plantHeader(buf, 0, 4, 1<<20)
	if recFits(buf, 0) {
		t.Error("a record claiming a megabyte inside a 128-byte buffer reported as fitting")
	}
	// The same, with a length chosen to wrap uint32 arithmetic.
	plantHeader(buf, 0, 4, 0xFFFFFFF0)
	if recFits(buf, 0) {
		t.Error("a wrapping ad length reported as fitting; the arithmetic has to be done wider than uint32")
	}
	// A record that really does fit.
	plantHeader(buf, 0, 4, 8)
	if !recFits(buf, 0) {
		t.Error("a record wholly inside the buffer reported as not fitting")
	}
}

// plantHeader writes just the two length fields recFits reads: the key length in the fixed header, and the
// ad length that follows the key.
func plantHeader(b []byte, off uint32, keyLen, adLen uint32) {
	binary.LittleEndian.PutUint32(b[off+recKeyLenOff:], keyLen)
	binary.LittleEndian.PutUint32(b[off+recKeyOff+keyLen:], adLen)
}
