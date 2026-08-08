package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Does the columnar accelerator earn its keep on an ARCHIVE?
//
// Maintenance builds it for mutable tables only (DB.Maintain, gated on SchemaScanHotTopN);
// maintainArchives does merge/reindex/index-autotune and never touches it. So a history table
// scans row-wise today. These benchmarks measure the same archive with and without it before
// anything is wired up: an archive is the big append-only scan target, so the answer decides
// whether it is worth the sampling and the block-per-segment rebuild on merge.

// archiveBenchAds seeds n history-shaped ads.
func archiveBenchAds(tb testing.TB, a *Archive, n int) {
	tb.Helper()
	for i := 0; i < n; i++ {
		ad, err := classad.ParseOld(fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\nJobStatus = %d\n"+
				"RequestMemory = %d\nRequestCpus = %d\nCompletionDate = %d\n"+
				"RemoteWallClockTime = %d.5\nExitCode = %d\nCmd = \"/home/u%d/run.sh\"",
			i/10, i%10, i%64, 3+i%2, ((i%16)+1)*512, 1+i%8, 1700000000+i, i%3600, i%2, i%64))
		if err != nil {
			tb.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			tb.Fatal(err)
		}
	}
}

// benchArchive builds an archive of n ads, optionally enabling the columnar accelerator over
// its backing collection (which is what a maintainArchives change would do).
func benchArchive(tb testing.TB, n int, columnar bool) *Archive {
	tb.Helper()
	a, err := CreateArchive(ArchiveOptions{Dir: tb.TempDir(), SegmentSize: 1 << 20})
	if err != nil {
		tb.Fatal(err)
	}
	archiveBenchAds(tb, a, n)
	if columnar {
		// Drive demand on the attributes the queries below read, so they land in the hot tier
		// exactly as a maintenance pass would arrange.
		for _, e := range []string{"RequestMemory >= 0", "RequestCpus >= 0", "ExitCode >= 0"} {
			q, err := vm.Parse(e)
			if err != nil {
				tb.Fatal(err)
			}
			for i := 0; i < 20; i++ {
				for range a.c.Query(q) {
				}
			}
		}
		if !a.c.BuildAndEnableSchemaScan(4000, 8) {
			tb.Fatal("BuildAndEnableSchemaScan returned false on an archive")
		}
	}
	return a
}

// archiveCountQueries are history-shaped numeric counts -- the shape the columnar path serves.
var archiveCountQueries = []struct{ name, expr string }{
	{"memory_range", "RequestMemory >= 4096"},
	{"cpus_eq", "RequestCpus == 4"},
	{"exit_nonzero", "ExitCode != 0"},
	// A same-field range. The columnar fast path serves single-int-field comparisons only, so a
	// two-field conjunction is outside its scope by design, not because this is an archive.
	{"memory_band", "RequestMemory >= 2048 && RequestMemory < 8192"},
}

const archiveBenchN = 50000

// BenchmarkArchiveCount measures COUNT-style queries over an archive with the accelerator off
// (today's behaviour) and on.
func BenchmarkArchiveCount(b *testing.B) {
	for _, columnar := range []bool{false, true} {
		mode := "row"
		if columnar {
			mode = "columnar"
		}
		a := benchArchive(b, archiveBenchN, columnar)
		for _, q := range archiveCountQueries {
			b.Run(q.name+"/"+mode, func(b *testing.B) {
				parsed, err := vm.Parse(q.expr)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if columnar {
						if n, ok := a.c.CountQuery(parsed); ok {
							if n < 0 {
								b.Fatal("negative")
							}
							continue
						}
						b.Fatal("columnar count declined; the accelerator is not serving this query")
					}
					n := 0
					for range a.c.Query(parsed) {
						n++
					}
					if n < 0 {
						b.Fatal("negative")
					}
				}
			})
		}
		_ = a.Close()
	}
}

// BenchmarkArchiveCountAgreement is not a benchmark of speed but a guard on the above: a
// columnar count that disagrees with the row scan would make the timings meaningless.
func BenchmarkArchiveCountAgreement(b *testing.B) {
	a := benchArchive(b, 5000, true)
	defer a.Close()
	for _, q := range archiveCountQueries {
		parsed, err := vm.Parse(q.expr)
		if err != nil {
			b.Fatal(err)
		}
		want := 0
		for range a.c.Query(parsed) {
			want++
		}
		got, ok := a.c.CountQuery(parsed)
		if !ok {
			b.Fatalf("%s: columnar count declined", q.name)
		}
		if got != want {
			b.Fatalf("%s: columnar %d != row %d", q.name, got, want)
		}
	}
	b.Logf("all %d queries agree between the columnar and row paths", len(archiveCountQueries))
}
