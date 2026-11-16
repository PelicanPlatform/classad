package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// What "the k newest rows of every owner" costs three ways, and why the middle one is not here.
//
// WHAT THIS RUNG BUYS, and where. TopKRowsPerGroup runs the SAME single projected scan that
// fetching everything runs, then keeps k rows per group instead of returning all of them. In
// this process that is a small LOSS, not a wash: 100k rows cost 33ms to scan and rank and 40-47ms
// to scan and keep, because every row pays a group-label render and a map probe, and a kept row
// is copied out of the scan's reused slice. What it buys is the ROWS -- 500 instead of 100,000
// to encode, to put on a wire and to decode at the other end -- and that term does not exist
// here. So these arms being close is the expected result, and the comparison that decides the
// feature is a client/server one this file cannot make.
//
// THE THIRD ARM IS A HEADSTONE. An earlier design computed a per-group CUTOFF and let the caller
// constrain by it: `(Owner == g1 && QDate >= t1) || ...`. It looks cheaper -- the second pass
// reads only the rows above the cutoffs -- and it is drastically worse, because the constraint is
// a DISJUNCTION. A conjunction of scalar comparisons is columnar-eligible; an OR of them is not,
// so the whole pass falls back to the record scan and pays about a full pass per term. The
// disjunction arm keeps that measurable so the idea is not re-proposed from first principles:
// one term costs what a whole scan costs, and a thousand terms cost a thousand of them.
//
// The cutoff thresholds are computed here in Go rather than by the store, because the arm exists
// to measure the CONSTRAINT and not whatever produced it.

func topKBenchDB(b *testing.B, owners, rows int) *DB {
	b.Helper()
	d, err := OpenConfig(Config{SegmentSize: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { d.Close() })
	tx := d.Begin()
	n := 0
	for o := 0; o < owners; o++ {
		for r := 0; r < rows; r++ {
			ad := classad.New()
			ad.InsertAttrString("Owner", fmt.Sprintf("o%d", o))
			ad.InsertAttr("QDate", int64(o*1_000_000+r))
			tx.NewClassAd(fmt.Sprintf("k%d", n), ad)
			n++
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	d.EnableSchemaScan(4000, 8)
	return d
}

// scanAndRank is today's path: project every matching row, rank per group, keep k.
func scanAndRank(b *testing.B, d *DB, k int) int {
	b.Helper()
	seq, err := d.QueryProject("true", []string{"Owner", "QDate"})
	if err != nil {
		b.Fatal(err)
	}
	per := map[string][]int64{}
	for row := range seq {
		owner, _ := row[0].StringValue() //nolint:errcheck // the fixture writes a string
		q, qerr := row[1].IntValue()
		if qerr != nil {
			continue
		}
		per[owner] = append(per[owner], q)
	}
	kept := 0
	for _, qs := range per {
		sort.Slice(qs, func(i, j int) bool { return qs[i] > qs[j] })
		if len(qs) > k {
			qs = qs[:k]
		}
		kept += len(qs)
	}
	return kept
}

// cutoffDisjunction is the abandoned design, kept as a measurement: thresholds in hand, read the
// rows above them with one OR'd constraint.
func cutoffDisjunction(b *testing.B, d *DB, cuts map[string]int64) int {
	b.Helper()
	terms := make([]string, 0, len(cuts))
	for owner, t := range cuts {
		terms = append(terms, fmt.Sprintf("(Owner == %q && QDate >= %d)", owner, t))
	}
	seq, err := d.QueryProject(strings.Join(terms, " || "), []string{"Owner", "QDate"})
	if err != nil {
		b.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	return n
}

// benchCutoffs is the k-th best QDate of each owner, which the fixture makes exactly computable.
func benchCutoffs(owners, rows, k int) map[string]int64 {
	cuts := make(map[string]int64, owners)
	for o := 0; o < owners; o++ {
		cuts[fmt.Sprintf("o%d", o)] = int64(o*1_000_000 + rows - k)
	}
	return cuts
}

func BenchmarkTopKRowsPerGroup(b *testing.B) {
	const k = 5
	for _, shape := range []struct{ owners, rows int }{
		{100, 100},
		{100, 1000},
		{1000, 100},
	} {
		d := topKBenchDB(b, shape.owners, shape.rows)
		name := fmt.Sprintf("owners=%d/rows=%d", shape.owners, shape.rows)
		cuts := benchCutoffs(shape.owners, shape.rows, k)
		wantRows := shape.owners * k

		b.Run(name+"/scan-and-rank", func(b *testing.B) {
			for b.Loop() {
				if got := scanAndRank(b, d, k); got != wantRows {
					b.Fatalf("kept %d rows, want %d", got, wantRows)
				}
			}
		})
		b.Run(name+"/topk-rows", func(b *testing.B) {
			for b.Loop() {
				groups, err := d.TopKRowsPerGroup("true", TopKRowsSpec{
					GroupCols: []GroupCol{{Attr: "Owner"}},
					OrderAttr: "QDate", Desc: true, K: k, Attrs: []string{"QDate"},
				})
				if err != nil {
					b.Fatal(err)
				}
				kept := 0
				for _, g := range groups {
					kept += len(g.Rows)
				}
				if kept != wantRows {
					b.Fatalf("kept %d rows, want %d", kept, wantRows)
				}
			}
		})
		b.Run(name+"/cutoff-disjunction", func(b *testing.B) {
			for b.Loop() {
				if got := cutoffDisjunction(b, d, cuts); got != wantRows {
					b.Fatalf("disjunction read %d rows, want %d", got, wantRows)
				}
			}
		})
	}
}
