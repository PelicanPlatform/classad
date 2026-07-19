package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// wideJobAd builds a realistic-width job ad (~25 attributes) with RequestCpus in
// a small range, mirroring the user's GROUP BY RequestCpus query.
func wideJobAd(i int) *classad.ClassAd {
	cpus := (i % 16) + 1
	text := fmt.Sprintf(`ClusterId = %d
ProcId = 0
Owner = "user%d"
RequestCpus = %d
RequestMemory = %d
RequestDisk = %d
JobStatus = %d
JobUniverse = 5
JobPrio = %d
ImageSize = %d
DiskUsage = %d
NumJobStarts = %d
Cmd = "/bin/sleep"
Args = "3600"
Iwd = "/home/user%d"
QDate = %d
EnteredCurrentStatus = %d
JobCurrentStartDate = %d
RemoteWallClockTime = %d.0
BytesSent = %d.0
BytesRecvd = %d.0
Requirements = (RequestCpus >= 1) && (Memory >= RequestMemory)
Rank = 0.0
NiceUser = false
WantCheckpoint = false`,
		i, i%1000, cpus, cpus*2048, 1024, (i%5)+1, i%20, cpus*512,
		cpus*256, i%3, i%1000, 1600000000+i, 1600000000+i, 1600000000+i,
		i%10000, i*17%1000000, i*13%1000000)
	ad, err := classad.ParseOld(text)
	if err != nil {
		panic(err)
	}
	return ad
}

func benchDB(b *testing.B, n int) *db.DB {
	b.Helper()
	d, err := db.Open("")
	if err != nil {
		b.Fatal(err)
	}
	tx := d.Begin()
	for i := 0; i < n; i++ {
		tx.NewClassAd(fmt.Sprintf("%d.0", i), wideJobAd(i))
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return d
}

const benchN = 50000

// BenchmarkAggregateGroupBy measures the full client path for the user's query:
// SELECT COUNT(*), RequestCpus WHERE RequestCpus >= 1 GROUP BY RequestCpus.
func BenchmarkAggregateGroupBy(b *testing.B) {
	d := benchDB(b, benchN)
	defer d.Close()
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close() }()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := c.Aggregate(context.Background(), "RequestCpus >= 1", []string{"RequestCpus"},
			[]AggSpec{{Func: AggCount, Arg: "*"}})
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 16 {
			b.Fatalf("groups = %d, want 16", len(rows))
		}
	}
}

// BenchmarkQueryDecodeOnly isolates the scan+decode cost the aggregate pays: it
// iterates db.Query (which fully decodes every matching ad to a *classad.ClassAd)
// and reads just the group attribute -- no RPC, no hashing.
func BenchmarkQueryDecodeOnly(b *testing.B) {
	d := benchDB(b, benchN)
	defer d.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		seq, err := d.Query("RequestCpus >= 1")
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for ad := range seq {
			_ = ad.EvaluateAttr("RequestCpus")
			n++
		}
		if n != benchN {
			b.Fatalf("scanned %d, want %d", n, benchN)
		}
	}
}

// BenchmarkQueryLimit1 vs BenchmarkQueryAll: with matches present, a pushed-down
// LIMIT 1 stops the scan at the first match instead of scanning every ad.
func BenchmarkQueryLimit1(b *testing.B) {
	d := benchDB(b, benchN)
	defer d.Close()
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close() }()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := c.QueryLimit(context.Background(), "RequestCpus >= 1", 1) // matches every ad
		if err != nil || len(rows) != 1 {
			b.Fatalf("rows=%d err=%v, want 1", len(rows), err)
		}
	}
}

func BenchmarkQueryAll(b *testing.B) {
	d := benchDB(b, benchN)
	defer d.Close()
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close() }()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := c.QueryLimit(context.Background(), "RequestCpus >= 1", 0)
		if err != nil || len(rows) != benchN {
			b.Fatalf("rows=%d err=%v, want %d", len(rows), err, benchN)
		}
	}
}

// BenchmarkQueryScanNoDecode measures the constraint scan without the caller
// touching the yielded ad, to separate the match/scan cost from the per-ad
// decode the aggregate forces by yielding a *classad.ClassAd.
func BenchmarkQueryScanNoDecode(b *testing.B) {
	d := benchDB(b, benchN)
	defer d.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		seq, err := d.Query("RequestCpus >= 1")
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for range seq {
			n++
		}
		if n != benchN {
			b.Fatalf("scanned %d, want %d", n, benchN)
		}
	}
}
