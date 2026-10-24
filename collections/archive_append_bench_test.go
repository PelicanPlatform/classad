package collections

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// Archive ingest benchmark: history ads appended one at a time, the way a history tailer
// feeds an archive.
//
//	go test -run '^$' -bench BenchmarkArchiveAppend -benchtime 20000x .
//	CLASSAD_HISTORY_FILE=/path/to/history go test -run '^$' -bench BenchmarkArchiveAppend .
//
// CLASSAD_HISTORY_FILE names a condor history file (records ended by a "***" banner line);
// without it the ads are synthesized from a history-shaped template.

// historyBenchAds returns the ads to append: the records of CLASSAD_HISTORY_FILE, or n
// synthesized history-shaped ads.
func historyBenchAds(tb testing.TB, n int) []*classad.ClassAd {
	tb.Helper()
	var ads []*classad.ClassAd
	if path := os.Getenv("CLASSAD_HISTORY_FILE"); path != "" {
		ads = readHistoryFile(tb, path)
	} else {
		for i := 0; i < n; i++ {
			ads = append(ads, syntheticHistoryAd(tb, i))
		}
	}
	for _, ad := range ads {
		_ = ad.AST() // sort once, outside the timed loop
	}
	return ads
}

// readHistoryFile parses a condor history file: each record's attribute lines end at a
// banner line beginning "***".
func readHistoryFile(tb testing.TB, path string) []*classad.ClassAd {
	tb.Helper()
	f, err := os.Open(path) //nolint:gosec // benchmark input named by the operator
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	var ads []*classad.ClassAd
	var rec bytes.Buffer
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.HasPrefix(line, []byte("***")) {
			if ad, perr := classad.ParseOld(rec.String()); perr == nil {
				ads = append(ads, ad)
			}
			rec.Reset()
			continue
		}
		rec.Write(line)
		rec.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		tb.Fatal(err)
	}
	if len(ads) == 0 {
		tb.Fatalf("%s: no history records", path)
	}
	return ads
}

// syntheticHistoryAd builds a completed-job ad of roughly the size and shape of a real
// history record (~120 attributes, ~3.5 KB of text).
func syntheticHistoryAd(tb testing.TB, i int) *classad.ClassAd {
	tb.Helper()
	var b strings.Builder
	owner := fmt.Sprintf("user%d", i%37)
	cluster, proc := 13177000+i/100, i%100
	completion := 1766900000 + i*7
	fmt.Fprintf(&b, "ClusterId = %d\nProcId = %d\nOwner = %q\nUser = \"%s@ap40.uw.osg-htc.org\"\n", cluster, proc, owner, owner)
	fmt.Fprintf(&b, "AccountingGroup = \"group_opportunistic.proj%d.%s\"\nAcctGroup = \"proj%d\"\n", i%11, owner, i%11)
	fmt.Fprintf(&b, "GlobalJobId = \"ap40.uw.osg-htc.org#%d.%d#%d\"\n", cluster, proc, completion-86400)
	fmt.Fprintf(&b, "CompletionDate = %d\nEnteredCurrentStatus = %d\nJobStatus = 4\nQDate = %d\n", completion, completion, completion-86400)
	fmt.Fprintf(&b, "JobCurrentStartDate = %d\nJobStartDate = %d\nFirstJobMatchDate = %d\n", completion-2500, completion-2500, completion-3000)
	fmt.Fprintf(&b, "RemoteWallClockTime = %d.0\nCommittedTime = %d\nCumulativeSlotTime = %d\n", 2500+i%900, 2500+i%900, 10000+i%3600)
	fmt.Fprintf(&b, "RemoteUserCpu = %d.0\nRemoteSysCpu = %d.0\nCpusUsage = %.6f\n", 9000+i%500, 40+i%9, 3.9+float64(i%10)/100)
	fmt.Fprintf(&b, "ImageSize = %d\nImageSize_RAW = %d\nDiskUsage = %d\nDiskUsage_RAW = %d\n", 10000000, 8479772+i%4096, 7500000, 5121548+i%4096)
	fmt.Fprintf(&b, "MemoryUsage = %d\nResidentSetSize = %d\nBytesSent = %d\nBytesRecvd = %d\n", 2048+i%512, 2000000+i%65536, 330832+i%9999, 7884+i%999)
	fmt.Fprintf(&b, "Cmd = \"/home/%s/project/sanitized/%016x\"\nArgs = \"/home/%s/project/in/%016x\"\n", owner, i*7919, owner, i*104729)
	fmt.Fprintf(&b, "Err = \"logs/%d_%d.err\"\nOut = \"logs/%d_%d.out\"\nUserLog = \"/home/%s/logs/%d.log\"\n", cluster, proc, cluster, proc, owner, cluster)
	fmt.Fprintf(&b, "LastRemoteHost = \"slot1_%d@glidein_%d_%d@node%d.example.edu\"\n", i%8, i%9973, i%7919, i%613)
	fmt.Fprintf(&b, "MATCH_EXP_JOB_GLIDEIN_Site = \"Site%d\"\nMATCH_EXP_JOB_GLIDEIN_ResourceName = \"Resource%d\"\n", i%53, i%131)
	fmt.Fprintf(&b, "ExitCode = %d\nExitBySignal = false\nNumJobStarts = %d\nNumShadowStarts = %d\n", i%3/2, 1+i%2, 1+i%2)
	for k := 0; k < 90; k++ {
		switch k % 4 {
		case 0:
			fmt.Fprintf(&b, "HistAttrInt%02d = %d\n", k, (i*31+k*17)%100000)
		case 1:
			fmt.Fprintf(&b, "HistAttrStr%02d = \"value-%d-%d\"\n", k, k, i%(k+3))
		case 2:
			fmt.Fprintf(&b, "HistAttrBool%02d = %t\n", k, (i+k)%3 == 0)
		default:
			fmt.Fprintf(&b, "HistAttrExpr%02d = (RequestMemory > %d) && (TARGET.Arch == \"X86_64\")\n", k, k*64)
		}
	}
	ad, err := classad.ParseOld(b.String())
	if err != nil {
		tb.Fatal(err)
	}
	return ad
}

// BenchmarkArchiveAppend appends b.N history ads to a fresh on-disk archive, one Append
// call per ad. It reports appends/s.
func BenchmarkArchiveAppend(b *testing.B) {
	ads := historyBenchAds(b, 2000)
	opts := ArchiveOptions{ZoneAttrs: []string{"CompletionDate"}}
	b.Run("durable", func(b *testing.B) {
		opts.Dir = b.TempDir()
		a, err := CreateArchive(opts)
		if err != nil {
			b.Fatal(err)
		}
		defer a.Close()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := a.Append(ads[i%len(ads)]); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "appends/s")
	})
	// deferred: AppendDeferred with a Flush every flushEvery appends, as a tailer that
	// checkpoints its position after each chunk it reads.
	const flushEvery = 1000
	b.Run("deferred", func(b *testing.B) {
		opts.Dir = b.TempDir()
		a, err := CreateArchive(opts)
		if err != nil {
			b.Fatal(err)
		}
		defer a.Close()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := a.AppendDeferred(ads[i%len(ads)]); err != nil {
				b.Fatal(err)
			}
			if (i+1)%flushEvery == 0 {
				if err := a.Flush(); err != nil {
					b.Fatal(err)
				}
			}
		}
		if err := a.Flush(); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "appends/s")
	})
}
