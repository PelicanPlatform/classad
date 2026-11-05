package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// LIKE FOR LIKE: the same two reads with FAN-OUT OFF for both.
//
// BenchmarkRawWireRefsNoWhere compares the `true` shortcut against an equivalent predicate and
// finds the shortcut 2.37x faster at 100k. That comparison is confounded, and this is the control
// that shows it.
//
// An archive sets ReverseScan (newest-first, matching condor_history order), and reverseScan
// "Forces serial scans (no fan-out), since fan-out has no cross-segment order". The CONSTRAINED
// wire read honours that -- its gate is `... && !c.reverseScan` -- so on an archive it is always
// serial. The UNCONSTRAINED shortcut, runParallelWireScan, has no such gate and fans out anyway.
//
// So the earlier 100k result is not columnar against non-columnar. It is SERIAL columnar against
// PARALLEL non-columnar, and the fan-out is doing the winning. With QueryParallelism=1 both arms
// are serial and the only remaining difference is the projection plan.
func BenchmarkRawWireRefsSerial(b *testing.B) {
	for _, n := range []int{20000, 100000} {
		cat, err := OpenCatalogConfig(CatalogConfig{Dir: b.TempDir(), QueryParallelism: 1})
		if err != nil {
			b.Fatal(err)
		}
		a, err := cat.CreateArchiveTable("history", ArchiveConfig{
			SegmentSize: 1 << 16,
			ValueAttrs:  []string{"ClusterId"},
		})
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < n; i++ {
			ad, perr := classad.ParseOld(fmt.Sprintf(
				"ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\nRequestCpus = %d\n"+
					"Owner = \"user%d\"\nCmd = \"/home/user%d/run.sh\"\nCompletionDate = %d",
				i, i%10, 1+i%5, 1024+(i%32)*512, 1+i%8, i%512, i%512, 1700000000+i))
			if perr != nil {
				b.Fatal(perr)
			}
			if aerr := a.Append(ad); aerr != nil {
				b.Fatal(aerr)
			}
		}
		if !a.BuildAndEnableSchemaScan(4000, 8) {
			b.Skip("no sealed segments to accelerate")
		}
		proj := []string{"ClusterId", "Owner", "JobStatus"}

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
						rows, qerr := a.QueryRawWireRefs(arm.constraint, proj, false)
						if qerr != nil {
							b.Fatal(qerr)
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
}
