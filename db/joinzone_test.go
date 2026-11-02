package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// HOW MUCH OF THE HASHED SIDE COULD ZONE MAPS SKIP?
//
// planIndexProbe already narrows the hashed side to the keys the streamed side holds, and it
// expresses that as a pure disjunction: `attr == v1 || attr == v2 || ...`. Segment zone maps
// already prune every scan path. But a disjunction prunes a segment only when its zone map
// rules out EVERY alternative, so a key set spread across the range keeps everything.
//
// A range conjunct over the same keys -- `attr >= min && attr <= max` -- is logically implied
// by the disjunction and adds no rows, but it is a top-level AND probe, which is the only
// shape zonePrune can use. This measures what that would be worth before any of it is built.
//
// The shape is the real one: an archive whose key correlates with insertion order (a job id
// in `history`) joined against a side covering a recent window of it.

func zoneFixture(tb testing.TB, records int) (*ArchiveTable, int) {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	// A small segment so the fixture spans many of them, and ClusterId zone-mapped, which is
	// what a join key on a job id would be.
	at, err := cat.CreateArchiveTable("history", ArchiveConfig{
		SegmentSize: 96 << 10,
		ZoneAttrs:   []string{"ClusterId"},
	})
	if err != nil {
		tb.Fatal(err)
	}
	for id := 0; id < records; id++ {
		ad, perr := classad.Parse(fmt.Sprintf(
			`[ ClusterId = %d; Owner = "u%d"; Pad = "padding value to give the record some size" ]`,
			id, id%7))
		if perr != nil {
			tb.Fatal(perr)
		}
		if werr := at.Append(ad); werr != nil {
			tb.Fatal(werr)
		}
	}
	return at, records
}

func TestZonePruneCeilingForAJoinProbe(t *testing.T) {
	const records = 40000
	at, total := zoneFixture(t, records)
	segments := at.Stats().Segments

	// The streamed side holds a RECENT WINDOW -- the last 5% of ids, which is what joining
	// epoch_history (recent run attempts) against history (all time) looks like.
	window := total / 20
	lo, hi := total-window, total-1
	keys := make([]string, 0, 64)
	step := window/64 + 1
	for k := lo; k <= hi && len(keys) < 64; k += step {
		keys = append(keys, fmt.Sprintf("ClusterId == %d", k))
	}
	disj := "(" + strings.Join(keys, " || ") + ")"
	ranged := fmt.Sprintf("ClusterId >= %d && ClusterId <= %d && %s", lo, hi, disj)

	// A SCATTERED key set over the whole range, for contrast: the rows genuinely are
	// everywhere, so nothing should be prunable by either form and the comparison says so.
	scattered := make([]string, 0, 64)
	for i := 0; i < 64; i++ {
		scattered = append(scattered, fmt.Sprintf("ClusterId == %d", i*(total/64)))
	}
	scatDisj := "(" + strings.Join(scattered, " || ") + ")"
	scatRanged := fmt.Sprintf("ClusterId >= 0 && ClusterId <= %d && %s", total-1, scatDisj)

	pruned := map[string]int{}
	for _, c := range []struct{ name, cons string }{
		{"clustered keys, disjunction (emitted today)", disj},
		{"clustered keys, range conjunct added", ranged},
		{"scattered keys, disjunction (emitted today)", scatDisj},
		{"scattered keys, range conjunct added", scatRanged},
	} {
		ex, err := at.Explain(c.cons)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		pruned[c.name] = ex.ZonePrunedSegments
		t.Logf("%-46s zonePruned=%3d/%d  reason=%s", c.name, ex.ZonePrunedSegments, segments, ex.Reason)
	}

	// THE POINT OF THIS TEST. A range conjunct over the same keys is logically implied by the
	// disjunction, and it was proposed on the theory that zonePrune can only use a top-level
	// AND probe, so the disjunction must prune weakly. It does not: a disjunctive plan is
	// pruned through zoneExcludesGroups, which rules a segment out when its zone map rules
	// out EVERY alternative -- and for the key set a join probe actually produces, clustered
	// because the other side covers a window, that is every segment outside the window.
	//
	// So the range buys nothing, and this asserts it rather than leaving the idea to be had
	// again. If it ever stops being true, this fails and the conjunct becomes worth adding.
	if a, b := pruned["clustered keys, disjunction (emitted today)"], pruned["clustered keys, range conjunct added"]; a != b {
		t.Errorf("the range conjunct changed pruning: disjunction %d, with range %d -- it is "+
			"no longer redundant and is worth emitting", a, b)
	}
	if got := pruned["clustered keys, disjunction (emitted today)"]; got == 0 {
		t.Errorf("a clustered key set pruned nothing; zone pruning is not reaching this plan")
	} else if got <= segments/2 {
		t.Errorf("a clustered key set covering 5%% of the range pruned only %d of %d segments",
			got, segments)
	}
	// A scattered key set is not a failure to prune, it is nothing to prune: the rows really
	// are in every segment. Asserted so the comparison above cannot be read as "pruning works
	// everywhere".
	if got := pruned["scattered keys, disjunction (emitted today)"]; got != 0 {
		t.Errorf("a key set spanning the whole range pruned %d segments; the rows are in all of them", got)
	}

	// Both forms must select exactly the same rows; the range is redundant by construction.
	countRows := func(cons string) int {
		seq, err := at.Query(cons)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for range seq {
			n++
		}
		return n
	}
	if a, b := countRows(disj), countRows(ranged); a != b {
		t.Fatalf("the range conjunct changed the answer: %d rows vs %d", a, b)
	} else if a == 0 {
		t.Fatal("neither form matched anything; the measurement is vacuous")
	}
}
