package collections

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// undecodableInline is an inline-names ad [Id = 5; Bad = <attribute reference by id 0>]. The id-keyed
// node has no table behind it in an inline ad, so every reader refuses the ad as malformed, and the
// compaction transcode refuses it too.
var undecodableInline = []byte{0xCA, 1, 0x02, 0, 2, 2, 'I', 'd', 0x04, 10, 3, 'B', 'a', 'd', 0x07, 0, 0}

// putRawWire commits wire as key's current version, bypassing the encoder: the only way to store a
// record the encoder would never write.
func putRawWire(c *Collection, key, wire []byte) {
	codec := c.currentCodec()
	h := c.h.Hash(key)
	sh := c.shards[c.shardOf(key, h)]
	sh.commitOne(pendingPut{hash: h, key: key, ad: codec.Compress(nil, wire), codec: codec})
}

// preservedRecord finds key's current record by walking every segment of shard 0, returning its
// segment and its decompressed wire.
func preservedRecord(t *testing.T, c *Collection, key []byte) (*segment, []byte) {
	t.Helper()
	sh := c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	for _, seg := range sh.segs {
		if seg == nil {
			continue
		}
		for off := 0; off < seg.used; {
			o := uint32(off)
			total := recTotalLen(seg.data, o)
			if total == 0 {
				break
			}
			if !recIsMarker(seg.data, o) && recSuperseded(seg.data, o) == seqMax && bytes.Equal(recKey(seg.data, o), key) {
				w, err := seg.codec.Decompress(nil, recAd(seg.data, o))
				if err != nil {
					t.Fatalf("record %q does not decompress: %v", key, err)
				}
				return seg, w
			}
			off += int(total)
		}
	}
	t.Fatalf("no current record for %q", key)
	return nil, nil
}

// dictNames lists a segment dictionary's names in id order.
func dictNames(h *segDictHandle) []string {
	out := make([]string, h.count())
	for i := range out {
		out[i] = string(h.name(uint32(i)))
	}
	return out
}

// captureUndecodable replaces CompactUndecodableHook for the test and returns what it was called with.
func captureUndecodable(t *testing.T) *[]CompactUndecodable {
	t.Helper()
	var got []CompactUndecodable
	prev := CompactUndecodableHook
	CompactUndecodableHook = func(e CompactUndecodable) { got = append(got, e) }
	t.Cleanup(func() { CompactUndecodableHook = prev })
	return &got
}

// TestCompactPreservesUndecodableRecord: a shard holding a record that does not decode used to abort
// every compaction. It now compacts everything else, and the record is copied with its bytes unchanged
// into a segment of its source's key form -- no dictionary for a record from an inline segment, an
// exact copy of the source's dictionary for one from an interned segment -- where every reader still
// refuses it. Each is counted and reported once, not again on later passes.
//
// Not parallel: it replaces CompactUndecodableHook and reads process-wide counters.
func TestCompactPreservesUndecodableRecord(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	opts := Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	reports := captureUndecodable(t)
	put := func(from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			if perr := c.Put(fmt.Appendf(nil, "k%04d", i), mustAd(t, fmt.Sprintf(`[Id = %d; Owner = "u%d"]`, i, i%7))); perr != nil {
				t.Fatal(perr)
			}
		}
	}
	const n = 1500
	put(0, n/2)
	badInline := []byte("bad-inline")
	putRawWire(c, badInline, undecodableInline)
	put(n/2, n)
	put(0, n/3) // superseded versions, so the shard has something to reclaim
	if _, ok := c.Get(badInline); ok {
		t.Fatal("setup: the undecodable record reads")
	}
	srcSeg, _ := preservedRecord(t, c, badInline)

	checkAll := func(stage string) {
		t.Helper()
		for i := 0; i < n; i++ {
			a, ok := c.Get(fmt.Appendf(nil, "k%04d", i))
			if !ok {
				t.Fatalf("%s: k%04d missing", stage, i)
			}
			if id, _ := a.EvaluateAttrInt("Id"); id != int64(i) {
				t.Fatalf("%s: k%04d has Id %d", stage, i, id)
			}
		}
	}
	compact := func() {
		c.compactShard(c.shards[0], c.currentCodec())
		c.reindexAfterCompaction()
	}

	preserved := CompactPreservedUndecodable()
	compact()
	if slices.Contains(c.shards[0].segs, srcSeg) {
		t.Fatal("the shard was not compacted: the undecodable record's source segment is still in place")
	}
	if got := CompactPreservedUndecodable() - preserved; got != 1 {
		t.Fatalf("CompactPreservedUndecodable rose by %d, want 1", got)
	}
	if len(*reports) != 1 || (*reports)[0].Key != string(badInline) || (*reports)[0].Shard != 0 || (*reports)[0].Blocked {
		t.Fatalf("reports = %+v, want one for %q in shard 0, not blocked", *reports, badInline)
	}
	checkAll("after compaction")
	seg, w := preservedRecord(t, c, badInline)
	if !bytes.Equal(w, undecodableInline) || seg.dict.Load() != nil {
		t.Fatalf("preserved record: bytes %x in a segment with a dictionary=%v; want %x in one without",
			w, seg.dict.Load() != nil, undecodableInline)
	}
	if _, ok := c.Get(badInline); ok {
		t.Fatal("the preserved record reads after compaction")
	}

	// A record that does not decode in an INTERNED segment: ids of its own dictionary, one of which
	// names nothing. Appended to a segment the compaction just interned.
	sh := c.shards[0]
	sh.mu.Lock()
	var interned *segment
	for _, s := range sh.segs {
		if s != nil && s.dict.Load() != nil && s.used+4096 < len(s.data) {
			interned = s
			break
		}
	}
	if interned == nil {
		sh.mu.Unlock()
		t.Fatal("setup: no interned segment with room")
	}
	srcNames := dictNames(interned.dict.Load())
	unknown := byte(len(srcNames) + 5)
	badInternedWire := []byte{0xCA, 1, 0, 0, 2, 0, 0x04, 14, unknown, 0x04, 2} // [<id 0> = 7; <unknown id> = 1]
	badInterned := []byte("bad-interned")
	if _, ok := interned.appendFlagged(sh.commitSeq, noLoc, badInterned, interned.codec.Compress(nil, badInternedWire), 0); !ok {
		sh.mu.Unlock()
		t.Fatal("setup: append failed")
	}
	sh.mu.Unlock()

	compact()
	if got := CompactPreservedUndecodable() - preserved; got != 2 {
		t.Fatalf("after the second pass CompactPreservedUndecodable rose by %d in all, want 2 (the inline record once, the interned one once)", got)
	}
	if len(*reports) != 2 || (*reports)[1].Key != string(badInterned) {
		t.Fatalf("reports = %+v, want a second for %q", *reports, badInterned)
	}
	checkAll("after the second compaction")
	seg, w = preservedRecord(t, c, badInterned)
	h := seg.dict.Load()
	if h == nil || !bytes.Equal(w, badInternedWire) || !slices.Equal(dictNames(h), srcNames) {
		t.Fatalf("preserved interned record: bytes %x, dictionary %v; want %x under %v", w, h != nil, badInternedWire, srcNames)
	}
	if _, w = preservedRecord(t, c, badInline); !bytes.Equal(w, undecodableInline) {
		t.Fatalf("the inline record changed on its second copy: %x", w)
	}

	// Reopen: both still there, unchanged, still refused.
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if c, err = Open(opts); err != nil {
		t.Fatal(err)
	}
	checkAll("after reopen")
	for key, want := range map[string][]byte{string(badInline): undecodableInline, string(badInterned): badInternedWire} {
		if _, w = preservedRecord(t, c, []byte(key)); !bytes.Equal(w, want) {
			t.Errorf("after reopen %q is %x, want %x", key, w, want)
		}
		if _, ok := c.Get([]byte(key)); ok {
			t.Errorf("after reopen %q reads", key)
		}
	}
}

// TestCompactBlockedByUnreadableSegment: a record whose bytes cannot be read at all -- here, every
// record of a segment whose dictionary failed its checksum -- cannot be copied with what it needs, so
// the shard is left uncompacted. That is reported once, and later passes leave the shard alone instead
// of sealing its active segment and walking every source again to stop at the same record.
//
// Not parallel: it replaces CompactUndecodableHook and reads process-wide counters.
func TestCompactBlockedByUnreadableSegment(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	opts := Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 600 {
		if perr := c.Put(fmt.Appendf(nil, "k%d", i), mustAd(t, fmt.Sprintf(`[Id = %d; Owner = "u%d"]`, i, i%3))); perr != nil {
			t.Fatal(perr)
		}
	}
	c.compactShard(c.shards[0], c.currentCodec())
	sh := c.shards[0]
	sh.mu.RLock()
	var path string
	var dictOff uint32
	for _, s := range sh.segs {
		if s != nil && s.dict.Load() != nil {
			path, dictOff = s.path, s.dictRecOff()
			break
		}
	}
	sh.mu.RUnlock()
	if path == "" {
		t.Fatal("setup: compaction interned no segment")
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	flipBit(t, path, int(dictOff)+recKeyOff+4+segDictHeaderBytes+8)
	_ = os.Remove(filepath.Join(filepath.Dir(path), dirSnapName))

	reports := captureUndecodable(t)
	if c, err = Open(opts); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	sh = c.shards[0]
	if err = c.Put([]byte("fresh"), mustAd(t, `[Id = 9999]`)); err != nil {
		t.Fatal(err)
	}

	blocked := CompactBlockedSegments()
	c.compactShard(sh, c.currentCodec())
	if got := CompactBlockedSegments() - blocked; got != 1 || len(*reports) != 1 || !(*reports)[0].Blocked {
		t.Fatalf("first pass: CompactBlockedSegments rose by %d, reports %+v; want 1 and one blocked report", got, *reports)
	}
	// The shard was left as it was, apart from its active segment, which the pass sealed.
	if err = c.Put([]byte("fresh2"), mustAd(t, `[Id = 10000]`)); err != nil {
		t.Fatal(err)
	}
	sh.mu.RLock()
	act := sh.act
	sh.mu.RUnlock()
	if act == nil {
		t.Fatal("setup: no active segment after a write")
	}

	c.compactShard(sh, c.currentCodec())
	sh.mu.RLock()
	stillActive := sh.act == act
	sh.mu.RUnlock()
	if !stillActive {
		t.Error("the second pass went ahead with a shard the first could not compact (it sealed the active segment)")
	}
	if got := CompactBlockedSegments() - blocked; got != 1 || len(*reports) != 1 {
		t.Errorf("second pass: CompactBlockedSegments rose by %d in all, %d reports; want 1 and 1", got, len(*reports))
	}
	for _, k := range []string{"fresh", "fresh2"} {
		if _, ok := c.Get([]byte(k)); !ok {
			t.Errorf("%s does not read", k)
		}
	}
}
