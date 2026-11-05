package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// WHAT WALKING THE PAIR LOOP IN PARALLEL IS WORTH.
//
// Both arms run the SAME join over the SAME catalog and differ only in whether the walk may
// split, so the difference is the walk and not the corpus. Row counts are compared before
// anything is timed: a faster arm that returns fewer rows is faster for the wrong reason.
//
// The fixture is the shape this was built for -- a completed-job archive joined to its run
// attempts, both columnarized, with ads at the width a real job ad has. A narrow ad or an
// uncolumnarized table measures the read instead, and the read was already parallel.
//
// MEASURED on an M2 Pro, 20k jobs x 4 attempts, -benchtime=10x -count=5, arms alternated:
//
//	serial walk (reads still parallel)   median 127.7 ms
//	parallel walk                        median  80.4 ms   (1.6x)
//	serial everything (-cpu=1)                  241.3 ms
//
// The ceiling for this shape -- P independent joins over pre-partitioned data, paying nothing
// to partition and with 1/P-sized hash tables -- measured 62.7 ms, so this keeps about three
// quarters of what was available. What it gives up to the ceiling is the price of preserving
// scan order (a lane per walker, drained in the dealing rotation) and of copying each emitted
// row out of the walker's buffer.
func BenchmarkJoinParallelWalk(b *testing.B) {
	const jobs = 20000
	cat := wideJoinFixture(b, jobs)
	spec := wideJoinSpec()

	count := func(serial bool) int {
		forceSerialJoinWalk = serial
		defer func() { forceSerialJoinWalk = false }()
		var st JoinStats
		seq, err := cat.JoinWire(spec, JoinLimits{}, false, &st)
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for range seq {
			n++
		}
		if st.Err != nil {
			b.Fatal(st.Err)
		}
		return n
	}
	before := joinParallelWalks.Load()
	par, ser := count(false), count(true)
	if par != ser || par != jobs*4 {
		b.Fatalf("parallel joined %d rows, serial %d, want %d", par, ser, jobs*4)
	}
	// Declining to walk in parallel is SILENT -- the serial walk answers identically -- so
	// without this the two arms could both be the serial path and the benchmark would report
	// their difference as noise and call it a result.
	if joinParallelWalks.Load() == before {
		b.Fatal("the join did not walk in parallel; both arms would be the serial walk")
	}

	// The arms alternate order round to round, because a Go benchmark's first arm warms the
	// page cache and the allocator for the second and a fixed order hands that advantage to
	// whichever runs second. Measured here: with parallel always first it read 1.08x, and
	// alternating it reads what it actually is.
	type arm struct {
		name   string
		serial bool
	}
	rounds := [][]arm{
		{{"parallel", false}, {"serial", true}},
		{{"serial", true}, {"parallel", false}},
	}
	for r, round := range rounds {
		for _, a := range round {
			b.Run(fmt.Sprintf("r%d/%s", r, a.name), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					count(a.serial)
				}
			})
		}
	}
}

// wideJoinFixture is a completed-job archive and its run-attempt archive, both columnarized,
// with ads at the width a real job ad carries.
func wideJoinFixture(tb testing.TB, jobs int) *Catalog {
	tb.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: tb.TempDir()})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 20})
	if err != nil {
		tb.Fatal(err)
	}
	ep, err := cat.CreateArchiveTable("epoch_history", ArchiveConfig{SegmentSize: 1 << 20})
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < jobs; i++ {
		var sb strings.Builder
		fmt.Fprintf(&sb, `[ ClusterId = %d; ProcId = 0; Owner = "user%02d"; JobStatus = 4; RequestMemory = %d`,
			i, i%32, 1024+(i%8)*512)
		for k := 0; k < 75; k++ {
			fmt.Fprintf(&sb, `; HAttr%02d = "a completed job attribute value"`, k)
		}
		sb.WriteString(" ]")
		ad, perr := classad.Parse(sb.String())
		if perr != nil {
			tb.Fatal(perr)
		}
		if aerr := hist.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
		for e := 0; e < 4; e++ {
			var eb strings.Builder
			fmt.Fprintf(&eb, `[ ClusterId = %d; NumShadowStarts = %d; RequestMemory = %d; MemoryUsage = %d`,
				i, e+1, 1024+(i%8)*512, 512+(e*64))
			for k := 0; k < 21; k++ {
				fmt.Fprintf(&eb, `; EAttr%02d = "a run attempt attribute"`, k)
			}
			eb.WriteString(" ]")
			ead, perr := classad.Parse(eb.String())
			if perr != nil {
				tb.Fatal(perr)
			}
			if aerr := ep.Append(ead); aerr != nil {
				tb.Fatal(aerr)
			}
		}
	}
	// Asserted, not assumed: with nothing sealed this declines and both arms would measure
	// the record path while the comment said columnar.
	if !hist.BuildAndEnableSchemaScan(8000, 16) || !ep.BuildAndEnableSchemaScan(8000, 16) {
		tb.Skip("no sealed segments to columnarize")
	}
	return cat
}

func wideJoinSpec() JoinSpec {
	return JoinSpec{
		Kind:  JoinInner,
		Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId", "Owner", "RequestMemory"}},
		Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts", "RequestMemory", "MemoryUsage"}},
		Cond:  []string{"h.ClusterId == e.ClusterId"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
}
