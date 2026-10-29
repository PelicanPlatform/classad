package collections

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// BenchmarkRawProjectedColumnarArchive measures the raw projected (old-ClassAd text) read of a
// history archive with and without the projection plan: the old arm reassembles every matched
// record and hands it to the projector (refRawProjected), the new arm builds each column-decided
// record from the projected columns (QueryRawProjectedStats).
//
// The archive is shaped like htcondordb's history table: 50k history-shaped ads (~120
// attributes each), ZSTD, inline-keyed sealed segments, columnarized, zone maps on the
// completion time. The constraint is a recent-history window the columns decide, matching half
// the archive. The two arms run interleaved, alternating which goes first each round, and the
// medians are reported as old-ms and new-ms.
//
//	go test -run '^$' -bench BenchmarkRawProjectedColumnarArchive -benchtime 1x
func BenchmarkRawProjectedColumnarArchive(b *testing.B) {
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
	b.Logf("%d ads, %d segments, %d columnarized", c.Len(), c.segmentCountForTest(), columnarSegmentsForTest(c))

	q, err := vm.Parse(fmt.Sprintf("CompletionDate >= %d", 1766900000+(ads/2)*7))
	if err != nil {
		b.Fatal(err)
	}
	one := []string{"ClusterId"}
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
	}{{"1attr", one}, {"48attr", wide}} {
		for _, chase := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/chaseRefs=%v", proj.name, chase), func(b *testing.B) {
				var oldSt, newSt ScanStats
				oldArm := func() int {
					oldSt = ScanStats{}
					n := 0
					for range refRawProjected(c, q, proj.attrs, chase, false, &oldSt) {
						n++
					}
					return n
				}
				newArm := func() int {
					newSt = ScanStats{}
					n := 0
					for range c.QueryRawProjectedStats(q, proj.attrs, chase, false, &newSt) {
						n++
					}
					return n
				}
				const rounds = 9
				var olds, news []time.Duration
				for i := 0; i < b.N; i++ {
					olds, news = olds[:0], news[:0]
					for r := 0; r < rounds; r++ {
						arms := []func() int{oldArm, newArm}
						times := []*[]time.Duration{&olds, &news}
						order := []int{0, 1}
						if r%2 == 1 {
							order = []int{1, 0}
						}
						var rows [2]int
						for _, k := range order {
							t0 := time.Now()
							rows[k] = arms[k]()
							*times[k] = append(*times[k], time.Since(t0))
						}
						if rows[0] != rows[1] || rows[0] == 0 {
							b.Fatalf("arms disagree or match nothing: old %d rows, new %d", rows[0], rows[1])
						}
					}
				}
				slices.Sort(olds)
				slices.Sort(news)
				b.ReportMetric(float64(olds[len(olds)/2].Microseconds())/1000, "old-ms")
				b.ReportMetric(float64(news[len(news)/2].Microseconds())/1000, "new-ms")
				b.ReportMetric(float64(oldSt.RecordsReassembled), "old-reassembled")
				b.ReportMetric(float64(newSt.RecordsReassembled), "new-reassembled")
			})
		}
	}
}
