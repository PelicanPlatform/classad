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
// (refQueryRawFromSeqYield) on a jobs-table shape: mutable, persistent, 16 shards of many small
// segments, indexed on ClusterId, JobStatus and Owner, encrypted at rest, churned by small commits of
// patches to random jobs into delta records and superseded versions, columnarized, then closed and
// reopened as a restarted server is. The two arms run interleaved, alternating which goes first each
// round, and the medians are reported as old-ns/page and new-ns/page. Pages start at the beginning,
// partway into a shard, and near the end.
//
//	go test -run '^$' -bench BenchmarkSeqReadPaged -benchtime 1x
func BenchmarkSeqReadPaged(b *testing.B) {
	const ads = 50000
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 3)
	}
	opts := Options{Dir: b.TempDir(), Shards: 16, SegmentSize: 1 << 17, DeltaMax: 8, GroupSchemaCount: -1,
		DataKey: key, EncryptedAttrs: []string{"Wide"},
		ValueAttrs: []string{"ClusterId", "JobStatus"}, CategoricalAttrs: []string{"Owner"}}
	c, err := Open(opts)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { c.Close() }() // c is replaced by the reopen below
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
	// Churn: jobs' status and usage change a few times each, in small commits touching random jobs,
	// as a queue mirror sees them.
	for range 2000 {
		tx := c.Begin()
		for range 100 {
			patch := classad.New()
			patch.InsertAttr("JobStatus", int64(1+rng.Intn(5)))
			patch.InsertAttr("RemoteUserCpu", int64(rng.Intn(100000)))
			tx.PatchAttrs([]byte(fmt.Sprintf("%d.0", rng.Intn(ads))), patch, nil)
		}
		tx.Commit()
	}
	c.Reindex()
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		b.Fatal("no schema")
	}
	c.ColumnarizeSealed()
	c.Reindex()
	// A clean close and reopen restores the shards from their directory snapshots, as a restarted
	// server does.
	if err = c.Close(); err != nil {
		b.Fatal(err)
	}
	if c, err = Open(opts); err != nil {
		b.Fatal(err)
	}
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
	// The cursor after the first n records of the order. The order is the same with or without a
	// filter, so one cursor serves every scenario. The middle is offset from half the table so it
	// falls partway into a shard rather than at one's start.
	at := func(n int) SeqCursor {
		if n == 0 {
			return SeqCursor{}
		}
		seq, page := c.QueryRawFromSeq(nil, proj, SeqCursor{}, n)
		for range seq {
		}
		return page.Next
	}
	const mid, end = ads/2 + ads/40, ads - 60
	type scenario struct {
		name  string
		expr  string
		limit int
		at    int
	}
	for _, sc := range []scenario{
		{"ClusterId-eq", `ClusterId == 31337`, 50, 0},
		{"JobStatus-eq/first", `JobStatus == 2`, 50, 0},
		{"JobStatus-eq/mid", `JobStatus == 2`, 50, mid},
		{"Owner-eq", `Owner == "user3"`, 50, 0},
		{"unfiltered/first", ``, 50, 0},
		{"unfiltered/mid", ``, 50, mid},
		{"unfiltered/end", ``, 50, end},
		{"RequestMemory-gt/first", `RequestMemory > 8192`, 50, 0},
		{"ClusterId-range/all", `ClusterId >= 1000 && ClusterId < 1200`, 0, 0},
	} {
		b.Run(sc.name, func(b *testing.B) {
			q := mustQ(sc.expr)
			after := at(sc.at)
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
