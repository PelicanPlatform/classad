package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func liveSegs(sh *shard) int {
	n := 0
	for _, s := range sh.segs {
		if s != nil {
			n++
		}
	}
	return n
}

func countDatFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".dat") {
			n++
		}
		return nil
	})
	return n
}

// TestRetentionRotate verifies Rotate drops whole oldest segments to obey MaxSegments,
// keeps the active segment, keeps the newest records, decrements Count, unlinks the
// dropped files, and leaves a state that reopens consistently.
func TestRetentionRotate(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{
		AppendOnly:  true,
		Dir:         dir,
		SegmentSize: 1 << 12, // tiny ⇒ many segments
		Retention:   Retention{MaxSegments: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	const n = 300
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d ]`, i))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}
	sh := c.shards[0]
	segsBefore := liveSegs(sh)
	if segsBefore <= 3 {
		t.Fatalf("need >3 segments to exercise rotation, got %d (raise n or lower SegmentSize)", segsBefore)
	}
	if got := c.Len(); got != n {
		t.Fatalf("Len before rotate = %d, want %d", got, n)
	}

	dropped, err := c.Rotate(0)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != segsBefore-3 {
		t.Fatalf("dropped %d segments, want %d (from %d down to 3)", dropped, segsBefore-3, segsBefore)
	}
	if got := liveSegs(sh); got != 3 {
		t.Fatalf("live segments after rotate = %d, want 3", got)
	}

	// Surviving records are the newest ones; the oldest N values are gone, and Count
	// reflects only what remains.
	var lo, hi int64 = 1 << 30, -1
	scanned := 0
	for ad := range c.Scan() {
		v, _ := ad.EvaluateAttrInt("N")
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
		scanned++
	}
	if hi != n-1 {
		t.Errorf("newest surviving N = %d, want %d (newest never dropped)", hi, n-1)
	}
	if lo == 0 {
		t.Errorf("oldest record (N=0) survived; rotation should have dropped it")
	}
	if got := c.Len(); got != scanned {
		t.Fatalf("Len after rotate = %d, but Scan saw %d", got, scanned)
	}
	if scanned >= n {
		t.Fatalf("Scan saw %d records, expected fewer than %d after rotation", scanned, n)
	}

	// The dropped segment files are unlinked: exactly one .dat per surviving segment.
	if files := countDatFiles(t, dir); files != 3 {
		t.Errorf(".dat files on disk = %d, want 3 (dropped segments unlinked)", files)
	}

	// Rotate is idempotent once within bounds.
	again, err := c.Rotate(0)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("second Rotate dropped %d, want 0 (already within bounds)", again)
	}
	c.Close()

	// Reopen: the surviving records and count persist; scan matches.
	c2, err := Open(Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 12, Retention: Retention{MaxSegments: 3}})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	reopened := 0
	for range c2.Scan() {
		reopened++
	}
	if reopened != scanned {
		t.Fatalf("after reopen Scan saw %d, want %d", reopened, scanned)
	}
	if got := c2.Len(); got != scanned {
		t.Fatalf("after reopen Len = %d, want %d", got, scanned)
	}
}

// TestRetentionMaxBytes verifies the MaxBytes bound and that a zero Retention (and a
// non-append-only collection) make Rotate inert.
func TestRetentionMaxBytes(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 12, Retention: Retention{MaxBytes: 3 << 12}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d ]`, i))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Rotate(0); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, s := range c.shards[0].segs {
		if s != nil {
			total += int64(s.used)
		}
	}
	if total > 3<<12 {
		// One over-bound step is allowed only when a single active segment exceeds the
		// bound; here segments are ~4KB and the bound is ~12KB, so it must hold.
		t.Errorf("retained bytes = %d, want <= %d", total, int64(3<<12))
	}
	c.Close()

	// Zero Retention ⇒ inert.
	c3, _ := Open(Options{AppendOnly: true, SegmentSize: 1 << 12})
	for i := 0; i < 200; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d ]`, i))
		_ = c3.Put([]byte("k"), ad)
	}
	if d, _ := c3.Rotate(0); d != 0 {
		t.Errorf("zero-Retention Rotate dropped %d, want 0", d)
	}
	c3.Close()

	// Non-append-only ⇒ inert even with a Retention set.
	c4 := New(Options{Retention: Retention{MaxSegments: 1}})
	if d, _ := c4.Rotate(0); d != 0 {
		t.Errorf("non-append-only Rotate dropped %d, want 0", d)
	}
}
