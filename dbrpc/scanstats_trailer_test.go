package dbrpc

import (
	"testing"

	"github.com/PelicanPlatform/classad/collections"
)

// The scan-stats trailer carries the index-probe counts, and a seven-count trailer from an
// older server still reads.
func TestScanStatsTrailer(t *testing.T) {
	want := collections.ScanStats{SegmentsTotal: 1, SegmentsPruned: 2, SegmentsScanned: 3, RecordsVisited: 4,
		RecordsColumnDecided: 5, RecordsReassembled: 6, RowsMatched: 7,
		SegmentsIndexed: 8, SegmentsIndexSkipped: 9, SegmentsUnindexed: 10, IndexCandidates: 11}
	r := &reader{b: putScanStats(nil, want)}
	if got := readScanStats(r); got != want || r.err != nil {
		t.Errorf("round trip: %+v (%v), want %+v", got, r.err, want)
	}
	old := putScanStats(nil, want)[:7*4]
	r = &reader{b: old}
	got := readScanStats(r)
	if r.err != nil || got.RowsMatched != 7 || got.SegmentsIndexed != 0 || got.IndexCandidates != 0 {
		t.Errorf("seven-count trailer: %+v (%v)", got, r.err)
	}
}
