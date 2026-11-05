package db

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// withSerialJoinWalk runs fn with the parallel walk disabled.
func withSerialJoinWalk(t *testing.T, fn func()) {
	t.Helper()
	forceSerialJoinWalk = true
	defer func() { forceSerialJoinWalk = false }()
	fn()
}

// parJoinFixture is a join wide enough and long enough to be walked in parallel.
func parJoinFixture(t testing.TB, jobs int) *Catalog {
	t.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 18})
	if err != nil {
		t.Fatal(err)
	}
	ep, err := cat.CreateArchiveTable("epoch_history", ArchiveConfig{SegmentSize: 1 << 18})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < jobs; i++ {
		ad, perr := classad.ParseOld(fmt.Sprintf(
			"ClusterId = %d\nProcId = 0\nOwner = \"user%02d\"\nJobStatus = 4\nRequestMemory = %d",
			i, i%32, 1024+(i%8)*512))
		if perr != nil {
			t.Fatal(perr)
		}
		if aerr := hist.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
		// A deliberately UNEVEN fan: some jobs produce no rows and some produce several, so
		// the walkers finish at different times and the drainer's rotation is actually
		// exercised rather than every lane arriving in step.
		for e := 0; e < i%4; e++ {
			ead, perr := classad.ParseOld(fmt.Sprintf(
				"ClusterId = %d\nNumShadowStarts = %d\nMemoryUsage = %d", i, e+1, 512+e*64))
			if perr != nil {
				t.Fatal(perr)
			}
			if aerr := ep.Append(ead); aerr != nil {
				t.Fatal(aerr)
			}
		}
	}
	return cat
}

func parJoinSpec(limit int) JoinSpec {
	return JoinSpec{
		Kind:  JoinInner,
		Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId", "Owner"}},
		Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts", "MemoryUsage"}},
		Cond:  []string{"h.ClusterId == e.ClusterId"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
		Limit: limit,
	}
}

func wireJoinRows(t *testing.T, cat *Catalog, spec JoinSpec) [][]byte {
	t.Helper()
	var st JoinStats
	seq, err := cat.JoinWire(spec, JoinLimits{}, false, &st)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for row := range seq {
		out = append(out, append([]byte(nil), row...)) // the row aliases a reused buffer
	}
	if st.Err != nil {
		t.Fatalf("join stopped early: %v", st.Err)
	}
	return out
}

// THE PARALLEL WALK MUST ANSWER EXACTLY WHAT THE SERIAL ONE DOES, IN THE SAME ORDER.
//
// Not "the same set": the FROM side of a history join is an archive, whose scan is
// newest-first, and a relay ships what it is handed. Losing that order would silently
// reorder condor_history, so the comparison is row for row by position.
//
// The counter is asserted because declining to walk in parallel is silent: without it this
// would be free to compare the serial path with itself and pass.
func TestParallelJoinWalkMatchesSerial(t *testing.T) {
	cat := parJoinFixture(t, 4000)
	for _, limit := range []int{0, 1, 7, 500} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			spec := parJoinSpec(limit)
			before := joinParallelWalks.Load()
			par := wireJoinRows(t, cat, spec)
			if joinParallelWalks.Load() == before {
				t.Fatal("the join did not walk in parallel; this would compare the serial walk with itself")
			}
			var ser [][]byte
			withSerialJoinWalk(t, func() { ser = wireJoinRows(t, cat, spec) })

			if len(par) != len(ser) {
				t.Fatalf("parallel produced %d rows, serial %d", len(par), len(ser))
			}
			for i := range par {
				if !bytes.Equal(par[i], ser[i]) {
					pa, _ := wire.DecodeInline(par[i])
					sa, _ := wire.DecodeInline(ser[i])
					t.Fatalf("row %d differs:\n parallel: %s\n serial:   %s",
						i, classad.FromAST(pa), classad.FromAST(sa))
				}
			}
		})
	}
}

// Stats have to survive being counted in P places and merged in one.
func TestParallelJoinWalkMergesStats(t *testing.T) {
	cat := parJoinFixture(t, 4000)
	spec := parJoinSpec(0)

	before := joinParallelWalks.Load()
	var pst JoinStats
	seq, err := cat.JoinWire(spec, JoinLimits{}, false, &pst)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	if joinParallelWalks.Load() == before {
		t.Fatal("the join did not walk in parallel")
	}

	var sst JoinStats
	withSerialJoinWalk(t, func() {
		s, serr := cat.JoinWire(spec, JoinLimits{}, false, &sst)
		if serr != nil {
			t.Fatal(serr)
		}
		for range s {
		}
	})

	if pst.ScanRows != sst.ScanRows {
		t.Errorf("ScanRows: parallel %d, serial %d", pst.ScanRows, sst.ScanRows)
	}
	if pst.Pairs != sst.Pairs {
		t.Errorf("Pairs: parallel %d, serial %d", pst.Pairs, sst.Pairs)
	}
	if pst.Rows != sst.Rows || pst.Rows != n {
		t.Errorf("Rows: parallel %d, serial %d, yielded %d", pst.Rows, sst.Rows, n)
	}
}
