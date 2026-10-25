package collections

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

// seqRunOffsets lists the records a run visits in w for minSeq at snapshot s0, walking from offset
// from.
func seqRunOffsets(w segWindow, s0, minSeq uint64, from uint32) []uint32 {
	r := &seqRun{w: w, off: from}
	var out []uint32
	for r.advance(s0, minSeq); r.ok; r.advance(s0, minSeq) {
		out = append(out, r.cur.off)
	}
	return out
}

// seqSeekWindow is a window over the whole of seg as written so far.
func seqSeekWindow(seg *segment) segWindow {
	return segWindow{data: seg.data, used: seg.used, codec: identityCodec{}, seg: seg, dmg: seg.damaged()}
}

// checkSeekMatchesWalk holds a seek to the walk it shortcuts: for every cursor sequence up to just
// past the segment's newest, a run started at the seek visits exactly the records a run started at
// offset 0 does.
func checkSeekMatchesWalk(t *testing.T, w segWindow, s0, top uint64) {
	t.Helper()
	for minSeq := uint64(0); minSeq <= top+1; minSeq++ {
		from := seekSeq(w, minSeq)
		want := seqRunOffsets(w, s0, minSeq, 0)
		if got := seqRunOffsets(w, s0, minSeq, from); !slices.Equal(got, want) {
			t.Fatalf("minSeq %d: from the seek (offset %d) the run visits %v, from the start %v", minSeq, from, got, want)
		}
	}
}

// TestSeekSeqMatchesWalk checks the seek against the walk over segments in sequence order and out of
// it, with markers and superseded records among them, as the segment grows past what the seek has
// covered, and through a window older than the one that last extended it.
func TestSeekSeqMatchesWalk(t *testing.T) {
	t.Parallel()
	const n = 300
	rng := rand.New(rand.NewSource(1))
	orders := map[string]func(i int) uint64{
		// Commits of several records each, in order: what writes produce.
		"commits": func(i int) uint64 { return uint64(1 + i/7) },
		// Order the seek must not rely on: a running maximum is what keeps a later low record from
		// being skipped.
		"shuffled":   func(int) uint64 { return uint64(1 + rng.Intn(n)) },
		"descending": func(i int) uint64 { return uint64(n - i) },
		"late-low": func(i int) uint64 {
			if i%41 == 40 {
				return 3
			}
			return uint64(10 + i)
		},
	}
	for name, seqOf := range orders {
		t.Run(name, func(t *testing.T) {
			seg := newSegment(1, 1<<20, identityCodec{})
			var top uint64
			appendSome := func(lo, hi int) {
				for i := lo; i < hi; i++ {
					seq := seqOf(i)
					top = max(top, seq)
					if i%53 == 0 {
						if _, ok := seg.appendMarker(seq, uint64(i)); !ok {
							t.Fatal("segment full")
						}
						continue
					}
					off, ok := seg.append(seq, noLoc, []byte(fmt.Sprintf("k%d", i)), []byte("ad"))
					if !ok {
						t.Fatal("segment full")
					}
					if i%5 == 0 {
						seg.supersedeRec(off, seq+uint64(n))
					}
				}
			}
			appendSome(0, n/3)
			early := seqSeekWindow(seg)
			checkSeekMatchesWalk(t, early, top+uint64(n), top)
			// Grown since: the seek extends what it covered.
			appendSome(n/3, n)
			checkSeekMatchesWalk(t, seqSeekWindow(seg), top+uint64(n), top)
			// A snapshot whose superseding writes it cannot see, so every record is visible.
			checkSeekMatchesWalk(t, seqSeekWindow(seg), top, top)
			// The window taken before the growth reads the extended seek, which covers records it
			// cannot see.
			checkSeekMatchesWalk(t, early, top+uint64(n), top)
		})
	}

	// In order, the seek leaves at most one block of records below the cursor to the walk.
	seg := newSegment(1, 1<<20, identityCodec{})
	var offs []uint32
	for i := range n {
		off, _ := seg.append(uint64(1+i), noLoc, []byte(fmt.Sprintf("k%d", i)), []byte("ad"))
		offs = append(offs, off)
	}
	w := seqSeekWindow(seg)
	for i := range n {
		if from := seekSeq(w, uint64(1+i)); from > offs[i] || from < offs[max(0, i-seqSeekBlock)] {
			t.Fatalf("seek for record %d (offset %d) lands at %d, outside one block before it", i, offs[i], from)
		}
	}
	if from := seekSeq(w, n+1); int(from) != seg.used {
		t.Fatalf("seek past every record lands at %d, not the end %d", from, seg.used)
	}
}

// TestSeekSeqDamagedWindow: the walk steps over a damaged record by its damage span, not by the
// length its header claims, so a window with damage must walk from the start. Here the damaged
// record's length is corrupt and points past four intact records that carry the newest sequence; a
// seek that followed the framing would never see them.
func TestSeekSeqDamagedWindow(t *testing.T) {
	t.Parallel()
	seg := newSegment(1, 1<<20, identityCodec{})
	var offs []uint32
	for i := range 100 {
		seq := uint64(1 + i)
		if i > 40 && i <= 44 {
			seq = 1000
		}
		off, _ := seg.append(seq, noLoc, []byte(fmt.Sprintf("k%d", i)), []byte("ad"))
		offs = append(offs, off)
	}
	binary.LittleEndian.PutUint32(seg.data[offs[40]+recTotalLenOff:], offs[45]-offs[40])
	seg.damage.Store(newDamageSet([]damageSpan{{start: offs[40], end: offs[41]}}))
	w := seqSeekWindow(seg)
	if got := seqRunOffsets(w, 2000, 1000, seekSeq(w, 1000)); !slices.Equal(got, offs[41:45]) {
		t.Fatalf("from the seek the run visits %v, want the four records at %v", got, offs[41:45])
	}
	checkSeekMatchesWalk(t, w, 2000, 1000)
}

// TestSeqReadConcurrentReadersShareTheSeek pages one collection from several readers at once while a
// writer rewrites keys and compaction runs, so readers extend the same segments' seeks concurrently
// -- the active segment's as it grows -- and build fresh ones over compaction's output. Each reader
// must still see every key exactly once. Run it under -race.
func TestSeqReadConcurrentReadersShareTheSeek(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2, SegmentSize: 4096})
	const n = 400
	for i := range n {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	var stop atomic.Bool
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		rng := rand.New(rand.NewSource(1))
		for w := 0; !stop.Load(); w++ {
			i := rng.Intn(n)
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; W=%d]`, i, w))); err != nil {
				t.Error(err)
				return
			}
			if w%200 == 0 {
				c.Compact()
			}
		}
	}()
	var readers sync.WaitGroup
	for r := range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			seen := make([]int, n)
			var cursor SeqCursor
			for pages := 0; ; pages++ {
				if pages > 10000 {
					t.Error("pagination did not terminate")
					return
				}
				seq, page := c.QueryRawFromSeq(nil, nil, cursor, 3+r)
				for raw := range seq {
					seen[rawIDOf(t, raw)]++
				}
				if !page.More {
					break
				}
				cursor = page.Next
			}
			for id, count := range seen {
				if count != 1 {
					t.Errorf("reader %d saw id %d %d times, want 1", r, id, count)
					return
				}
			}
		}()
	}
	readers.Wait()
	stop.Store(true)
	writers.Wait()
}
