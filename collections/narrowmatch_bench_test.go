package collections

import (
	"fmt"
	"os"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// The epoch-history shape: wide ads (~150 attributes) of which only a handful are present in
// nearly every record, so the derived base schema is narrow (the eight identity/time fields) and
// the attributes a query filters on are mostly in the remnant. A tenth of the records carry
// VacateReasonCode, the attribute the benchmark query selects on.

const epochBenchBaseT = 1766900000

// epochBenchAd builds epoch-history ad i. Every non-base attribute is present in ~80% of the
// records (keyed on i and the attribute), which keeps it under the schema's presence floor.
func epochBenchAd(tb testing.TB, i int) *classad.ClassAd {
	tb.Helper()
	var b strings.Builder
	owner := fmt.Sprintf("user%d", i%37)
	cluster, proc := 13177000+i/100, i%100
	wt := epochBenchBaseT + i*7
	fmt.Fprintf(&b, "ClusterId = %d\nProcId = %d\nRunInstanceID = %d\nEpochWriteDate = %d\n", cluster, proc, i%5, wt)
	fmt.Fprintf(&b, "EnteredHistoryTime = %d\nNumShadowStarts = %d\nOwner = %q\n", wt+3, 1+i%3, owner)
	if i%20 != 0 {
		typ := "EPOCH"
		if i%9 == 0 {
			typ = "SPAWN"
		}
		fmt.Fprintf(&b, "EpochAdType = %q\n", typ)
	}
	if i%10 == 3 {
		fmt.Fprintf(&b, "VacateReasonCode = %d\nVacateReasonSubCode = %d\nVacateReason = \"vacated by startd (code %d)\"\n", 20+i%7, i%3, 20+i%7)
	}
	present := func(k int) bool { return (i*2654435761+k*40503)%100 < 80 }
	put := func(k int, format string, args ...any) {
		if present(k) {
			fmt.Fprintf(&b, format, args...)
		}
	}
	put(1, "User = \"%s@ap40.uw.osg-htc.org\"\n", owner)
	put(2, "AccountingGroup = \"group_opportunistic.proj%d.%s\"\n", i%11, owner)
	put(3, "GlobalJobId = \"ap40.uw.osg-htc.org#%d.%d#%d\"\n", cluster, proc, wt-86400)
	put(4, "JobStatus = %d\n", 2+i%3)
	put(5, "QDate = %d\n", wt-86400)
	put(6, "JobCurrentStartDate = %d\n", wt-2500)
	put(7, "RemoteWallClockTime = %d.0\n", 2500+i%900)
	put(8, "CumulativeSlotTime = %d\n", 10000+i%3600)
	put(9, "RemoteUserCpu = %d.0\n", 9000+i%500)
	put(10, "RemoteSysCpu = %d.0\n", 40+i%9)
	put(11, "ImageSize = %d\n", 10000000)
	put(12, "DiskUsage = %d\n", 7500000)
	put(13, "ResidentSetSize = %d\n", 2000000+i%65536)
	put(14, "MemoryUsage = ((ResidentSetSize + 1023) / 1024)\n")
	put(15, "RequestMemory = %d\n", 2048+(i%4)*1024)
	put(16, "Cmd = \"/home/%s/project/sanitized/%016x\"\n", owner, i*7919)
	put(17, "Args = \"/home/%s/project/in/%016x\"\n", owner, i*104729)
	put(18, "LastRemoteHost = \"slot1_%d@glidein_%d_%d@node%d.example.edu\"\n", i%8, i%9973, i%7919, i%613)
	put(19, "ExitCode = %d\n", i%3/2)
	put(20, "Requirements = (TARGET.Arch == \"X86_64\") && (TARGET.Memory >= RequestMemory)\n")
	put(21, "PeriodicRemove = (JobStatus == 2) && (CurrentTime - JobCurrentStartDate > 86400)\n")
	for k := 0; k < 120; k++ {
		switch k % 4 {
		case 0:
			put(100+k, "EpochAttrInt%03d = %d\n", k, (i*31+k*17)%100000)
		case 1:
			put(100+k, "EpochAttrStr%03d = \"value-%d-%d\"\n", k, k, i%(k+3))
		case 2:
			put(100+k, "EpochAttrBool%03d = %t\n", k, (i+k)%3 == 0)
		default:
			put(100+k, "EpochAttrReal%03d = %d.25\n", k, i%(k+11))
		}
	}
	ad, err := classad.ParseOld(b.String())
	if err != nil {
		tb.Fatal(err)
	}
	return ad
}

// epochBenchArchive builds the production-shaped archive: ZSTD with a trained dictionary (the
// first segments are written before the retrain, the rest under it, as a long-running archive
// is), sealed segments columnarized under the derived narrow schema.
func epochBenchArchive(tb testing.TB, n int) *Archive {
	tb.Helper()
	codec, err := NewZSTDCodec(nil)
	if err != nil {
		tb.Fatal(err)
	}
	a, err := CreateArchive(ArchiveOptions{Dir: tb.TempDir(), Codec: codec, SegmentSize: 2 << 20,
		ZoneAttrs: []string{"EpochWriteDate", "EnteredHistoryTime"}})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { a.Close() })
	for i := 0; i < n; i++ {
		if i == n/20 {
			if _, err := a.RetrainDict(0); err != nil {
				tb.Fatalf("RetrainDict: %v", err)
			}
		}
		if err := a.Append(epochBenchAd(tb, i)); err != nil {
			tb.Fatal(err)
		}
	}
	if !a.BuildAndEnableSchemaScan(4096, 8) {
		tb.Fatal("no columnar schema derived")
	}
	return a
}

// epochBenchQuery is the production query shape: the type test reads a schema field, the time
// bound reads a schema field, and the selective test reads an attribute the schema does not carry.
func epochBenchQuery(tb testing.TB, n int) *vm.Query {
	tb.Helper()
	q, err := vm.Parse(fmt.Sprintf(`(EpochAdType =?= undefined || EpochAdType == "EPOCH") && EpochWriteDate >= %d && VacateReasonCode isnt undefined`,
		epochBenchBaseT+(n/20)*7))
	if err != nil {
		tb.Fatal(err)
	}
	return q
}

var epochBenchProjection = []string{"ClusterId", "ProcId", "RunInstanceID", "EpochWriteDate", "Owner", "EpochAdType",
	"VacateReasonCode", "VacateReasonSubCode", "VacateReason", "LastRemoteHost", "JobCurrentStartDate",
	"RemoteWallClockTime", "MemoryUsage", "RequestMemory", "ExitCode"}

func epochBenchN() int {
	if s := os.Getenv("EPOCH_BENCH_N"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 60000
}

// narrowBenchArm is one configuration of the scan: which of the two record-level savings are on.
type narrowBenchArm struct {
	name             string
	narrow, conjunct bool
}

var narrowBenchArms = []narrowBenchArm{
	{"base", false, false},
	{"conjunct", false, true},
	{"narrow", true, false},
	{"both", true, true},
}

func (a narrowBenchArm) set() func() {
	n, cj := narrowMatchOff, conjunctRejectOff
	narrowMatchOff, conjunctRejectOff = !a.narrow, !a.conjunct
	return func() { narrowMatchOff, conjunctRejectOff = n, cj }
}

// runNarrowBenchArms times read once per arm per round, interleaved and alternating the arm order
// each round, and reports each arm's median in ms (and its last ScanStats in the log). read
// returns the row count, which must agree across arms.
func runNarrowBenchArms(b *testing.B, rounds int, read func(st *ScanStats) int) {
	times := make([][]time.Duration, len(narrowBenchArms))
	rows := make([]int, len(narrowBenchArms))
	stats := make([]ScanStats, len(narrowBenchArms))
	for r := 0; r < rounds; r++ {
		for j := range narrowBenchArms {
			i := j
			if r%2 == 1 {
				i = len(narrowBenchArms) - 1 - j
			}
			restore := narrowBenchArms[i].set()
			stats[i] = ScanStats{}
			t0 := time.Now()
			rows[i] = read(&stats[i])
			times[i] = append(times[i], time.Since(t0))
			restore()
		}
	}
	for i, a := range narrowBenchArms {
		if rows[i] != rows[0] {
			b.Fatalf("arm %s returned %d rows, base %d", a.name, rows[i], rows[0])
		}
		sort.Slice(times[i], func(x, y int) bool { return times[i][x] < times[i][y] })
		b.ReportMetric(float64(times[i][len(times[i])/2].Microseconds())/1000, a.name+"-ms")
		b.Logf("%-8s rows=%d decided=%d conjRejected=%d narrow=%d reassembled=%d", a.name, rows[i],
			stats[i].RecordsColumnDecided, stats[i].RecordsConjunctRejected, stats[i].RecordsNarrowMatched,
			stats[i].RecordsReassembled)
	}
}

// BenchmarkEpochNarrowMatch runs the epoch query through QueryRawProjected with and without
// chaseRefs, plus two regression shapes -- a query the columns decide outright and a whole-table
// projected scan -- each under every arm, interleaved, reporting medians.
//
//	go test -run '^$' -bench BenchmarkEpochNarrowMatch -benchtime 1x
//
// EPOCH_BENCH_CPUPROFILE=path profiles the "both" arm of the epoch query alone (run with
// GOMAXPROCS=1: on darwin the profiler misattributes samples across threads).
func BenchmarkEpochNarrowMatch(b *testing.B) {
	n := epochBenchN()
	a := epochBenchArchive(b, n)
	c := a.c
	b.Logf("%d ads, %d segments, %d columnarized, schema %d fields", c.Len(), c.segmentCountForTest(),
		columnarSegmentsForTest(c), len(c.schemaScan.Load().schema.fields))
	q := epochBenchQuery(b, n)
	if path := os.Getenv("EPOCH_BENCH_CPUPROFILE"); path != "" {
		f, err := os.Create(path)
		if err != nil {
			b.Fatal(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			b.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			for range c.QueryRawProjectedStats(q, epochBenchProjection, false, false, nil) {
			}
		}
		pprof.StopCPUProfile()
		f.Close()
		return
	}
	columnar, err := vm.Parse(fmt.Sprintf(`EpochWriteDate >= %d && NumShadowStarts == 2`, epochBenchBaseT+(n/2)*7))
	if err != nil {
		b.Fatal(err)
	}
	all, err := vm.Parse("true")
	if err != nil {
		b.Fatal(err)
	}
	const rounds = 9
	for _, tc := range []struct {
		name  string
		q     *vm.Query
		chase bool
	}{
		{"epoch/chaseRefs=false", q, false},
		{"epoch/chaseRefs=true", q, true},
		{"columnar", columnar, false},
		{"wholetable", all, false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				runNarrowBenchArms(b, rounds, func(st *ScanStats) int {
					rows := 0
					for range c.QueryRawProjectedStats(tc.q, epochBenchProjection, tc.chase, false, st) {
						rows++
					}
					return rows
				})
			}
		})
	}
}
