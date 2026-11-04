package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
)

// A PROJECTED WIRE READ WITH NO WHERE, OVER A COLUMNARIZED ARCHIVE.
//
// QueryRawWireRefsStats short-circuits an empty or `true` constraint to ScanRawWireRefs, and that
// path passes NO projection plan. Its constrained sibling says what that costs -- "Passing no plan
// at all costs the columnar fast path on every record: measured on 20k real job ads it was 83ms
// with the plan against 255ms without" -- and the unconstrained TEXT twin, ScanRawProjected, does
// build one: "No query, so every record is a candidate for the columnar projection". Of the three
// siblings, only the unconstrained WIRE read goes without.
//
// That looks like a plain omission. It is not, and this benchmark exists to stop it being
// "fixed" as one.
//
// MEASURED, 30 iterations x 3, best of each arm, over a columnarized archive:
//
//	n        true-shortcut          equivalent predicate      allocations
//	20,000   17.0 ms                13.7 ms  (1.24x FASTER)   265k -> 20k   (13x fewer)
//	100,000  29.3 ms  (2.37x FASTER) 69.5 ms                  371k -> 100k  (3.7x fewer)
//
// The columnar path allocates far less at BOTH sizes and is faster at one of them. Routing the
// no-WHERE read through it -- the obvious reading of the three siblings -- would make the 100k
// case 2.4x slower.
//
// WHAT THE PER-RECORD NUMBERS SAY. The shortcut costs 0.85 us/record at 20k and 0.29 us/record at
// 100k; the columnar path is flat at 0.69 and 0.70. One of them is getting something back from
// size and the other is not, which is the shape of a fan-out engaging on one path only. Both
// gates read identically (qsem != nil && len(tasks) >= 2 && totalBytes >= parallelMinBytes), so
// that is a hypothesis this benchmark does NOT settle -- the fan-out counters are unexported, and
// confirming it needs a test inside collections.
//
// The control predicate names JobStatus, which is NOT value-indexed here. An earlier version used
// `ClusterId >= 0` and measured the shortcut 2.9x faster at 100k -- because ClusterId IS
// value-indexed, so that predicate changed the READ PLAN into an index scan rather than isolating
// the projection plan. The control has to be a predicate the planner cannot use an index for, or
// it is not a control.

func BenchmarkRawWireRefsNoWhere(b *testing.B) {
	for _, n := range []int{20000, 100000} {
		cat, a := archiveCountFixture(b, n)
		proj := []string{"ClusterId", "Owner", "JobStatus"}

		// Agreement first: the two spellings must return the same number of rows, or the
		// faster one is faster for the wrong reason.
		count := func(constraint string) int {
			rows, err := a.QueryRawWireRefs(constraint, proj, false)
			if err != nil {
				b.Fatalf("%q: %v", constraint, err)
			}
			got := 0
			for range rows {
				got++
			}
			return got
		}
		viaTrue, viaPred := count("true"), count("JobStatus >= 0")
		if viaTrue != n || viaPred != n {
			b.Fatalf("n=%d: `true` gave %d rows, `JobStatus >= 0` gave %d", n, viaTrue, viaPred)
		}

		for round := 0; round < 2; round++ {
			arms := []struct {
				name       string
				constraint string
			}{{"true-shortcut", "true"}, {"equivalent-predicate", "JobStatus >= 0"}}
			if round%2 == 1 {
				arms[0], arms[1] = arms[1], arms[0]
			}
			for _, arm := range arms {
				b.Run(fmt.Sprintf("n=%d/r%d/%s", n, round, arm.name), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						rows, err := a.QueryRawWireRefs(arm.constraint, proj, false)
						if err != nil {
							b.Fatal(err)
						}
						got := 0
						for range rows {
							got++
						}
						if got != n {
							b.Fatalf("%d rows, want %d", got, n)
						}
					}
				})
			}
		}
		cat.Close()
	}
	_ = collections.ScanStats{}
}
