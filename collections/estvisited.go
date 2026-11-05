package collections

import "math"

// How big is this read? -- the question EstCandidates looks like it answers and does not.
//
// EstCandidates is what the INDEX hands the scan. That is the right number for "how much did
// the index prune", which is what EXPLAIN reports it for, and the wrong number for "what will
// this read cost": it is ZERO for a plan that scans, and it leaves out the segments no index
// covers, which the read walks in full.
//
// A caller pricing a read -- a join deciding between narrowing a side and reading it whole --
// reaches for EstCandidates because it is the field that looks like a size, and so prices a
// full scan, the most expensive read there is, at nothing. EstVisited is that caller's
// number, derived here so that every caller gets the same one rather than each reconstructing
// it from the plan string and getting it wrong in its own way.

// estVisited derives the records a read visits from what the explain already established.
//
// segTotal is the collection's segment count, needed only for a scan: an indexed plan
// classifies every segment it saw, so its own counts add up, while a scan reports the
// segments its zone maps ruled out and nothing else.
func estVisited(ex QueryExplain, segTotal int) int64 {
	switch ex.Plan {
	case "empty":
		return 0 // the indexed conjuncts contradict: no shard is opened

	case "indexed", "indexed-union":
		v := float64(ex.EstCandidates)
		// Segments no index covers are walked whole and are absent from EstCandidates.
		// Apportioned by segment count, for want of a per-segment record count; segments
		// of one collection hold comparable numbers of records, so the error is in the
		// spread of segment fill rather than in the estimate's order.
		if classed := ex.IndexSegments + ex.IndexSkippedSegments + ex.UnindexedSegments +
			ex.ZonePrunedSegments; ex.UnindexedSegments > 0 && classed > 0 {
			v += float64(ex.TotalAds) * float64(ex.UnindexedSegments) / float64(classed)
		}
		return clampEst(v, ex.TotalAds)

	default: // serial-scan, parallel-scan
		v := float64(ex.TotalAds)
		if ex.ZonePrunedSegments > 0 && segTotal > 0 {
			v *= 1 - math.Min(1, float64(ex.ZonePrunedSegments)/float64(segTotal))
		}
		return int64(math.Round(v))
	}
}
