package collections

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// groupCorpus builds ads with a planted structure:
//   - base attributes on every ad (ClusterId, Owner)
//   - a "ran" group (RemoteHost, StartDate, CpusProvisioned) on ranFrac of them, together
//   - a "container" group (ContainerImage, WantContainer) on conFrac, together and INDEPENDENTLY
//     of the ran group, so the two cannot be captured by partitioning ads into clusters
//   - a scattered attribute on a pseudo-random 40%, belonging to no group
func groupCorpus(n int, ranFrac, conFrac int) []string {
	out := make([]string, 0, n)
	for i := range n {
		var sb strings.Builder
		fmt.Fprintf(&sb, `[ ClusterId=%d; Owner="user%d"`, i, i%7)
		if i%100 < ranFrac {
			fmt.Fprintf(&sb, `; RemoteHost="slot1@e%d"; StartDate=%d; CpusProvisioned=%d`, i%37, 1700000000+i, 1+i%4)
		}
		if (i/3)%100 < conFrac {
			fmt.Fprintf(&sb, `; ContainerImage="img%d"; WantContainer=true`, i%5)
		}
		if i%5 < 2 {
			fmt.Fprintf(&sb, `; Scattered=%d`, i)
		}
		sb.WriteString(" ]")
		out = append(out, sb.String())
	}
	return out
}

func loadGroupCorpus(t *testing.T, c *Collection, texts []string) {
	t.Helper()
	for i, txt := range texts {
		ad, err := classad.Parse(txt)
		if err != nil {
			t.Fatalf("parse %d: %v", i, err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
}

func groupWith(info GroupSchemaInfo, attr string) (GroupSchemaEntry, bool) {
	for _, g := range info.Groups {
		if slices.ContainsFunc(g.Attrs, func(a string) bool { return strings.EqualFold(a, attr) }) {
			return g, true
		}
	}
	return GroupSchemaEntry{}, false
}

// TestGroupSchemasFindsCoOccurringSets is the core claim: attributes that appear and disappear
// together are recovered as one group, with the right membership split, and WITHOUT being told
// anything about what they mean.
func TestGroupSchemasFindsCoOccurringSets(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	loadGroupCorpus(t, c, groupCorpus(600, 40, 30))

	info := c.GroupSchemas(4096, 4)
	if info.Sampled == 0 {
		t.Fatal("nothing sampled")
	}
	ran, ok := groupWith(info, "RemoteHost")
	if !ok {
		t.Fatalf("no group carried RemoteHost; groups = %+v", info.Groups)
	}
	// The whole run bundle, and nothing else.
	want := []string{"CpusProvisioned", "RemoteHost", "StartDate"}
	got := append([]string(nil), ran.Attrs...)
	for i := range got {
		got[i] = strings.ToLower(got[i])
	}
	slices.Sort(got)
	for i := range want {
		want[i] = strings.ToLower(want[i])
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("run group = %v, want exactly %v", got, want)
	}
	if ran.InFrac < 0.35 || ran.InFrac > 0.45 {
		t.Errorf("run group in-fraction = %.3f, want ~0.40", ran.InFrac)
	}
	if ran.PartialFrac != 0 {
		t.Errorf("run group partial = %.4f, want 0 (members co-occur by construction)", ran.PartialFrac)
	}
	if ran.InFrac+ran.NoneFrac+ran.PartialFrac < 0.999 {
		t.Errorf("in+none+partial = %.4f, want 1", ran.InFrac+ran.NoneFrac+ran.PartialFrac)
	}

	// The independent container bundle must ALSO be found -- the two overlap freely, which is
	// exactly what a partition of ads into clusters could not express.
	con, ok := groupWith(info, "ContainerImage")
	if !ok {
		t.Fatalf("no group carried ContainerImage; groups = %+v", info.Groups)
	}
	if len(con.Attrs) != 2 {
		t.Errorf("container group = %v, want 2 members", con.Attrs)
	}
}

// TestGroupSchemasExcludeBaseAttributes: an attribute the base schema already carries needs no
// group, and reporting one would suggest work with no benefit.
func TestGroupSchemasExcludeBaseAttributes(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	loadGroupCorpus(t, c, groupCorpus(400, 40, 30))
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	info := c.GroupSchemas(4096, 6)
	for _, g := range info.Groups {
		for _, a := range g.Attrs {
			if strings.EqualFold(a, "ClusterId") || strings.EqualFold(a, "Owner") {
				t.Errorf("group %v contains base attribute %s", g.Attrs, a)
			}
		}
	}
	if info.BaseFields == 0 {
		t.Error("BaseFields = 0; the report must say what it derived against")
	}
}

// TestGroupSchemasRankedByCells: the group worth the most columnar coverage comes first, since
// that is the only reason to spend a schema pointer on it.
func TestGroupSchemasRankedByCells(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	// run group: 3 attrs x 40% ; container group: 2 attrs x 30% -> run must rank higher
	loadGroupCorpus(t, c, groupCorpus(600, 40, 30))
	info := c.GroupSchemas(4096, 4)
	if len(info.Groups) < 2 {
		t.Fatalf("want at least 2 groups, got %d", len(info.Groups))
	}
	for i := 1; i < len(info.Groups); i++ {
		if info.Groups[i].Cells > info.Groups[i-1].Cells {
			t.Errorf("groups not ranked by cells: #%d has %d > #%d's %d",
				i+1, info.Groups[i].Cells, i, info.Groups[i-1].Cells)
		}
	}
	if _, ok := groupWith(info, "RemoteHost"); !ok {
		t.Error("the highest-coverage group was not reported")
	}
}

// TestGroupSchemasReportIsDeterministic: two derivations from the same data must produce the same
// groups in the same order, or the drift report would show movement that is really just tie
// ordering.
func TestGroupSchemasReportIsDeterministic(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	loadGroupCorpus(t, c, groupCorpus(500, 40, 30))
	a := c.GroupSchemas(4096, 4)
	b := c.GroupSchemas(4096, 4)
	if len(a.Groups) != len(b.Groups) {
		t.Fatalf("group counts differ: %d vs %d", len(a.Groups), len(b.Groups))
	}
	for i := range a.Groups {
		if !slices.Equal(a.Groups[i].Attrs, b.Groups[i].Attrs) {
			t.Errorf("group %d differs between runs: %v vs %v", i+1, a.Groups[i].Attrs, b.Groups[i].Attrs)
		}
	}
}

// TestGroupSchemasPersistAndDrift: derivations are checkpointed so a comparison does not depend on
// someone having captured the earlier report, and the drift summary reads them back.
func TestGroupSchemasPersistAndDrift(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	loadGroupCorpus(t, c, groupCorpus(500, 40, 30))
	first := c.GroupSchemas(4096, 4)
	if len(first.Groups) == 0 {
		t.Fatal("no groups derived on a persistent collection (inline records not canonicalized?)")
	}
	c.GroupSchemas(4096, 4)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(Options{Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	d := c2.GroupSchemaDrift()
	if d.Derivations < 2 {
		t.Fatalf("retained %d derivations across a restart, want >= 2", d.Derivations)
	}
	if d.OfFirst == 0 {
		t.Fatal("drift report has no first-derivation groups")
	}
	if d.Retained != d.OfFirst {
		t.Errorf("retained %d of %d groups on unchanged data; want all", d.Retained, d.OfFirst)
	}
	if d.MaxPartialFrac != 0 {
		t.Errorf("worst partial = %.4f on unchanged data, want 0", d.MaxPartialFrac)
	}
}

// TestGroupSchemaAgreementAcrossSegments: the per-segment check is the guard against committing a
// schema pointer to a group that only one segment's sample produced.
func TestGroupSchemaAgreementAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, SegmentSize: 1 << 13}) // small, so several segments seal
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	loadGroupCorpus(t, c, groupCorpus(3000, 40, 30))

	ag := c.GroupSchemaAgreement(4096, 4)
	if ag.Segments < 2 {
		t.Skipf("only %d sealed segment(s); nothing to compare", ag.Segments)
	}
	if len(ag.PerGroup) == 0 {
		t.Fatal("no per-group agreement reported")
	}
	// The planted groups are uniform across the corpus, so the top group must reproduce in
	// essentially every segment. A low number here would mean the derivation is picking up
	// per-segment noise rather than structure.
	if ag.PerGroup[0] < 0.8 {
		t.Errorf("top group reproduced in only %.0f%% of %d segments; planted groups are uniform",
			ag.PerGroup[0]*100, ag.Segments)
	}
}

// TestGroupSchemasRankByCellsNotSize pins the ranking RULE, on a corpus where ranking by member
// count and ranking by coverage disagree: a 2-attribute group on 90% of ads recovers more
// occurrences (1.8 per ad) than a 5-attribute group on 20% (1.0 per ad), and coverage is what a
// schema pointer is spent to buy.
func TestGroupSchemasRankByCellsNotSize(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	for i := range 1000 {
		var sb strings.Builder
		fmt.Fprintf(&sb, `[ ClusterId=%d; Owner="u%d"`, i, i%5)
		if i%10 < 8 { // wide reach (below the 90% base floor), few attrs
			fmt.Fprintf(&sb, `; WideA=%d; WideB=%d`, i, i+1)
		}
		if i%5 == 0 { // narrow reach, many attrs
			fmt.Fprintf(&sb, `; NarrowA=%d; NarrowB=%d; NarrowC=%d; NarrowD=%d; NarrowE=%d`, i, i, i, i, i)
		}
		sb.WriteString(" ]")
		ad, err := classad.Parse(sb.String())
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	info := c.GroupSchemas(4096, 4)
	if len(info.Groups) < 2 {
		t.Fatalf("want 2 groups, got %+v", info.Groups)
	}
	first := info.Groups[0]
	if len(first.Attrs) != 2 {
		t.Fatalf("first group = %v; want the 2-attribute wide group, which recovers more cells",
			first.Attrs)
	}
	wide, wok := groupWith(info, "WideA")
	narrow, nok := groupWith(info, "NarrowA")
	if !wok || !nok {
		t.Fatalf("both groups must be reported; got %+v", info.Groups)
	}
	if wide.Cells <= narrow.Cells {
		t.Errorf("wide cells %d <= narrow cells %d; ranking premise broken", wide.Cells, narrow.Cells)
	}
}
