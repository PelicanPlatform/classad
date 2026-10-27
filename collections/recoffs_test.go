package collections

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// revStore opens an append log that yields newest-first, with a segment size the caller picks so
// a test can decide whether its records fit one segment.
func revStore(t *testing.T, segSize int) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, AppendOnly: true, ReverseScan: true, SegmentSize: segSize})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// revPut appends one ad carrying its own ordinal, which is what the newest-first assertions read.
func revPut(t *testing.T, c *Collection, i int) {
	t.Helper()
	ad, err := classad.Parse(fmt.Sprintf(`[ Seq=%d; Owner="user%d"; Pad="%s" ]`, i, i%7, "xxxxxxxxxxxxxxxx"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put([]byte(fmt.Sprintf("k%08d", i)), ad); err != nil {
		t.Fatal(err)
	}
}

// revSeqs runs a newest-first query and returns the Seq of each ad, up to limit (0: all).
func revSeqs(t *testing.T, c *Collection, expr string, limit int) []int {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for ad := range c.Query(q) {
		v, ok := ad.EvaluateAttrInt("Seq")
		if !ok {
			t.Fatalf("ad without Seq: %s", ad.String())
		}
		out = append(out, int(v))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// TestReverseScanOnActiveSegmentIsNewestFirst is the correctness floor the offset table has to
// keep: an unsealed segment's records, replayed backward, are the ones most recently appended, in
// order, whether the walk framed the window itself or read offsets kept from an earlier scan.
func TestReverseScanOnActiveSegmentIsNewestFirst(t *testing.T) {
	c := revStore(t, 1<<20)
	const n = 400
	for i := range n {
		revPut(t, c, i)
	}
	for _, limit := range []int{1, 5, 50, 0} {
		for pass := range 2 { // pass 1 runs against the table pass 0 published
			got := revSeqs(t, c, "true", limit)
			want := limit
			if want == 0 || want > n {
				want = n
			}
			if len(got) != want {
				t.Fatalf("limit %d pass %d: %d rows, want %d", limit, pass, len(got), want)
			}
			for i, s := range got {
				if s != n-1-i {
					t.Fatalf("limit %d pass %d: row %d is Seq=%d, want %d", limit, pass, i, s, n-1-i)
				}
			}
		}
	}
	// A constraint that skips most records still has to walk backward through the ones it skips.
	got := revSeqs(t, c, `Owner == "user3"`, 5)
	for i, s := range got {
		if s%7 != 3 {
			t.Fatalf("filtered row %d is Seq=%d, which is not user3", i, s)
		}
		if i > 0 && s >= got[i-1] {
			t.Fatalf("filtered rows are not newest-first: %v", got)
		}
	}
}

// TestReverseScanOffsetsAreExtendedNotRebuilt is the test the change exists for: a newest-first
// scan of the segment still being appended to must not re-read every record's length header to
// find the last few. The first scan frames the segment, and every later one only covers what was
// appended since.
//
// Without the table the segment carries none at all, so this fails outright rather than merely
// measuring slower.
func TestReverseScanOffsetsAreExtendedNotRebuilt(t *testing.T) {
	c := revStore(t, 1<<20)
	for i := range 200 {
		revPut(t, c, i)
	}
	revSeqs(t, c, "true", 5)

	act := c.shards[0].act
	n1, upto1, ok := act.recOffsState()
	if !ok {
		t.Fatal("the active segment kept no record offsets after a newest-first scan")
	}
	if n1 != 200 {
		t.Fatalf("offsets cover %d records, want 200", n1)
	}
	live := act.recOffs.Load().offs // the published slice itself, not a copy
	first := append([]uint32(nil), live...)

	// Few enough that the extension fits the backing array the first scan left room in, so
	// "extended" and "rebuilt" are distinguishable: an extension appends past the published
	// length in place, a rebuild allocates.
	for i := 200; i < 220; i++ {
		revPut(t, c, i)
	}
	// The window from before the appends is still describable: its offsets are a prefix.
	revSeqs(t, c, "true", 5)
	n2, upto2, ok := act.recOffsState()
	if !ok {
		t.Fatal("the active segment dropped its record offsets")
	}
	if n2 != 220 {
		t.Fatalf("offsets cover %d records after 20 more appends, want 220", n2)
	}
	if grown := act.recOffs.Load().offs; cap(live) >= len(grown) && &live[0] != &grown[0] {
		t.Fatal("the offsets were rebuilt rather than extended: the second scan reframed the segment")
	}
	if upto2 <= upto1 {
		t.Fatalf("offsets still stop at %d after appends (was %d)", upto2, upto1)
	}
	for i, o := range first {
		if act.recOffs.Load().offs[i] != o {
			t.Fatalf("extending rewrote offset %d: %d, was %d", i, act.recOffs.Load().offs[i], o)
		}
	}
	if act != c.shards[0].act {
		t.Fatal("the segment rolled during the test; shrink the record count or grow the segment")
	}
}

// TestReverseScanOffsetsDroppedWhenSegmentSeals holds the memory bound: offsets are kept for the
// one segment being appended to, so a store with a thousand sealed segments carries one table, not
// a thousand.
func TestReverseScanOffsetsDroppedWhenSegmentSeals(t *testing.T) {
	c := revStore(t, 1<<16)
	for i := range 200 {
		revPut(t, c, i)
	}
	revSeqs(t, c, "true", 5)
	sh := c.shards[0]
	first := sh.act
	if _, _, ok := first.recOffsState(); !ok {
		t.Fatal("the active segment kept no record offsets")
	}
	for i := 200; sh.act == first; i++ {
		revPut(t, c, i)
		if i > 20000 {
			t.Fatal("the segment never rolled")
		}
	}
	if _, _, ok := first.recOffsState(); ok {
		t.Fatal("a sealed segment kept its record offsets")
	}
	revSeqs(t, c, "true", 5)
	if _, _, ok := sh.act.recOffsState(); !ok {
		t.Fatal("the new write target kept no record offsets")
	}
	if _, _, ok := first.recOffsState(); ok {
		t.Fatal("a scan rebuilt a sealed segment's record offsets")
	}
	// A scan that walks the whole store backward, through every sealed segment, must not
	// leave a table behind in each one -- that is what would make the memory a function of
	// the store rather than of the segment being written.
	revSeqs(t, c, "true", 0)
	// Every segment but the write target: one table for the whole store.
	tables := 0
	for _, seg := range sh.segs {
		if seg == nil {
			continue
		}
		if _, _, ok := seg.recOffsState(); ok {
			tables++
		}
	}
	if tables != 1 {
		t.Fatalf("%d segments carry a record-offset table across %d segments, want 1", tables, len(sh.segs))
	}
}

// TestReverseScanDuringAppends is the failure mode the structure is shaped around: the segment a
// newest-first scan reads is the one being appended to, and a table that grew by reallocating under
// a reader would be a data race and a torn read, not a theoretical one. Run with -race.
//
// Each scan must see a contiguous newest-first run -- every record of an append log is visible, so
// the K newest are K consecutive ordinals -- however far the writer has got.
func TestReverseScanDuringAppends(t *testing.T) {
	c := revStore(t, 1<<18)
	for i := range 100 {
		revPut(t, c, i)
	}
	var written atomic.Int64
	written.Store(100)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 100; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			ad, err := classad.Parse(fmt.Sprintf(`[ Seq=%d; Owner="user%d"; Pad="%s" ]`, i, i%7, "xxxxxxxxxxxxxxxx"))
			if err != nil {
				return
			}
			if err := c.Put([]byte(fmt.Sprintf("k%08d", i)), ad); err != nil {
				return
			}
			written.Store(int64(i) + 1)
		}
	}()
	q, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	var fail atomic.Pointer[string]
	for r := range 4 {
		wg.Add(1)
		go func(limit int) {
			defer wg.Done()
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				var got []int
				for ad := range c.Query(q) {
					v, ok := ad.EvaluateAttrInt("Seq")
					if !ok {
						msg := "ad without Seq during concurrent appends"
						fail.Store(&msg)
						return
					}
					got = append(got, int(v))
					if len(got) >= limit {
						break
					}
				}
				if len(got) != limit {
					msg := fmt.Sprintf("newest-first scan returned %d rows, want %d", len(got), limit)
					fail.Store(&msg)
					return
				}
				for i, s := range got {
					if s != got[0]-i {
						msg := fmt.Sprintf("newest-first rows are not a contiguous run: %v", got)
						fail.Store(&msg)
						return
					}
				}
				if int64(got[0]) >= written.Load()+1 {
					msg := fmt.Sprintf("newest row Seq=%d is past what was written (%d)", got[0], written.Load())
					fail.Store(&msg)
					return
				}
			}
		}(2 + r*3)
	}
	time.Sleep(2 * time.Second)
	close(stop)
	wg.Wait()
	if msg := fail.Load(); msg != nil {
		t.Fatal(*msg)
	}
	if written.Load() < 1000 {
		t.Fatalf("only %d records were appended; the test did not exercise growth", written.Load())
	}
}
