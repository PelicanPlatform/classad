package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// benchScanPathsQuery matches a large fraction of a real pool dump, so every arm
// below actually emits thousands of records rather than measuring the per-query
// setup of a scan that returns nothing.
const benchScanPathsQuery = `Cpus >= 1 && Memory > 1000`

// benchScanPathsProj is a monitoring-shaped projection: a dozen attributes out of
// the several hundred a startd ad carries.
var benchScanPathsProj = []string{
	"Name", "Machine", "OpSys", "Arch", "State", "Activity",
	"LoadAvg", "Memory", "Cpus", "EnteredCurrentActivity", "MyCurrentTime", "TotalSlots",
}

// BenchmarkScanPaths measures each filtered scan path -- the four that share the
// query-planning prologue plus the raw-decode path that shares it too -- over ONE
// store per representation, built once in the parent. Building a store per arm has
// swamped real differences here before: segment layout, dictionary state and hot-set
// convergence all vary run to run, and that variance lands entirely inside the arm
// that paid for it.
//
// The persistent (inline-names) store serves the four paths that work on it; QueryRaw
// needs an interned collection (it yields nothing for an inline one), so it gets the
// in-memory store. Both hold the same ads.
func BenchmarkScanPaths(b *testing.B) {
	sample := loadCorpus(b)
	const n = 5000

	pc := populatePersistent(b, sample, n, b.TempDir())
	defer pc.Close()
	mc := populate(b, sample, n)

	q, err := vm.Parse(benchScanPathsQuery)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("Query", func(b *testing.B) {
		b.ReportAllocs()
		got := 0
		for i := 0; i < b.N; i++ {
			got = 0
			for range pc.Query(q) {
				got++
			}
		}
		reportScanned(b, got)
	})
	b.Run("QueryProject", func(b *testing.B) {
		b.ReportAllocs()
		got := 0
		for i := 0; i < b.N; i++ {
			got = 0
			for range pc.QueryProject(q, []string{"Cpus", "Memory"}) {
				got++
			}
		}
		reportScanned(b, got)
	})
	b.Run("QueryRawProjected", func(b *testing.B) {
		b.ReportAllocs()
		got := 0
		for i := 0; i < b.N; i++ {
			got = 0
			for range pc.QueryRawProjected(q, benchScanPathsProj, false, true) {
				got++
			}
		}
		reportScanned(b, got)
	})
	b.Run("QueryRawWire", func(b *testing.B) {
		b.ReportAllocs()
		got := 0
		for i := 0; i < b.N; i++ {
			got = 0
			for range pc.QueryRawWire(q, benchScanPathsProj, true) {
				got++
			}
		}
		reportScanned(b, got)
	})
	b.Run("QueryRaw", func(b *testing.B) {
		b.ReportAllocs()
		got := 0
		for i := 0; i < b.N; i++ {
			got = 0
			for range mc.QueryRaw(q) {
				got++
			}
		}
		reportScanned(b, got)
	})
	b.Run("QueryRawFromSeq", func(b *testing.B) {
		b.ReportAllocs()
		got := 0
		for i := 0; i < b.N; i++ {
			got = 0
			seq, _ := pc.QueryRawFromSeq(q, benchScanPathsProj, SeqCursor{}, 0)
			for range seq {
				got++
			}
		}
		reportScanned(b, got)
	})
}

// BenchmarkScanPathsPerQuery isolates the FIXED per-query cost of each scan path:
// the planning prologue, the match state it builds, and the emitter -- over a store
// holding a single ad, so the per-record work is one record instead of thousands.
//
// It is deliberately the toy scan BenchmarkScanPaths is not. Per-record regressions
// hide here; a per-query allocation (a closure the prologue did not use to make, a
// value that newly escapes) hides in BenchmarkScanPaths, where it is one allocation
// in tens of thousands. The two together cover both.
func BenchmarkScanPathsPerQuery(b *testing.B) {
	sample := loadCorpus(b)

	pc := populatePersistent(b, sample, 1, b.TempDir())
	defer pc.Close()
	mc := populate(b, sample, 1)

	q, err := vm.Parse(benchScanPathsQuery)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("Query", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for range pc.Query(q) {
			}
		}
	})
	b.Run("QueryProject", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for range pc.QueryProject(q, []string{"Cpus", "Memory"}) {
			}
		}
	})
	b.Run("QueryRawProjected", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for range pc.QueryRawProjected(q, benchScanPathsProj, false, true) {
			}
		}
	})
	b.Run("QueryRawWire", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for range pc.QueryRawWire(q, benchScanPathsProj, true) {
			}
		}
	})
	b.Run("QueryRaw", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for range mc.QueryRaw(q) {
			}
		}
	})
	b.Run("QueryRawFromSeq", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			seq, _ := pc.QueryRawFromSeq(q, benchScanPathsProj, SeqCursor{}, 0)
			for range seq {
			}
		}
	})
}

// reportScanned records how many records an arm actually emitted, so a run whose
// store or predicate drifted (and is therefore measuring a different amount of work)
// is visible in the benchmark output rather than silently comparable.
func reportScanned(b *testing.B, got int) {
	b.ReportMetric(float64(got), "rows")
}
