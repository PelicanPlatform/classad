package collections

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// readSkipTotal sums RecordReadSkips over every reason.
func readSkipTotal() int64 {
	var n int64
	for _, v := range RecordReadSkips() {
		n += v
	}
	return n
}

// readSkipsOf returns RecordReadSkips' count for one reason.
func readSkipsOf(reason string) int64 { return RecordReadSkips()[reason] }

// keyCheck is what a reopened store returned for the keys written to it.
type keyCheck struct {
	missing   []string // keys that did not read
	uncounted []string // of those, keys whose failed read RecordReadSkips did not count
}

// checkEveryKey reads every key of orig from c. Each must read back exactly as orig holds it, or not
// at all: a key that reads back as anything else -- shortened, or another key's ad -- fails the test.
// It returns the keys that did not read, and which of those the failed read did not count.
func checkEveryKey(t *testing.T, c *Collection, orig map[string]string) keyCheck {
	t.Helper()
	keys := make([]string, 0, len(orig))
	for k := range orig {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out keyCheck
	wrong := 0
	for _, k := range keys {
		before := readSkipTotal()
		ad, ok := c.Get([]byte(k))
		if !ok {
			out.missing = append(out.missing, k)
			if readSkipTotal() == before {
				out.uncounted = append(out.uncounted, k)
			}
			continue
		}
		if got := ad.String(); got != orig[k] {
			wrong++
			if wrong <= 3 {
				t.Errorf("key %s reads back as a different ad:\n  got  %s\n  want %s", k, got, orig[k])
			}
		}
	}
	if wrong > 0 {
		t.Fatalf("%d keys read back wrong", wrong)
	}
	return out
}

// checkScan scans c and requires every ad it returns to be one of orig's, at most as often as orig
// holds it. It returns how many ads came back and how many reads the scan counted as skipped.
func checkScanAds(t *testing.T, c *Collection, orig map[string]string) (n int, skips int64) {
	t.Helper()
	left := map[string]int{}
	for _, a := range orig {
		left[a]++
	}
	before := readSkipTotal()
	for ad := range c.Scan() {
		n++
		s := ad.String()
		if left[s] == 0 {
			t.Errorf("a scan returned an ad that was not written: %s", s)
			continue
		}
		left[s]--
	}
	return n, readSkipTotal() - before
}

// getEveryKey opens opts and returns every key's ad text, keyed by key, for the n keys "<i>.0".
func getEveryKey(t *testing.T, opts Options, n int) map[string]string {
	t.Helper()
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out := map[string]string{}
	for i := range n {
		k := fmt.Sprintf("%d.0", i)
		if ad, ok := c.Get([]byte(k)); ok {
			out[k] = ad.String()
		}
	}
	if len(out) != n {
		t.Fatalf("the undamaged store reads %d keys, want %d", len(out), n)
	}
	return out
}

// keysOfSegment returns the keys of the keyed records in a segment file.
func keysOfSegment(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, o := range recordOffsets(t, path) {
		if !recIsMarker(b, uint32(o)) {
			out[string(recKey(b, uint32(o)))] = true
		}
	}
	return out
}

// TestOpenCountsRecordsPastDamage: with no sidecar to hand it the extent, Open's recovery walk stops at
// the first record that fails its checksum. In a columnarized segment that leaves the dictionary and
// the payload -- both after the records -- unread, so the records before the damage cannot be read
// either: 1475 of 2000 ads came back, and nothing counted the other 525. Every key must now read
// exactly as written or be counted: the records past the stop at Open, the ones before it per read.
func TestOpenCountsRecordsPastDamage(t *testing.T) {
	base := t.TempDir()
	payloadFixture(t, base)
	orig := getEveryKey(t, payloadFixtureOpts(base), payloadFixtureAds)

	dir := copyStore(t, base)
	target := firstColumnarSegment(t, dir)
	offs := recordOffsets(t, target)
	stop := offs[len(offs)/2]
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	// What the walk will not reach: the keyed records after the damaged one.
	past, inSeg := 0, 0
	for _, o := range offs {
		if recIsMarker(data, uint32(o)) {
			continue
		}
		inSeg++
		if o > stop {
			past++
		}
	}
	flipBit(t, target, stop+recKeyOff+2)
	if err = os.Remove(target + ".idx"); err != nil {
		t.Fatal(err)
	}

	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	var seg *FsckSegment
	for i := range r.Segments {
		if r.Segments[i].Path == target {
			seg = &r.Segments[i]
		}
	}
	if seg == nil || seg.OpenStop == nil {
		t.Fatalf("fsck does not say where an open stops:\n%s", r.String())
	}
	if st := seg.OpenStop; st.Offset != stop || st.Records != past || st.Damaged != 1 || !st.Dictionary || !st.ColumnarPayload {
		t.Errorf("fsck OpenStop = %+v, want offset %d, %d records, 1 damaged, dictionary and payload", *st, stop, past)
	}
	if !strings.Contains(r.String(), "a normal open stops at") {
		t.Errorf("the fsck report does not explain the stop:\n%s", r.String())
	}

	var diag OpenIndexDiag
	prev := OpenIndexDiagHook
	OpenIndexDiagHook = func(d OpenIndexDiag) { diag = d }
	defer func() { OpenIndexDiagHook = prev }()
	pastBefore, unusableBefore := RecordsPastDamage(), SegDictsUnusable()
	c, err := Open(payloadFixtureOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if d := RecordsPastDamage() - pastBefore; d != int64(past+1) {
		t.Errorf("RecordsPastDamage rose by %d, want %d (the damaged record and the %d after it)", d, past+1, past)
	}
	if SegDictsUnusable() == unusableBefore {
		t.Error("the segment's dictionary was not reached, and nothing counted it")
	}
	if len(diag.DamageStops) != 1 || diag.DamageStops[0].Path != target || diag.DamageStops[0].Records != past {
		t.Errorf("OpenIndexDiag.DamageStops = %+v, want one stop in %s with %d records", diag.DamageStops, target, past)
	}

	kc := checkEveryKey(t, c, orig)
	if len(kc.missing) != inSeg {
		t.Errorf("%d keys did not read, want the target segment's %d", len(kc.missing), inSeg)
	}
	// A key past the stop was never loaded, so its miss is counted at Open, not per read.
	if len(kc.uncounted) != past+1 {
		t.Errorf("%d missing keys were not counted by their read, want the %d Open counted", len(kc.uncounted), past+1)
	}
	n, skips := checkScanAds(t, c, orig)
	if want := len(orig) - inSeg; n != want {
		t.Errorf("a scan returned %d ads, want %d", n, want)
	}
	if want := int64(inSeg - past - 1); skips != want {
		t.Errorf("a scan counted %d skipped reads, want %d (the records before the stop)", skips, want)
	}
}

// internedAds is how many ads buildInternedStore writes.
const internedAds = 2000

// internedStoreOpts are the options buildInternedStore writes and reopens its store with.
func internedStoreOpts(dir string) Options {
	return Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16}
}

// buildInternedStore writes internedAds ads, trains a dictionary so the sealed segments are rewritten
// interned (each with its attribute dictionary record) but not columnarized, and closes the store.
// Its sealed segments carry sidecars whose extent Open trusts.
func buildInternedStore(t *testing.T, dir string) {
	t.Helper()
	c, err := Open(internedStoreOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	for i := range internedAds {
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d ]`, i, i%10, i%7, i%6))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.RetrainDict(256); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

// firstInternedSegment returns the first segment file under dir holding a dictionary record, and
// that record's offset.
func firstInternedSegment(t *testing.T, dir string) (string, int) {
	t.Helper()
	for _, p := range segmentFiles(t, dir) {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range recordOffsets(t, p) {
			if recIsDict(b, uint32(o)) {
				return p, o
			}
		}
	}
	t.Fatal("no interned segment on disk")
	return "", 0
}

// TestScanSkipsAreCountedByReason: a scan skips a record it cannot read rather than fail, so the
// skip is the only sign the answer is short. Every scan path must count it, with its reason.
func TestScanSkipsAreCountedByReason(t *testing.T) {
	t.Run("record-crc", func(t *testing.T) {
		dir := t.TempDir()
		buildInternedStore(t, dir)
		opts := internedStoreOpts(dir)
		orig := getEveryKey(t, opts, internedAds)
		target, _ := firstInternedSegment(t, dir)
		offs := recordOffsets(t, target)
		victim := offs[len(offs)/2]
		b, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		key := string(recKey(b, uint32(victim)))
		// The body, not the key: the directory still names the record, so every path reaches it.
		flipRecordBody(t, target, victim)

		c, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		kc := checkEveryKey(t, c, orig)
		if len(kc.missing) != 1 || kc.missing[0] != key || len(kc.uncounted) != 0 {
			t.Fatalf("missing %v (uncounted %v), want only %s, counted", kc.missing, kc.uncounted, key)
		}
		paths := []struct {
			name string
			run  func() int
		}{
			{"Scan", func() int {
				n := 0
				for range c.Scan() {
					n++
				}
				return n
			}},
			{"QueryRawWire", func() int { return len(readAll(t, c, "true")) }},
			{"Query", func() int {
				q, err := vm.Parse("ProcId >= 0")
				if err != nil {
					t.Fatal(err)
				}
				n := 0
				for range c.Query(q) {
					n++
				}
				return n
			}},
			{"ForEachAd", func() int {
				n := 0
				c.ForEachAd(func(string, *classad.ClassAd) bool { n++; return true })
				return n
			}},
			{"Keys", func() int { return len(c.Keys()) }},
		}
		for _, p := range paths {
			before := readSkipsOf("record-crc")
			if got := p.run(); got != internedAds-1 {
				t.Errorf("%s returned %d, want %d", p.name, got, internedAds-1)
			}
			if d := readSkipsOf("record-crc") - before; d != 1 {
				t.Errorf("%s counted %d record-crc skips, want 1", p.name, d)
			}
		}
	})

	t.Run("col-damaged", func(t *testing.T) {
		base := t.TempDir()
		payloadFixture(t, base)
		orig := getEveryKey(t, payloadFixtureOpts(base), payloadFixtureAds)
		target := firstColumnarSegment(t, base)
		data, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := payloadOffset(data)
		flipBit(t, target, payload+recKeyOff+4+colPayloadHdr+2)
		inSeg := keysOfSegment(t, target)
		if len(inSeg) == 0 {
			t.Fatal("no keys in the target segment")
		}

		c, err := Open(payloadFixtureOpts(base))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		before := readSkipsOf("col-damaged")
		kc := checkEveryKey(t, c, orig)
		if len(kc.missing) != len(inSeg) || len(kc.uncounted) != 0 {
			t.Errorf("%d keys missing (%d uncounted), want the segment's %d, all counted",
				len(kc.missing), len(kc.uncounted), len(inSeg))
		}
		if d := readSkipsOf("col-damaged") - before; d != int64(len(inSeg)) {
			t.Errorf("point reads counted %d col-damaged skips, want %d", d, len(inSeg))
		}
		before = readSkipsOf("col-damaged")
		n, _ := checkScanAds(t, c, orig)
		if n != len(orig)-len(inSeg) {
			t.Errorf("a scan returned %d ads, want %d", n, len(orig)-len(inSeg))
		}
		if d := readSkipsOf("col-damaged") - before; d != int64(len(inSeg)) {
			t.Errorf("a scan counted %d col-damaged skips, want %d", d, len(inSeg))
		}
	})
}

// TestRecordReadSkipReasonNames pins that every reason has a name and the names are distinct.
func TestRecordReadSkipReasonNames(t *testing.T) {
	names := RecordReadSkipReasonNames()
	if len(names) != int(skipMax) {
		t.Fatalf("%d names, want %d", len(names), skipMax)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if n == "unknown" || seen[n] {
			t.Errorf("reason name %q is unknown or repeated", n)
		}
		seen[n] = true
	}
}
