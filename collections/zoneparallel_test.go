package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// zonedParallelFixture is an append-only, zone-mapped collection whose full-scan queries take
// the cross-segment fan-out path: enough records to seal many segments (so there are >= 2
// tasks), QueryParallelism above 1, and the size gate lifted so a small corpus still fans out.
// The counting codec makes "was this segment read at all" observable, since a pruned segment's
// records are never decompressed.
func zonedParallelFixture(t *testing.T, n int) (*Collection, *decompCountingCodec) {
	t.Helper()
	codec := &decompCountingCodec{}
	c := New(Options{
		AppendOnly:       true,
		Shards:           1,
		SegmentSize:      1 << 12,
		ZoneAttrs:        []string{"Ts"},
		Codec:            codec,
		QueryParallelism: 8,
	})
	c.parallelMinBytes = 0 // force fan-out: the corpus is small on purpose
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d; Ts = %d ]`, i, i))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}
	if sealedWithZones(c.shards[0]) == 0 {
		t.Fatal("expected sealed segments to carry zone maps")
	}
	return c, codec
}

// TestZoneMapPruneParallelQuery covers the one scan path the shared plan could not reach: the
// cross-segment fan-out gathers its own windows instead of going through the serial walk, so a
// zoned collection full-scanned in parallel what the same query skipped serially.
//
// Both halves matter. Pruning that drops a match is silent AND faster than the right answer, so
// the test asserts the skipping and the completeness of the result over the same fixture.
func TestZoneMapPruneParallelQuery(t *testing.T) {
	const n = 400
	c, codec := zonedParallelFixture(t, n)
	sh := c.shards[0]
	activeRecords := int64(countActiveRecords(sh))

	// A predicate above the whole Ts range matches nothing, so every sealed segment is provably
	// empty of matches and must never be read. The active segment carries no zone map and is
	// always walked.
	q, _ := vm.Parse(`Ts >= 100000`)
	codec.decompressions.Store(0)
	rows := 0
	for range c.Query(q) {
		rows++
	}
	if rows != 0 {
		t.Fatalf("Query(Ts>=100000) returned %d rows, want 0", rows)
	}
	if got := codec.decompressions.Load(); got > activeRecords {
		t.Fatalf("parallel Query decompressed %d records for an out-of-range predicate (active seg has %d); "+
			"the fan-out path is not zone-pruning", got, activeRecords)
	}

	// Completeness: a predicate that straddles the zone boundaries must return every matching
	// record, each one actually satisfying it.
	for _, lo := range []int{0, 1, 137, 380, n - 1} {
		q2, _ := vm.Parse(fmt.Sprintf(`Ts >= %d`, lo))
		got := 0
		for ad := range c.Query(q2) {
			ts, ok := ad.EvaluateAttrInt("Ts")
			if !ok || int(ts) < lo {
				t.Fatalf("Query(Ts>=%d) yielded Ts=%d (ok=%v)", lo, ts, ok)
			}
			got++
		}
		if want := n - lo; got != want {
			t.Errorf("Query(Ts>=%d) returned %d rows, want %d", lo, got, want)
		}
	}
}

// TestZoneMapPruneParallelStats asserts the pruning in the units the scan reports it in:
// SegmentsPruned on the plan's stats sink. Query has no stats-bearing entry point, so the
// fan-out is driven directly -- which is also the only way to be sure the counts come from the
// parallel path rather than from a serial fallback.
func TestZoneMapPruneParallelStats(t *testing.T) {
	const n = 400
	c, _ := zonedParallelFixture(t, n)

	q, _ := vm.Parse(`Ts >= 100000`)
	var st ScanStats
	rows := 0
	c.runParallelQuery(c.newQueryPlan(q, nil, &st, false), func(*classad.ClassAd) bool {
		rows++
		return true
	}, false)
	if rows != 0 {
		t.Fatalf("runParallelQuery(Ts>=100000) yielded %d rows, want 0", rows)
	}
	if st.SegmentsPruned == 0 {
		t.Fatalf("runParallelQuery pruned no segments for an out-of-range predicate "+
			"(total=%d scanned=%d); the fan-out path is not passing zone probes",
			st.SegmentsTotal, st.SegmentsScanned)
	}
	if st.SegmentsScanned+st.SegmentsPruned != st.SegmentsTotal {
		t.Errorf("segment counts do not add up: total=%d pruned=%d scanned=%d",
			st.SegmentsTotal, st.SegmentsPruned, st.SegmentsScanned)
	}

	// A predicate inside the range must leave some segment unpruned and return every match --
	// pruning that drops a match would show here as a short count, not as an error.
	q2, _ := vm.Parse(`Ts >= 380`)
	var st2 ScanStats
	got := 0
	c.runParallelQuery(c.newQueryPlan(q2, nil, &st2, false), func(*classad.ClassAd) bool {
		got++
		return true
	}, false)
	if got != n-380 {
		t.Errorf("runParallelQuery(Ts>=380) yielded %d rows, want %d", got, n-380)
	}
	if st2.SegmentsScanned == 0 {
		t.Errorf("an in-range predicate pruned every segment (total=%d)", st2.SegmentsTotal)
	}
}

// TestZoneMapParallelMatchesSerial pins the fan-out path to the serial one: over the same
// corpus and the same queries, parallel and serial scans must return identical multisets of
// Ts values. A pruning bug that drops a segment's matches shows up here as a difference rather
// than as a plausible-looking shorter answer.
func TestZoneMapParallelMatchesSerial(t *testing.T) {
	const n = 400
	par, _ := zonedParallelFixture(t, n)

	serial := New(Options{AppendOnly: true, Shards: 1, SegmentSize: 1 << 12,
		ZoneAttrs: []string{"Ts"}, QueryParallelism: 1})
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d; Ts = %d ]`, i, i))
		if err := serial.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}

	collect := func(c *Collection, expr string) map[int64]int {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]int{}
		for ad := range c.Query(q) {
			ts, _ := ad.EvaluateAttrInt("Ts")
			out[ts]++
		}
		return out
	}
	for _, expr := range []string{
		`Ts >= 380`,
		`Ts < 25`,
		`Ts >= 100 && Ts < 300`,
		`Ts >= 100000`,
		`N % 7 == 0 && Ts >= 200`,
	} {
		want := collect(serial, expr)
		got := collect(par, expr)
		if len(want) != len(got) {
			t.Errorf("%s: parallel returned %d distinct Ts, serial %d", expr, len(got), len(want))
			continue
		}
		for ts, nw := range want {
			if got[ts] != nw {
				t.Errorf("%s: Ts=%d appeared %d times in parallel, %d serially", expr, ts, got[ts], nw)
			}
		}
	}
}
