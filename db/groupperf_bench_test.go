package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// Is server-side grouping actually faster than client-side grouping?
//
// There is no logical difference between the two: both read every matching record's group
// attribute and fold it into a bucket. The server does it closer to the bytes, with the column
// already decoded, so it should win outright. If it does not, the server implementation is
// doing work the client's is not, and THAT is the thing to fix -- not something to design
// around.
//
// The arms:
//
//	server-grouped  the columnar GROUP BY: AggregateColsStats, one row per group back
//	client-grouped  QueryProject of the group attribute, folded into a Go map here
//
// client-grouped is the honest baseline, not a strawman: it is exactly what a caller does today
// when it does not trust the server to group, and it is what the top-k rung in #696 did by hand.

func groupPerfDB(b *testing.B, groups, rows int) *DB {
	b.Helper()
	d, err := OpenConfig(Config{SegmentSize: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { d.Close() })
	tx := d.Begin()
	n := 0
	for g := 0; g < groups; g++ {
		for r := 0; r < rows; r++ {
			ad := classad.New()
			ad.InsertAttrString("Owner", fmt.Sprintf("o%d", g))
			// OwnerID is Owner as a number: same cardinality, same record, different type.
			// It is what BenchmarkGroupByColumnKind groups by to isolate the column's type.
			ad.InsertAttr("OwnerID", int64(g))
			ad.InsertAttr("Cpus", int64(r%16))
			ad.InsertAttr("QDate", int64(g*1_000_000+r))
			tx.NewClassAd(fmt.Sprintf("k%d", n), ad)
			n++
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	d.EnableSchemaScan(4000, 8)
	// PROVENANCE GATE. A fixture whose segments are all still active has no columnar blocks, so
	// every arm below silently measures the per-record brute path and the comparison says nothing
	// about the columnar one. At 1 MiB segments that needs a few hundred thousand records; 100k
	// seals nothing at all. Fail rather than skip: a benchmark that quietly measures the wrong
	// path is worse than one that does not run.
	if info := d.SchemaScanInfo(); info.CoveredSegments == 0 {
		b.Fatalf("no columnar coverage (%d sealed, %d covered): this fixture measures the brute path",
			info.SealedSegments, info.CoveredSegments)
	}
	return d
}

// BenchmarkGroupServerVsClient is the first-principles check. Same data, same question, both
// sides.
func BenchmarkGroupServerVsClient(b *testing.B) {
	for _, shape := range []struct{ groups, rows int }{
		{10, 40000},
		{100, 4000},
		{1000, 400},
	} {
		d := groupPerfDB(b, shape.groups, shape.rows)
		name := fmt.Sprintf("groups=%d/rows=%d", shape.groups, shape.rows)

		// COUNT(*) GROUP BY Owner -- the cheapest possible grouped question, so the number is
		// the GROUPING cost and not an aggregate's.
		b.Run(name+"/server-count", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				rows, err := d.AggregateColsStats("true", []GroupCol{{Attr: "Owner"}},
					[]AggSpec{{Func: AggCount, Arg: "*"}}, nil, nil)
				if err != nil {
					b.Fatal(err)
				}
				if len(rows) != shape.groups {
					b.Fatalf("groups = %d, want %d", len(rows), shape.groups)
				}
			}
		})
		b.Run(name+"/client-count", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				seq, err := d.QueryProject("true", []string{"Owner"})
				if err != nil {
					b.Fatal(err)
				}
				per := map[string]int{}
				for row := range seq {
					s, serr := row[0].StringValue()
					if serr != nil {
						continue
					}
					per[s]++
				}
				if len(per) != shape.groups {
					b.Fatalf("groups = %d, want %d", len(per), shape.groups)
				}
			}
		})

		// SUM(Cpus) GROUP BY Owner -- now the aggregate has to read a second column too.
		b.Run(name+"/server-sum", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				rows, err := d.AggregateColsStats("true", []GroupCol{{Attr: "Owner"}},
					[]AggSpec{{Func: AggSum, Arg: "Cpus"}}, nil, nil)
				if err != nil {
					b.Fatal(err)
				}
				if len(rows) != shape.groups {
					b.Fatalf("groups = %d, want %d", len(rows), shape.groups)
				}
			}
		})
		b.Run(name+"/client-sum", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				seq, err := d.QueryProject("true", []string{"Owner", "Cpus"})
				if err != nil {
					b.Fatal(err)
				}
				per := map[string]int64{}
				for row := range seq {
					s, serr := row[0].StringValue()
					if serr != nil {
						continue
					}
					c, cerr := row[1].IntValue()
					if cerr != nil {
						continue
					}
					per[s] += c
				}
				if len(per) != shape.groups {
					b.Fatalf("groups = %d, want %d", len(per), shape.groups)
				}
			}
		})
	}
}

// BenchmarkGroupByColumnKind asks whether the group column's TYPE decides the cost: a numeric
// column against a string one, same rows, same question.
//
// The arms group by OwnerID and Owner, which hold the same value in the same records and differ
// only in type. An earlier version grouped by Cpus and Owner -- and Cpus has 16 distinct values
// where Owner has 100, so it compared two different queries and read the difference as the
// column type's. At equal cardinality the apparent 1.7x penalty on the string column is gone:
// the two arms land within the run-to-run spread, which on the machine this was written on is
// wider than the gap it was supposed to show. Hence the group-count assertion below, which the
// Cpus version would have failed.
//
// Read the ALLOCATION column, not the time. Allocations are stable across runs and still differ
// -- a string column interns each distinct value it meets -- where ns/op here has swung 5x
// between runs of unchanged code. A timing conclusion drawn from one run of this benchmark is
// not a conclusion.
func BenchmarkGroupByColumnKind(b *testing.B) {
	const groups = 100
	d := groupPerfDB(b, groups, 4000)
	for _, attr := range []string{"OwnerID", "Owner"} {
		b.Run(attr, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				rows, err := d.AggregateColsStats("true", []GroupCol{{Attr: attr}},
					[]AggSpec{{Func: AggCount, Arg: "*"}}, nil, nil)
				if err != nil {
					b.Fatal(err)
				}
				// Both arms must ask the same question. Without this the two differ in group
				// cardinality and the benchmark silently measures that instead of the type.
				if len(rows) != groups {
					b.Fatalf("groups = %d, want %d", len(rows), groups)
				}
			}
		})
	}
}
