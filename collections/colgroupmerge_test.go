package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// nearCorpus plants SIX attributes that almost always co-occur, in three exact patterns that differ
// only on a few ads: {NA,NB}, {NC,ND}, {NE,NF}. With fewer group slots than patterns, exact
// grouping can only cover some of them; widening lets the same slots cover all six.
//
// That budget pressure is the whole point. A merge is never profitable on its own -- both patterns
// are supersets of their intersection, so keeping them apart always scores at least as many cells
// -- so relaxing pays only by FIT.
func nearCorpus(n, strays int) []string {
	out := make([]string, 0, n)
	s1, s2 := 0, 0
	for i := range n {
		s := fmt.Sprintf(`[ ClusterId=%d; Owner="u%d"`, i, i%5)
		if i%10 < 4 {
			s += fmt.Sprintf(`; NA=%d; NB=%d`, i, i*2)
			if s1 < strays && i%20 == 0 {
				s1++ // holds {NA,NB} but not {NC,ND}
			} else {
				s += fmt.Sprintf(`; NC=%d; ND=%d`, i*3, i*4)
			}
			if s2 < strays && i%20 == 10 {
				s2++ // holds the others but not {NE,NF}
			} else {
				s += fmt.Sprintf(`; NE=%d; NF=%d`, i*5, i*6)
			}
		}
		out = append(out, s+" ]")
	}
	return out
}

func cellsOf(info GroupSchemaInfo) (cells int, worstPartial float64) {
	for _, g := range info.Groups {
		cells += g.Cells
		if g.PartialFrac > worstPartial {
			worstPartial = g.PartialFrac
		}
	}
	return
}

// TestGroupMergeRecoversMoreCells: relaxing from exact co-occurrence to a near-pattern merge must
// recover more columnar cells, at the cost of a small partial rate.
func TestGroupMergeRecoversMoreCells(t *testing.T) {
	texts := nearCorpus(2000, 6)

	exact := New(Options{Shards: 1})
	defer exact.Close()
	loadGroupCorpus(t, exact, texts)
	exCells, exPartial := cellsOf(exact.GroupSchemas(4096, 2))

	relaxed := New(Options{Shards: 1, GroupMergeJaccard: 0.95, GroupMaxPartialFrac: 0.05})
	defer relaxed.Close()
	loadGroupCorpus(t, relaxed, texts)
	rxCells, rxPartial := cellsOf(relaxed.GroupSchemas(4096, 2))

	if exPartial != 0 {
		t.Errorf("exact grouping produced %.4f partial; it is impossible by construction", exPartial)
	}
	if rxCells <= exCells {
		t.Errorf("relaxed recovered %d cells, exact %d; with more patterns than slots, widening "+
			"must fit more attributes into the same slots", rxCells, exCells)
	}
	if rxPartial == 0 {
		t.Error("relaxed grouping produced no partial at all; it did not merge anything")
	}
	t.Logf("exact %d cells / %.3f%% partial; relaxed %d cells / %.3f%% partial",
		exCells, exPartial*100, rxCells, rxPartial*100)
}

// TestGroupMergeRespectsPartialCeiling: a merge that would push the partial rate past the ceiling
// is refused. The ceiling is the real control -- a partial record reads its group attributes the
// slow way, so past some fraction a wider group is worse than a narrower one.
func TestGroupMergeRespectsPartialCeiling(t *testing.T) {
	texts := nearCorpus(2000, 120) // 15% of the group's ads lack NC

	loose := New(Options{Shards: 1, GroupMergeJaccard: 0.5, GroupMaxPartialFrac: 1.0})
	defer loose.Close()
	loadGroupCorpus(t, loose, texts)
	_, loosePartial := cellsOf(loose.GroupSchemas(4096, 2))

	capped := New(Options{Shards: 1, GroupMergeJaccard: 0.5, GroupMaxPartialFrac: 0.001})
	defer capped.Close()
	loadGroupCorpus(t, capped, texts)
	_, cappedPartial := cellsOf(capped.GroupSchemas(4096, 2))

	if loosePartial <= 0.001 {
		t.Skipf("fixture produced only %.4f partial uncapped; the ceiling would not bind", loosePartial)
	}
	if cappedPartial > 0.001 {
		t.Errorf("ceiling 0.1%% but worst partial is %.4f%%", cappedPartial*100)
	}
}

// TestGroupMergeKeepsAnswers: with relaxed groups the partial records read their group attributes
// from the base block's cold tail. The answers must not move.
func TestGroupMergeKeepsAnswers(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14,
		GroupSchemaCount: 4, GroupStabilityRuns: 1,
		GroupMergeJaccard: 0.95, GroupMaxPartialFrac: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i, txt := range nearCorpus(3000, 30) {
		ad, err := classad.Parse(txt)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	queries := []string{`NC > 1000`, `NC is undefined`, `NA > 500 && NC < 9000`, `NA > NC`}
	want := map[string]int{}
	for _, q := range queries {
		qq, err := vm.Parse(q)
		if err != nil {
			t.Fatal(err)
		}
		for range c.Query(qq) {
			want[q]++
		}
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Skip("schema scan did not enable")
	}
	st := c.schemaScan.Load()
	if st == nil || len(st.groups) == 0 {
		t.Fatal("no groups built")
	}
	for _, q := range queries {
		qq, err := vm.Parse(q)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := c.CountQuery(qq)
		if !ok {
			t.Errorf("%q: columnar count declined", q)
			continue
		}
		if got != want[q] {
			t.Errorf("%q: columnar %d, row scan %d", q, got, want[q])
		}
	}
}
