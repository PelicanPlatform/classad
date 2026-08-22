package collections

// ScanStats accumulates, for one query scan, how the work broke down -- so a diagnostic
// (EXPLAIN ANALYZE) can show WHERE a slow scan spent itself rather than leaving it to be
// guessed. All counts are for a single scan; pass a fresh &ScanStats to a *Stats query method.
//
// The distinction that matters is RecordsReassembled vs RecordsColumnDecided: a scan that
// decides the WHERE from the columnar prefilter and emits only matches touches the record arena
// RowsMatched times; a scan that reassembles most of what it visits is the slow shape (the
// record is rebuilt from the arena -- ~O(ad width) each -- even for non-matches).
type ScanStats struct {
	SegmentsTotal   int // segments in the snapshot
	SegmentsPruned  int // dropped by zone maps before any record was read
	SegmentsScanned int // segments actually walked

	RecordsVisited       int // records the scan iterated
	RecordsColumnDecided int // WHERE answered from the columnar prefilter (no reassembly to test it)
	RecordsReassembled   int // record rebuilt from the arena (the ~O(ad width) path)
	RowsMatched          int // records that satisfied the WHERE and were emitted
}

func (s *ScanStats) visit() {
	if s != nil {
		s.RecordsVisited++
	}
}
func (s *ScanStats) columnDecided() {
	if s != nil {
		s.RecordsColumnDecided++
	}
}
func (s *ScanStats) reassembled() {
	if s != nil {
		s.RecordsReassembled++
	}
}
func (s *ScanStats) matched() {
	if s != nil {
		s.RowsMatched++
	}
}
func (s *ScanStats) segments(total, pruned int) {
	if s != nil {
		s.SegmentsTotal += total
		s.SegmentsPruned += pruned
		s.SegmentsScanned += total - pruned
	}
}
