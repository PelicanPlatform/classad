package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// The shape that made archived job pages slow on a real access point: one big cluster whose
// procs finished over weeks, in a history archive htcondordb-style (ClusterId value-indexed,
// columnar segments, reopened so nothing is warm). Its newest proc is near the head of the
// newest-first walk; its oldest is behind every other proc of the cluster. Each fetch is
// `ClusterId == C && ProcId == P` with LIMIT 1.
//
//	go test ./collections/ -run '^$' -bench ArchiveOldRowFetch -benchtime 20x
//
// "scan" is the same lookup written so no index can serve it (ClusterId + 0), for scale.
func BenchmarkArchiveOldRowFetch(b *testing.B) {
	const (
		records = 200000
		every   = 4 // one record in four is the big cluster's: 50,000 procs
	)
	dir := b.TempDir()
	opts := ArchiveOptions{Dir: dir, SegmentSize: 1 << 20,
		ValueAttrs: []string{"ClusterId"}, CategoricalAttrs: []string{"Owner"},
		ZoneAttrs: []string{"CompletionDate", "EnteredHistoryTime"}}
	a, err := CreateArchive(opts)
	if err != nil {
		b.Fatal(err)
	}
	for i := range records {
		cluster, proc := 100000+i/40, i%40
		if i%every == 0 {
			cluster, proc = 50000, i/every
		}
		ad, perr := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\nJobStatus = 4\n"+
			"CompletionDate = %d\nEnteredHistoryTime = %d\nRemoteWallClockTime = %d.0\nExitCode = 0\n"+
			"Cmd = \"/home/u/run.sh\"\nArgs = \"--seed %d --input /data/set%d --output /out/%d\"\n"+
			"RequestMemory = 2048\nRequestCpus = 1\nMemoryUsage = %d\nIwd = \"/home/u%d/work\"",
			cluster, proc, i%9, 1700000000+i, 1700000000+i, 3000+i%600, i, i%17, i, 900+i%300, i%9))
		if perr != nil {
			b.Fatal(perr)
		}
		if err := a.Append(ad); err != nil {
			b.Fatal(err)
		}
	}
	if !a.BuildAndEnableSchemaScan(5000, 32) {
		b.Fatal("no columnar schema")
	}
	a.Close()
	if a, err = OpenArchive(opts); err != nil {
		b.Fatal(err)
	}
	defer a.Close()
	newest := (records - 1) / every
	for _, c := range []struct{ name, constraint string }{
		{"old", "ClusterId == 50000 && ProcId == 1"},
		{"middle", fmt.Sprintf("ClusterId == 50000 && ProcId == %d", newest/2)},
		{"new", fmt.Sprintf("ClusterId == 50000 && ProcId == %d", newest)},
		{"scan-old", "ClusterId + 0 == 50000 && ProcId == 1"},
	} {
		q, err := vm.Parse(c.constraint)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				n := 0
				for range a.QueryLimit(q, 1) {
					n++
				}
				if n != 1 {
					b.Fatalf("%s: %d rows", c.constraint, n)
				}
			}
		})
	}
}
