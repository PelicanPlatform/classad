package collections

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// seqLeaseClock is a settable clock for the lease tests, installed on every shard of a collection.
type seqLeaseClock struct {
	mu  sync.Mutex
	now time.Time
}

func (k *seqLeaseClock) Now() time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.now
}

func (k *seqLeaseClock) Advance(d time.Duration) {
	k.mu.Lock()
	k.now = k.now.Add(d)
	k.mu.Unlock()
}

func installSeqLeaseClock(c *Collection) *seqLeaseClock {
	k := &seqLeaseClock{now: time.Unix(1_000_000, 0)}
	for _, sh := range c.shards {
		sh.mu.Lock()
		sh.leaseNow = k.Now
		sh.mu.Unlock()
	}
	return k
}

// seqPageIDs reads one page and returns the Id of each row.
func seqPageIDs(t *testing.T, c *Collection, cursor SeqCursor, limit int) ([]int, SeqPage) {
	t.Helper()
	seq, page := c.QueryRawFromSeq(nil, nil, cursor, limit)
	var ids []int
	for raw := range seq {
		ids = append(ids, rawIDOf(t, raw))
	}
	return ids, *page
}

// rewriteAll replaces every key 0..n-1 with a fresh version.
func rewriteAll(t *testing.T, c *Collection, n, gen int) {
	t.Helper()
	for i := range n {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; Gen=%d]`, i, gen))); err != nil {
			t.Fatal(err)
		}
	}
}

// patchAll patches every key 0..n-1, which writes a delta record where delta mode is on.
func patchAll(t *testing.T, c *Collection, n, gen int) {
	t.Helper()
	tx := c.Begin()
	for i := range n {
		p := classad.New()
		p.InsertAttr("Gen", int64(gen))
		tx.PatchAttrs([]byte(fmt.Sprintf("k%d", i)), p, nil)
	}
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("patch commit conflicted")
	}
}

func putAll(t *testing.T, c *Collection, n int) {
	t.Helper()
	for i := range n {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; Gen=0]`, i))); err != nil {
			t.Fatal(err)
		}
	}
}

// openDeltaStore opens a persistent single-shard collection in delta mode.
func openDeltaStore(t *testing.T, lease time.Duration) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 14, DeltaMax: 8, GroupSchemaCount: -1, SeqCursorLease: lease})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// finishPaging pages from cursor to the end, failing on an error, and returns the ids.
func finishPaging(t *testing.T, c *Collection, cursor SeqCursor, limit int) []int {
	t.Helper()
	var ids []int
	for pages := 0; ; pages++ {
		if pages > 10000 {
			t.Fatal("pagination did not terminate")
		}
		got, page := seqPageIDs(t, c, cursor, limit)
		if page.Err != nil {
			t.Fatalf("page %d: %v", pages, page.Err)
		}
		ids = append(ids, got...)
		if !page.More {
			return ids
		}
		cursor = page.Next
	}
}

// checkEachOnce fails unless ids holds every id 0..n-1 exactly once.
func checkEachOnce(t *testing.T, ids []int, n int) {
	t.Helper()
	if err := eachOnce(ids, n); err != nil {
		t.Fatal(err)
	}
}

// eachOnce reports how ids fails to hold every id 0..n-1 exactly once, or nil.
func eachOnce(ids []int, n int) error {
	seen := make([]int, n)
	for _, id := range ids {
		if id < 0 || id >= n {
			return fmt.Errorf("id %d out of range", id)
		}
		seen[id]++
	}
	missing := 0
	for id, count := range seen {
		if count > 1 {
			return fmt.Errorf("id %d seen %d times", id, count)
		}
		if count == 0 {
			missing++
		}
	}
	if missing > 0 {
		return fmt.Errorf("%d of %d ids missing from the listing", missing, n)
	}
	return nil
}

// TestSeqCursorSurvivesRewriteAndCompaction: every key is rewritten between two pages and the shard
// is compacted, which used to reclaim every version the pagination's snapshot sees and return the
// rest of the listing short (55 of 400 rows). The lease keeps those versions, in the history stream
// time travel uses.
//
// Rewriting every key leaves whole segments dead, which the dead-segment reclaim would unlink;
// rewriting three keys in four leaves segments mostly dead, which compaction rewrites, copying what
// the lease keeps into history segments.
func TestSeqCursorSurvivesRewriteAndCompaction(t *testing.T) {
	t.Parallel()
	const n = 400
	for _, tc := range []struct {
		name    string
		rewrite func(i int) bool
	}{
		{"reclaim", func(int) bool { return true }},
		{"rewrite", func(i int) bool { return i%4 != 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(Options{Shards: 1, SegmentSize: 4096})
			putAll(t, c, n)
			first, page := seqPageIDs(t, c, SeqCursor{}, 20)
			if page.Err != nil || !page.More {
				t.Fatalf("first page: more %v err %v", page.More, page.Err)
			}
			for i := range n {
				if tc.rewrite(i) {
					if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; Gen=1]`, i))); err != nil {
						t.Fatal(err)
					}
				}
			}
			c.Compact()
			checkEachOnce(t, append(first, finishPaging(t, c, page.Next, 20)...), n)
		})
	}
}

// TestSeqCursorDeltaStoreDefersReclaim is the same listing over a store with delta records, where
// compaction cannot keep a superseded delta without its base: the lease defers the shard's
// reclamation instead.
func TestSeqCursorDeltaStoreDefersReclaim(t *testing.T) {
	t.Parallel()
	const n = 400
	c := openDeltaStore(t, 0)
	defer c.Close()
	putAll(t, c, n)
	first, page := seqPageIDs(t, c, SeqCursor{}, 20)
	for gen := 1; gen <= 3; gen++ {
		patchAll(t, c, n, gen)
	}
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no delta records; the delta path goes untested")
	}
	deferrals, _, _ := SeqCursorStats()
	c.Compact()
	if after, _, _ := SeqCursorStats(); after == deferrals {
		t.Fatal("the compaction pass was not deferred")
	}
	checkEachOnce(t, append(first, finishPaging(t, c, page.Next, 20)...), n)
}

// TestSeqCursorExpiredLeaseFailsLoudly: once a lease has run out and maintenance reclaims past the
// snapshot, a resumed cursor is refused with ErrSeqCursorExpired and yields nothing -- never a
// listing that silently ends early -- and the restarted listing is complete.
func TestSeqCursorExpiredLeaseFailsLoudly(t *testing.T) {
	t.Parallel()
	const n = 400
	for _, tc := range []struct {
		name  string
		open  func(t *testing.T) *Collection
		write func(t *testing.T, c *Collection, gen int)
	}{
		{"whole-records", func(*testing.T) *Collection { return New(Options{Shards: 1, SegmentSize: 4096}) },
			func(t *testing.T, c *Collection, gen int) { rewriteAll(t, c, n, gen) }},
		{"delta-records", func(t *testing.T) *Collection { return openDeltaStore(t, 0) },
			func(t *testing.T, c *Collection, gen int) { patchAll(t, c, n, gen) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.open(t)
			defer c.Close()
			clock := installSeqLeaseClock(c)
			putAll(t, c, n)
			_, page := seqPageIDs(t, c, SeqCursor{}, 20)
			for gen := 1; gen <= 3; gen++ {
				tc.write(t, c, gen)
			}
			clock.Advance(defaultSeqCursorLease + time.Second)
			compacted := c.Compact()
			clock.Advance(defaultSeqCursorLease + time.Second)
			if compacted+c.Compact() == 0 {
				t.Fatal("nothing compacted")
			}
			ids, resumed := seqPageIDs(t, c, page.Next, 20)
			if !errors.Is(resumed.Err, ErrSeqCursorExpired) {
				t.Fatalf("resumed page: err %v, want ErrSeqCursorExpired", resumed.Err)
			}
			if len(ids) != 0 || resumed.More {
				t.Fatalf("an expired cursor yielded %d rows, more %v", len(ids), resumed.More)
			}
			checkEachOnce(t, finishPaging(t, c, SeqCursor{}, 20), n)
		})
	}
}

// TestSeqCursorDeltaDeferralIsBounded: a delta store's reclamation waits for a lease at most one
// lease period, however many new paginations keep arriving, so their garbage cannot pile up for
// ever.
func TestSeqCursorDeltaDeferralIsBounded(t *testing.T) {
	t.Parallel()
	const n = 200
	c := openDeltaStore(t, 0)
	defer c.Close()
	clock := installSeqLeaseClock(c)
	putAll(t, c, n)
	for gen := 1; ; gen++ {
		if gen > 20 {
			t.Fatal("reclamation stayed deferred for more than a lease period")
		}
		// A new pagination takes a fresh snapshot, and a write lands after it.
		_, page := seqPageIDs(t, c, SeqCursor{}, 5)
		patchAll(t, c, n, gen)
		if c.Compact() > 0 {
			// The pass proceeded by revoking the leases, so the newest pagination's
			// cursor must now be refused rather than read short.
			if _, p := seqPageIDs(t, c, page.Next, 5); !errors.Is(p.Err, ErrSeqCursorExpired) {
				t.Fatalf("after the revoking pass: err %v, want ErrSeqCursorExpired", p.Err)
			}
			if gen < 4 {
				t.Fatalf("compacted on pass %d, inside the deferral period", gen)
			}
			return
		}
		clock.Advance(defaultSeqCursorLease / 4)
	}
}

// TestSeqCursorWithoutLeaseFailsLoudly: with leasing disabled the reclaim runs at once, and the
// cursor is refused rather than read short.
func TestSeqCursorWithoutLeaseFailsLoudly(t *testing.T) {
	t.Parallel()
	const n = 400
	c := New(Options{Shards: 1, SegmentSize: 4096, SeqCursorLease: -1})
	putAll(t, c, n)
	_, page := seqPageIDs(t, c, SeqCursor{}, 20)
	rewriteAll(t, c, n, 1)
	if c.Compact() == 0 {
		t.Fatal("nothing compacted")
	}
	if _, p := seqPageIDs(t, c, page.Next, 20); !errors.Is(p.Err, ErrSeqCursorExpired) {
		t.Fatalf("err %v, want ErrSeqCursorExpired", p.Err)
	}
}

// TestSeqCursorAcrossReopen: what an earlier process reclaimed is not recorded, so a cursor issued
// before a reopen is refused once its shard has moved -- and still served when it has not.
func TestSeqCursorAcrossReopen(t *testing.T) {
	t.Parallel()
	const n = 300
	opts := Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 13}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	putAll(t, c, n)
	first, page := seqPageIDs(t, c, SeqCursor{}, 20)
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if c, err = Open(opts); err != nil {
		t.Fatal(err)
	}
	// Nothing written since: the cursor still names a complete snapshot.
	checkEachOnce(t, append(first, finishPaging(t, c, page.Next, 20)...), n)

	_, page = seqPageIDs(t, c, SeqCursor{}, 20)
	rewriteAll(t, c, n, 1)
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if c, err = Open(opts); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, p := seqPageIDs(t, c, page.Next, 20); !errors.Is(p.Err, ErrSeqCursorExpired) {
		t.Fatalf("after writes and a reopen: err %v, want ErrSeqCursorExpired", p.Err)
	}
}

// TestSeqCursorColumnarizeDropRaisesFloor: columnarizing a sealed segment drops the superseded delta
// records at or below the retain floor, which reclaims versions as compaction does. Once the lease
// is gone, a cursor whose snapshot saw them is refused.
func TestSeqCursorColumnarizeDropRaisesFloor(t *testing.T) {
	t.Parallel()
	c := openDeltaStore(t, 0)
	defer c.Close()
	clock := installSeqLeaseClock(c)
	rng := rand.New(rand.NewSource(1))
	now := time.Now().Unix()
	next := 0
	for ; next < 600; next++ {
		if err := c.Put([]byte(fmt.Sprintf("%d.0", next)), seqFixtureAd(next, now, rng)); err != nil {
			t.Fatal(err)
		}
	}
	seq, page := c.QueryRawFromSeq(nil, nil, SeqCursor{}, 20)
	for range seq {
	}
	for range 400 {
		seqWriter(t, c, rng, &next, now)
	}
	clock.Advance(defaultSeqCursorLease + time.Second)
	c.Reindex()
	before := colDeltasDropped.Load()
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema")
	}
	c.ColumnarizeSealed()
	if colDeltasDropped.Load() == before {
		t.Fatal("columnarization dropped no delta records; the drop path goes untested")
	}
	seq, resumed := c.QueryRawFromSeq(nil, nil, page.Next, 20)
	for range seq {
		t.Fatal("an expired cursor yielded a row")
	}
	if !errors.Is(resumed.Err, ErrSeqCursorExpired) {
		t.Fatalf("err %v, want ErrSeqCursorExpired", resumed.Err)
	}
}

// TestSeqReadConcurrentRewritesAndCompaction pages from several readers while a writer rewrites the
// keys they count and compaction runs. Within the lease every reader must see every key exactly
// once; with a lease too short to cover a listing, a reader may be refused, and must then restart
// and see every key exactly once -- never finish a listing short. Run it under -race.
func TestSeqReadConcurrentRewritesAndCompaction(t *testing.T) {
	t.Parallel()
	const n = 300
	for _, tc := range []struct {
		name  string
		lease time.Duration
		delta bool
	}{
		{"whole-records", 0, false},
		{"delta-records", 0, true},
		{"short-lease", time.Millisecond, false},
		{"short-lease-delta", time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var c *Collection
			if tc.delta {
				c = openDeltaStore(t, tc.lease)
			} else {
				c = New(Options{Shards: 2, SegmentSize: 4096, SeqCursorLease: tc.lease})
			}
			defer c.Close()
			putAll(t, c, n)
			var stop atomic.Bool
			var writer sync.WaitGroup
			writer.Add(1)
			go func() {
				defer writer.Done()
				// A bounded run: with a lease shorter than a listing, a listing completes only once
				// the rewrites and compactions stop overtaking it.
				for gen := 1; gen <= 2000 && !stop.Load(); gen++ {
					i := gen % n
					if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; Gen=%d]`, i, gen))); err != nil {
						t.Error(err)
						return
					}
					if gen%50 == 0 {
						c.Compact()
					}
				}
			}()
			var expired atomic.Int64
			var readers sync.WaitGroup
			for r := range 4 {
				readers.Add(1)
				go func() {
					defer readers.Done()
					completed := 0
				listing:
					for attempt := 0; completed < 3; attempt++ {
						if attempt >= 10000 {
							t.Errorf("reader %d completed %d listings in 10000 attempts", r, completed)
							return
						}
						var ids []int
						var cursor SeqCursor
						for pages := 0; pages < 10000; pages++ {
							seq, page := c.QueryRawFromSeq(nil, nil, cursor, 3+r)
							for raw := range seq {
								ids = append(ids, rawIDOf(t, raw))
							}
							if page.Err != nil {
								if !errors.Is(page.Err, ErrSeqCursorExpired) {
									t.Errorf("reader %d: %v", r, page.Err)
									return
								}
								expired.Add(1)
								continue listing
							}
							if !page.More {
								if err := eachOnce(ids, n); err != nil {
									t.Errorf("reader %d: %v", r, err)
									return
								}
								completed++
								continue listing
							}
							cursor = page.Next
							// Give the writer and compaction room to land between pages.
							time.Sleep(100 * time.Microsecond)
						}
						t.Errorf("reader %d: pagination did not terminate", r)
						return
					}
				}()
			}
			readers.Wait()
			stop.Store(true)
			writer.Wait()
			t.Logf("%d cursors refused as expired", expired.Load())
			if tc.lease == 0 && expired.Load() > 0 {
				t.Fatalf("%d cursors refused inside the lease", expired.Load())
			}
			// A delta store refuses only after a whole deferral period, which a loaded machine may
			// not reach in this run; the deterministic tests cover that path.
			if tc.lease > 0 && !tc.delta && expired.Load() == 0 {
				t.Fatal("no cursor outlived its lease; the refusal path goes untested")
			}
		})
	}
}
