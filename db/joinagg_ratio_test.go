package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// What a grouped result stops putting on the wire, as a property rather than as a number.
//
// The claim this feature rests on is not "grouping is smaller" -- that is obvious -- but that
// the grouped result does not SCALE with the join's output the way a composite row does.
// Doubling the expansion ratio (right rows per left row) doubles the rows a streaming join
// sends and leaves the grouped answer where it was.
//
// Bytes are counted as old-ClassAd text for the rows, since that is the form a join streams,
// and as the rendered group and value strings for the aggregate, since that is what an AggRow
// carries. Nothing here asserts a speed; it asserts the shape of the two curves.

// ratioFixture builds a jobs/history equi join with a controlled expansion ratio and a fixed
// number of owners, so the joined rows scale and the groups do not.
func ratioFixture(t *testing.T, leftRows, expansion, owners int) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	jobs, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateTable("hist")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= leftRows; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("ProcId", int64(0))
		ad.InsertAttrString("Owner", fmt.Sprintf("user%04d@chtc.wisc.edu", i%owners))
		ad.InsertAttr("JobStatus", int64(4))
		ad.InsertAttr("RequestCpus", int64(1))
		ad.InsertAttr("RequestMemory", int64(2048))
		ad.InsertAttrString("Cmd", "/home/user/analysis/run_payload.sh")
		ad.InsertAttrString("Iwd", "/home/user/analysis/workdir/run0001")
		ad.InsertAttr("QDate", int64(1762000000+i))
		if werr := jobs.Put(fmt.Sprintf("%d", i), ad); werr != nil {
			t.Fatal(werr)
		}
		for e := 0; e < expansion; e++ {
			h := classad.New()
			h.InsertAttr("ClusterId", int64(i))
			h.InsertAttr("ExitCode", int64((i+e)%8))
			h.InsertAttr("CompletionDate", int64(1762100000+i))
			h.InsertAttr("RemoteWallClockTime", int64(3600+i))
			h.InsertAttrString("LastRemoteHost", "slot1_3@e1234.chtc.wisc.edu")
			h.InsertAttr("NumJobStarts", int64(1))
			if werr := hist.Put(fmt.Sprintf("%d-%d", i, e), h); werr != nil {
				t.Fatal(werr)
			}
		}
	}
	return cat
}

// arm runs one expansion ratio and returns the bytes a streaming join would send (whole ads,
// and projected to just what the aggregate reads), the bytes the grouped answer carries, and
// the joined row count.
func ratioArm(t *testing.T, leftRows, expansion, owners int) (whole, projected, grouped, rows int) {
	t.Helper()
	cat := ratioFixture(t, leftRows, expansion, owners)
	base := JoinSpec{
		Kind:  JoinInner,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "hist", Alias: "h"},
		Cond:  []string{"j.ClusterId == h.ClusterId"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
	stream := func(spec JoinSpec) (bytes, n int) {
		var st JoinStats
		seq, err := cat.Join(spec, JoinLimits{}, &st)
		if err != nil {
			t.Fatal(err)
		}
		for row := range seq {
			bytes += len(row.MarshalOld())
			n++
		}
		if st.Err != nil {
			t.Fatal(st.Err)
		}
		return bytes, n
	}
	whole, rows = stream(base)
	// A client that already knew it was about to group would ask for nothing but the two
	// attributes it reads. That is the honest comparison, and the one asserted on.
	narrow := base
	narrow.Left.Project = []string{"Owner"}
	narrow.Right.Project = []string{"ExitCode"}
	projected, _ = stream(narrow)

	var st JoinStats
	agg, err := cat.JoinAggregate(JoinAggSpec{Join: base,
		GroupCols: []GroupCol{{Attr: "j.Owner"}},
		Aggs:      []AggSpec{{Func: AggCount, Arg: "*"}}}, JoinLimits{}, &st, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range agg {
		grouped += len(strings.Join(r.Group, "")) + len(strings.Join(r.Values, ""))
	}
	// An arm that produced nothing would measure only rejection, and an arm whose groups
	// did not come out as asked would make the denominator meaningless.
	if rows != leftRows*expansion || st.Rows != rows {
		t.Fatalf("expansion %d joined %d rows (stats %d), want %d", expansion, rows, st.Rows, leftRows*expansion)
	}
	if len(agg) != owners {
		t.Fatalf("expansion %d reduced to %d groups, want %d", expansion, len(agg), owners)
	}
	return whole, projected, grouped, rows
}

func TestJoinAggregateResultDoesNotScaleWithTheJoin(t *testing.T) {
	const leftRows, owners = 500, 20
	type arm struct{ whole, projected, grouped, rows int }
	got := map[int]arm{}
	for _, exp := range []int{1, 2, 4} {
		w, p, g, n := ratioArm(t, leftRows, exp, owners)
		got[exp] = arm{w, p, g, n}
		t.Logf("expansion %2d: %6d joined rows, whole-ad %8d B, projected %7d B, grouped %4d B "+
			"(whole:grouped %.0fx, projected:grouped %.0fx)",
			exp, n, w, p, g, float64(w)/float64(g), float64(p)/float64(g))
	}

	// The numerator scales with the join's output.
	for _, exp := range []int{2, 4} {
		ratio := float64(got[exp].projected) / float64(got[1].projected)
		if ratio < float64(exp)*0.95 || ratio > float64(exp)*1.05 {
			t.Errorf("expansion %d sends %.2fx the bytes of expansion 1, want about %dx",
				exp, ratio, exp)
		}
	}
	// The denominator does not. It moves only by the digits the counts grew.
	if g1, g4 := got[1].grouped, got[4].grouped; float64(g4) > float64(g1)*1.2 {
		t.Errorf("the grouped answer grew from %d B to %d B across a 4x expansion; it is "+
			"supposed to be bounded by the groups, not the rows", g1, g4)
	}
	// Which is the whole argument: the saving grows with the expansion rather than being a
	// constant factor. Stated with margin, since the exact bytes are fixture detail.
	r1 := float64(got[1].projected) / float64(got[1].grouped)
	r4 := float64(got[4].projected) / float64(got[4].grouped)
	if r1 < 50 {
		t.Errorf("at expansion 1 the grouped answer is only %.0fx smaller; the fixture is "+
			"not the shape this measures", r1)
	}
	if r4 < r1*3 {
		t.Errorf("4x the expansion bought %.0fx against %.0fx; the saving is supposed to "+
			"scale with the join's output, not be a constant factor", r4, r1)
	}
}
