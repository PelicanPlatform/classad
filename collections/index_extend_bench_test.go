package collections

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// benchQueueAd is a job ad of roughly a real queue's width: ~100 attributes, a few of them indexed.
func benchQueueAd(i int, now int64, r *rand.Rand) *classad.ClassAd {
	a := classad.New()
	a.InsertAttr("ClusterId", int64(1000000+i/10))
	a.InsertAttr("ProcId", int64(i%10))
	a.InsertAttrString("Owner", fmt.Sprintf("user%d", i%97))
	st := int64(1)
	if r.Intn(3) == 0 {
		st = 5
	}
	a.InsertAttr("JobStatus", st)
	a.InsertAttr("QDate", now-3*86400+int64(r.Intn(2*86400)))
	a.InsertAttr("EnteredCurrentStatus", now-2*86400+int64(r.Intn(86400)))
	for k := 0; k < 80; k++ {
		a.InsertAttr(fmt.Sprintf("NumAttr%02d", k), int64(r.Intn(100000)))
	}
	for k := 0; k < 20; k++ {
		a.InsertAttrString(fmt.Sprintf("StrAttr%02d", k), fmt.Sprintf("/home/user%d/path/value_%d_%d", i%97, k, r.Intn(50)))
	}
	return a
}

// BenchmarkIndexedReadAfterWrites is a job-queue mirror some time after its last Reindex: every
// job loaded and indexed, then patched in rounds (delta records, about a tenth of them changing
// JobStatus and EnteredCurrentStatus) with no Reindex in between. Each sub-benchmark is one
// indexed read shape against it.
func BenchmarkIndexedReadAfterWrites(b *testing.B) {
	c, err := Open(Options{Dir: b.TempDir(), Shards: 16, DeltaMax: 16,
		ValueAttrs:       []string{"JobStatus", "EnteredCurrentStatus", "QDate", "CompletionDate", "ClusterId"},
		CategoricalAttrs: []string{"Owner"}})
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	r := rand.New(rand.NewSource(1))
	const now, n = int64(1_800_000_000), 30000
	key := func(j int) []byte { return []byte(fmt.Sprintf("%d.%d", 1000000+j/10, j%10)) }
	for i := 0; i < n; i += 500 {
		var batch []AdUpdate
		for j := i; j < i+500; j++ {
			batch = append(batch, AdUpdate{Key: key(j), Ad: benchQueueAd(j, now, r)})
		}
		if err := c.Update(batch); err != nil {
			b.Fatal(err)
		}
	}
	c.Reindex()
	for round := 0; round < 6; round++ {
		w := c.Begin()
		for j := 0; j < n; j++ {
			if r.Intn(100) >= 15 {
				continue
			}
			p := classad.New()
			p.InsertAttr("NumAttr00", int64(r.Intn(100000)))
			p.InsertAttr("NumAttr01", now)
			if r.Intn(10) == 0 {
				p.InsertAttr("JobStatus", 2)
				p.InsertAttr("EnteredCurrentStatus", now-int64(r.Intn(7200)))
			}
			w.PatchAttrs(key(j), p, nil)
		}
		w.Commit()
	}
	for _, s := range []struct{ name, expr string }{
		{"empty", "ClusterId == -5"},
		{"range-and-eq", fmt.Sprintf("EnteredCurrentStatus >= %d && JobStatus == 2", now-3600)},
		{"range", fmt.Sprintf("QDate >= %d", now-3600)},
		{"point", "ClusterId == 1001234"},
	} {
		q, err := vm.Parse(s.expr)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(s.name, func(b *testing.B) {
			var st ScanStats
			for i := 0; i < b.N; i++ {
				st = ScanStats{}
				for range c.QueryRawWireStats(q, []string{"ClusterId"}, false, &st) {
				}
			}
			b.ReportMetric(float64(st.RecordsVisited), "visited/op")
		})
	}
}
