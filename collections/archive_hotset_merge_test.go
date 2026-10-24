package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// onDiskHotNames walks every record of every segment and returns how many records carry
// each attribute in their STORED hot header, folded.
//
// It reads the record's own bytes (recordWireIn, which honors the segment's encoding) and
// resolves names against that segment's dictionary. The sampling helpers are no use here:
// CollectSamplesRecent routes an interned segment's records through wireToInline, which
// re-encodes them inline with NO hot header -- so a sampled record reports an empty hot
// header whatever is on disk, and a test built on one measures the sampler.
func onDiskHotNames(t *testing.T, a *Archive) (names map[string]int, records int) {
	t.Helper()
	names = map[string]int{}
	c := a.c
	sh := c.shards[0]
	sh.mu.RLock()
	segs := append([]*segment(nil), sh.segs...)
	sh.mu.RUnlock()
	var buf []byte
	for _, s := range segs {
		if s == nil || s.used == 0 {
			continue
		}
		dict := s.dict.Load()
		for off := uint32(0); off < uint32(s.used); {
			rl := recTotalLen(s.data, off)
			if rl == 0 {
				break
			}
			if recIsMarker(s.data, off) || recIsDelta(s.data, off) {
				off += rl
				continue
			}
			w, err := c.recordWireIn(s, s.data, off, buf[:0])
			if err != nil { // the trailing dict record is not an ad
				off += rl
				continue
			}
			buf = w
			ad := wire.Ad(w)
			n := 0
			if _, ok := ad.ForEachHotInlineBuf(nil, func(_ uint32, name, _ []byte) bool {
				names[strings.ToLower(string(name))]++
				n++
				return true
			}); ok {
				records++
				off += rl
				continue
			}
			if dict == nil {
				off += rl
				continue
			}
			if _, ok := ad.ForEachHotBuf(nil, func(id uint32, _ []byte) bool {
				if nm, ok := dict.resolve(id); ok {
					names[strings.ToLower(nm)]++
				}
				return true
			}); ok {
				records++
			}
			off += rl
		}
	}
	return names, records
}

// TestArchiveMergeCarriesTheCurrentHotSet pins behaviour it would be easy to assume away in
// either direction: an archive's merge pass re-encodes the records it merges (resealSegments,
// shared with Rewrite and the codec upgrade), and writes them under the hot set in effect AT
// MERGE TIME -- not the one they were appended under.
//
// So a hot-set refresh is NOT confined to new records forever. It reaches old ones as the
// merge pass folds their segments together: gradually, bounded by KeepRecent and by whether a
// cold run is ever merged, but it reaches them. Anything claiming an archive can only converge
// via an explicit Rewrite is wrong, and this is what says so.
func TestArchiveMergeCarriesTheCurrentHotSet(t *testing.T) {
	a, err := CreateArchive(ArchiveOptions{
		Dir:         t.TempDir(),
		SegmentSize: 1 << 12, // small, so a few hundred records seal many segments
		HotAttrs:    []string{"Alpha"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	const total = 600
	for i := range total {
		ad := mustAd(t, fmt.Sprintf(`[ Alpha=%d; Beta="b%d"; Gamma=%d; Pad=%q ]`,
			i, i%9, i*3, strings.Repeat("z", 60)))
		if aerr := a.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	before, seen := onDiskHotNames(t, a)
	if seen == 0 {
		t.Fatal("read no records off disk; the walk is broken, not the behaviour")
	}
	if before["alpha"] == 0 {
		t.Fatalf("records were not written under the configured hot set: %v", before)
	}
	if before["beta"] != 0 {
		t.Fatalf("Beta is in %d records' hot headers before anything asked for it", before["beta"])
	}

	a.AddHotAttrs("Beta")

	segsBefore := a.Stats().Segments
	if segsBefore < 10 {
		t.Fatalf("need several segments to merge, got %d", segsBefore)
	}
	a.MergePass(MergeOptions{
		TargetSegments: 2, TriggerSegments: 4, MinMergeBytes: 1, KeepRecent: 1, MaxRun: 64,
	})
	if segsAfter := a.Stats().Segments; segsAfter >= segsBefore {
		t.Fatalf("segments %d -> %d: nothing merged, so this proves nothing", segsBefore, segsAfter)
	}

	after, seen := onDiskHotNames(t, a)
	if seen == 0 {
		t.Fatal("read no records off disk after the merge")
	}
	if after["beta"] == 0 {
		t.Errorf("no record carries Beta in its stored hot header after a merge under a hot set "+
			"that includes it: %v", after)
	}
	// Alpha was hot before and still is, so the merge must not have dropped it either.
	if after["alpha"] == 0 {
		t.Errorf("the merge dropped Alpha from every hot header: %v", after)
	}

	// And the records still read back whole under their rewritten headers.
	q, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for ad := range a.Query(q) {
		if _, ok := ad.EvaluateAttrInt("Alpha"); !ok {
			t.Fatalf("record lost Alpha across the merge: %v", ad)
		}
		n++
	}
	if n != total {
		t.Errorf("read back %d records after the merge, want %d", n, total)
	}
}
