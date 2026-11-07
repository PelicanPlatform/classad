package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// What does intersecting two single-attribute posting lists cost, against one composite
// lookup over the (ClusterId, ProcId) tuple? The same job-queue data is loaded twice --
// once with ClusterId and ProcId indexed separately ("intersect"), once with a composite
// over the pair as well ("composite") -- and the same `ClusterId == c && ProcId == p`
// lookup runs against both.
//
//	go test ./collections/ -run '^$' -bench Composite -benchtime 200x
//
// The shapes bracket the two ways the intersection is expensive. In "narrow" the ProcId
// posting is so much wider than the ClusterId one that the planner drops it (worthProbing)
// and hands the scan every record of the cluster to re-verify. In "mid" and "wide" both
// probes run, and a posting of hundreds or thousands of offsets is merged to keep one.
func benchComposite(b *testing.B, clusters, procs int) {
	build := func(composite bool) *Collection {
		codec, err := NewZSTDCodec(nil)
		if err != nil {
			b.Fatal(err)
		}
		opts := Options{Shards: 8, Codec: codec, ValueAttrs: []string{"ClusterId", "ProcId"}}
		if composite {
			opts.CompositeAttrs = [][]string{{"ClusterId", "ProcId"}}
		}
		c := New(opts)
		for cl := range clusters {
			for p := range procs {
				ad, perr := classad.ParseOld(fmt.Sprintf(
					"ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\nJobStatus = %d\n"+
						"Cmd = \"/home/u/run.sh\"\nRequestMemory = 2048\nRequestCpus = 1\n"+
						"Args = \"--seed %d --input /data/set%d\"\nIwd = \"/home/u%d/work\"\n"+
						"QDate = %d\nRemoteWallClockTime = %d.0\n",
					cl, p, cl%97, 1+(cl+p)%5, cl*p, p%17, cl%97, 1700000000+cl, 300+p))
				if perr != nil {
					b.Fatal(perr)
				}
				if putErr := c.Put([]byte(fmt.Sprintf("%d.%d", cl, p)), ad); putErr != nil {
					b.Fatal(putErr)
				}
			}
		}
		c.Reindex()
		return c
	}
	plain, comp := build(false), build(true)
	q, err := vm.Parse(fmt.Sprintf("ClusterId == %d && ProcId == %d", clusters/2, procs/2))
	if err != nil {
		b.Fatal(err)
	}
	for _, arm := range []struct {
		name string
		c    *Collection
	}{{"intersect", plain}, {"composite", comp}} {
		var st ScanStats
		rows := 0
		for range arm.c.QueryProjectStats(q, []string{"ClusterId"}, &st) {
			rows++
		}
		if rows != 1 {
			b.Fatalf("%s: %d rows, want 1", arm.name, rows)
		}
		b.Logf("%-10s visited=%d candidates=%d  %s", arm.name, st.RecordsVisited, st.IndexCandidates, arm.c.ExplainQuery(q).Reason)
		b.Run(arm.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				got := 0
				for range arm.c.QueryProject(q, []string{"ClusterId"}) {
					got++
				}
				if got != 1 {
					b.Fatalf("%d rows", got)
				}
			}
		})
	}
}

func BenchmarkCompositeNarrowCluster(b *testing.B) { benchComposite(b, 8000, 10) }
func BenchmarkCompositeMidCluster(b *testing.B)    { benchComposite(b, 2000, 40) }
func BenchmarkCompositeWideCluster(b *testing.B)   { benchComposite(b, 100, 800) }
