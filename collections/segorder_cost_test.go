package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestSegmentOrderKeyReadOncePerSegment pins the ordering key's cost model: segFirstSeq is a
// read from the segment's mapping, and reopen must take it once per segment rather than once
// per comparison.
//
// Sorting n segments asks for their keys O(n log n) times, so a key read inside the comparator
// turns a single pass over the segments into repeated scattered touches across all of them.
// Every segment is freshly mmapped at this point and none of it is resident, so those touches
// are page faults, and the phase that pays them reports no progress while it does -- which is
// how it stayed unnoticed on a 7,022-segment archive whose open was already minutes long.
//
// TestSegmentOrderIsContentNotFilename covers the order itself. It passes either way: both
// shapes produce the identical order, which is exactly why this needs its own test.
func TestSegmentOrderKeyReadOncePerSegment(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	// Enough records to force many segments: the comparison count grows as n log n while the
	// segment count grows as n, so the two cost models only separate once n is well past 1.
	const n = 600
	for i := 0; i < n; i++ {
		ad, _ := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nPad = %q", i, strings.Repeat("z", 60)))
		if appendErr := a.Append(ad); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	if closeErr := a.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	segFirstSeqReads.Store(0)
	b, err := OpenArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	reads := segFirstSeqReads.Load()
	segs := 0
	for _, sh := range b.c.shards {
		segs += len(sh.segs)
	}
	if closeErr := b.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	if segs < 8 {
		t.Fatalf("%d segments is too few to tell one read per segment from one per comparison; "+
			"the fixture is not exercising the sort", segs)
	}
	// One per segment is the budget. A comparator that reads the key would be at least
	// n*log2(n) reads -- for 8 segments that is already 24, and it grows from there.
	if reads > int64(segs) {
		t.Errorf("reopen read the ordering key %d times for %d segments, want at most one per "+
			"segment; the key is being read per comparison rather than once as the segment loads",
			reads, segs)
	}
	if reads == 0 {
		t.Errorf("the ordering key was never read for %d segments, so this test proves nothing "+
			"about where it is read", segs)
	}
}
