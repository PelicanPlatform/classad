package collections

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"maps"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// segCountersByName returns every live segment's scan-pruning counters, keyed by shard and file
// name (stable across a reopen, unlike a segment's id).
func segCountersByName(c *Collection) map[string]segBounds {
	out := map[string]segBounds{}
	for i, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil {
				out[fmt.Sprintf("%d/%s", i, filepath.Base(seg.path))] = segBounds{seg.minSeq, seg.maxSeq, seg.maxSup, seg.dead}
			}
		}
		sh.mu.RUnlock()
	}
	return out
}

// openCountingSnapshots opens opts and reports how many shards restored their directory from a
// snapshot and how many rebuilt it by walking their records.
func openCountingSnapshots(t *testing.T, opts Options) (c *Collection, restored, rebuilt int) {
	t.Helper()
	onDirSnapLoad = func(ok bool) {
		if ok {
			restored++
		} else {
			rebuilt++
		}
	}
	defer func() { onDirSnapLoad = nil }()
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c, restored, rebuilt
}

// getAll reads every key below n, rendering each ad (or its absence) for comparison.
func getAll(c *Collection, n int) []string {
	out := make([]string, n)
	for i := range n {
		if ad, ok := c.Get([]byte(fmt.Sprintf("%d.0", i))); ok {
			out[i] = ad.String()
		} else {
			out[i] = "<absent>"
		}
	}
	return out
}

// staleDirEntries counts directory entries that do not name a record of their bucket.
func staleDirEntries(c *Collection) int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for h, l := range sh.dir {
			if int(l.seg) >= len(sh.segs) || !dirEntryValid(sh.segs[l.seg], l.off, h, c.h) {
				n++
			}
		}
		sh.mu.RUnlock()
	}
	return n
}

// TestDirSnapshotReopenMatchesWalk runs randomized histories -- puts, patches (delta records),
// rewrites, deletes, compaction, reindexing, columnarizing -- then closes cleanly and reopens. The
// reopen must restore every shard from its snapshot, restore each segment's counters to the values
// the closing process held, and those must equal what a full record walk derives from the same
// segments. Every key reads back the same through each reopen.
func TestDirSnapshotReopenMatchesWalk(t *testing.T) {
	configs := map[string]func(dir string) Options{
		"mutable": func(dir string) Options {
			return Options{Dir: dir, Shards: 2, SegmentSize: 1 << 14, DeltaMax: 8, GroupSchemaCount: -1}
		},
		"append-only": func(dir string) Options {
			return Options{Dir: dir, Shards: 2, SegmentSize: 1 << 13, AppendOnly: true, GroupSchemaCount: -1}
		},
	}
	for name, mk := range configs {
		for seed := int64(1); seed <= 6; seed++ {
			t.Run(fmt.Sprintf("%s/seed%d", name, seed), func(t *testing.T) {
				dir := t.TempDir()
				opts := mk(dir)
				c, err := Open(opts)
				if err != nil {
					t.Fatal(err)
				}
				rng := rand.New(rand.NewSource(seed))
				now := time.Now().Unix()
				next := 0
				for ; next < 200; next++ {
					if err := c.Put([]byte(fmt.Sprintf("%d.0", next)), seqFixtureAd(next, now, rng)); err != nil {
						t.Fatal(err)
					}
				}
				for range 300 {
					seqWriter(t, c, rng, &next, now)
					switch rng.Intn(40) {
					case 0:
						c.Compact()
					case 1:
						c.Reindex()
					case 2:
						c.BuildAndEnableSchemaScan(4096, 8)
						c.ColumnarizeSealed()
					}
				}
				// End on a columnarize, which leaves a deleted key's directory head stale.
				c.BuildAndEnableSchemaScan(4096, 8)
				c.ColumnarizeSealed()
				before := segCountersByName(c)
				ads := getAll(c, next)
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}

				c, restored, rebuilt := openCountingSnapshots(t, opts)
				if restored != len(c.shards) || rebuilt != 0 {
					c.Close()
					t.Fatalf("clean reopen: %d shards restored from the snapshot, %d rebuilt; want all restored", restored, rebuilt)
				}
				fromSnap := segCountersByName(c)
				gotAds := getAll(c, next)
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				if !maps.Equal(fromSnap, before) {
					t.Errorf("counters restored from the snapshot differ from the closing process's:\n%s", diffCounters(before, fromSnap))
				}
				if !slices.Equal(gotAds, ads) {
					t.Errorf("reads differ after the snapshot reopen")
				}

				removeDirSnapshots(t, dir)
				c, restored, rebuilt = openCountingSnapshots(t, opts)
				defer c.Close()
				if rebuilt != len(c.shards) || restored != 0 {
					t.Fatalf("reopen without snapshots: %d restored, %d rebuilt", restored, rebuilt)
				}
				walked := segCountersByName(c)
				if opts.AppendOnly {
					// An append-only walk derives sequence bounds only; dead bytes and supersession
					// are what the process kept (keyless records), which the walk never derives.
					for k, b := range walked {
						s := fromSnap[k]
						b.maxSup, b.dead = s.maxSup, s.dead
						walked[k] = b
					}
				}
				if !maps.Equal(walked, fromSnap) {
					t.Errorf("counters restored from the snapshot differ from a full walk's:\n%s", diffCounters(walked, fromSnap))
				}
			})
		}
	}
}

func diffCounters(want, got map[string]segBounds) string {
	var s string
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			s += fmt.Sprintf("  %s: want %+v, got %+v (present %v)\n", k, w, g, ok)
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			s += fmt.Sprintf("  %s: unexpected %+v\n", k, g)
		}
	}
	return s
}

// TestDirSnapshotReopenKeepsGarbageAndSkips: garbage written before a clean restart still drives
// compaction and dead-segment reclaim after it, and a segment with nothing visible is still skipped.
func TestDirSnapshotReopenKeepsGarbageAndSkips(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Dir: dir, Shards: 1, SegmentSize: 1 << 13}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	put := func(i, gen int) {
		t.Helper()
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; Gen=%d; Pad="%0120d" ]`, i, gen, i))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	// Two full generations, then delete three keys in four: the first generation's segments are
	// fully dead (reclaimable, nothing visible now) and the second's are three-quarters dead, which
	// puts the shard over the compaction threshold.
	for gen := range 2 {
		for i := range 400 {
			put(i, gen)
		}
	}
	for i := range 400 {
		if i%4 != 0 {
			c.Delete([]byte(fmt.Sprintf("%d.0", i)))
		}
	}
	type state struct {
		dead              int64
		compact, reclaim  bool
		hidden, unbounded int
		commitSeq         uint64
	}
	observe := func() state {
		sh := c.shards[0]
		sh.mu.RLock()
		defer sh.mu.RUnlock()
		s := state{dead: c.Stats().DeadBytes, compact: sh.shouldCompact(), reclaim: sh.hasReclaimableSegment(), commitSeq: sh.commitSeq}
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 {
				continue
			}
			if !seg.visibleAt(sh.commitSeq) {
				s.hidden++
			}
			if seg.maxSeq == 0 {
				s.unbounded++
			}
		}
		return s
	}
	before := observe()
	if before.dead == 0 || !before.compact || !before.reclaim || before.hidden == 0 || before.unbounded != 0 {
		t.Fatalf("fixture: want garbage, a compaction trigger, a reclaimable and a skippable segment: %+v", before)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, restored, _ := openCountingSnapshots(t, opts)
	defer func() { c.Close() }()
	if restored != 1 {
		t.Fatalf("clean reopen did not restore from the snapshot")
	}
	if after := observe(); after != before {
		t.Fatalf("after a clean reopen:\n got  %+v\n want %+v", after, before)
	}
	// And the decisions act on it: compaction reclaims the pre-restart garbage.
	c.Compact()
	if got := c.Stats().DeadBytes; got >= before.dead/2 {
		t.Fatalf("compaction after the reopen left %d dead bytes of %d", got, before.dead)
	}
	for i := range 400 {
		ad, ok := c.Get([]byte(fmt.Sprintf("%d.0", i)))
		if ok != (i%4 == 0) {
			t.Fatalf("key %d after compaction: present=%v", i, ok)
		}
		if !ok {
			continue
		}
		if gen, _ := ad.EvaluateAttrInt("Gen"); gen != 1 {
			t.Fatalf("key %d after compaction: Gen=%d", i, gen)
		}
	}
}

// TestDirSnapshotAcceptedAfterSegmentRewrite: deleting a key whose current record is a delta leaves
// that delta as its bucket's head, and columnarizing the segment drops superseded deltas and moves
// every other offset; when nothing else of the key is in that segment the head is not evicted, so
// the directory names bytes that are no longer that record. The snapshot
// leaves such heads out, and the reopen takes it.
func TestDirSnapshotAcceptedAfterSegmentRewrite(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14, DeltaMax: 8, GroupSchemaCount: -1}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(3))
	now := time.Now().Unix()
	const n = 600
	for i := range n / 2 {
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), seqFixtureAd(i, now, rng)); err != nil {
			t.Fatal(err)
		}
	}
	// Patch then delete every seventh key: the delta lands in a later segment than the key's whole
	// record, so once that segment is columnarized nothing of the key is left in it.
	for i := 0; i < n/2; i += 7 {
		patch := classad.New()
		patch.InsertAttr("JobStatus", 4)
		tx := c.Begin()
		tx.PatchAttrs([]byte(fmt.Sprintf("%d.0", i)), patch, nil)
		if r := tx.Commit(); r.Committed != 1 {
			t.Fatalf("patch not committed: %+v", r)
		}
		c.Delete([]byte(fmt.Sprintf("%d.0", i)))
	}
	for i := n / 2; i < n; i++ { // seal the segments holding those deltas
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), seqFixtureAd(i, now, rng)); err != nil {
			t.Fatal(err)
		}
	}
	c.BuildAndEnableSchemaScan(4096, 8) // columnarizes every sealed segment
	if columnarSegmentsForTest(c) == 0 {
		t.Fatal("fixture: nothing columnarized")
	}
	if staleDirEntries(c) == 0 {
		t.Fatal("fixture: no directory head went stale, so this does not test the rewrite case")
	}
	ads := getAll(c, n)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, restored, rebuilt := openCountingSnapshots(t, opts)
	defer c.Close()
	if restored != 1 || rebuilt != 0 {
		t.Fatalf("reopen after a columnarize: restored=%d rebuilt=%d, want the snapshot", restored, rebuilt)
	}
	if got := getAll(c, n); !slices.Equal(got, ads) {
		t.Fatalf("reads differ after the reopen")
	}
	if want := n - (n/2+6)/7; c.Len() != want {
		t.Fatalf("Len=%d, want %d", c.Len(), want)
	}
}

// TestDirSnapshotAcceptedOutOfSeqOrder: a compaction that runs while writes continue appends its
// destination segment -- the oldest records -- after the segment those writes went to, so the
// closing process holds its segments out of the first-sequence order a reopen loads them in. The
// snapshot names segments by file, not position, and is taken.
func TestDirSnapshotAcceptedOutOfSeqOrder(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Dir: dir, Shards: 1, SegmentSize: 1 << 13}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	put := func(i, gen int) {
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; Gen=%d; Pad="%080d" ]`, i, gen, i))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	// Three keys in four deleted: over the compaction threshold, with no segment fully dead (a
	// shard of only those is reclaimed rather than compacted).
	const n = 600
	for i := range n {
		put(i, 0)
	}
	for i := range n {
		if i%4 != 0 {
			c.Delete([]byte(fmt.Sprintf("%d.0", i)))
		}
	}
	compactPhase2Hook = func([]*segment) {
		for i := n; i < n+50; i++ {
			put(i, 0) // lands in a post-barrier segment, before the destination in segment order
		}
	}
	c.Compact()
	compactPhase2Hook = nil
	sh := c.shards[0]
	var firsts []uint64
	for _, seg := range sh.segs {
		if f, ok := segFirstSeq(seg); ok {
			firsts = append(firsts, f)
		}
	}
	if slices.IsSorted(firsts) {
		t.Fatalf("fixture: segments are in first-sequence order %v; the compaction did not interleave", firsts)
	}
	put(0, 9) // and some garbage in the destination
	before := segCountersByName(c)
	ads := getAll(c, n+50)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, restored, _ := openCountingSnapshots(t, opts)
	defer c.Close()
	if restored != 1 {
		t.Fatal("reopen after an interleaved compaction did not restore from the snapshot")
	}
	if got := segCountersByName(c); !maps.Equal(got, before) {
		t.Fatalf("counters differ:\n%s", diffCounters(before, got))
	}
	if got := getAll(c, n+50); !slices.Equal(got, ads) {
		t.Fatal("reads differ after the reopen")
	}
}

// rewriteDirSnapshot decodes shard 0's snapshot under dir, lets edit change it, and writes it back
// with a fresh CRC -- a well-formed snapshot carrying whatever edit put in it.
func rewriteDirSnapshot(t *testing.T, dir string, edit func(hdr []byte, segs []snapSegRec, ents []byte) ([]byte, []snapSegRec, []byte)) {
	t.Helper()
	path := filepath.Join(dir, "0", dirSnapName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := data[:len(data)-4]
	r := snapReader{b: body}
	r.u32()
	if v := r.u32(); v != dirSnapVersion {
		t.Fatalf("snapshot version %d", v)
	}
	r.u64()
	r.u64()
	nSegs := int(r.u32())
	hdr := append([]byte(nil), body[:r.pos]...)
	var segs []snapSegRec
	for range nSegs {
		s := snapSegRec{used: r.u64(), name: r.str16()}
		s.b = segBounds{r.u64(), r.u64(), r.u64(), int64(r.u64())}
		segs = append(segs, s)
	}
	ents := append([]byte(nil), body[r.pos:]...)
	if r.err {
		t.Fatal("snapshot did not decode")
	}
	hdr, segs, ents = edit(hdr, segs, ents)
	out := hdr
	for _, s := range segs {
		out = binary.LittleEndian.AppendUint64(out, s.used)
		out = binary.LittleEndian.AppendUint16(out, uint16(len(s.name)))
		out = append(out, s.name...)
		if binary.LittleEndian.Uint32(hdr[4:]) != dirSnapVersionV1 {
			out = binary.LittleEndian.AppendUint64(out, s.b.minSeq)
			out = binary.LittleEndian.AppendUint64(out, s.b.maxSeq)
			out = binary.LittleEndian.AppendUint64(out, s.b.maxSup)
			out = binary.LittleEndian.AppendUint64(out, uint64(s.b.dead))
		}
	}
	out = append(out, ents...)
	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

type snapSegRec struct {
	used uint64
	name string
	b    segBounds
}

// garbageFixture writes a single-shard store with rewrites and deletes and closes it, returning its
// options and the counters the closing process held.
func garbageFixture(t *testing.T) (Options, map[string]segBounds, []string) {
	t.Helper()
	opts := Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 13}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	for gen := range 3 {
		for i := range 120 {
			ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; Gen=%d; Pad="%060d" ]`, i, gen, i))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 120; i += 5 {
		c.Delete([]byte(fmt.Sprintf("%d.0", i)))
	}
	before := segCountersByName(c)
	ads := getAll(c, 120)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return opts, before, ads
}

// TestDirSnapshotV1StillRestores: a snapshot written before the counters were carried is still
// taken, and the counters are derived from the record headers -- equal to the closing process's.
func TestDirSnapshotV1StillRestores(t *testing.T) {
	opts, before, ads := garbageFixture(t)
	rewriteDirSnapshot(t, opts.Dir, func(hdr []byte, segs []snapSegRec, ents []byte) ([]byte, []snapSegRec, []byte) {
		binary.LittleEndian.PutUint32(hdr[4:], dirSnapVersionV1)
		return hdr, segs, ents
	})
	if cs, ok := fsckCommitSeq(opts.Dir); !ok || cs == 0 {
		t.Fatalf("fsck does not read a version 1 snapshot: %d %v", cs, ok)
	}
	c, restored, _ := openCountingSnapshots(t, opts)
	defer c.Close()
	if restored != 1 {
		t.Fatal("a version 1 snapshot was not restored")
	}
	if got := segCountersByName(c); !maps.Equal(got, before) {
		t.Fatalf("counters after a version 1 restore:\n%s", diffCounters(before, got))
	}
	if got := getAll(c, 120); !slices.Equal(got, ads) {
		t.Fatal("reads differ")
	}
}

// TestFsckReadsDirSnapshotV2: fsck's supersession bound comes from the snapshot header, which it must
// still recognise in the current version.
func TestFsckReadsDirSnapshotV2(t *testing.T) {
	opts, _, _ := garbageFixture(t)
	if cs, ok := fsckCommitSeq(opts.Dir); !ok || cs == 0 {
		t.Fatalf("fsck does not read a current snapshot: %d %v", cs, ok)
	}
}

// TestDirSnapshotRefusesInconsistent: a well-formed snapshot (valid CRC, matching segment set) whose
// contents do not describe the segments is refused, and the reopen walks the records instead.
func TestDirSnapshotRefusesInconsistent(t *testing.T) {
	cases := map[string]func(hdr []byte, segs []snapSegRec, ents []byte) ([]byte, []snapSegRec, []byte){
		"dead past the extent": func(hdr []byte, segs []snapSegRec, ents []byte) ([]byte, []snapSegRec, []byte) {
			segs[0].b.dead = int64(segs[0].used) + 8
			return hdr, segs, ents
		},
		"sequence past the commit": func(hdr []byte, segs []snapSegRec, ents []byte) ([]byte, []snapSegRec, []byte) {
			segs[0].b.maxSeq = binary.LittleEndian.Uint64(hdr[8:]) + 1
			return hdr, segs, ents
		},
		"supersession past the commit": func(hdr []byte, segs []snapSegRec, ents []byte) ([]byte, []snapSegRec, []byte) {
			segs[0].b.maxSup = binary.LittleEndian.Uint64(hdr[8:]) + 1
			return hdr, segs, ents
		},
		"entry names another bucket's record": func(hdr []byte, segs []snapSegRec, ents []byte) ([]byte, []snapSegRec, []byte) {
			// Entries follow their count: hash(8) slot(4) off(4). Give the first one the second's hash.
			copy(ents[8:16], ents[8+16:16+16])
			return hdr, segs, ents
		},
		"entry inside a record": func(hdr []byte, segs []snapSegRec, ents []byte) ([]byte, []snapSegRec, []byte) {
			off := binary.LittleEndian.Uint32(ents[8+12:])
			binary.LittleEndian.PutUint32(ents[8+12:], off+8)
			return hdr, segs, ents
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			opts, before, ads := garbageFixture(t)
			rewriteDirSnapshot(t, opts.Dir, edit)
			c, restored, rebuilt := openCountingSnapshots(t, opts)
			defer c.Close()
			if restored != 0 || rebuilt != 1 {
				t.Fatalf("inconsistent snapshot: restored=%d rebuilt=%d, want it refused", restored, rebuilt)
			}
			if got := segCountersByName(c); !maps.Equal(got, before) {
				t.Fatalf("counters after the walk:\n%s", diffCounters(before, got))
			}
			if got := getAll(c, 120); !slices.Equal(got, ads) {
				t.Fatal("reads differ")
			}
		})
	}
}

// TestDirSnapshotSecondCloseKeepsNewerSnapshot: closing a collection twice, with another Open and
// Close of the same store in between, leaves the newer snapshot in place -- the second Close has
// unmapped segments and a stale directory -- and does not read the unmapped segments.
func TestDirSnapshotSecondCloseKeepsNewerSnapshot(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Dir: dir, Shards: 1, SegmentSize: 1 << 12}
	put := func(c *Collection, i int) {
		t.Helper()
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; Pad="%060d" ]`, i, i))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c1, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		put(c1, i)
	}
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 100; i < 200; i++ {
		put(c2, i)
	}
	if err := c2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}
	c3, restored, _ := openCountingSnapshots(t, opts)
	defer c3.Close()
	if restored != 1 {
		t.Fatal("the newer snapshot was not restored")
	}
	if c3.Len() != 200 {
		t.Fatalf("Len=%d, want 200", c3.Len())
	}
}
