package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// What a ranking window costs, with and without the cutoff -- and the measurement that says the
// obvious way to SPEND a cutoff does not work.
//
// The query is "the k newest jobs of every owner". Today a client answers it by fetching every
// matching row and ranking them itself; with AggTopKThreshold it asks for the per-group cutoffs
// and then for the rows above them. Both arms end with the same answer in hand, and the
// benchmark fails if they disagree, so the numbers are comparable.
//
// THE RESULT, on 100k rows (Apple M2 Pro): the cutoff pass is cheap -- 30-42ms, about what one
// full projected scan costs -- and the second pass is not. Written as the OR of the per-group
// windows, `(Owner == g1 && QDate >= t1) || ...`, it costs 1.0s at 100 groups and 5.4s at 1000,
// against 27ms to fetch all 100k rows and rank them. Adding a categorical index on the group
// column and a value index on the order column changes none of it.
//
// WHY, measured rather than guessed: ONE of those conjuncts alone fetches its 5 rows in 23ms,
// the same as a full scan, because a conjunction of scalar comparisons is columnar-eligible. The
// disjunction is not, so the whole second pass falls back to the record scan and pays roughly a
// pass per term. The cost is the SHAPE of the constraint, not the number of rows it keeps.
//
// So the cutoff is not spendable with the constraints that exist. What it needs is a predicate
// that answers "this row's group has a cutoff, and its order value passes it" in O(1) per row --
// a per-group range filter reading the group column's dictionary -- rather than a disjunction
// that grows a term per group. Until that exists this aggregate has no profitable caller, which
// is what this benchmark is here to keep stating.
//
// The axis that matters is ROWS PER GROUP, not rows: a cutoff narrows a group to about k, so what
// it could save is everything above k, and a table of one-row groups has nothing for it to save.

// topKBenchDB seeds owners x rows jobs with distinct QDates per owner.
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
			ad.InsertAttr("Cpus", int64(r%8))
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

// fetchAndRank is today's path: every matching row is projected out, then ranked per group.
func fetchAndRank(b *testing.B, d *DB, k int) int {
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

// cutoffThenFetch is the pushed-down path: one grouped pass for the cutoffs, then the rows above
// them. The second pass's constraint is the OR of the per-group windows, which is the shape a
// client would send.
func cutoffThenFetch(b *testing.B, d *DB, k int) int {
	b.Helper()
	cuts, err := d.AggregateColsStats("true", []GroupCol{{Attr: "Owner"}},
		[]AggSpec{{Func: AggTopKThreshold, Arg: "QDate", K: k, Desc: true}}, nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	terms := make([]string, 0, len(cuts))
	for _, r := range cuts {
		if r.Values[0] == "undefined" {
			// Fewer than k rows in the group: take it whole, there is nothing to cut.
			terms = append(terms, fmt.Sprintf("Owner == %s", r.Group[0]))
			continue
		}
		terms = append(terms, fmt.Sprintf("(Owner == %s && QDate >= %s)", r.Group[0], r.Values[0]))
	}
	seq, err := d.QueryProject(strings.Join(terms, " || "), []string{"Owner", "QDate"})
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
		// Still ranked: the cutoff admits ties, so the client's own ranking does the cutting.
		sort.Slice(qs, func(i, j int) bool { return qs[i] > qs[j] })
		if len(qs) > k {
			qs = qs[:k]
		}
		kept += len(qs)
	}
	return kept
}

// BenchmarkTopKPerGroup runs both arms over the same tables. The arms are interleaved per shape
// rather than run as two whole suites, so a drift in machine state hits both alike. The
// cutoff-then-fetch arm is expected to LOSE, for the reason above; it is here so that the
// primitive that would make it win can be measured against it.
func BenchmarkTopKPerGroup(b *testing.B) {
	const k = 5
	for _, shape := range []struct {
		owners, rows int
	}{
		{100, 10},   // 1k rows, 10 per group: little above the cutoff to save
		{100, 100},  // 10k rows, 100 per group
		{100, 1000}, // 100k rows, 1000 per group: the shape a history table has
		{1000, 100}, // 100k rows over 10x the groups: the OR grows, the saving stays
	} {
		d := topKBenchDB(b, shape.owners, shape.rows)
		name := fmt.Sprintf("owners=%d/rows=%d", shape.owners, shape.rows)

		// Both arms must agree on the answer, or the comparison is between two
		// different questions.
		var wantKept int
		b.Run(name+"/fetch-and-rank", func(b *testing.B) {
			for b.Loop() {
				wantKept = fetchAndRank(b, d, k)
			}
		})
		b.Run(name+"/cutoff-then-fetch", func(b *testing.B) {
			for b.Loop() {
				if got := cutoffThenFetch(b, d, k); got != wantKept {
					b.Fatalf("cutoff kept %d rows, fetch-and-rank kept %d", got, wantKept)
				}
			}
		})
	}
}
