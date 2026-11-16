package dbrpc

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// What a ranking window costs ACROSS THE WIRE, which is the only place the per-group cutoff can
// pay for itself.
//
// The embedded benchmark (db/aggtopk_bench_test.go) measures the storage work and the cutoff
// loses there by construction: it runs two passes where fetching everything runs one, and a
// columnar scan of 100k rows is only ~27ms. Nothing about the cutoff can beat one scan locally.
//
// Its whole value is rows NOT SENT. "The 5 newest jobs per owner" over 100 owners is 500 rows
// where the client-side ranking takes 100,000, and each of those rows is encoded by the server
// and decoded by the client. This benchmark is that comparison: both arms end with the same
// answer in the client's hands, over a real client/server pair.
//
// The transport is an in-process pipe, so what it charges is ENCODE AND DECODE and not link
// bandwidth or latency. That is the conservative direction -- a real network only adds cost to
// the arm that sends 200x more rows -- but it does mean the margin here is a floor, not the
// figure a deployment would see.

// topKWireDB seeds owners x rows jobs with distinct QDates per owner.
func topKWireDB(b *testing.B, owners, rows int) *db.DB {
	b.Helper()
	d, err := db.Open("")
	if err != nil {
		b.Fatal(err)
	}
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

// rankBatches folds a stream of value batches into the top k QDates per owner, which is what a
// client answering the window does with whatever rows it was given.
type ranker struct {
	per  map[string][]int64
	rows int
}

func (rk *ranker) add(bt *ValueBatch) {
	if rk.per == nil {
		rk.per = map[string][]int64{}
	}
	owner, qdate := &bt.Cols[0], &bt.Cols[1]
	for i := 0; i < bt.Rows; i++ {
		if i >= len(owner.Strs) || i >= len(qdate.Ints) {
			continue
		}
		rk.per[owner.Strs[i]] = append(rk.per[owner.Strs[i]], qdate.Ints[i])
		rk.rows++
	}
}

func (rk *ranker) top(k int) int {
	kept := 0
	for _, qs := range rk.per {
		sort.Slice(qs, func(i, j int) bool { return qs[i] > qs[j] })
		if len(qs) > k {
			qs = qs[:k]
		}
		kept += len(qs)
	}
	return kept
}

// fetchAllAndRank is today's path: every matching row crosses the wire.
func fetchAllAndRank(b *testing.B, c *Client, k int) (kept, shipped int) {
	b.Helper()
	rk := &ranker{}
	err := c.QueryValuesStream(context.Background(), DefaultTable, "true",
		[]string{"Owner", "QDate"}, 0, false, func(bt *ValueBatch) bool {
			rk.add(bt)
			return true
		})
	if err != nil {
		b.Fatal(err)
	}
	return rk.top(k), rk.rows
}

// cutoffThenFetchWire asks the server for the per-group cutoffs, then for the rows above them.
func cutoffThenFetchWire(b *testing.B, c *Client, k int) (kept, shipped int) {
	b.Helper()
	ctx := context.Background()
	cuts, err := c.Aggregate(ctx, "true", []string{"Owner"},
		[]AggSpec{{Func: db.AggTopKThreshold, Arg: "QDate", K: k, Desc: true}})
	if err != nil {
		b.Fatal(err)
	}
	terms := make([]string, 0, len(cuts))
	for _, r := range cuts {
		if r.Values[0] == "undefined" {
			terms = append(terms, fmt.Sprintf("Owner == %s", r.Group[0]))
			continue
		}
		terms = append(terms, fmt.Sprintf("(Owner == %s && QDate >= %s)", r.Group[0], r.Values[0]))
	}
	rk := &ranker{}
	err = c.QueryValuesStream(ctx, DefaultTable, strings.Join(terms, " || "),
		[]string{"Owner", "QDate"}, 0, false, func(bt *ValueBatch) bool {
			rk.add(bt)
			return true
		})
	if err != nil {
		b.Fatal(err)
	}
	return rk.top(k), rk.rows
}

// BenchmarkTopKOverTheWire interleaves the arms per shape so machine drift hits both alike, and
// reports rows-shipped alongside the time -- the quantity the cutoff exists to reduce, which a
// nanoseconds-per-op figure alone would hide.
func BenchmarkTopKOverTheWire(b *testing.B) {
	const k = 5
	for _, shape := range []struct{ owners, rows int }{
		{10, 1000},
		{100, 1000},
		{1000, 100},
	} {
		d := topKWireDB(b, shape.owners, shape.rows)
		s := NewServer(d)
		cconn, sconn := netPipe()
		go func() { _ = s.ServeConn(sconn) }()
		c := NewClient(cconn)

		name := fmt.Sprintf("owners=%d/rows=%d", shape.owners, shape.rows)
		var wantKept int
		b.Run(name+"/fetch-all-and-rank", func(b *testing.B) {
			var shipped int
			for b.Loop() {
				wantKept, shipped = fetchAllAndRank(b, c, k)
			}
			b.ReportMetric(float64(shipped), "rows-shipped")
		})
		b.Run(name+"/cutoff-then-fetch", func(b *testing.B) {
			var shipped int
			for b.Loop() {
				var kept int
				kept, shipped = cutoffThenFetchWire(b, c, k)
				if kept != wantKept {
					b.Fatalf("cutoff kept %d rows, fetch-all kept %d", kept, wantKept)
				}
			}
			b.ReportMetric(float64(shipped), "rows-shipped")
		})
		c.Close()
		s.Close()
		d.Close()
	}
}
