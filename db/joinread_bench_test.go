package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Where a join's time goes on the shape it is actually wanted for: history joined to
// epoch_history, to watch one job's requested memory change across its run attempts.
//
// history is wide (a real completed-job ad is ~80 attributes) and epoch_history is narrower
// but has several rows per job. The query wants a handful of attributes out of both. So the
// question this answers, before anything is built on top of it, is how much of the join is
// the CONDITION (what the previous step made cheaper) and how much is just reading rows that
// the query never asked for.

const (
	memHistWidth  = 80 // attributes on a completed-job ad
	memEpochWidth = 25 // attributes on a run-attempt ad
	memEpochs     = 4  // run attempts per job
)

func memEvolveFixture(tb testing.TB, jobs int) *Catalog {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateTable("history")
	if err != nil {
		tb.Fatal(err)
	}
	epoch, err := cat.CreateTable("epoch_history")
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < jobs; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, `[ ClusterId = %d; ProcId = 0; Owner = "user%02d"; JobStatus = 4; RequestMemory = %d`,
			i, i%32, 1024+(i%8)*512)
		for k := 0; k < memHistWidth-5; k++ {
			fmt.Fprintf(&b, `; HAttr%02d = "a completed job attribute value"`, k)
		}
		b.WriteString(" ]")
		ad, perr := classad.Parse(b.String())
		if perr != nil {
			tb.Fatal(perr)
		}
		if werr := hist.Put(fmt.Sprintf("h%d", i), ad); werr != nil {
			tb.Fatal(werr)
		}
		for e := 0; e < memEpochs; e++ {
			var eb strings.Builder
			fmt.Fprintf(&eb, `[ ClusterId = %d; ProcId = 0; NumShadowStarts = %d; RequestMemory = %d; MemoryUsage = %d`,
				i, e+1, 1024+(i%8)*512+e*256, 900+e*200)
			for k := 0; k < memEpochWidth-5; k++ {
				fmt.Fprintf(&eb, `; EAttr%02d = "a run attempt attribute value"`, k)
			}
			eb.WriteString(" ]")
			ead, perr := classad.Parse(eb.String())
			if perr != nil {
				tb.Fatal(perr)
			}
			if werr := epoch.Put(fmt.Sprintf("e%d.%d", i, e), ead); werr != nil {
				tb.Fatal(werr)
			}
		}
	}
	return cat
}

func memEvolveSpec() JoinSpec {
	return JoinSpec{
		Kind:  JoinInner,
		Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId", "Owner", "RequestMemory"}},
		Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts", "RequestMemory", "MemoryUsage"}},
		Cond:  []string{"h.ClusterId == e.ClusterId"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
}

// BenchmarkProbeWhereJoinTimeGoes splits the join into parts that can be measured
// separately, so the next step is aimed at the big one rather than the interesting one.
//
// NOT a before/after: the join arm runs with whatever the package currently does, and the
// read arms are components, not alternatives to it. BenchmarkJoinReadProjection is the A/B.
//
// Read it with enough iterations to warm up (-benchtime 15x or more). With a handful, the
// first arm pays for the page cache and the allocator and reads far slower than it is, which
// is enough to make the join look more expensive than the read it contains.
func BenchmarkProbeWhereJoinTimeGoes(b *testing.B) {
	const jobs = 2000
	cat := memEvolveFixture(b, jobs)
	spec := memEvolveSpec()

	// (a) the whole join, as it stands today.
	b.Run("join", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var st JoinStats
			seq, err := cat.Join(spec, JoinLimits{}, &st)
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
			if n != jobs*memEpochs {
				b.Fatalf("got %d rows, want %d", n, jobs*memEpochs)
			}
		}
	})

	// (b) the read floor: both sides scanned into ClassAds and discarded, which is what the
	// join does before it examines a single pair.
	b.Run("read-both-sides-as-ads", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			n := 0
			for _, t := range []string{"history", "epoch_history"} {
				d, ok := cat.Table(t)
				if !ok {
					b.Fatalf("no table %s", t)
				}
				seq, err := d.Query("true")
				if err != nil {
					b.Fatal(err)
				}
				for range seq {
					n++
				}
			}
			if n != jobs*(1+memEpochs) {
				b.Fatalf("got %d rows, want %d", n, jobs*(1+memEpochs))
			}
		}
	})

	// (c) the same two scans read wire-form with the projection pushed down -- the rows the
	// query actually asked for, undecoded. The gap between (b) and (c) is the headroom.
	b.Run("read-both-sides-wire-projected", func(b *testing.B) {
		b.ReportAllocs()
		proj := map[string][]string{
			"history":       {"ClusterId", "Owner", "RequestMemory"},
			"epoch_history": {"ClusterId", "NumShadowStarts", "RequestMemory", "MemoryUsage"},
		}
		for b.Loop() {
			n := 0
			for _, t := range []string{"history", "epoch_history"} {
				d, ok := cat.Table(t)
				if !ok {
					b.Fatalf("no table %s", t)
				}
				seq, err := d.QueryRawWire("true", proj[t], false)
				if err != nil {
					b.Fatal(err)
				}
				for range seq {
					n++
				}
			}
			if n != jobs*(1+memEpochs) {
				b.Fatalf("got %d rows, want %d", n, jobs*(1+memEpochs))
			}
		}
	})

	// (d) the same wire-projected scans, then decoded back into ClassAds -- what a join
	// that keeps returning ads would pay. The gap between (c) and (d) is the price of the
	// object, and it decides whether the read win survives handing ads to the caller.
	b.Run("read-wire-projected-then-decode", func(b *testing.B) {
		b.ReportAllocs()
		proj := map[string][]string{
			"history":       {"ClusterId", "Owner", "RequestMemory"},
			"epoch_history": {"ClusterId", "NumShadowStarts", "RequestMemory", "MemoryUsage"},
		}
		for b.Loop() {
			n := 0
			for _, t := range []string{"history", "epoch_history"} {
				d, ok := cat.Table(t)
				if !ok {
					b.Fatalf("no table %s", t)
				}
				seq, err := d.QueryRawWire("true", proj[t], false)
				if err != nil {
					b.Fatal(err)
				}
				for row := range seq {
					a, derr := wire.DecodeInline(row)
					if derr != nil {
						b.Fatal(derr)
					}
					if classad.FromAST(a) == nil {
						b.Fatal("nil ad")
					}
					n++
				}
			}
			if n != jobs*(1+memEpochs) {
				b.Fatalf("got %d rows, want %d", n, jobs*(1+memEpochs))
			}
		}
	})
}

// BenchmarkJoinReadProjection is the A/B: the same join, both ways, in one process.
//
// The arms alternate order round to round because a Go benchmark's first arm warms the page
// cache and the allocator for the second, and a fixed order hands that advantage to whichever
// runs last.
func BenchmarkJoinReadProjection(b *testing.B) {
	const jobs = 2000
	cat := memEvolveFixture(b, jobs)
	spec := memEvolveSpec()

	run := func(b *testing.B, whole bool) {
		forceWholeRead = whole
		defer func() { forceWholeRead = false }()
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			var st JoinStats
			seq, err := cat.Join(spec, JoinLimits{}, &st)
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
			if n != jobs*memEpochs {
				b.Fatalf("got %d rows, want %d", n, jobs*memEpochs)
			}
		}
	}

	for round := 0; round < 2; round++ {
		arms := []struct {
			name  string
			whole bool
		}{{"whole-ads", true}, {"projected", false}}
		if round%2 == 1 {
			arms[0], arms[1] = arms[1], arms[0]
		}
		for _, a := range arms {
			b.Run(fmt.Sprintf("r%d/%s", round, a.name), func(b *testing.B) { run(b, a.whole) })
		}
	}
}
