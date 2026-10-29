package collections

import "strconv"

// AggTier names WHICH strategy answered an aggregate -- the thing a caller previously had to
// INFER from the scan counts being zero. That inference was wrong in two ways: it rested on an
// accidental property (the fast paths happen not to reassemble records, so they leave the counts
// at zero) rather than on anything stated, and it could not tell a columnar answer from a
// constraint that was proved empty without reading anything, since both report all-zero counts.
//
// The values ride on the wire as an int32 appended to the scan-stats trailer, so they are
// permanent: add new tiers at the end, never renumber. TierUnreported is the zero value so a
// response from a peer that never sent a tier -- an older server, or an op whose trailer predates
// the field -- reads as "did not say" rather than claiming a tier it never reported.
type AggTier int

const (
	// TierUnreported means no tier was reported: the zero value, NOT a claim about the strategy.
	TierUnreported AggTier = 0
	// TierMatchNone: the constraint was proved to match nothing, so no data was read at all.
	TierMatchNone AggTier = 1
	// TierIndex: answered from per-segment metadata -- postings, stored record counts, zone maps
	// -- without opening a segment's records.
	TierIndex AggTier = 2
	// TierColumnar: answered from the columnar accelerator, reading only the columns the query
	// touches rather than reassembling records.
	TierColumnar AggTier = 3
	// TierScan: answered by walking records (the fall-through path); the scan counts in the rest
	// of ScanStats describe it.
	TierScan AggTier = 4
)

// String names the tier for a diagnostic (EXPLAIN ANALYZE prints it next to the counts).
func (t AggTier) String() string {
	switch t {
	case TierUnreported:
		return "unreported"
	case TierMatchNone:
		return "match-none"
	case TierIndex:
		return "index"
	case TierColumnar:
		return "columnar"
	case TierScan:
		return "scan"
	}
	return "AggTier(" + strconv.Itoa(int(t)) + ")"
}

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
	RecordsColumnDecided int // WHERE answered without reassembling the record to test it
	// The two ways a record counted in RecordsColumnDecided was decided other than by evaluating the
	// whole query from its columns. RecordsConjunctRejected: one of the query's top-level conjuncts
	// was answered from the columns and was not true, so the record cannot match. RecordsNarrowMatched:
	// the query was evaluated on the record narrowed to the attributes it reads, gathered from the
	// columns, the cold tail and the remnant without rebuilding the rest.
	RecordsConjunctRejected int
	RecordsNarrowMatched    int
	RecordsReassembled      int // record rebuilt from the arena (the ~O(ad width) path)
	// RecordsClosureReassembled counts records that COULD have been projected straight from
	// their columns but were reassembled anyway, because the projected ad referred to an
	// attribute the projection left out and the consumer evaluates what it receives. A large
	// share here means the projection names expressions reaching outside it, and the columnar
	// fast path cannot serve that caller; zero means the closure costs nothing.
	RecordsClosureReassembled int
	RowsMatched               int // records that satisfied the WHERE and were emitted

	// The index probe, when the scan visited index candidates instead of every record:
	// SegmentsIndexed segments answered from their postings (IndexCandidates offsets in all),
	// SegmentsIndexSkipped proved by their index to hold no candidate, and SegmentsUnindexed
	// walked in full because no index covered the probe there (an unsealed segment, or one
	// sealed before the attribute was indexed). All zero on a plain scan.
	SegmentsIndexed      int
	SegmentsIndexSkipped int
	SegmentsUnindexed    int
	IndexCandidates      int

	// Tier is WHICH strategy produced the answer, for the query paths that fan out over several
	// (see AggTier). It is reported, not inferred: TierUnreported means nothing said, which is
	// what a plain scan path that never tags itself and an older server's response both leave.
	Tier AggTier
}

// SetTier records which strategy answered. Nil-safe like the counters, so a path can tag itself
// without first checking whether the caller wanted stats.
func (s *ScanStats) SetTier(t AggTier) {
	if s != nil {
		s.Tier = t
	}
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
func (s *ScanStats) conjunctRejected() {
	if s != nil {
		s.RecordsConjunctRejected++
	}
}
func (s *ScanStats) narrowMatched() {
	if s != nil {
		s.RecordsNarrowMatched++
	}
}
func (s *ScanStats) reassembled() {
	if s != nil {
		s.RecordsReassembled++
	}
}
func (s *ScanStats) closureReassembled() {
	if s != nil {
		s.RecordsClosureReassembled++
	}
}
func (s *ScanStats) matched() {
	if s != nil {
		s.RowsMatched++
	}
}

// indexSegment records one segment's index verdict: n candidate offsets from its postings,
// skipped when the index proved it holds none, or unindexed when it was walked in full.
func (s *ScanStats) indexSegment(n int, skipped, unindexed bool) {
	if s == nil {
		return
	}
	switch {
	case unindexed:
		s.SegmentsUnindexed++
	case skipped:
		s.SegmentsIndexSkipped++
	default:
		s.SegmentsIndexed++
		s.IndexCandidates += n
	}
}

func (s *ScanStats) segments(total, pruned int) {
	if s != nil {
		s.SegmentsTotal += total
		s.SegmentsPruned += pruned
		s.SegmentsScanned += total - pruned
	}
}

// mergeWorker folds a parallel worker's private counts into s.
//
// Only the per-RECORD counters. The segment verdicts (total, pruned, and the index ones) are
// settled on the calling goroutine before any worker starts, so adding them here would report
// each segment once per worker.
func (s *ScanStats) mergeWorker(w *ScanStats) {
	if s == nil || w == nil {
		return
	}
	s.RecordsVisited += w.RecordsVisited
	s.RecordsColumnDecided += w.RecordsColumnDecided
	s.RecordsConjunctRejected += w.RecordsConjunctRejected
	s.RecordsNarrowMatched += w.RecordsNarrowMatched
	s.RecordsReassembled += w.RecordsReassembled
	s.RecordsClosureReassembled += w.RecordsClosureReassembled
	s.RowsMatched += w.RowsMatched
}
