package collections

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// plainRows evaluates q against every record of an unfiltered scan, which neither zone-prunes nor uses
// the columns: the ground truth the column aggregates are compared to.
func plainRows(tb testing.TB, a *Archive, q *vm.Query) []*classad.ClassAd {
	tb.Helper()
	var out []*classad.ClassAd
	for ad := range a.c.Scan() {
		if q.Matches(ad) {
			out = append(out, ad)
		}
	}
	return out
}

func adInt(tb testing.TB, ad *classad.ClassAd, attr string) int64 {
	tb.Helper()
	v, ok := ad.EvaluateAttrInt(attr)
	if !ok {
		tb.Fatalf("record without %s", attr)
	}
	return v
}

// TestColumnAggregatesZonePruneSchemaLagSegments: every column aggregate that falls back to a row walk
// for a segment whose schema lacks the filtered field first asks that segment's zone map, so the old
// segments are skipped -- and each answer stays exactly the plain evaluation's.
func TestColumnAggregatesZonePruneSchemaLagSegments(t *testing.T) {
	for _, poison := range []bool{false, true} {
		t.Run(fmt.Sprintf("poison=%v", poison), func(t *testing.T) {
			f, a := newSchemaLagArchive(t, 3000, 300, poison)
			c := a.c
			exprs := []string{
				fmt.Sprintf(`Ts >= %d`, f.now),
				fmt.Sprintf(`Ts >= %d && Val > 0`, f.now),
				`Ts >= CurrentTime - 600`,
			}
			for _, expr := range exprs {
				q := mustQuery(t, expr)
				rows := plainRows(t, a, q)
				hasStraggler, hasPoison := false, false
				for _, ad := range rows {
					switch int(adInt(t, ad, "N")) {
					case f.straggler:
						hasStraggler = true
					case f.poison:
						hasPoison = true
					}
				}
				if !hasStraggler || len(rows) < f.new/2 {
					t.Fatalf("fixture: plain %s returned %d rows (straggler %v)", expr, len(rows), hasStraggler)
				}
				readsCT := strings.Contains(expr, "CurrentTime")
				if hasPoison != (poison && readsCT) {
					t.Fatalf("fixture: plain %s returned the poison record: %v", expr, hasPoison)
				}

				// Each tier, and that it pruned. The predicate tiers serve a CurrentTime query only when
				// every window is proven free of it; with the poison record they decline, and the
				// CurrentTime-folded probes may then prune only the proven segments.
				predTiers := !readsCT || !poison
				check := func(tier string, served bool, run func() bool) {
					t.Helper()
					before := segZonePrunes.Load()
					ok := run()
					if ok != served {
						t.Fatalf("%s %s: served=%v, want %v", tier, expr, ok, served)
					}
					if !ok {
						return
					}
					if n := segZonePrunes.Load() - before; n < 10 {
						t.Errorf("%s %s: pruned %d segments on their zone maps", tier, expr, n)
					}
				}

				check("CountQuery", true, func() bool {
					n, ok := c.CountQuery(q)
					if ok && n != len(rows) {
						t.Errorf("CountQuery(%s) = %d, plain %d", expr, n, len(rows))
					}
					return ok
				})
				check("ColumnarEvalCount", true, func() bool {
					n, ok := c.ColumnarEvalCount(q)
					if ok && n != len(rows) {
						t.Errorf("ColumnarEvalCount(%s) = %d, plain %d", expr, n, len(rows))
					}
					return ok
				})
				check("VectorEvalCount", true, func() bool {
					n, ok := c.VectorEvalCount(q)
					if ok && n != len(rows) {
						t.Errorf("VectorEvalCount(%s) = %d, plain %d", expr, n, len(rows))
					}
					if split := lastVecSplit.Load(); ok && split.prunedSegs < 10 {
						t.Errorf("VectorEvalCount(%s) split reports %d pruned segments", expr, split.prunedSegs)
					}
					return ok
				})

				var wantSum, wantMin, wantMax int64 = 0, math.MaxInt64, math.MinInt64
				wantGroups := map[int64][2]int64{} // Grp -> {count, sum(Val)}
				var order []int64
				for _, ad := range rows {
					v := adInt(t, ad, "Val")
					wantSum += v
					if v < wantMin {
						wantMin = v
					}
					if v > wantMax {
						wantMax = v
					}
					g := adInt(t, ad, "Grp")
					e := wantGroups[g]
					wantGroups[g] = [2]int64{e[0] + 1, e[1] + v}
					order = append(order, adInt(t, ad, "Ts"))
				}
				check("NumStatsQuery", predTiers, func() bool {
					st, ok := c.NumStatsQuery(q, "Val")
					if ok && (st.N != len(rows) || st.IntSum != wantSum || st.IntMin != wantMin || st.IntMax != wantMax) {
						t.Errorf("NumStatsQuery(%s) = n=%d sum=%d min=%d max=%d, plain n=%d sum=%d min=%d max=%d",
							expr, st.N, st.IntSum, st.IntMin, st.IntMax, len(rows), wantSum, wantMin, wantMax)
					}
					return ok
				})
				checkGroups := func(gs []GroupStats) {
					t.Helper()
					if len(gs) != len(wantGroups) {
						t.Errorf("GroupStatsQuery(%s): %d groups, plain %d", expr, len(gs), len(wantGroups))
					}
					for _, g := range gs {
						k, _ := g.Values[0].IntValue()
						if w := wantGroups[k]; int64(g.Count) != w[0] || g.Stats[0].IntSum != w[1] {
							t.Errorf("GroupStatsQuery(%s) group %d: count=%d sum=%d, plain %v", expr, k, g.Count, g.Stats[0].IntSum, w)
						}
					}
				}
				// GroupStatsQuery serves through the predicate tier or, when it declines, the vector one;
				// both prune.
				check("GroupStatsQuery", true, func() bool {
					gs, ok := c.GroupStatsQuery(q, "Grp", []string{"Val"})
					if ok {
						checkGroups(gs)
					}
					return ok
				})
				check("vecGroupStats", true, func() bool {
					grp := []uint32{mustID(t, c, "Grp")}
					acc, ok := c.vecGroupStats(q, grp, []uint32{mustID(t, c, "Val")}, []int64{0}, nil, c.schemaScan.Load())
					if !ok {
						return false
					}
					gs, ok := c.shapeGroupStats(acc, grp, []uint32{mustID(t, c, "Val")}, nil)
					if ok {
						checkGroups(gs)
					}
					if split := lastGroupSplit.Load(); split.prunedSegs < 10 {
						t.Errorf("vecGroupStats(%s) split reports %d pruned segments", expr, split.prunedSegs)
					}
					return ok
				})
				slices.Sort(order)
				slices.Reverse(order)
				const k = 10
				check("TopKOrderThreshold", predTiers, func() bool {
					var st ScanStats
					th, seen, ok := c.TopKOrderThreshold(q, "Ts", true, k, &st)
					if !ok {
						return false
					}
					if seen != len(rows) || int64(th) != order[k-1] {
						t.Errorf("TopKOrderThreshold(%s) = %v seen %d, plain %d seen %d", expr, th, seen, order[k-1], len(rows))
					}
					if st.SegmentsPruned < 10 || st.SegmentsPruned+st.SegmentsScanned != st.SegmentsTotal {
						t.Errorf("TopKOrderThreshold(%s) stats: total=%d pruned=%d scanned=%d", expr, st.SegmentsTotal, st.SegmentsPruned, st.SegmentsScanned)
					}
					return true
				})
			}
		})
	}
}

func mustID(tb testing.TB, c *Collection, attr string) uint32 {
	tb.Helper()
	id, ok := c.intern.LookupID(attr)
	if !ok {
		tb.Fatalf("%s not interned", attr)
	}
	return id
}
