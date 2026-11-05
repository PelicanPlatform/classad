package collections

import "testing"

// estVisited is a derivation over fields the explain has already established, so it is
// tested as one: the walk that produces those fields is covered where it is written.
func TestEstVisitedFromPlan(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ex       QueryExplain
		segTotal int
		want     int64
	}{{
		name: "empty plan reads nothing",
		ex:   QueryExplain{Plan: "empty", TotalAds: 1000, EstCandidates: 999},
		want: 0,
	}, {
		// The failure this whole field exists for: EstCandidates is 0 on a scan, so
		// reading it as a size prices the most expensive read there is at nothing.
		name: "a scan reads the whole table",
		ex:   QueryExplain{Plan: "serial-scan", TotalAds: 1000, EstCandidates: 0},
		want: 1000,
	}, {
		name:     "a scan less what the zone maps rule out",
		ex:       QueryExplain{Plan: "serial-scan", TotalAds: 1000, ZonePrunedSegments: 4},
		segTotal: 10,
		want:     600,
	}, {
		name:     "zone pruning without a segment total cannot be applied",
		ex:       QueryExplain{Plan: "parallel-scan", TotalAds: 1000, ZonePrunedSegments: 4},
		segTotal: 0,
		want:     1000,
	}, {
		name: "an indexed plan reads its candidates",
		ex:   QueryExplain{Plan: "indexed", TotalAds: 1000, EstCandidates: 120, IndexSegments: 10},
		want: 120,
	}, {
		// Segments no index covers are walked whole and are absent from EstCandidates.
		name: "plus the segments no index covers",
		ex: QueryExplain{Plan: "indexed", TotalAds: 1000, EstCandidates: 120,
			IndexSegments: 8, UnindexedSegments: 2},
		want: 320, // 120 + 1000*(2/10)
	}, {
		name: "clamped to the table",
		ex: QueryExplain{Plan: "indexed", TotalAds: 1000, EstCandidates: 990,
			IndexSegments: 5, UnindexedSegments: 5},
		want: 1000,
	}, {
		name: "a union reads its candidates",
		ex:   QueryExplain{Plan: "indexed-union", TotalAds: 1000, EstCandidates: 300, IndexSegments: 10},
		want: 300,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := estVisited(tc.ex, tc.segTotal); got != tc.want {
				t.Errorf("estVisited = %d, want %d", got, tc.want)
			}
		})
	}
}
