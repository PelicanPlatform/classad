package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// adsByKey opens dir and returns every fixture key's ad text, keyed by the key. A key that does not
// read is absent from the map.
func adsByKey(t *testing.T, dir string) map[string]string {
	t.Helper()
	c, err := Open(payloadFixtureOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out := map[string]string{}
	for i := range payloadFixtureAds {
		k := fmt.Sprintf("%d.0", i)
		if ad, ok := c.Get([]byte(k)); ok {
			out[k] = ad.String()
		}
	}
	return out
}

// firstColumnarSegment returns the first segment file under dir holding a columnar payload.
func firstColumnarSegment(t *testing.T, dir string) string {
	t.Helper()
	for _, p := range segmentFiles(t, dir) {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := payloadOffset(b); ok {
			return p
		}
	}
	t.Fatal("no columnarized segment on disk")
	return ""
}

// TestRepairLeavesDamagedColumnarizedSegmentAlone: a columnarized segment's payload finds each
// record's row by the record's arena offset. Packing the records around a bad one moved every record
// after it onto its predecessor's row, so each of them read back as a different key's ad, and the
// record whose offset matched no row came back as its bare remnant. Repair must leave such a segment
// as it is: every ad still reads as written, except the damaged record, which reads as missing.
func TestRepairLeavesDamagedColumnarizedSegmentAlone(t *testing.T) {
	base := t.TempDir()
	want := payloadFixture(t, base)
	orig := adsByKey(t, base)
	if len(orig) != payloadFixtureAds {
		t.Fatalf("the undamaged fixture reads %d keys, want %d", len(orig), payloadFixtureAds)
	}

	cases := []struct {
		name string
		// pick returns the file offsets of the bits to flip, given the segment's record offsets and
		// the payload's offset.
		pick func(offs []int, payload int) []int
	}{
		// A remnant's key, mid-segment.
		{"remnant", func(offs []int, _ int) []int { return []int{offs[len(offs)/2] + recKeyOff + 2} }},
		// The payload's body as well. The payload is the last record, so damage to it alone reads as
		// a torn tail, which repair never rewrites; the remnant is what makes the segment a candidate.
		{"payload", func(offs []int, payload int) []int {
			return []int{offs[len(offs)/2] + recKeyOff + 2, payload + recKeyOff + 4 + colPayloadHdr + 2}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := copyStore(t, base)
			target := firstColumnarSegment(t, dir)
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			payload, _ := payloadOffset(data)
			offs := recordOffsets(t, target)
			if len(offs) < 8 {
				t.Fatal("segment too small")
			}
			for _, o := range tc.pick(offs, payload) {
				flipBit(t, target, o)
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
			if seg == nil || !segmentNeedsRepair(*seg) {
				t.Fatal("the damage did not register; the test proves nothing")
			}
			if !seg.Columnarized {
				t.Error("fsck does not report the segment as columnarized")
			}
			if !strings.Contains(r.String(), "columnarized: repair leaves this segment alone") {
				t.Errorf("the fsck report does not say repair will leave it:\n%s", r.String())
			}
			original := snapshotFiles(t, dir)

			for _, dry := range []bool{true, false} {
				rep, rerr := Repair(dir, RepairOptions{DryRun: dry})
				if rerr != nil {
					t.Fatal(rerr)
				}
				t.Logf("dry=%v\n%s", dry, rep.String())
				for _, s := range rep.Segments {
					if s.Path == target {
						t.Errorf("dry=%v: repair rewrote the columnarized segment: %+v", dry, s)
					}
				}
				if len(rep.Columnarized) != 1 || rep.Columnarized[0] != target {
					t.Errorf("dry=%v: repair Columnarized = %v, want [%s]", dry, rep.Columnarized, target)
				}
				if !strings.Contains(rep.String(), "LEFT ALONE "+filepath.Base(target)) {
					t.Errorf("dry=%v: the repair report does not name the segment", dry)
				}
			}
			assertFilesUnchanged(t, original, "Repair")
			if _, serr := os.Stat(filepath.Join(dir, quarantineDir)); serr == nil {
				t.Error("repair quarantined something")
			}

			misses := ColNativeRowMisses()
			got := adsByKey(t, dir)
			wrong := 0
			for k, ad := range got {
				if ad != orig[k] {
					wrong++
					if wrong <= 3 {
						t.Errorf("key %s reads back as a different ad:\n  got  %s\n  want %s", k, ad, orig[k])
					}
				}
			}
			if wrong > 0 {
				t.Fatalf("%d keys read back wrong", wrong)
			}
			if tc.name == "remnant" && len(got) != payloadFixtureAds-1 {
				t.Errorf("%d keys read, want %d: only the damaged record may be missing", len(got), payloadFixtureAds-1)
			}

			// A scan must not return anything that was not written, either.
			c, err := Open(payloadFixtureOpts(dir))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			left := map[string]int{}
			for _, w := range want {
				left[w]++
			}
			for _, a := range readAll(t, c, "true") {
				if left[a] == 0 {
					t.Errorf("a scan returned an ad that was not written: %s", a)
					continue
				}
				left[a]--
			}
			if d := ColNativeRowMisses() - misses; d != 0 {
				t.Errorf("%d reads found a record with no payload row", d)
			}
		})
	}
}

// TestColNativeRecordWithoutRowFails: a remnant whose segment's payload has no row at its offset is
// a record missing every schema'd attribute. The read must fail, and be counted, rather than return
// the remnant as though it were the whole ad.
func TestColNativeRecordWithoutRowFails(t *testing.T) {
	dir := t.TempDir()
	want := payloadFixture(t, dir)
	c, err := Open(payloadFixtureOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Move one row's offset off its record, as a payload describing moved records would. One past
	// the record start keeps the map ascending (records are 8-aligned) and matches no record.
	var seg *segment
	var key string
	var off uint32
	for _, s := range c.shards[0].segs {
		if s == nil || s.colNative.Load() == nil {
			continue
		}
		cn := s.colNative.Load()
		i := cn.seg.offsLen() / 2
		off = cn.seg.offAt(i)
		if !recIsStripped(s.data, off) {
			t.Fatal("the payload's row does not name a remnant")
		}
		cs := *cn.seg
		cs.offsB = append([]byte(nil), cn.seg.offsB...)
		cs.offsB[i*4]++
		moved := *cn
		moved.seg = &cs
		s.colNative.Store(&moved)
		seg, key = s, string(recKey(s.data, off))
		break
	}
	if seg == nil {
		t.Fatal("no columnarized segment")
	}

	before := ColNativeRowMisses()
	if _, err := c.recordWire(seg, off, nil); err != errColRowMissing {
		t.Errorf("recordWire = %v, want errColRowMissing", err)
	}
	if ad, ok := c.Get([]byte(key)); ok {
		t.Errorf("Get(%s) returned an ad for a record with no payload row: %s", key, ad.String())
	}
	got := readAll(t, c, "true")
	if len(got) != len(want)-1 {
		t.Errorf("a scan returned %d ads, want %d (every ad but the one without a row)", len(got), len(want)-1)
	}
	left := map[string]int{}
	for _, w := range want {
		left[w]++
	}
	sort.Strings(got)
	for _, a := range got {
		if left[a] == 0 {
			t.Errorf("a scan returned an ad that was not written: %q", a)
			continue
		}
		left[a]--
	}
	if ColNativeRowMisses() == before {
		t.Error("the refused reads were not counted")
	}
}
