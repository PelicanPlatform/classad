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
// It IS a real gap, and the first version of this comment got the reason wrong. The numbers and
// the correction are both kept, because the mistake is the instructive part.
//
// MEASURED, 30 iterations x 3, best of each arm, over a columnarized archive:
//
//	n        true-shortcut           equivalent predicate     allocations
//	20,000   17.0 ms                 13.7 ms  (1.24x faster)  265k -> 20k   (13x fewer)
//	100,000  29.3 ms  (2.37x faster) 69.5 ms                  371k -> 100k  (3.7x fewer)
//
// Read alone that says the columnar projection loses at scale, and that routing the no-WHERE read
// through it would be a 2.4x regression at 100k. That reading is WRONG: the two arms do not get
// the same parallelism.
//
// An archive sets ReverseScan ("newest-first Query, matching condor_history order"), and
// reverseScan is documented to force "serial scans (no fan-out), since fan-out has no
// cross-segment order". The CONSTRAINED wire read honours that -- its gate ends `&& !c.reverseScan`
// -- so on an archive it is ALWAYS serial. runParallelWireScan, which serves the unconstrained
// read, carries no such gate and fans out regardless. So the 100k row above is serial columnar
// against parallel non-columnar, and the fan-out is doing the winning.
//
// Same benchmark at -cpu=1, where neither arm can use more than one P:
//
//	n        true-shortcut   equivalent predicate
//	20,000   24.5 ms         14.8 ms  (1.65x faster)
//	100,000  73.6 ms         77.0 ms  (about even)
//
// The shortcut loses 2.5x without parallelism (29.3 -> 73.6); the columnar path barely moves
// (69.5 -> 77.0) because it never had any. Like for like the columnar projection is better at
// 20k, even at 100k, and allocates 3.7x to 13x less throughout.
//
// SO THE HEADROOM IS THE COMBINATION NOTHING CURRENTLY OFFERS: the columnar projection with the
// fan-out. The unconstrained read has fan-out without the projection; the constrained read has
// the projection without fan-out, on archives, for every filtered history query there is.
//
// A caveat on what could not be controlled here: CatalogConfig.QueryParallelism does NOT reach
// archives -- CreateArchiveTable calls openArchiveTable directly and never goes through
// tableConfig -- so setting it to 1 is a no-op and -cpu=1 is the only lever that works.
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
