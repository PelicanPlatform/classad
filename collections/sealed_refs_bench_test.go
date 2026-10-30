package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// BenchmarkSealedAttrReads times the two reads a sealed attribute used to push off the columnar
// path: a reference-chasing wire-row projection naming one, and a query filtering on one (matched
// on the narrowed record, then projected from the columns). The fixture is an encrypted store whose
// sealed segments are columnarized, with Owner and Token sealed.
func BenchmarkSealedAttrReads(b *testing.B) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 5)
	}
	c, err := Open(Options{Dir: b.TempDir(), Shards: 4, SegmentSize: 1 << 16,
		DataKey: key, EncryptedAttrs: []string{"Owner", "Token"}})
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	const n = 20000
	put := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			ad := fmt.Sprintf(`[ MyType="Job"; TargetType="Machine"; ClusterId=%d; ProcId=%d; JobStatus=%d;`+
				` Owner="user%d"; Token="tok%d"; RequestMemory=%d; RequestDisk=%d; ResidentSetSize=%d;`+
				` Requirements = (TARGET.Memory >= RequestMemory); Cmd="/bin/job%d"; Iwd="/home/user%d/run%d" ]`,
				i, i%10, 1+i%5, i%7, i, (1+i%8)*1024, (1+i%4)*2048, (1+i%6)*512, i%13, i%7, i%100)
			if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), mustAd(b, ad)); err != nil {
				b.Fatal(err)
			}
		}
	}
	put(0, n)
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		b.Fatal("no columnar schema derived")
	}
	c.ColumnarizeSealed()
	put(n, n+500) // an active tail
	if columnarSegmentsForTest(c) == 0 {
		b.Fatal("fixture columnarized nothing")
	}

	run := func(b *testing.B, read func(st *ScanStats) int) {
		var st ScanStats
		rows := read(&st)
		if rows == 0 {
			b.Fatal("no rows")
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			read(nil)
		}
		b.StopTimer()
		b.ReportMetric(float64(rows), "rows")
		b.Logf("decided=%d narrow=%d reassembled=%d closureReassembled=%d", st.RecordsColumnDecided,
			st.RecordsNarrowMatched, st.RecordsReassembled, st.RecordsClosureReassembled)
	}
	b.Run("wire-rows", func(b *testing.B) {
		q, err := vm.Parse("ProcId < 4")
		if err != nil {
			b.Fatal(err)
		}
		proj := []string{"ClusterId", "ProcId", "JobStatus", "Owner"}
		run(b, func(st *ScanStats) int {
			rows := 0
			for range c.QueryRawWireRefsStats(q, proj, false, st) {
				rows++
			}
			return rows
		})
	})
	b.Run("narrowed-match", func(b *testing.B) {
		q, err := vm.Parse(`Owner == "user3" && ProcId < 4`)
		if err != nil {
			b.Fatal(err)
		}
		proj := []string{"ClusterId"}
		run(b, func(st *ScanStats) int {
			rows := 0
			for range c.QueryRawWireStats(q, proj, false, st) {
				rows++
			}
			return rows
		})
	})
}
