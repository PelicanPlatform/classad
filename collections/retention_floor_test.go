package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// buildAgedArchive opens an append-only collection with a CompletionDate zone and n records
// carrying a strictly increasing CompletionDate (base..base+n-1), so older segments have
// strictly older zone maxima. Returns the collection, its shard, and the interned age id.
func buildAgedArchive(t *testing.T, ret Retention, n, base int) (*Collection, *shard, uint32) {
	t.Helper()
	c, err := Open(Options{
		AppendOnly:  true,
		Dir:         t.TempDir(),
		SegmentSize: 1 << 12, // tiny ⇒ many segments
		ZoneAttrs:   []string{"CompletionDate"},
		Retention:   ret,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d; CompletionDate = %d ]`, i, base+i))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}
	sh := c.shards[0]
	if liveSegs(sh) <= 4 {
		t.Fatalf("need several segments to exercise retention, got %d", liveSegs(sh))
	}
	cdID, ok := c.intern.LookupID("CompletionDate")
	if !ok {
		t.Fatal("CompletionDate not interned")
	}
	return c, sh, cdID
}

// assertOldestAtLeast checks every live sealed segment's newest CompletionDate is >= want
// (i.e. rotation stopped at the right floor and never dropped younger data).
func assertOldestAtLeast(t *testing.T, sh *shard, cdID uint32, want float64) {
	t.Helper()
	for i, seg := range sh.segs {
		if seg == nil || seg == sh.act {
			continue
		}
		if z, ok := seg.zones[cdID]; ok && z.Max < want {
			t.Errorf("segment %d survived with CompletionDate max %.0f < %.0f", i, z.Max, want)
		}
	}
}

// TestCeilingIgnoresGCFloor verifies the correction: a hard ceiling (here MaxSegments, and
// MaxAge) is enforced regardless of the GC floor, so a slow or absent consumer -- which holds
// a low or unset floor -- can never keep data past the configured retention policy.
func TestCeilingIgnoresGCFloor(t *testing.T) {
	// MaxSegments ceiling: no floor set at all (nothing acknowledged) still rotates to 3.
	c, sh, _ := buildAgedArchive(t, Retention{MaxSegments: 3}, 300, 1000)
	defer c.Close()
	c.SetGCFloor(0)
	if _, err := c.Rotate(0); err != nil {
		t.Fatal(err)
	}
	if got := liveSegs(sh); got != 3 {
		t.Fatalf("MaxSegments with no ack floor left %d segments, want 3 (ceiling must win)", got)
	}

	// MaxAge ceiling: un-acknowledged data (floor unset) older than MaxAge is still dropped.
	c2, sh2, cd2 := buildAgedArchive(t, Retention{MaxAgeAttr: "CompletionDate", MaxAge: 100}, 300, 1000)
	defer c2.Close()
	const now = 1000 + 300 - 1 // newest CompletionDate
	c2.SetGCFloor(0)           // nothing consumed
	before := liveSegs(sh2)
	if _, err := c2.Rotate(now); err != nil {
		t.Fatal(err)
	}
	if liveSegs(sh2) >= before {
		t.Fatalf("MaxAge dropped nothing (%d→%d); un-acked old data must still age out", before, liveSegs(sh2))
	}
	assertOldestAtLeast(t, sh2, cd2, now-100) // everything older than now-MaxAge is gone
}

// TestGCFloorEarlyDrainRespectsMinAge verifies the GC floor drains consumed records EARLY
// (below any MaxAge), but the MinAge minimum-retention floor keeps consumed-but-young records
// so the store behaves as a short-lived queue with a guaranteed drain window.
func TestGCFloorEarlyDrainRespectsMinAge(t *testing.T) {
	const n, base = 300, 1000
	const now = base + n - 1 // 1299

	// Loose MaxAge (won't fire) + a MinAge floor of 100 ⇒ nothing younger than now-100=1199
	// may be reclaimed, even once consumed.
	c, sh, cdID := buildAgedArchive(t, Retention{MaxAgeAttr: "CompletionDate", MaxAge: 100000, MinAgeAttr: "CompletionDate", MinAge: 100}, n, base)
	defer c.Close()

	// Subscribers have acknowledged everything below 1250. Early GC would like to drop up to
	// 1250, but MinAge caps it at 1199: records in [1199,1250) are consumed yet too young.
	c.SetGCFloor(1250)
	if _, err := c.Rotate(now); err != nil {
		t.Fatal(err)
	}
	liveAfter := liveSegs(sh)
	assertOldestAtLeast(t, sh, cdID, 1199) // MinAge floor held, not the 1250 GC floor
	// And it really did drain the fully-consumed, old-enough segments.
	for i, seg := range sh.segs {
		if seg == nil || seg == sh.act {
			continue
		}
		if z, ok := seg.zones[cdID]; ok && z.Max < 1199 {
			t.Fatalf("segment %d below the MinAge floor survived (max %.0f)", i, z.Max)
		}
	}

	// Keep the same GC floor (1250) but advance the clock to 1360 so now-MinAge=1260: the
	// previously-protected consumed records in [1199,1250) are now older than MinAge and
	// finally drain, down to the GC floor.
	c.SetGCFloor(1250)
	if _, err := c.Rotate(1360); err != nil {
		t.Fatal(err)
	}
	assertOldestAtLeast(t, sh, cdID, 1250)
	if liveSegs(sh) >= liveAfter {
		t.Fatalf("advancing the clock past MinAge should have drained more (%d→%d)", liveAfter, liveSegs(sh))
	}
}

// TestGCFloorUnsetNoEarlyDrain verifies that with no GC floor set, Rotate does no early
// draining -- only the configured ceilings apply. MinAge is configured with NO MaxAge, to
// confirm MinAge works independently of MaxAge.
func TestGCFloorUnsetNoEarlyDrain(t *testing.T) {
	// Only a MinAge (its own MinAgeAttr, no MaxAgeAttr/MaxAge, no floor): nothing should drop.
	c, sh, _ := buildAgedArchive(t, Retention{MinAgeAttr: "CompletionDate", MinAge: 100}, 300, 1000)
	defer c.Close()
	before := liveSegs(sh)
	if d, err := c.Rotate(1000 + 299); err != nil || d != 0 {
		t.Fatalf("Rotate with only MinAge dropped %d (err %v), want 0 (no floor, no ceiling)", d, err)
	}
	if liveSegs(sh) != before {
		t.Fatalf("segments changed %d→%d with nothing to enforce", before, liveSegs(sh))
	}
}

// TestMaxBytesWinsOverMinAge verifies a hard ceiling evicts even data younger than MinAge:
// MinAge (with a GC floor that has "consumed" everything) protects records from the early
// drain, but MaxBytes still rotates the store down under byte pressure.
func TestMaxBytesWinsOverMinAge(t *testing.T) {
	// MinAge is enormous (protects every record from the drain); a GC floor above everything
	// marks it all consumed. Only MaxBytes should force eviction.
	c, sh, _ := buildAgedArchive(t, Retention{MaxBytes: 3 << 12, MinAgeAttr: "CompletionDate", MinAge: 1e9}, 300, 1000)
	defer c.Close()
	c.SetGCFloor(1e18) // everything below is "consumed" -- but MinAge protects it from the drain
	before := liveSegs(sh)
	if _, err := c.Rotate(1000 + 299); err != nil {
		t.Fatal(err)
	}
	if liveSegs(sh) >= before {
		t.Fatalf("MaxBytes dropped nothing (%d→%d); it must win over MinAge under pressure", before, liveSegs(sh))
	}
	var total int64
	for _, seg := range sh.segs {
		if seg != nil {
			total += int64(seg.used)
		}
	}
	if total > 3<<12 {
		t.Fatalf("after Rotate total bytes %d still over MaxBytes %d", total, 3<<12)
	}
}
