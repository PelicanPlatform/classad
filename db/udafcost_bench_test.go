package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// WHAT A USER-DEFINED AGGREGATE COSTS, measured rather than argued.
//
// The registry's doc comment claims a user-defined aggregate forfeits the columnar value pass and
// falls to the record scan. That claim was wrong once already -- it named the wrong path -- so it is
// worth a number rather than another reading of the code. Each pair below runs the SAME arithmetic
// twice over the same store: once as the built-in, which takes whichever columnar rung applies, and
// once as a registered aggregate computing the identical answer, which falls to the projected scan.
// The ratio is the cliff.

// benchSum is an exact integer sum: the same answer SUM gives over an integer column, so the two
// arms of each pair differ only in which rung of the ladder serves them.
type benchSum struct{}

func (benchSum) NewAccumulator() Accumulator { return &benchSumAcc{} }

type benchSumAcc struct{ n int64 }

func (a *benchSumAcc) Add(v classad.Value) {
	if i, err := v.IntValue(); err == nil {
		a.n += i
	}
}
func (a *benchSumAcc) Merge(other Accumulator) { a.n += other.(*benchSumAcc).n }
func (a *benchSumAcc) Result() classad.Value   { return classad.NewIntValue(a.n) }

// benchSumCol is benchSum that also takes column values, so the two differ only in which rung of
// the ladder serves them. Summing integers is order-independent and ignores non-numbers either way,
// which is what lets it make the promises ColumnAccumulator requires.
type benchSumCol struct{}

func (benchSumCol) NewAccumulator() Accumulator { return &benchSumColAcc{} }

type benchSumColAcc struct{ benchSumAcc }

func (a *benchSumColAcc) Merge(other Accumulator) { a.n += other.(*benchSumColAcc).n }
func (a *benchSumColAcc) AddNum(v NumValue) {
	if v.Kind == NumInt {
		a.n += v.I
	}
}

// benchAggFixture is a mutable table with the accelerator on: a numeric column to aggregate and a
// low-cardinality one to group by.
func benchAggFixture(b *testing.B, n int) *DB {
	b.Helper()
	d, err := OpenConfig(Config{SegmentSize: 1 << 16})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { d.Close() })
	tx := d.Begin()
	for i := 0; i < n; i++ {
		ad := classad.New()
		ad.InsertAttr("JobStatus", int64(i%5))
		ad.InsertAttr("Cpus", int64(i%64))
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	if !d.EnableSchemaScan(4000, 8) {
		b.Skip("no sealed segments to accelerate")
	}
	return d
}

func BenchmarkAggregateBuiltinVsUser(b *testing.B) {
	const n = 50000
	name, colName := "bench_sum", "bench_sum_col"
	for n, a := range map[string]Aggregate{name: benchSum{}, colName: benchSumCol{}} {
		if _, ok := LookupAggregate(n); !ok {
			if err := RegisterAggregate(n, a); err != nil {
				b.Fatal(err)
			}
		}
	}
	d := benchAggFixture(b, n)

	cases := []struct {
		what   string
		groups []GroupCol
		aggs   []AggSpec
	}{
		{"ungrouped/builtin", nil, []AggSpec{{Func: AggSum, Arg: "Cpus"}}},
		{"ungrouped/user-scan", nil, []AggSpec{{Func: AggUser, UserFunc: name, Arg: "Cpus"}}},
		{"ungrouped/user-column", nil, []AggSpec{{Func: AggUser, UserFunc: colName, Arg: "Cpus"}}},
		{"grouped/builtin", []GroupCol{{Attr: "JobStatus"}}, []AggSpec{{Func: AggSum, Arg: "Cpus"}}},
		{"grouped/user-scan", []GroupCol{{Attr: "JobStatus"}}, []AggSpec{{Func: AggUser, UserFunc: name, Arg: "Cpus"}}},
		// Grouped now has a columnar rung of its own, so this arm and the one above it differ in
		// which rung serves them rather than in whether any does. Kept as a pair so a regression
		// that quietly sent a grouped user aggregate back to the scan shows up as a ratio.
		{"grouped/user-column", []GroupCol{{Attr: "JobStatus"}}, []AggSpec{{Func: AggUser, UserFunc: colName, Arg: "Cpus"}}},
	}
	for _, c := range cases {
		b.Run(c.what, func(b *testing.B) {
			// The premise: both arms really do compute the same answer, or the ratio compares
			// two different questions.
			rows, err := d.AggregateColsStats("true", c.groups, c.aggs, nil, nil)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(len(rows)), "groups")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := d.AggregateColsStats("true", c.groups, c.aggs, nil, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestBenchmarkArmsAgreeOnTheAnswer is the benchmark's premise as a test, so a ratio measured above
// cannot quietly be a comparison of two different aggregates.
func TestBenchmarkArmsAgreeOnTheAnswer(t *testing.T) {
	name := mustRegister(t, "arms_sum", benchSum{})
	d, err := OpenConfig(Config{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	for i := 0; i < 5000; i++ {
		ad := classad.New()
		ad.InsertAttr("JobStatus", int64(i%5))
		ad.InsertAttr("Cpus", int64(i%64))
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	d.EnableSchemaScan(4000, 8)

	for _, groups := range [][]GroupCol{nil, {{Attr: "JobStatus"}}} {
		builtin, err := d.AggregateColsStats("true", groups, []AggSpec{{Func: AggSum, Arg: "Cpus"}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		user, err := d.AggregateColsStats("true", groups, []AggSpec{{Func: AggUser, UserFunc: name, Arg: "Cpus"}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(builtin) != len(user) {
			t.Fatalf("groups=%v: %d rows built-in, %d user", groups, len(builtin), len(user))
		}
		// Compared BY GROUP KEY, not by position. The two rungs do not agree on row order --
		// the columnar one emits groups sorted, the projected scan emits them in the order the
		// scan first saw them, which is not insertion order either. Neither is wrong (a GROUP BY
		// without an ORDER BY promises no order) but it means an index-wise comparison here
		// compares different groups, which is how this test first "failed".
		want := map[string]string{}
		for _, r := range builtin {
			want[fmt.Sprint(r.Group)] = r.Values[0]
		}
		for _, r := range user {
			k := fmt.Sprint(r.Group)
			w, ok := want[k]
			if !ok {
				t.Fatalf("groups=%v: the user aggregate produced group %s, which the built-in did not", groups, k)
			}
			if r.Values[0] != w {
				t.Fatalf("groups=%v group %s: built-in %q, user %q", groups, k, w, r.Values[0])
			}
		}
	}
}
