package collections

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// BenchmarkColumnarProjectionWireArchive times QueryRawWire over a columnarized history archive,
// where nearly every matched record is served by the columnar projection: the read whose per-record
// cost the integrity gate (checksum, dictionary) adds to.
//
// The archive is 50k history-shaped ads (~120 attributes each), ZSTD, inline-keyed sealed segments,
// columnarized, with read verification on (the default). The constraint is decided from the
// columns and matches half the archive. Each round times the read once; the median is reported as
// ms. Compare two builds by alternating their test binaries.
//
//	go test -run '^$' -bench BenchmarkColumnarProjectionWireArchive -benchtime 1x
func BenchmarkColumnarProjectionWireArchive(b *testing.B) {
	const ads = 50000
	codec, err := NewZSTDCodec(nil)
	if err != nil {
		b.Fatal(err)
	}
	a, err := CreateArchive(ArchiveOptions{Dir: b.TempDir(), SegmentSize: 4 << 20, Codec: codec,
		ValueAttrs: []string{"ClusterId"}, CategoricalAttrs: []string{"Owner"},
		ZoneAttrs: []string{"CompletionDate"}})
	if err != nil {
		b.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < ads; i++ {
		ad := syntheticHistoryAd(b, i)
		ad.InsertAttrString("MyType", "Job")
		ad.InsertAttrString("TargetType", "Machine")
		if err = a.Append(ad); err != nil {
			b.Fatal(err)
		}
	}
	if !a.BuildAndEnableSchemaScan(4096, 16) {
		b.Fatal("no columnar schema derived")
	}
	c := a.c
	if !c.VerifyingReads() {
		b.Fatal("read verification is off")
	}
	q, err := vm.Parse(fmt.Sprintf("CompletionDate >= %d", 1766900000+(ads/2)*7))
	if err != nil {
		b.Fatal(err)
	}
	wide := []string{"ClusterId", "ProcId", "Owner", "User", "AccountingGroup", "AcctGroup", "GlobalJobId",
		"CompletionDate", "EnteredCurrentStatus", "JobStatus", "QDate", "JobCurrentStartDate", "JobStartDate",
		"FirstJobMatchDate", "RemoteWallClockTime", "CommittedTime", "CumulativeSlotTime", "RemoteUserCpu",
		"RemoteSysCpu", "CpusUsage", "ImageSize", "DiskUsage", "MemoryUsage", "ResidentSetSize", "BytesSent",
		"BytesRecvd", "Cmd", "Args", "Err", "Out", "UserLog", "LastRemoteHost", "MATCH_EXP_JOB_GLIDEIN_Site",
		"MATCH_EXP_JOB_GLIDEIN_ResourceName", "ExitCode", "ExitBySignal", "NumJobStarts", "NumShadowStarts",
		"HistAttrInt00", "HistAttrStr01", "HistAttrBool02", "HistAttrInt04", "HistAttrStr05", "HistAttrBool06",
		"HistAttrInt08", "HistAttrStr09", "HistAttrBool10", "HistAttrInt12"}
	for _, proj := range []struct {
		name  string
		attrs []string
	}{{"1attr", []string{"ClusterId"}}, {"48attr", wide}} {
		b.Run(proj.name, func(b *testing.B) {
			var st ScanStats
			read := func() int {
				st = ScanStats{}
				n := 0
				for range c.QueryRawWireStats(q, proj.attrs, false, &st) {
					n++
				}
				return n
			}
			if read() == 0 {
				b.Fatal("matched nothing")
			}
			const rounds = 15
			var times []time.Duration
			for i := 0; i < b.N; i++ {
				times = times[:0]
				for r := 0; r < rounds; r++ {
					t0 := time.Now()
					read()
					times = append(times, time.Since(t0))
				}
			}
			slices.Sort(times)
			b.ReportMetric(float64(times[len(times)/2].Microseconds())/1000, "ms")
			b.ReportMetric(float64(st.RowsMatched-st.RecordsReassembled), "projected")
		})
	}
}
