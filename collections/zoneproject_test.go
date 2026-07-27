package collections

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// decompCountingCodec wraps identity storage and counts Decompress calls, so a test can prove that a
// zone-pruned scan never touches (decompresses) the records of a skipped segment.
type decompCountingCodec struct{ decompressions atomic.Int64 }

func (c *decompCountingCodec) Compress(dst, src []byte) []byte { return append(dst, src...) }
func (c *decompCountingCodec) Decompress(dst, src []byte) ([]byte, error) {
	c.decompressions.Add(1)
	return append(dst, src...), nil
}
func (c *decompCountingCodec) Name() string { return "counting" }

// TestZoneMapPruneProjectedPaths is the regression test for the archive-aggregate bug: a range
// query on a zone-mapped attribute must skip whole sealed segments in the PROJECTED scan paths
// (QueryProject, which backs COUNT/GROUP BY aggregates, and QueryRawProjected, which backs
// column-projected SELECTs) -- not only in the full Query path. Before the fix these two paths
// never set qp.zoneProbes, so a `SELECT COUNT(*) ... WHERE Ts > <future>` full-scanned every
// segment instead of pruning. We prove pruning by counting record decompressions: a fully-out-of-
// range predicate must decompress nothing.
func TestZoneMapPruneProjectedPaths(t *testing.T) {
	codec := &decompCountingCodec{}
	c := New(Options{AppendOnly: true, SegmentSize: 1 << 12, ZoneAttrs: []string{"Ts"}, Codec: codec})
	const n = 400
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d; Ts = %d ]`, i, i))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}
	sh := c.shards[0]
	sealed := sealedWithZones(sh)
	if sealed == 0 {
		t.Fatal("expected sealed segments to carry zone maps")
	}

	// A predicate above the whole Ts range matches nothing and -- with pruning -- must not
	// decompress a single record of any sealed segment. (The active/unsealed segment has no zone
	// map and is always walked, so allow its records to be touched.)
	activeRecords := int64(countActiveRecords(sh))

	// --- QueryProject (aggregate path) ---
	q, _ := vm.Parse(`Ts >= 100000`)
	codec.decompressions.Store(0)
	rows := 0
	for range c.QueryProject(q, []string{"N"}) {
		rows++
	}
	if rows != 0 {
		t.Fatalf("QueryProject(Ts>=100000) returned %d rows, want 0", rows)
	}
	if got := codec.decompressions.Load(); got > activeRecords {
		t.Fatalf("QueryProject decompressed %d records for an out-of-range predicate (active seg has %d); "+
			"sealed segments were not pruned", got, activeRecords)
	}

	// --- QueryRawProjected (projected-SELECT path) ---
	codec.decompressions.Store(0)
	rows = 0
	for range c.QueryRawProjected(q, []string{"N"}, false, false) {
		rows++
	}
	if rows != 0 {
		t.Fatalf("QueryRawProjected(Ts>=100000) returned %d rows, want 0", rows)
	}
	if got := codec.decompressions.Load(); got > activeRecords {
		t.Fatalf("QueryRawProjected decompressed %d records for an out-of-range predicate (active seg has %d); "+
			"sealed segments were not pruned", got, activeRecords)
	}

	// Correctness: a selective in-range predicate returns exactly the right count through the
	// projected path (pruning must never drop a match).
	q2, _ := vm.Parse(`Ts >= 380`)
	got := 0
	for vals := range c.QueryProject(q2, []string{"Ts"}) {
		ts, _ := vals[0].IntValue()
		if ts < 380 {
			t.Fatalf("QueryProject(Ts>=380) yielded Ts=%d < 380", ts)
		}
		got++
	}
	if got != n-380 {
		t.Errorf("QueryProject(Ts>=380) returned %d rows, want %d", got, n-380)
	}
}

// countActiveRecords returns how many records live in the shard's unsealed (active) segment,
// which has no zone map and is therefore always scanned.
func countActiveRecords(sh *shard) int {
	if sh.act == nil {
		return 0
	}
	n := 0
	for off := 0; off < sh.act.used; {
		total := recTotalLen(sh.act.data, uint32(off))
		if total == 0 {
			break
		}
		n++
		off += int(total)
	}
	return n
}
