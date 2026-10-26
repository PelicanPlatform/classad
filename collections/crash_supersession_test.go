package collections

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// stampReads reads every key below n, rendering each ad (or its absence).
func stampReads(c *Collection, n int) []string {
	out := make([]string, n)
	for i := range n {
		if ad, ok := c.Get([]byte(fmt.Sprintf("k%d", i))); ok {
			out[i] = ad.String()
		} else {
			out[i] = "<absent>"
		}
	}
	return out
}

func stampAd(t *testing.T, i, gen int) *classad.ClassAd {
	t.Helper()
	ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; Gen=%d; JobStatus=%d; Pad="%040d" ]`, i, gen, 1+gen%5, i))
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

// liveRecord locates key's current record in the live store: its file, offset and length.
func liveRecord(t *testing.T, c *Collection, key string) (path string, off, tl uint32) {
	t.Helper()
	h := c.h.Hash([]byte(key))
	sh := c.shards[c.shardOf([]byte(key), h)]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	l, ok := sh.findCurrent(sh.dirGet(h), []byte(key))
	if !ok {
		if l, ok = sh.lookupSealed([]byte(key), h); !ok {
			t.Fatalf("%s has no current record", key)
		}
	}
	seg := sh.segs[l.seg]
	return seg.path, l.off, recTotalLen(seg.data, l.off)
}

// lastMarker locates the newest delete marker in shard 0's write target.
func lastDeleteMarker(t *testing.T, c *Collection) (path string, off, tl uint32) {
	t.Helper()
	sh := c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	act := sh.act
	found := false
	for o := 0; o < act.used; {
		tl := recTotalLen(act.data, uint32(o))
		if tl == 0 {
			break
		}
		if _, ok := recDeleteMarkerKey(act.data, uint32(o)); ok {
			path, off, found = act.path, uint32(o), true
		}
		o += int(tl)
	}
	if !found {
		t.Fatal("no delete marker in the write target")
	}
	return path, off, recTotalLen(act.data, off)
}

// liveImage copies the store's files as they stand in the page cache: everything written, durable
// or not. A test then takes back what a crash would have lost.
func liveImage(t *testing.T, c *Collection, dir string) string {
	t.Helper()
	for _, sh := range c.shards {
		sh.mu.RLock()
	}
	img := copyTree(t, dir)
	for _, sh := range c.shards {
		sh.mu.RUnlock()
	}
	return img
}

// patchImage overwrites bytes of a file in an image, addressed by the live file's path.
func patchImage(t *testing.T, dir, img, livePath string, off int64, b []byte) {
	t.Helper()
	rel, err := filepath.Rel(dir, livePath)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(img, rel), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
}

// openRecovered opens an image without a directory snapshot (a crash leaves none) and returns it
// with what recovery reported.
func openRecovered(t *testing.T, opts Options) (*Collection, OpenIndexDiag) {
	t.Helper()
	var d OpenIndexDiag
	OpenIndexDiagHook = func(x OpenIndexDiag) { d = x }
	defer func() { OpenIndexDiagHook = nil }()
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c, d
}

// checkRecovered opens an image and compares every key with want, then again after a clean Close
// (from the directory snapshot) and with time travel on (which walks the records again).
func checkRecovered(t *testing.T, opts Options, n int, want []string) OpenIndexDiag {
	t.Helper()
	c, d := openRecovered(t, opts)
	if got := stampReads(c, n); !slices.Equal(got, want) {
		c.Close()
		t.Fatalf("recovered: %s", diffStampReads(want, got))
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	got := stampReads(c, n)
	c.Close()
	if !slices.Equal(got, want) {
		t.Fatalf("reopened from the snapshot: %s", diffStampReads(want, got))
	}
	tt := opts
	tt.TimeTravel = &TimeTravelOptions{MaxDistance: time.Hour}
	c, err = Open(tt)
	if err != nil {
		t.Fatal(err)
	}
	got = stampReads(c, n)
	c.Close()
	if !slices.Equal(got, want) {
		t.Fatalf("reopened with time travel: %s", diffStampReads(want, got))
	}
	return d
}

func diffStampReads(want, got []string) string {
	var s strings.Builder
	n := 0
	for i := range want {
		if want[i] != got[i] {
			if n < 3 {
				fmt.Fprintf(&s, "\n  k%d:\n    want %s\n    got  %s", i, want[i], got[i])
			}
			n++
		}
	}
	return fmt.Sprintf("%d keys differ%s", n, s.String())
}

// stampFixture opens a one-shard store under a fresh directory with n keys written durably.
func stampFixture(t *testing.T, n, deltaMax int) (*Collection, Options) {
	t.Helper()
	opts := Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 14, DeltaMax: deltaMax}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), stampAd(t, i, 0)); err != nil {
			t.Fatal(err)
		}
	}
	return c, opts
}

// TestCrashStampWithoutRecord: an overwrite's stamp on the key's old version reached the disk and
// its new record did not -- absent, or torn so it fails its checksum. Recovery must give the key
// its old value, not report it deleted.
func TestCrashStampWithoutRecord(t *testing.T) {
	for _, torn := range []bool{false, true} {
		t.Run(fmt.Sprintf("torn=%v", torn), func(t *testing.T) {
			const n = 40
			c, opts := stampFixture(t, n, 0)
			before := stampReads(c, n)
			if err := c.Put([]byte("k7"), stampAd(t, 7, 1)); err != nil {
				t.Fatal(err)
			}
			path, off, tl := liveRecord(t, c, "k7")
			img := liveImage(t, c, opts.Dir)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			from := off
			if torn {
				from = off + tl/2
			}
			patchImage(t, opts.Dir, img, path, int64(from), make([]byte, off+tl-from))
			o := opts
			o.Dir = img
			d := checkRecovered(t, o, n, before)
			if d.SupersessionsUndone != 1 {
				t.Errorf("SupersessionsUndone=%d, want 1", d.SupersessionsUndone)
			}
		})
	}
}

// TestCrashDeltaStampWithoutRecord: the same for a delta chain. The second delta is lost; the key
// reads as it did after the first.
func TestCrashDeltaStampWithoutRecord(t *testing.T) {
	const n = 40
	c, opts := stampFixture(t, n, 8)
	patch := func(v int64) {
		p := classad.New()
		p.InsertAttr("JobStatus", v)
		tx := c.Begin()
		tx.PatchAttrs([]byte("k7"), p, nil)
		if r := tx.Commit(); r.Committed != 1 {
			t.Fatalf("patch: %+v", r)
		}
	}
	patch(4)
	afterFirst := stampReads(c, n)
	patch(5)
	path, off, tl := liveRecord(t, c, "k7")
	img := liveImage(t, c, opts.Dir)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	patchImage(t, opts.Dir, img, path, int64(off), make([]byte, tl))
	o := opts
	o.Dir = img
	checkRecovered(t, o, n, afterFirst)
}

// TestCrashDelete: a delete is its stamp plus a delete marker in the record stream. With the stamp
// and no marker the delete did not survive; with the marker and no stamp it did, and is replayed.
func TestCrashDelete(t *testing.T) {
	for _, variant := range []string{"marker-lost", "stamp-lost", "both"} {
		t.Run(variant, func(t *testing.T) {
			const n = 40
			c, opts := stampFixture(t, n, 0)
			before := stampReads(c, n)
			oldPath, oldOff, _ := liveRecord(t, c, "k7")
			if !c.Delete([]byte("k7")) {
				t.Fatal("delete")
			}
			after := stampReads(c, n)
			mPath, mOff, mTL := lastDeleteMarker(t, c)
			img := liveImage(t, c, opts.Dir)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			want := after
			switch variant {
			case "marker-lost":
				patchImage(t, opts.Dir, img, mPath, int64(mOff), make([]byte, mTL))
				want = before
			case "stamp-lost":
				var b [8]byte
				binary.LittleEndian.PutUint64(b[:], seqMax)
				patchImage(t, opts.Dir, img, oldPath, int64(oldOff+recSupOff), b[:])
			}
			o := opts
			o.Dir = img
			d := checkRecovered(t, o, n, want)
			if variant == "stamp-lost" && d.DeletesReplayed != 1 {
				t.Errorf("DeletesReplayed=%d, want 1", d.DeletesReplayed)
			}
		})
	}
}

// TestCrashPartialTransaction: a multi-key commit cut off partway through its records. Every key
// whose record survived reads its new value; every other key its old one (an overwritten key is
// never lost, a deleted one comes back), and nothing else changes.
func TestCrashPartialTransaction(t *testing.T) {
	const n = 40
	c, opts := stampFixture(t, n, 0)
	before := stampReads(c, n)
	tx := c.Begin()
	for _, i := range []int{3, 11, 19, 27} {
		tx.Put([]byte(fmt.Sprintf("k%d", i)), stampAd(t, i, 2))
	}
	tx.Delete([]byte("k30"))
	if r := tx.Commit(); r.Committed != 5 {
		t.Fatalf("commit: %+v", r)
	}
	after := stampReads(c, n)
	// The commit's records in stream order: cut the stream after the second one.
	sh := c.shards[0]
	sh.mu.RLock()
	seq := sh.commitSeq
	act := sh.act
	var offs []uint32
	for o := 0; o < act.used; {
		tl := recTotalLen(act.data, uint32(o))
		if tl == 0 {
			break
		}
		if recSeq(act.data, uint32(o)) == seq {
			offs = append(offs, uint32(o))
		}
		o += int(tl)
	}
	survivors := map[string]bool{}
	for _, o := range offs[:2] {
		k := recKey(act.data, o)
		if dk, ok := recDeleteMarkerKey(act.data, o); ok {
			k = dk
		}
		survivors[string(k)] = true
	}
	cut, end, path := offs[2], act.used, act.path
	sh.mu.RUnlock()
	if len(offs) != 5 {
		t.Fatalf("fixture: %d records at the commit's sequence, want 5", len(offs))
	}
	img := liveImage(t, c, opts.Dir)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	patchImage(t, opts.Dir, img, path, int64(cut), make([]byte, end-int(cut)))
	want := slices.Clone(before)
	for i := range n {
		if survivors[fmt.Sprintf("k%d", i)] {
			want[i] = after[i]
		}
	}
	o := opts
	o.Dir = img
	checkRecovered(t, o, n, want)
}

// TestCrashLegacyStoreBelievesStamps: a store with no horizon file was last written by a build that
// wrote no delete markers, so a stamp with nothing behind it there is a delete. Recovery believes
// it, as before.
func TestCrashLegacyStoreBelievesStamps(t *testing.T) {
	const n = 40
	c, opts := stampFixture(t, n, 0)
	if !c.Delete([]byte("k7")) {
		t.Fatal("delete")
	}
	after := stampReads(c, n)
	mPath, mOff, mTL := lastDeleteMarker(t, c)
	img := liveImage(t, c, opts.Dir)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	patchImage(t, opts.Dir, img, mPath, int64(mOff), make([]byte, mTL)) // as an earlier build left it
	if err := os.Remove(filepath.Join(img, horizonName)); err != nil {
		t.Fatal(err)
	}
	o := opts
	o.Dir = img
	d := checkRecovered(t, o, n, after)
	if d.SupersessionsUndone != 0 {
		t.Errorf("SupersessionsUndone=%d on a store without a horizon", d.SupersessionsUndone)
	}
}

// TestCrashIntactImageChangesNothing: a crash that lost nothing -- the page cache reached the disk
// whole, as after a killed process -- leaves recovery nothing to undo or replay. Recovery must not
// take back a stamp that has its commit behind it: that would change what every older snapshot
// sees, even where a current read would come out right.
func TestCrashIntactImageChangesNothing(t *testing.T) {
	const n = 60
	opts := Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 13, DeltaMax: 4}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rng := rand.New(rand.NewSource(11))
	for i := range n {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), stampAd(t, i, 0)); err != nil {
			t.Fatal(err)
		}
	}
	for step := range 300 {
		tx := c.Begin()
		key := []byte(fmt.Sprintf("k%d", rng.Intn(n)))
		switch rng.Intn(3) {
		case 0:
			p := classad.New()
			p.InsertAttr("JobStatus", int64(step))
			tx.PatchAttrs(key, p, nil)
		case 1:
			tx.Put(key, stampAd(t, rng.Intn(n), step))
		default:
			tx.Delete(key)
		}
		tx.SetDurable(false)
		tx.Commit()
	}
	want := stampReads(c, n)
	img := liveImage(t, c, opts.Dir)
	o := opts
	o.Dir = img
	r, d := openRecovered(t, o)
	defer r.Close()
	if d.SupersessionsUndone != 0 || d.DeletesReplayed != 0 {
		t.Errorf("recovery of an intact image undid %d stamps and replayed %d deletes", d.SupersessionsUndone, d.DeletesReplayed)
	}
	if got := stampReads(r, n); !slices.Equal(got, want) {
		t.Errorf("intact image: %s", diffStampReads(want, got))
	}
}

// TestCrashCloseFlushesDeferredStamps: a sync pass leaves a delete's stamp unflushed, relying on its
// delete marker, but the directory snapshot a clean Close writes is reopened without reading
// markers. Close flushes the stamps first, so a machine crash after it loses no delete.
func TestCrashCloseFlushesDeferredStamps(t *testing.T) {
	shadow := newDiskShadow(t)
	const n = 300 // several segments: the deleted keys' records are in sealed ones, whose pages no
	// later record msync covers
	c, opts := stampFixture(t, n, 0)
	for _, i := range []int{3, 7, 21} {
		if !c.Delete([]byte(fmt.Sprintf("k%d", i))) {
			t.Fatal("delete")
		}
	}
	want := stampReads(c, n)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	img := shadow.image(t, c, opts.Dir, func() bool { return true }) // only what an msync covered
	if _, err := os.Stat(filepath.Join(img, "0", dirSnapName)); err != nil {
		t.Fatalf("fixture: no directory snapshot: %v", err)
	}
	o := opts
	o.Dir = img
	r, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := stampReads(r, n); !slices.Equal(got, want) {
		t.Fatalf("reopened from the snapshot after a crash: %s", diffStampReads(want, got))
	}
}

// TestCrashRollSyncsOutgoingSegment: the write target a commit leaves is durable before the next
// one is written to, so a crash cannot take records out of the middle of the stream.
func TestCrashRollSyncsOutgoingSegment(t *testing.T) {
	var mu sync.Mutex
	synced := map[string]int{}
	f := func(seg *segment, start int, b []byte) {
		mu.Lock()
		synced[seg.path] = max(synced[seg.path], start+len(b))
		mu.Unlock()
	}
	msyncCopyHook.Store(&f)
	defer msyncCopyHook.Store(nil)
	c, opts := stampFixture(t, 0, 0)
	defer c.Close()
	_ = opts
	sh := c.shards[0]
	tx := func(i int) {
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("k%d", i)), stampAd(t, i, 0))
		tx.SetDurable(false) // nothing here syncs but the roll
		tx.Commit()
	}
	first := ""
	for i := 0; ; i++ {
		tx(i)
		sh.mu.RLock()
		path, used, n := sh.act.path, sh.act.used, len(sh.segs)
		sh.mu.RUnlock()
		if first == "" {
			first = path
		}
		if n > 1 {
			mu.Lock()
			got := synced[first]
			mu.Unlock()
			sh.mu.RLock()
			want := sh.segs[0].used
			sh.mu.RUnlock()
			if got < want {
				t.Fatalf("rolled from %s with %d of %d bytes durable", filepath.Base(first), got, want)
			}
			_ = used
			return
		}
	}
}

// TestRewritesRaiseHorizon: compaction, dead-segment reclaim, columnarizing and the sealed-attribute
// migration drop or replace records crash recovery reads stamps against, so each raises the durable
// horizon over what it rewrites before it does.
func TestRewritesRaiseHorizon(t *testing.T) {
	const n = 300
	onDisk := func(dir string) uint64 {
		t.Helper()
		h := readHorizons(dir, 1)[0]
		if h == horizonUnknown {
			t.Fatal("no horizon")
		}
		return h
	}
	// writes commits nondurably, so nothing but a rewrite's own barrier syncs them, and returns
	// the shard's commit sequence after.
	writes := func(c *Collection, gen int, del bool) uint64 {
		for i := range n {
			tx := c.Begin()
			tx.Put([]byte(fmt.Sprintf("k%d", i)), stampAd(t, i, gen))
			if del && i%3 == 0 {
				tx.Delete([]byte(fmt.Sprintf("k%d", (i+1)%n)))
			}
			tx.SetDurable(false)
			tx.Commit()
		}
		c.shards[0].mu.RLock()
		defer c.shards[0].mu.RUnlock()
		return c.shards[0].commitSeq
	}
	newStore := func(o Options) *Collection {
		c, err := Open(o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	t.Run("compact", func(t *testing.T) {
		o := Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 13}
		c := newStore(o)
		seq := writes(c, 1, true)
		if onDisk(o.Dir) >= seq {
			t.Fatal("fixture: the horizon already covers the writes")
		}
		c.compactShard(c.shards[0], c.currentCodec())
		if h := onDisk(o.Dir); h < seq {
			t.Fatalf("compaction left the horizon at %d, below the %d it compacted", h, seq)
		}
	})
	t.Run("reclaim", func(t *testing.T) {
		o := Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 13}
		c := newStore(o)
		writes(c, 1, false)
		seq := writes(c, 2, false) // every first-generation record is dead
		if !c.shards[0].hasReclaimableSegment() {
			t.Fatal("fixture: nothing to reclaim")
		}
		if c.reclaimDeadShard(c.shards[0]) == 0 {
			t.Fatal("fixture: nothing reclaimed")
		}
		if h := onDisk(o.Dir); h < seq {
			t.Fatalf("reclaim left the horizon at %d, below the %d it covered", h, seq)
		}
	})
	t.Run("columnarize", func(t *testing.T) {
		o := Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 13, GroupSchemaCount: -1}
		c := newStore(o)
		writes(c, 1, true)
		c.BuildAndEnableSchemaScan(4096, 8)
		var top uint64
		sh := c.shards[0]
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg.columnarized() {
				top = max(top, seg.maxSeq)
			}
		}
		sh.mu.RUnlock()
		if top == 0 {
			t.Fatal("fixture: nothing columnarized")
		}
		if h := onDisk(o.Dir); h < top {
			t.Fatalf("columnarizing left the horizon at %d, below the %d it rewrote", h, top)
		}
	})
	t.Run("seal-migration", func(t *testing.T) {
		dir := t.TempDir()
		legacyPlaintextStore(t, dir, "ClaimId-horizon", 400)
		_, dataKey := deriveDataKey(t)
		o := Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16, DataKey: dataKey}
		c := newStore(o)
		for i := range 50 { // commits the migration's barrier has to cover
			tx := c.Begin()
			tx.Put([]byte(fmt.Sprintf("x%d", i)), stampAd(t, i, 1))
			tx.SetDurable(false)
			tx.Commit()
		}
		seqs := make([]uint64, len(c.shards))
		for i, sh := range c.shards {
			sh.mu.RLock()
			seqs[i] = sh.commitSeq
			sh.mu.RUnlock()
		}
		if c.MigrateSealedAttrs(0) == 0 {
			t.Fatal("fixture: nothing migrated")
		}
		for i := range c.shards {
			h := readHorizons(dir, len(c.shards))[i]
			if h == horizonUnknown || h < seqs[i] {
				t.Fatalf("shard %d: migration left the horizon at %d, below %d", i, h, seqs[i])
			}
		}
	})
}

// --- randomized crash differential ---

// diskShadow keeps, per segment file, the bytes an msync made durable: the image a machine crash
// leaves, before the kernel's arbitrary writeback of everything else.
type diskShadow struct {
	mu    sync.Mutex
	files map[*segment][]byte
}

func newDiskShadow(t *testing.T) *diskShadow {
	d := &diskShadow{files: map[*segment][]byte{}}
	f := func(seg *segment, start int, b []byte) {
		d.mu.Lock()
		defer d.mu.Unlock()
		img := d.files[seg]
		if len(img) < start+len(b) {
			img = append(img, make([]byte, start+len(b)-len(img))...)
		}
		copy(img[start:], b)
		d.files[seg] = img
	}
	msyncCopyHook.Store(&f)
	t.Cleanup(func() { msyncCopyHook.Store(nil) })
	return d
}

// crashImage builds what a machine crash leaves of the live store under dir: every page of a
// segment file that changed since its last msync is either the page-cache version or the durable
// one, chosen at random; everything else is as in the page cache.
func (d *diskShadow) crashImage(t *testing.T, c *Collection, dir string, rng *rand.Rand) string {
	return d.image(t, c, dir, func() bool { return rng.Intn(2) == 0 })
}

// image is crashImage with the choice made by lose: true keeps a changed page's durable version.
func (d *diskShadow) image(t *testing.T, c *Collection, dir string, lose func() bool) string {
	t.Helper()
	img := liveImage(t, c, dir)
	d.mu.Lock()
	defer d.mu.Unlock()
	byPath := map[string]*segment{}
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil {
				byPath[seg.path] = seg
			}
		}
		sh.mu.RUnlock()
	}
	err := filepath.Walk(img, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".dat") {
			return err
		}
		rel, _ := filepath.Rel(img, p)
		live, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		durable := d.files[byPath[filepath.Join(dir, rel)]]
		const page = 4096
		for start := 0; start < len(live); start += page {
			end := min(start+page, len(live))
			var dpage []byte
			if start < len(durable) {
				dpage = durable[start:min(end, len(durable))]
			}
			dfull := make([]byte, end-start)
			copy(dfull, dpage)
			if !bytes.Equal(dfull, live[start:end]) && lose() {
				copy(live[start:end], dfull)
			}
		}
		return os.WriteFile(p, live, info.Mode())
	})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// TestCrashDifferential runs random histories of nondurable commits -- puts, delta patches,
// deletes, multi-key transactions -- with a durable commit now and then and maintenance between,
// and takes crash images in which each page written since its last msync independently did or did
// not reach the disk. Each key of a recovered image must read as it did after some commit no
// earlier than the last durable one: never lost, never a value it did not have.
func TestCrashDifferential(t *testing.T) {
	for seed := int64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			shadow := newDiskShadow(t)
			opts := Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 13, DeltaMax: 6, GroupSchemaCount: -1}
			c, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { c.Close() }()
			const n = 60
			// history[i] is every value key i has had since the last durable commit (oldest first).
			history := make([][]string, n)
			read := func(i int) string {
				if ad, ok := c.Get([]byte(fmt.Sprintf("k%d", i))); ok {
					return ad.String()
				}
				return "<absent>"
			}
			shardSeqs := func() []uint64 {
				out := make([]uint64, len(c.shards))
				for si, sh := range c.shards {
					sh.mu.RLock()
					out[si] = sh.commitSeq
					sh.mu.RUnlock()
				}
				return out
			}
			shardOf := func(i int) int {
				k := []byte(fmt.Sprintf("k%d", i))
				return c.shardOf(k, c.h.Hash(k))
			}
			// durable resets the history of every key in the given shards (nil: all): a durable
			// commit syncs the shards it wrote to, and only those.
			durable := func(shards map[int]bool) {
				for i := range n {
					if shards == nil || shards[shardOf(i)] {
						history[i] = []string{read(i)}
					}
				}
			}
			record := func(i int) {
				if v := read(i); v != history[i][len(history[i])-1] {
					history[i] = append(history[i], v)
				}
			}
			for i := range n {
				if err := c.Put([]byte(fmt.Sprintf("k%d", i)), stampAd(t, i, 0)); err != nil {
					t.Fatal(err)
				}
			}
			durable(nil)
			images := 0
			for step := range 400 {
				tx := c.Begin()
				touched := map[int]bool{}
				for range 1 + rng.Intn(3) {
					i := rng.Intn(n)
					key := []byte(fmt.Sprintf("k%d", i))
					switch r := rng.Intn(10); {
					case r < 4:
						p := classad.New()
						p.InsertAttr("JobStatus", int64(rng.Intn(9)))
						p.InsertAttr(fmt.Sprintf("Step%d", step%4), int64(step))
						tx.PatchAttrs(key, p, nil)
					case r < 8:
						tx.Put(key, stampAd(t, i, step))
					default:
						tx.Delete(key)
					}
					touched[i] = true
				}
				durableCommit := rng.Intn(25) == 0
				tx.SetDurable(durableCommit)
				seqs := shardSeqs()
				tx.Commit()
				for i := range touched {
					record(i)
				}
				if durableCommit {
					// Synced: the shards the commit changed. One whose writes all came to nothing
					// (a delete of an absent key) is not synced.
					shards := map[int]bool{}
					for si, s := range shardSeqs() {
						if s != seqs[si] {
							shards[si] = true
						}
					}
					durable(shards)
				}
				switch rng.Intn(60) {
				case 0:
					c.Compact()
				case 1:
					c.Reindex()
				case 2:
					c.BuildAndEnableSchemaScan(4096, 8)
				}
				if step%40 == 39 {
					img := shadow.crashImage(t, c, opts.Dir, rng)
					images++
					o := opts
					o.Dir = img
					r, err := Open(o)
					if err != nil {
						t.Fatal(err)
					}
					recovered := make([]string, n)
					for i := range n {
						got := "<absent>"
						if ad, ok := r.Get([]byte(fmt.Sprintf("k%d", i))); ok {
							got = ad.String()
						}
						if !slices.Contains(history[i], got) {
							r.Close()
							t.Fatalf("image at step %d: k%d recovered as\n  %s\nwhich it never was since the last durable commit:\n  %s",
								step, i, got, strings.Join(history[i], "\n  "))
						}
						recovered[i] = got
					}
					if err = r.Close(); err != nil {
						t.Fatal(err)
					}
					// What recovery decided is what the files now say: a clean reopen (from the
					// snapshot) and a time-travel one (walking the records again) read the same.
					for _, tt := range []bool{false, true} {
						o2 := o
						if tt {
							o2.TimeTravel = &TimeTravelOptions{MaxDistance: time.Hour}
						}
						r, err = Open(o2)
						if err != nil {
							t.Fatal(err)
						}
						got := stampReads(r, n)
						r.Close()
						if !slices.Equal(got, recovered) {
							t.Fatalf("image at step %d reopened (time travel %v): %s", step, tt, diffStampReads(recovered, got))
						}
					}
				}
			}
			if images == 0 {
				t.Fatal("no images")
			}
		})
	}
}
