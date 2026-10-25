package collections

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// BenchmarkSeqReadPaged times one page of the paged read against the walk it replaced
// (refQueryRawFromSeqYield) on a jobs-table shape: mutable, persistent, indexed on ClusterId,
// JobStatus and Owner, encrypted at rest, churned by patches into delta records and superseded versions, then
// columnarized. The two arms run interleaved, alternating which goes first each round, and the
// medians are reported as old-ns/page and new-ns/page.
//
//	go test -run '^$' -bench BenchmarkSeqReadPaged -benchtime 1x
func BenchmarkSeqReadPaged(b *testing.B) {
	const ads = 50000
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 3)
	}
	c, err := Open(Options{Dir: b.TempDir(), Shards: 8, SegmentSize: 1 << 19, DeltaMax: 8, GroupSchemaCount: -1,
		DataKey: key, EncryptedAttrs: []string{"Wide"},
		ValueAttrs: []string{"ClusterId", "JobStatus"}, CategoricalAttrs: []string{"Owner"}})
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	rng := rand.New(rand.NewSource(1))
	now := time.Now().Unix()
	put := func(lo, hi int) {
		tx := c.Begin()
		for i := lo; i < hi; i++ {
			tx.Put([]byte(fmt.Sprintf("%d.0", i)), seqFixtureAd(i, now, rng))
		}
		if r := tx.Commit(); r.Conflicted() {
			b.Fatal("seed commit conflicted")
		}
	}
	for i := 0; i < ads; i += 500 {
		put(i, i+500)
	}
	// Churn: every job's status and usage change a few times, as a queue mirror sees them.
	for round := 0; round < 4; round++ {
		for i := 0; i < ads; i += 500 {
			tx := c.Begin()
			for j := i; j < i+500; j++ {
				patch := classad.New()
				patch.InsertAttr("JobStatus", int64(1+rng.Intn(5)))
				patch.InsertAttr("RemoteUserCpu", int64(rng.Intn(100000)))
				tx.PatchAttrs([]byte(fmt.Sprintf("%d.0", j)), patch, nil)
			}
			tx.Commit()
		}
	}
	c.Reindex()
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		b.Fatal("no schema")
	}
	c.ColumnarizeSealed()
	c.Reindex()
	b.Logf("%d ads, %d segments, %d columnarized", c.Len(), c.segmentCountForTest(), columnarSegmentsForTest(c))

	proj := []string{"ClusterId", "ProcId", "Owner", "JobStatus", "QDate", "RequestMemory", "Cmd"}
	mustQ := func(e string) *vm.Query {
		if e == "" {
			return nil
		}
		q, err := vm.Parse(e)
		if err != nil {
			b.Fatal(err)
		}
		return q
	}
	// A cursor halfway through the order, for the mid-table pages. The order is the same with or
	// without a filter, so one cursor serves every scenario.
	mid := func() SeqCursor {
		seq, page := c.QueryRawFromSeq(nil, proj, SeqCursor{}, ads/2)
		for range seq {
		}
		return page.Next
	}
	type scenario struct {
		name  string
		expr  string
		limit int
		mid   bool
	}
	for _, sc := range []scenario{
		{"ClusterId-eq", `ClusterId == 31337`, 50, false},
		{"JobStatus-eq/first", `JobStatus == 2`, 50, false},
		{"JobStatus-eq/mid", `JobStatus == 2`, 50, true},
		{"Owner-eq", `Owner == "user3"`, 50, false},
		{"unfiltered/first", ``, 50, false},
		{"unfiltered/mid", ``, 50, true},
		{"RequestMemory-gt/first", `RequestMemory > 8192`, 50, false},
		{"ClusterId-range/all", `ClusterId >= 1000 && ClusterId < 1200`, 0, false},
	} {
		b.Run(sc.name, func(b *testing.B) {
			q := mustQ(sc.expr)
			var after SeqCursor
			if sc.mid {
				after = mid()
			}
			oldArm := func() int {
				n := 0
				refQueryRawFromSeqYield(c, q, proj, after, sc.limit, func(RawAd) bool { n++; return true })
				return n
			}
			newArm := func() int {
				n := 0
				seq, _ := c.QueryRawFromSeq(q, proj, after, sc.limit)
				for range seq {
					n++
				}
				return n
			}
			const rounds = 15
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
					for _, a := range order {
						t0 := time.Now()
						rows[a] = arms[a]()
						*times[a] = append(*times[a], time.Since(t0))
					}
					if rows[0] != rows[1] {
						b.Fatalf("arms disagree: old %d rows, new %d", rows[0], rows[1])
					}
				}
			}
			slices.Sort(olds)
			slices.Sort(news)
			b.ReportMetric(float64(olds[len(olds)/2].Nanoseconds()), "old-ns/page")
			b.ReportMetric(float64(news[len(news)/2].Nanoseconds()), "new-ns/page")
		})
	}
}
