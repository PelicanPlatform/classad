package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

// shortTrailerConn is a server-side transport that truncates every scan-stats trailer to the
// ELEVEN counts a server from before the tier wrote, and forwards everything else untouched. It
// is how the back-compat case is constructed rather than assumed: the client really does receive
// an older server's frame, through the real decode path.
type shortTrailerConn struct {
	MsgConn
}

func (c *shortTrailerConn) WriteMsg(b []byte) error {
	if _, status, body, ok := respHeader(b); ok && status == stStreamStats {
		head := len(b) - len(body.b) // the frame header, whatever its size
		if len(body.b) > 11*4 {
			b = b[:head+11*4]
		}
	}
	return c.MsgConn.WriteMsg(b)
}

// catServerPairShortTrailer is catServerPair against such a server.
func catServerPairShortTrailer(t *testing.T) (*Client, func()) {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(&shortTrailerConn{MsgConn: sconn}, ServeOptions{}) }()
	c := NewClient(cconn)
	return c, func() { c.Close(); s.Close(); cat.Close() }
}

// seedTierArchive creates "history" and appends n records. SegmentSize is small on purpose: with
// the default 8 MiB a fixture this size stays in one unsealed segment, where no segment is sealed
// and the fixture, not the code, decides which strategy can run.
func seedTierArchive(t *testing.T, c *Client, n int) (matched int) {
	t.Helper()
	ctx := context.Background()
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{
		SegmentSize: 1 << 12,
		ZoneAttrs:   []string{"ClusterId"},
	}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	for i := 0; i < n; i++ {
		status := 2
		if i%5 == 0 {
			status = 4
			matched++
		}
		if err := c.ArchiveAppend(ctx, "history", fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nJobStatus = %d", i, i%50, status)); err != nil {
			t.Fatalf("ArchiveAppend: %v", err)
		}
	}
	return matched
}

// TestArchiveAggregateTierOverRPC: the tier crosses the wire and distinguishes two strategies
// over the SAME archive -- an unconstrained COUNT(*) answered from the retained count without
// opening a segment, and a MAX over an attribute no accelerator covers, which walks records. Both
// used to arrive as the same all-zero / nonzero trailer and left the caller to infer the rest.
func TestArchiveAggregateTierOverRPC(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{})
	defer cleanup()
	ctx := context.Background()
	const n = 600
	matched := seedTierArchive(t, c, n)

	var fast collections.ScanStats
	rows, err := c.ArchiveAggregateStats(ctx, "history", "true", nil,
		[]AggSpec{{Func: AggCount, Arg: "*"}}, &fast)
	if err != nil {
		t.Fatalf("COUNT(*): %v", err)
	}
	if len(rows) != 1 || rows[0].Values[0] != fmt.Sprint(n) {
		t.Fatalf("COUNT(*) = %+v, want %d", rows, n)
	}
	if fast.Tier != collections.TierIndex {
		t.Errorf("COUNT(*) tier = %v, want %v (stats %+v)", fast.Tier, collections.TierIndex, fast)
	}

	var slow collections.ScanStats
	rows, err = c.ArchiveAggregateStats(ctx, "history", "JobStatus == 4", nil,
		[]AggSpec{{Func: AggMax, Arg: "ProcId"}}, &slow)
	if err != nil {
		t.Fatalf("MAX(ProcId): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("MAX(ProcId): want one row, got %+v", rows)
	}
	if slow.Tier != collections.TierScan {
		t.Errorf("MAX(ProcId) tier = %v, want %v (stats %+v)", slow.Tier, collections.TierScan, slow)
	}
	if slow.RowsMatched != matched {
		t.Errorf("RowsMatched = %d, want %d", slow.RowsMatched, matched)
	}

	// A constraint proved empty: no data read, and distinguishable from the fast paths above,
	// which is the case the zero-counters inference could never resolve.
	var none collections.ScanStats
	rows, err = c.ArchiveAggregateStats(ctx, "history", "false", nil,
		[]AggSpec{{Func: AggCount, Arg: "*"}}, &none)
	if err != nil {
		t.Fatalf("match-none: %v", err)
	}
	if len(rows) != 1 || rows[0].Values[0] != "0" {
		t.Fatalf("match-none COUNT(*) = %+v, want 0", rows)
	}
	if none.Tier != collections.TierMatchNone {
		t.Errorf("match-none tier = %v, want %v", none.Tier, collections.TierMatchNone)
	}
}

// TestArchiveAggregateTierOldServer is the wire-compatibility guarantee, constructed rather than
// assumed: against a server whose stats trailer stops at the eleven counts it knew before the
// tier existed, the client still gets its rows AND the full scan breakdown, with the tier left at
// TierUnreported -- "the server did not say", never a guess at which strategy ran.
func TestArchiveAggregateTierOldServer(t *testing.T) {
	c, cleanup := catServerPairShortTrailer(t)
	defer cleanup()
	ctx := context.Background()
	matched := seedTierArchive(t, c, 600)

	var st collections.ScanStats
	rows, err := c.ArchiveAggregateStats(ctx, "history", "JobStatus == 4", nil,
		[]AggSpec{{Func: AggMax, Arg: "ProcId"}}, &st)
	if err != nil {
		t.Fatalf("ArchiveAggregateStats against a server with a pre-tier trailer: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want one row, got %+v", rows)
	}
	if st.Tier != collections.TierUnreported {
		t.Errorf("tier = %v, want %v: a server that cannot report one must not appear to",
			st.Tier, collections.TierUnreported)
	}
	// Everything the shorter trailer does carry is still decoded, counts and index-probe block
	// alike -- truncating the field this change appended must cost nothing else.
	if st.RowsMatched != matched || st.RecordsVisited == 0 {
		t.Errorf("scan breakdown lost on a short trailer: %+v (want RowsMatched %d)", st, matched)
	}
}

// TestAggTierTrailerIsAppended pins the trailer layout the compatibility above rests on: the tier
// is the TWELFTH int32, appended after the index-probe counts exactly as those were appended after
// the original seven, and a reader that stops at eleven -- an older client reading a newer trailer
// -- recovers every count it knows unchanged.
func TestAggTierTrailerIsAppended(t *testing.T) {
	st := collections.ScanStats{SegmentsTotal: 1, SegmentsPruned: 2, SegmentsScanned: 3, RecordsVisited: 4,
		RecordsColumnDecided: 5, RecordsReassembled: 6, RowsMatched: 7,
		SegmentsIndexed: 8, SegmentsIndexSkipped: 9, SegmentsUnindexed: 10, IndexCandidates: 11,
		Tier: collections.TierColumnar}

	full := putScanStats(nil, st)
	if len(full) != 12*4 {
		t.Fatalf("trailer is %d bytes, want %d (eleven counts plus the tier)", len(full), 12*4)
	}
	r := &reader{b: full}
	if got := readScanStats(r); got != st || r.err != nil {
		t.Errorf("round trip: %+v (%v), want %+v", got, r.err, st)
	}

	// An older client reads only what it knows and leaves the rest in the frame, where the
	// message transport drops it with the frame -- it cannot desync anything.
	r = &reader{b: full}
	want := st
	want.Tier = collections.TierUnreported
	if got := readScanStatsElevenCount(r); got != want {
		t.Errorf("eleven-count read of a tiered trailer: %+v, want %+v", got, want)
	}

	// A newer client reads an older server's eleven-count trailer: the counts, then "did not say".
	r = &reader{b: full[:11*4]}
	got := readScanStats(r)
	if r.err != nil || got.Tier != collections.TierUnreported {
		t.Errorf("eleven-count trailer: tier %v (%v), want %v", got.Tier, r.err, collections.TierUnreported)
	}
	if got.IndexCandidates != 11 || got.RowsMatched != 7 {
		t.Errorf("eleven-count trailer lost counts: %+v", got)
	}

	// And the seven-count trailer older still, which must keep reading as it always did.
	r = &reader{b: full[:7*4]}
	got = readScanStats(r)
	if r.err != nil || got.RowsMatched != 7 || got.IndexCandidates != 0 || got.Tier != collections.TierUnreported {
		t.Errorf("seven-count trailer: %+v (%v)", got, r.err)
	}
}

// readScanStatsElevenCount is readScanStats as it stood before the tier was appended: it decodes
// the eleven counts and stops. Kept here so the "older client reads a newer trailer" direction is
// exercised against a real decoder rather than an assertion about one.
func readScanStatsElevenCount(r *reader) collections.ScanStats {
	s := collections.ScanStats{
		SegmentsTotal:        int(r.i32()),
		SegmentsPruned:       int(r.i32()),
		SegmentsScanned:      int(r.i32()),
		RecordsVisited:       int(r.i32()),
		RecordsColumnDecided: int(r.i32()),
		RecordsReassembled:   int(r.i32()),
		RowsMatched:          int(r.i32()),
	}
	if r.err == nil && len(r.b) >= 16 {
		s.SegmentsIndexed = int(r.i32())
		s.SegmentsIndexSkipped = int(r.i32())
		s.SegmentsUnindexed = int(r.i32())
		s.IndexCandidates = int(r.i32())
	}
	return s
}
