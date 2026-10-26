package collections

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// recoveryReads reads every key below n, rendering each ad (or its absence).
func recoveryReads(c *Collection, n int) []string {
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

// diffReads reports the keys whose reads differ, with the first few values.
func diffReads(want, got []string) string {
	var s string
	n := 0
	for i := range want {
		if want[i] != got[i] {
			if n < 3 {
				s += fmt.Sprintf("\n  %d.0:\n    want %s\n    got  %s", i, want[i], got[i])
			}
			n++
		}
	}
	return fmt.Sprintf("%d keys differ%s", n, s)
}

func openOrFatal(t *testing.T, opts Options) *Collection {
	t.Helper()
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// openCountingRepairs opens opts and returns the collection with the chain links Open repaired.
func openCountingRepairs(t *testing.T, opts Options) (*Collection, int) {
	t.Helper()
	var repaired int
	OpenIndexDiagHook = func(d OpenIndexDiag) { repaired = d.ChainLinksRepaired }
	defer func() { OpenIndexDiagHook = nil }()
	return openOrFatal(t, opts), repaired
}

// TestDeltaCrashRecoveryKeepsMiddleDeltas: a key whose whole record sits in a sealed segment and
// whose two deltas sit in the write target. Recovering without a directory snapshot relinked the
// current delta to nothing, so the read merged the base with the newest delta only and returned
// the value the middle delta had overwritten. The relink was written into the file, so a later
// clean reopen read the same wrong value.
func TestDeltaCrashRecoveryKeepsMiddleDeltas(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14, DeltaMax: 8, GroupSchemaCount: -1}
	c := openOrFatal(t, opts)
	rng := rand.New(rand.NewSource(1))
	now := time.Now().Unix()
	next := 0
	for ; next < 200; next++ {
		if err := c.Put([]byte(fmt.Sprintf("%d.0", next)), seqFixtureAd(next, now, rng)); err != nil {
			t.Fatal(err)
		}
	}
	for range 300 {
		seqWriter(t, c, rng, &next, now)
	}
	want := recoveryReads(c, next)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	removeDirSnapshots(t, dir)
	c, repaired := openCountingRepairs(t, opts)
	if got := recoveryReads(c, next); !slices.Equal(got, want) {
		t.Fatalf("after a recovery without a snapshot: %s", diffReads(want, got))
	}
	if repaired != 0 {
		t.Errorf("recovery of a store this build wrote repaired %d chain links, want 0", repaired)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openOrFatal(t, opts) // from the snapshot: what the recovery left in the files
	defer c.Close()
	if got := recoveryReads(c, next); !slices.Equal(got, want) {
		t.Fatalf("after the clean reopen that followed: %s", diffReads(want, got))
	}
}

// pageCacheImage copies a live store's files: what a crash at this instant leaves on disk, with every
// write that returned in the page cache. There is no directory snapshot in it (Open consumed it).
func pageCacheImage(t *testing.T, c *Collection, dir string) string {
	t.Helper()
	for _, sh := range c.shards {
		sh.mu.RLock() // no write lands mid-copy
	}
	img := copyTree(t, dir)
	for _, sh := range c.shards {
		sh.mu.RUnlock()
	}
	return img
}

// recLoc names a record by its file and offset, which a crash image shares with the live store.
type recLoc struct {
	path string
	off  uint32
	tl   uint32
	last bool // the last record written to its segment
}

// currentRecord locates key's current record in the live store.
func currentRecord(c *Collection, key string) (recLoc, bool) {
	h := c.h.Hash([]byte(key))
	sh := c.shards[c.shardOf([]byte(key), h)]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	l, ok := sh.findCurrent(sh.dirGet(h), []byte(key))
	if !ok {
		if l, ok = sh.lookupSealed([]byte(key), h); !ok {
			return recLoc{}, false
		}
	}
	seg := sh.segs[l.seg]
	tl := recTotalLen(seg.data, l.off)
	return recLoc{seg.path, l.off, tl, int(l.off+tl) == seg.used}, true
}

// imagePath maps a live segment path into a crash image of the store at dir.
func imagePath(t *testing.T, dir, img, path string) string {
	t.Helper()
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(img, rel)
}

// tearWrite turns a crash image into one taken while key's last write was being made: the new
// record's second half never reached the disk (its checksum fails, so recovery stops there), and
// neither did the supersession it stamped on the key's previous version.
func tearWrite(t *testing.T, dir, img string, rec, prev recLoc) {
	t.Helper()
	f, err := os.OpenFile(imagePath(t, dir, img, rec.path), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt(make([]byte, rec.tl/2), int64(rec.off+rec.tl/2)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	f, err = os.OpenFile(imagePath(t, dir, img, prev.path), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], seqMax)
	if _, err = f.WriteAt(b[:], int64(prev.off+recSupOff)); err != nil {
		t.Fatal(err)
	}
}

// TestDeltaCrashRecoveryDifferential runs random histories with delta records -- patches, whole
// rewrites, deletes, and maintenance (compaction, which interns, reindexing, columnarizing) -- and
// takes crash images along the way, some torn in the middle of a write. Every key of every image,
// recovered without a snapshot, must read exactly as the process read it at that instant (for a
// torn write, as it read before the write). The same holds for the image reopened with time travel
// on, and for the store's own recovery after the run.
func TestDeltaCrashRecoveryDifferential(t *testing.T) {
	t.Parallel()
	maintain := map[string]bool{"writes-only": false, "with-maintenance": true}
	for name, maint := range maintain {
		for seed := int64(1); seed <= 4; seed++ {
			t.Run(fmt.Sprintf("%s/seed%d", name, seed), func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				opts := Options{Dir: dir, Shards: 2, SegmentSize: 1 << 14, DeltaMax: 8, GroupSchemaCount: -1}
				c := openOrFatal(t, opts)
				defer func() { c.Close() }()
				rng := rand.New(rand.NewSource(seed))
				now := time.Now().Unix()
				next := 0
				for ; next < 150; next++ {
					if err := c.Put([]byte(fmt.Sprintf("%d.0", next)), seqFixtureAd(next, now, rng)); err != nil {
						t.Fatal(err)
					}
				}
				type image struct {
					dir  string
					want []string
					what string
				}
				var images []image
				for step := range 360 {
					if step%90 == 89 {
						if step%180 == 179 {
							// Tear a patch of an existing key: the image is taken just after it.
							key := fmt.Sprintf("%d.0", rng.Intn(next))
							before := recoveryReads(c, next)
							prev, hadPrev := currentRecord(c, key)
							patch := classad.New()
							patch.InsertAttr("JobStatus", int64(7))
							tx := c.Begin()
							tx.PatchAttrs([]byte(key), patch, nil)
							tx.Commit()
							rec, ok := currentRecord(c, key)
							img := pageCacheImage(t, c, dir)
							if ok && hadPrev && rec.last && rec != prev {
								tearWrite(t, dir, img, rec, prev)
								images = append(images, image{img, before, fmt.Sprintf("torn patch of %s at step %d", key, step)})
							}
						} else {
							images = append(images, image{pageCacheImage(t, c, dir), recoveryReads(c, next), fmt.Sprintf("step %d", step)})
						}
					}
					seqWriter(t, c, rng, &next, now)
					if maint {
						switch rng.Intn(30) {
						case 0:
							c.Compact()
						case 1:
							c.Reindex()
						case 2:
							c.BuildAndEnableSchemaScan(4096, 8)
						}
					}
				}
				final := recoveryReads(c, next)
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				removeDirSnapshots(t, dir)
				images = append(images, image{dir, final, "after the run"})
				for _, im := range images {
					o := opts
					o.Dir = im.dir
					r := openOrFatal(t, o)
					got := recoveryReads(r, len(im.want))
					if err := r.Close(); err != nil {
						t.Fatal(err)
					}
					if !slices.Equal(got, im.want) {
						t.Errorf("recovered image (%s): %s", im.what, diffReads(im.want, got))
						continue
					}
					// Time travel forces the record walk on every open, and refuses new deltas but
					// must still read the ones on disk.
					removeDirSnapshots(t, im.dir)
					o.TimeTravel = &TimeTravelOptions{MaxDistance: time.Hour}
					r = openOrFatal(t, o)
					got = recoveryReads(r, len(im.want))
					r.Close()
					if !slices.Equal(got, im.want) {
						t.Errorf("recovered image (%s) with time travel: %s", im.what, diffReads(im.want, got))
					}
				}
			})
		}
	}
}

// TestDeltaRecoveryRepairsCutChains: a store an earlier build recovered has the links between its
// write target's records cut, and a clean Close then writes a snapshot over it, so the next open
// does not walk the records. That open must still read every key right, and report the repair.
func TestDeltaRecoveryRepairsCutChains(t *testing.T) {
	for _, viaSnapshot := range []bool{true, false} {
		t.Run(fmt.Sprintf("snapshot=%v", viaSnapshot), func(t *testing.T) {
			dir := t.TempDir()
			opts := Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14, DeltaMax: 8, GroupSchemaCount: -1}
			c := openOrFatal(t, opts)
			rng := rand.New(rand.NewSource(5))
			now := time.Now().Unix()
			const n = 200
			for i := range n {
				if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), seqFixtureAd(i, now, rng)); err != nil {
					t.Fatal(err)
				}
			}
			// Two patches to each of a few keys whose whole record is sealed by now: both deltas
			// land in the write target, the second linked to the first.
			for round := range 2 {
				for i := 0; i < 60; i += 3 {
					patch := classad.New()
					patch.InsertAttr("RequestMemory", int64(1000*round+i))
					tx := c.Begin()
					tx.PatchAttrs([]byte(fmt.Sprintf("%d.0", i)), patch, nil)
					if r := tx.Commit(); r.Committed != 1 {
						t.Fatalf("patch not committed: %+v", r)
					}
				}
			}
			want := recoveryReads(c, n)
			// What the earlier recovery left: every link out of a write-target record cut.
			sh := c.shards[0]
			sh.mu.Lock()
			act := sh.act
			cut := 0
			for off := 0; off < act.used; {
				o := uint32(off)
				tl := recTotalLen(act.data, o)
				if tl == 0 {
					break
				}
				if !recIsMarker(act.data, o) && recNext(act.data, o).seg == act.id {
					setRecNext(act.data, o, noLoc)
					cut++
				}
				off += int(tl)
			}
			sh.mu.Unlock()
			if cut == 0 {
				t.Fatal("fixture: no chain inside the write target to cut")
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if !viaSnapshot {
				removeDirSnapshots(t, dir)
			}
			c, repaired := openCountingRepairs(t, opts)
			defer c.Close()
			if got := recoveryReads(c, n); !slices.Equal(got, want) {
				t.Fatalf("reopen of a store with cut chains: %s", diffReads(want, got))
			}
			if repaired != cut {
				t.Errorf("ChainLinksRepaired=%d, want %d", repaired, cut)
			}
		})
	}
}

// TestDeltaReadFindsUnlinkedActiveVersion: a delta version the chain does not reach is still found,
// because the versions it did reach do not join up. Cut while the store is open, so no Open repairs
// it first.
func TestDeltaReadFindsUnlinkedActiveVersion(t *testing.T) {
	dir := t.TempDir()
	c := openOrFatal(t, Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14, DeltaMax: 8, GroupSchemaCount: -1})
	defer c.Close()
	rng := rand.New(rand.NewSource(9))
	now := time.Now().Unix()
	for i := range 200 {
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), seqFixtureAd(i, now, rng)); err != nil {
			t.Fatal(err)
		}
	}
	for round := range 2 {
		patch := classad.New()
		patch.InsertAttr("RequestMemory", int64(100+round))
		patch.InsertAttr(fmt.Sprintf("Round%d", round), int64(round))
		tx := c.Begin()
		tx.PatchAttrs([]byte("3.0"), patch, nil)
		if r := tx.Commit(); r.Committed != 1 {
			t.Fatalf("patch not committed: %+v", r)
		}
	}
	want := recoveryReads(c, 200)
	rec, _ := currentRecord(c, "3.0")
	sh := c.shards[0]
	sh.mu.Lock()
	if filepath.Base(rec.path) != filepath.Base(sh.act.path) || recNext(sh.act.data, rec.off).seg != sh.act.id {
		sh.mu.Unlock()
		t.Fatal("fixture: the current delta is not linked to an earlier one in the write target")
	}
	setRecNext(sh.act.data, rec.off, noLoc)
	sh.mu.Unlock()
	scans := deltaChainGapScans.Load()
	if got := recoveryReads(c, 200); !slices.Equal(got, want) {
		t.Fatalf("read with the chain cut: %s", diffReads(want, got))
	}
	if deltaChainGapScans.Load() == scans {
		t.Fatal("the gap was not what found the missing version")
	}
}

// TestRecoveryTimeIndexIgnoresPayloadMarkers: a columnarized segment's payload record is a marker
// too, but not a time checkpoint. Recovery read its body as one, putting sequence-0 checkpoints at
// arbitrary times into the time index of a time-travel collection.
func TestRecoveryTimeIndexIgnoresPayloadMarkers(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Dir: dir, Shards: 1, SegmentSize: 1 << 12,
		TimeTravel: &TimeTravelOptions{MaxDistance: time.Hour, CheckpointInterval: time.Millisecond}}
	c := openOrFatal(t, opts)
	for g := range 4 {
		for i := range 60 {
			ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; G=%d; Owner="u%d" ]`, i, g, i%3))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Microsecond)
		}
		c.Compact()
	}
	c.BuildAndEnableSchemaScan(4096, 8)
	if columnarSegmentsForTest(c) == 0 {
		t.Fatal("fixture: nothing columnarized")
	}
	// Checkpoints in the write target, which no rewrite above has touched: the recovered index
	// must hold these and nothing else.
	for i := range 20 {
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; G=9 ]`, i))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openOrFatal(t, opts) // time travel: the record walk rebuilds the time index
	defer c.Close()
	sh := c.shards[0]
	if len(sh.tseq.entries) == 0 {
		t.Fatal("no checkpoints recovered")
	}
	lo := uint64(time.Now().Add(-time.Hour).UnixMilli())
	for i, e := range sh.tseq.entries {
		if e.seq == 0 || e.millis < lo {
			t.Fatalf("checkpoint %d is not one the collection wrote: %+v", i, e)
		}
	}
}

// TestResealCarriesOnlyTimeMarkers: resealing an interned or columnarized segment does not carry its
// dictionary or columnar record across as a time checkpoint.
func TestResealCarriesOnlyTimeMarkers(t *testing.T) {
	dir := t.TempDir()
	c := openOrFatal(t, Options{Dir: dir, SegmentSize: 1 << 12, AppendOnly: true})
	defer c.Close()
	for i := range 400 {
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; Owner="u%d"; Cmd="/bin/x" ]`, i, i%5))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.InternSealed() // every sealed segment gains a dictionary record
	interned := 0
	for _, seg := range c.shards[0].segs {
		if seg != nil && seg.dict.Load() != nil {
			interned++
		}
	}
	if interned == 0 {
		t.Fatal("fixture: nothing interned")
	}
	c.Rewrite() // reseals every segment, the interned ones included
	c.Rewrite()
	junk := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			for off := 0; off < seg.used; {
				o := uint32(off)
				tl := recTotalLen(seg.data, o)
				if tl == 0 {
					break
				}
				if recIsTimeMarker(seg.data, o) && recSeq(seg.data, o) == 0 {
					junk++
				}
				off += int(tl)
			}
		}
		sh.mu.RUnlock()
	}
	if junk != 0 {
		t.Fatalf("%d sequence-0 time checkpoints carried across reseals", junk)
	}
	if c.Len() != 400 {
		t.Fatalf("Len=%d, want 400", c.Len())
	}
}
