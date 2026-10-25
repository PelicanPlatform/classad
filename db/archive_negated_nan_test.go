package db

import (
	"fmt"
	"math"
	"strconv"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// A NEGATED ordering comparison against a column that holds a NaN.
//
// `!(x <= c)` is not `x > c`. Every comparison with a NaN is FALSE -- false, not undefined -- so a
// stored NaN makes `x <= c` false and `!(x <= c)` TRUE, while `x > c` is false of it. Treating the
// negation as the flipped comparison loses exactly the NaN records, and it lost them in three
// separate places: the ungrouped columnar count (which answers from the probe, with no zone map in
// sight), the grouped columnar count, and -- worst -- zone pruning, which decides which segments the
// ROW SCAN reads, so the scan itself went short.
//
// The oracle is per-ad evaluation of the same expression, never the scan: once a zone map existed the
// scan was wrong too.
//
// Columns in the fixture:
//
//	Rate   -- numbers, with a literal NaN in some records (InsertAttrFloat; NaN has no ClassAd
//	          literal spelling, so it can only be stored programmatically).
//	Clean  -- the same numbers with no NaN anywhere: the control. Its zone map is Full, so the
//	          negated form is still decidable from the zone and must still prune.
//	Calc   -- a NaN that ARRIVES BY ARITHMETIC (`0.0/0.0`), which is stored as an expression and
//	          read from the cold tail rather than the column. A different storage path, and the
//	          fast paths must either get it right or decline.
//	Mixed  -- an INTEGER in almost every record and a literal NaN in a few. An integer column
//	          cannot hold a NaN, so those records escape their slot and are read from the cold
//	          tail -- the one path where the integer plane kernels (which may flip the operator,
//	          since an integer is never NaN) and the float predicate have to agree.
//	Bucket -- an integer group key, for the grouped counts.
//	Seq    -- a monotonically increasing integer. NaN-free by construction (an integer column
//	          cannot hold one), so it exercises the integer plane kernels, and monotonic, so a
//	          window over it must still prune whole segments.
func negNaNAd(i int) *classad.ClassAd {
	src := fmt.Sprintf("Owner = \"u%d\"\nBucket = %d\nSeq = %d\nRate = %d.5\nClean = %d.5\nMixed = %d\n", i%4, i%3, i, i%50, i%50, i%50)
	if i%11 == 7 {
		src += "Calc = 0.0/0.0\n" // an expression that evaluates to NaN
	} else {
		src += fmt.Sprintf("Calc = %d.5\n", i%50)
	}
	ad, err := classad.ParseOld(src)
	if err != nil {
		panic(err)
	}
	if i%37 == 3 {
		ad.InsertAttrFloat("Rate", math.NaN())  // a stored literal NaN in a real column
		ad.InsertAttrFloat("Mixed", math.NaN()) // ... and in an otherwise-integer one
	}
	return ad
}

const negNaNRecords = 600

// negNaNTruth counts the records the expression is TRUE of, by evaluating it per ad. This is the
// only trustworthy oracle here: the row scan is itself downstream of zone pruning.
func negNaNTruth(t *testing.T, constraint string) int {
	t.Helper()
	e, err := classad.ParseExpr(constraint)
	if err != nil {
		t.Fatalf("parse %q: %v", constraint, err)
	}
	n := 0
	for i := range negNaNRecords {
		v := negNaNAd(i).EvaluateExprWithTarget(e, nil)
		if b, err := v.BoolValue(); err == nil && b {
			n++
		}
	}
	return n
}

// negNaNTruthBy is negNaNTruth split by the Bucket group key.
func negNaNTruthBy(t *testing.T, constraint string) map[string]int {
	t.Helper()
	e, err := classad.ParseExpr(constraint)
	if err != nil {
		t.Fatalf("parse %q: %v", constraint, err)
	}
	out := map[string]int{}
	for i := range negNaNRecords {
		ad := negNaNAd(i)
		v := ad.EvaluateExprWithTarget(e, nil)
		if b, err := v.BoolValue(); err != nil || !b {
			continue
		}
		out[strconv.Itoa(i%3)]++
	}
	return out
}

// negNaNShapes are the storage configurations the same data is checked under. Zone maps and value
// indexes are what a negated ordering comparison can be decided WITHOUT reading records, so each
// has to be present in at least one shape and absent in another: without the bare shape nothing
// would show that the columnar count is wrong on its own, and without the indexed shapes nothing
// would show that the segment skipping is.
var negNaNShapes = []struct {
	name   string
	zones  []string
	values []string
}{
	{name: "bare"},
	{name: "zoned", zones: []string{"Rate", "Clean", "Calc", "Bucket", "Seq", "Mixed"}},
	// Value indexes go on the NaN-free columns only. A value index over a column that holds a
	// stored NaN crashes while the index is being built, which is a bug of its own and not this
	// one: the posting map is keyed by the float value, and a NaN key can never be looked up again
	// (NaN != NaN), so the later pass over the sorted keys reads a nil bitmap. Reproducible on
	// main; left alone here.
	{name: "indexed", zones: []string{"Rate", "Clean", "Calc", "Bucket", "Seq", "Mixed"},
		values: []string{"Clean", "Seq", "Bucket", "Calc"}},
}

func negNaNArchive(t *testing.T, shape int) *ArchiveTable {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	cfg := ArchiveConfig{SegmentSize: 16 << 10, CategoricalAttrs: []string{"Owner"}}
	cfg.ZoneAttrs = negNaNShapes[shape].zones
	cfg.ValueAttrs = negNaNShapes[shape].values
	h, err := cat.CreateArchiveTable("history", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range negNaNRecords {
		if err := h.Append(negNaNAd(i)); err != nil {
			t.Fatal(err)
		}
	}
	h.MergePass(MergeOptions{})
	if !h.BuildAndEnableSchemaScan(2000, 32) {
		t.Fatal("no columnar schema: the fixture is not exercising the accelerators")
	}
	return h
}

func negNaNScan(t *testing.T, h *ArchiveTable, constraint string) (int, collections.ScanStats) {
	t.Helper()
	var st collections.ScanStats
	seq, err := h.QueryProjectStats(constraint, []string{"Rate"}, &st)
	if err != nil {
		t.Fatalf("scan %q: %v", constraint, err)
	}
	n := 0
	for range seq {
		n++
	}
	return n, st
}

// TestNegatedOrderingNaN is the regression: for every negated ordering comparison, over a column
// with a stored NaN, a column with an arithmetic NaN and a NaN-free control, the row scan and both
// columnar counts must agree with per-ad evaluation -- with and without a zone map.
func TestNegatedOrderingNaN(t *testing.T) {
	var constraints []string
	for _, attr := range []string{"Rate", "Clean", "Calc", "Seq", "Mixed"} {
		for _, op := range []string{"<", "<=", ">", ">="} {
			// Finite bounds that straddle the data, and the infinite ones: +Inf as an upper bound
			// makes every ordinary number satisfy `x <= c`, so the NaN records are the ONLY ones
			// its negation keeps -- which is where a lost NaN is a total loss rather than a
			// shortfall.
			for _, bound := range []string{"10", "0.5", "49.5", "1.0e999", "-1.0e999"} {
				constraints = append(constraints,
					fmt.Sprintf("!(%s %s %s)", attr, op, bound),
					fmt.Sprintf("%s %s %s", attr, op, bound), // the positive control
				)
			}
		}
		constraints = append(constraints,
			fmt.Sprintf("!(%s <= 10) && Owner == \"u0\"", attr), // mixed with a categorical conjunct
			fmt.Sprintf("!(%s == 0.5)", attr),                   // negated equality: flipping IS sound
			fmt.Sprintf("!(%s != 0.5)", attr),
		)
	}

	for shape, sh := range negNaNShapes {
		t.Run(sh.name, func(t *testing.T) {
			h := negNaNArchive(t, shape)
			groups := []GroupCol{{Attr: "Bucket"}}
			aggs := []AggSpec{{Func: AggCount, Arg: "*"}}
			for _, c := range constraints {
				want := negNaNTruth(t, c)
				if n, _ := negNaNScan(t, h, c); n != want {
					t.Errorf("%s: row scan %d, want %d", c, n, want)
				}
				if n, ok := h.CountConstraint(c); ok && n != want {
					t.Errorf("%s: CountConstraint %d, want %d", c, n, want)
				}
				wantBy := negNaNTruthBy(t, c)
				if rows, ok := GroupedFromColumns(h, c, groups, aggs); ok {
					got := map[string]int{}
					for _, r := range rows {
						n, err := strconv.Atoi(r.Values[0])
						if err != nil {
							t.Fatalf("%s: group count %q: %v", c, r.Values[0], err)
						}
						if n != 0 {
							got[r.Group[0]] = n
						}
					}
					if len(got) != len(wantBy) {
						t.Errorf("%s: GroupedFromColumns %v, want %v", c, got, wantBy)
					}
					for k, v := range wantBy {
						if got[k] != v {
							t.Errorf("%s: GroupedFromColumns[%s] = %d, want %d", c, k, got[k], v)
						}
					}
				}
				if gcs, ok := h.GroupCountConstraint(c, "Bucket"); ok {
					got := map[string]int{}
					for _, gc := range gcs {
						k := fmt.Sprint(gc.Value)
						if i, err := gc.Value.IntValue(); err == nil {
							k = strconv.FormatInt(i, 10)
						}
						if gc.Count != 0 {
							got[k] = gc.Count
						}
					}
					// Both directions: a group the fix loses wholesale is as wrong as one it
					// undercounts, and that is how GroupedFromColumns failed at an infinite bound.
					if len(got) != len(wantBy) {
						t.Errorf("%s: GroupCountConstraint %v, want %v", c, got, wantBy)
					}
					for k, v := range wantBy {
						if got[k] != v {
							t.Errorf("%s: GroupCountConstraint[%s] = %d, want %d", c, k, got[k], v)
						}
					}
				}
			}
		})
	}
}

// TestNegatedOrderingNaNFastPaths holds the line on what the fix COSTS. The NaN-free control column
// must keep every fast path it had: the columnar count still answers it, and its zone map (Full, so
// a NaN is ruled out) still prunes the negated form. The column that holds a NaN gives up zone
// pruning for the negated form -- that is the price -- but keeps the columnar count, because
// evaluating the comparison per value and negating the result is exact for a NaN too.
func TestNegatedOrderingNaNFastPaths(t *testing.T) {
	// The zone-mapped shape, not the value-indexed one: pruning whole segments from their zone
	// maps is the mechanism under test, and a value index routes the positive forms down the
	// candidate path instead, where the segment counts measure something else.
	h := negNaNArchive(t, 1)

	// The control: no record of it satisfies `!(Clean <= +Inf)`, the zone is Full, so every
	// segment is prunable and the scan reads none of them.
	cleanNeg := "!(Clean <= 1.0e999)"
	if n := negNaNTruth(t, cleanNeg); n != 0 {
		t.Fatalf("fixture: %s matches %d records, want 0", cleanNeg, n)
	}
	n, st := negNaNScan(t, h, cleanNeg)
	if n != 0 {
		t.Errorf("%s: scan %d rows, want 0", cleanNeg, n)
	}
	// Every SEALED segment is pruned; the active one carries no zone map and is always read.
	if st.SegmentsTotal < 2 || st.SegmentsPruned != st.SegmentsTotal-1 {
		t.Errorf("%s: pruned %d of %d segments, want all but the active one -- a Full zone still decides a negated ordering",
			cleanNeg, st.SegmentsPruned, st.SegmentsTotal)
	}

	// The positive form on the same column prunes exactly as it always did: that is the whole
	// value of a zone map and the fix must not have cost any of it.
	for _, c := range []string{"Clean > 1.0e999", "Clean < -1.0e999", "Rate > 1.0e999", "Rate < -1.0e999"} {
		n, st := negNaNScan(t, h, c)
		if n != 0 {
			t.Errorf("%s: scan %d rows, want 0", c, n)
		}
		if st.SegmentsTotal < 2 || st.SegmentsPruned != st.SegmentsTotal-1 {
			t.Errorf("%s: pruned %d of %d segments, want all but the active one", c, st.SegmentsPruned, st.SegmentsTotal)
		}
	}

	// A windowed query still costs the window, not the archive.
	// A windowed query still costs the window, not the archive -- in both forms. Seq is
	// monotonic, so most segments fall outside the window; it is NaN-free, so even the negated
	// form keeps its pruning.
	for _, window := range []string{"Seq >= 500", "!(Seq < 500)", "Seq < 100", "!(Seq >= 100)"} {
		want := negNaNTruth(t, window)
		n, st := negNaNScan(t, h, window)
		if n != want {
			t.Errorf("%s: scan %d rows, want %d", window, n, want)
		}
		if st.SegmentsPruned == 0 {
			t.Errorf("%s: pruned no segments of %d -- windowed pruning regressed", window, st.SegmentsTotal)
		}
	}

	// Both columns keep the columnar count under negation.
	for _, c := range []string{"!(Clean <= 10)", "!(Clean > 10)", "!(Rate <= 10)", "!(Rate >= 10)", "!(Seq <= 100)", "!(Seq > 100)"} {
		got, ok := h.CountConstraint(c)
		if !ok {
			t.Errorf("%s: CountConstraint declined -- the columnar predicate path should answer this", c)
			continue
		}
		if want := negNaNTruth(t, c); got != want {
			t.Errorf("%s: CountConstraint %d, want %d", c, got, want)
		}
	}
}
