package db

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// intervalFixture builds the canonical band-join shape: intervals on one side, points on the
// other. Each job holds [Lo, Hi] and each event a Ts, and the join asks which events fall
// inside which job's interval.
//
// Jobs are spaced ten apart and the width is the caller's, so the fan-out per job is width+1
// and the events a job admits are contiguous -- which is the property the sort exploits and
// the one a scan cannot see.
func intervalFixture(t testing.TB, jobs, events, width int) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	job, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := cat.CreateArchiveTable("ev", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= jobs; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("Lo", int64(i*10))
		ad.InsertAttr("Hi", int64(i*10+width))
		if aerr := job.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	// Appended in an order that is NOT the band's order, so a test passing because the
	// side happened to arrive sorted is not a test that passes. The stride is coprime with
	// the count, so it visits every event exactly once.
	for k := 0; k < events; k++ {
		ts := (k * 7919) % events
		ad := classad.New()
		ad.InsertAttr("Ts", int64(ts))
		ad.InsertAttrString("Tag", fmt.Sprintf("e%d", ts))
		if aerr := ev.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	return cat
}

// bandSpec is the fixture's join: every event whose Ts lies in a job's interval.
func bandSpec(kind JoinKind, b JoinBand, cond ...string) JoinSpec {
	return JoinSpec{
		Kind:  kind,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "ev", Alias: "e"},
		Cond:  cond,
		Band:  []JoinBand{b},
	}
}

// wholeBand is the fixture's band, both ends closed.
func wholeBand() JoinBand { return JoinBand{Attr: "Ts", Lo: "j.Lo", Hi: "j.Hi"} }

// drainBand runs a join and renders each row as "job:event".
func drainBand(t *testing.T, cat *Catalog, spec JoinSpec, lim JoinLimits) ([]string, *JoinStats) {
	t.Helper()
	var st JoinStats
	seq, err := cat.Join(spec, lim, &st)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	var out []string
	for row := range seq {
		out = append(out, bandPair(row))
	}
	sort.Strings(out)
	return out, &st
}

// bandPair renders a joined row. A real is spelled through its float so a Ts of 2.5 is
// distinguishable from one of 2 -- evalText renders both as "undefined" and "2".
func bandPair(row *classad.ClassAd) string {
	cl := evalText(row, "j.ClusterId")
	ex, err := classad.ParseExpr("e.Ts")
	if err != nil {
		return cl + ":!"
	}
	v := ex.Eval(row)
	switch {
	case v.IsInteger():
		n, _ := v.IntValue()
		return fmt.Sprintf("%s:%d", cl, n)
	case v.IsReal():
		f, _ := v.RealValue()
		return fmt.Sprintf("%s:%g", cl, f)
	case v.IsString():
		s, _ := v.StringValue()
		return cl + ":" + s
	}
	return cl + ":undefined"
}

// wantBand is the oracle: the fixture's answer worked out from the fixture's own rules,
// independently of anything the join does.
func wantBand(jobs, events, width int, loOpen, hiOpen bool) []string {
	var out []string
	for i := 1; i <= jobs; i++ {
		lo, hi := i*10, i*10+width
		for ts := 0; ts < events; ts++ {
			if ts < lo || (loOpen && ts == lo) {
				continue
			}
			if ts > hi || (hiOpen && ts == hi) {
				continue
			}
			out = append(out, fmt.Sprintf("%d:%d", i, ts))
		}
	}
	sort.Strings(out)
	return out
}

// THE ANSWER.

func TestBandJoinAnswersTheOracle(t *testing.T) {
	const jobs, events, width = 40, 500, 6
	cat := intervalFixture(t, jobs, events, width)
	got, st := drainBand(t, cat, bandSpec(JoinInner, wholeBand()), JoinLimits{})
	if st.Err != nil {
		t.Fatalf("join stopped early: %v", st.Err)
	}
	if want := wantBand(jobs, events, width, false, false); !equalStrs(got, want) {
		t.Fatalf("band join returned %d rows, want %d\n got %v\nwant %v",
			len(got), len(want), first(got, 8), first(want, 8))
	}
	// Provenance: a window per streamed job, or the rows above were produced by the scan
	// this is supposed to replace and the test proves nothing about the band.
	if st.BandWindows != jobs {
		t.Fatalf("the join searched %d windows for %d jobs; it did not run banded",
			st.BandWindows, jobs)
	}
}

// The same rows, from the same data, through the ON condition alone. The band is an access
// path and not a different question, so the two must agree exactly -- and this is the only
// test that can see a window that is subtly off by one, because the scan has no window.
func TestBandJoinAgreesWithTheUnbandedScan(t *testing.T) {
	const jobs, events, width = 40, 500, 6
	for _, c := range []struct {
		name string
		band JoinBand
		cond string
	}{
		{"closed", wholeBand(), "e.Ts >= j.Lo && e.Ts <= j.Hi"},
		{"loOpen", JoinBand{Attr: "Ts", Lo: "j.Lo", Hi: "j.Hi", LoOpen: true},
			"e.Ts > j.Lo && e.Ts <= j.Hi"},
		{"hiOpen", JoinBand{Attr: "Ts", Lo: "j.Lo", Hi: "j.Hi", HiOpen: true},
			"e.Ts >= j.Lo && e.Ts < j.Hi"},
		{"bothOpen", JoinBand{Attr: "Ts", Lo: "j.Lo", Hi: "j.Hi", LoOpen: true, HiOpen: true},
			"e.Ts > j.Lo && e.Ts < j.Hi"},
		{"loOnly", JoinBand{Attr: "Ts", Lo: "j.Lo"}, "e.Ts >= j.Lo"},
		{"hiOnly", JoinBand{Attr: "Ts", Hi: "j.Hi"}, "e.Ts <= j.Hi"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cat := intervalFixture(t, jobs, events, width)
			banded, bst := drainBand(t, cat, bandSpec(JoinInner, c.band), JoinLimits{})
			scan, sst := drainBand(t, cat, JoinSpec{
				Kind:  JoinInner,
				Left:  JoinSide{Table: "jobs", Alias: "j"},
				Right: JoinSide{Table: "ev", Alias: "e"},
				Cond:  []string{c.cond},
			}, JoinLimits{})
			if bst.BandWindows == 0 {
				t.Fatalf("the banded arm did not run banded")
			}
			if sst.BandWindows != 0 {
				t.Fatalf("the scan arm ran banded; it is supposed to be the comparison")
			}
			if !equalStrs(banded, scan) {
				t.Fatalf("band returned %d rows, the scan %d; first differing %v vs %v",
					len(banded), len(scan), first(banded, 6), first(scan, 6))
			}
			// The scan is the shape this replaces: every event against every job.
			if sst.Pairs <= bst.Pairs {
				t.Fatalf("the band examined %d pairs and the scan %d; the band is "+
					"supposed to examine fewer", bst.Pairs, sst.Pairs)
			}
		})
	}
}

// THE COST.

// The pairs a band examines are its matches, not the product of the sides. That is the whole
// claim, and it is what makes a join that cannot finish as a scan finish as a band.
func TestBandJoinExaminesOnlyItsWindow(t *testing.T) {
	const jobs, events, width = 40, 500, 6
	cat := intervalFixture(t, jobs, events, width)
	got, st := drainBand(t, cat, bandSpec(JoinInner, wholeBand()), JoinLimits{})
	if st.Pairs != len(got) {
		t.Fatalf("the band examined %d pairs for %d rows; a window admits only what matches",
			st.Pairs, len(got))
	}
	if st.BandCandidates != st.Pairs {
		t.Fatalf("the windows admitted %d rows and %d pairs were examined; they are the "+
			"same rows", st.BandCandidates, st.Pairs)
	}
	// What the scan would have been, from the fixture rather than from a second run.
	if scan := jobs * events; st.Pairs*10 > scan {
		t.Fatalf("the band examined %d pairs against a scan's %d; too close to be the "+
			"algorithm", st.Pairs, scan)
	}
}

// The headline. A pair budget the scan cannot meet is one the band finishes well inside,
// because they do not examine the same pairs -- so this is a join that has no answer without
// a band and a complete one with it.
func TestBandJoinRunsUnderAPairBudgetTheScanExceeds(t *testing.T) {
	const jobs, events, width = 40, 500, 6
	cat := intervalFixture(t, jobs, events, width)
	// Under the scan's 20,000 pairs and over the band's 280.
	const budget = 2000
	band, bst := drainBand(t, cat, bandSpec(JoinInner, wholeBand()), JoinLimits{MaxPairs: budget})
	if bst.Err != nil {
		t.Fatalf("the band did not finish inside %d pairs: %v", budget, bst.Err)
	}
	if want := wantBand(jobs, events, width, false, false); !equalStrs(band, want) {
		t.Fatalf("the band answered %d rows under the budget, want %d", len(band), len(want))
	}
	_, sst := drainBand(t, cat, JoinSpec{
		Kind:  JoinInner,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "ev", Alias: "e"},
		Cond:  []string{"e.Ts >= j.Lo && e.Ts <= j.Hi"},
	}, JoinLimits{MaxPairs: budget})
	if sst.Err == nil {
		t.Fatalf("the scan finished inside %d pairs; the budget does not separate them", budget)
	}
	if !strings.Contains(sst.Err.Error(), "candidate pairs") {
		t.Fatalf("the scan stopped for %v, not for the pair budget", sst.Err)
	}
}

// THE ROWS A WINDOW CANNOT ADMIT.

// intervalOddFixture is the fixture with rows whose band attribute is not an orderable number:
// a string, an absent attribute, and a NaN. None of them can satisfy any window, and each is
// a different way for a sorted side to go wrong -- a string that compares as zero, an absent
// attribute that reads undefined, a NaN that breaks the ordering the search assumes.
func intervalOddFixture(t testing.TB) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	job, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := cat.CreateArchiveTable("ev", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range []struct{ id, lo, hi int64 }{{1, 0, 10}, {2, 100, 200}} {
		ad := classad.New()
		ad.InsertAttr("ClusterId", j.id)
		ad.InsertAttr("Lo", j.lo)
		ad.InsertAttr("Hi", j.hi)
		if aerr := job.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	// A job whose bounds are not numbers at all.
	bad := classad.New()
	bad.InsertAttr("ClusterId", int64(3))
	bad.InsertAttrString("Lo", "early")
	bad.InsertAttrString("Hi", "late")
	if aerr := job.Append(bad); aerr != nil {
		t.Fatal(aerr)
	}
	add := func(mk func(*classad.ClassAd)) {
		ad := classad.New()
		mk(ad)
		if aerr := ev.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	add(func(a *classad.ClassAd) { a.InsertAttr("Ts", 5) })           // inside job 1
	add(func(a *classad.ClassAd) { a.InsertAttrFloat("Ts", 2.5) })    // inside job 1, a real
	add(func(a *classad.ClassAd) { a.InsertAttr("Ts", 150) })         // inside job 2
	add(func(a *classad.ClassAd) { a.InsertAttrString("Ts", "x") })   // a string
	add(func(a *classad.ClassAd) { a.InsertAttrString("Other", "") }) // no Ts at all
	add(func(a *classad.ClassAd) { a.InsertAttrFloat("Ts", math.NaN()) })
	return cat
}

func TestBandJoinAdmitsNothingThatIsNotAnOrderableNumber(t *testing.T) {
	cat := intervalOddFixture(t)
	got, st := drainBand(t, cat, bandSpec(JoinInner, wholeBand()), JoinLimits{})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	// Job 3's bounds are strings, so its window admits nothing at all -- and the string,
	// absent and NaN events join to no job.
	want := []string{"1:2.5", "1:5", "2:150"}
	if !equalStrs(got, want) {
		t.Fatalf("band join returned %v, want %v", got, want)
	}
}

// A LEFT join keeps the row whose window admitted nothing, which is the case most easily lost:
// the band leaves no candidate, exactly as an undefined key leaves no bucket, and the step's
// kind is what decides whether that is a dropped row or a null-extended one.
func TestBandLeftJoinKeepsAJobWhoseWindowIsEmpty(t *testing.T) {
	cat := intervalOddFixture(t)
	got, st := drainBand(t, cat, bandSpec(JoinLeft, wholeBand()), JoinLimits{})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	want := []string{"1:2.5", "1:5", "2:150", "3:undefined"}
	if !equalStrs(got, want) {
		t.Fatalf("left band join returned %v, want %v", got, want)
	}
}

// A semi join over a band stops at the first row of the window, so a job with a hundred
// events in range costs one pair -- the two narrowings compose rather than one replacing the
// other.
func TestBandSemiJoinStopsAtTheFirstRowOfTheWindow(t *testing.T) {
	const jobs, events, width = 20, 500, 9
	cat := intervalFixture(t, jobs, events, width)
	got, st := drainBand(t, cat, bandSpec(JoinSemi, wholeBand()), JoinLimits{})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if len(got) != jobs {
		t.Fatalf("the semi band join returned %d rows, want one per job (%d)", len(got), jobs)
	}
	if st.Pairs != jobs {
		t.Fatalf("the semi band join examined %d pairs for %d jobs; a window's first row "+
			"answers it", st.Pairs, jobs)
	}
	if st.BandWindows != jobs {
		t.Fatalf("the semi band join searched %d windows; it did not run banded", st.BandWindows)
	}
}

func TestBandAntiJoinKeepsTheJobsWhoseWindowIsEmpty(t *testing.T) {
	cat := intervalOddFixture(t)
	got, st := drainBand(t, cat, bandSpec(JoinAnti, wholeBand()), JoinLimits{})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if want := []string{"3:undefined"}; !equalStrs(got, want) {
		t.Fatalf("anti band join returned %v, want %v", got, want)
	}
}

// COMPOSITION.

// A band and an equality on one step: the bucket narrows and every band is then checked per
// candidate, because there is no single ordering a window could be searched over inside a
// hash. The rows must be the same as the band alone filtered by the equality.
func TestBandWithAnEqualityAnswersTheSameRows(t *testing.T) {
	cat := intervalOddFixture(t)
	spec := bandSpec(JoinInner, wholeBand())
	spec.Equi = []JoinEqui{{Left: "j.ClusterId", Right: "ClusterId"}}
	got, st := drainBand(t, cat, spec, JoinLimits{})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	// No event carries a ClusterId, so every key is undefined and nothing joins -- the
	// point being that the band did not quietly admit rows the hash excluded.
	if len(got) != 0 {
		t.Fatalf("band with an equality returned %v, want nothing", got)
	}
	if st.BandWindows != 0 {
		t.Fatalf("a hashed step searched %d windows; it is narrowed by the bucket",
			st.BandWindows)
	}
}

// Two bands on one step. The first is searched and the rest are evaluated per candidate, so
// a second window has to narrow the answer and not be forgotten.
func TestASecondBandNarrowsWhatTheFirstAdmitted(t *testing.T) {
	cat := intervalFixture(t, 10, 200, 20)
	spec := bandSpec(JoinInner, wholeBand())
	// Ts above j.Lo+5 as well, which cuts the low five of every window.
	spec.Band = append(spec.Band, JoinBand{Attr: "Ts", Lo: "j.Lo + 5"})
	got, st := drainBand(t, cat, spec, JoinLimits{})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	var want []string
	for i := 1; i <= 10; i++ {
		for ts := i*10 + 5; ts <= i*10+20 && ts < 200; ts++ {
			want = append(want, fmt.Sprintf("%d:%d", i, ts))
		}
	}
	sort.Strings(want)
	if !equalStrs(got, want) {
		t.Fatalf("two bands returned %d rows, want %d", len(got), len(want))
	}
	// The second band is a filter, not a window, so the pairs examined are the FIRST
	// band's window and the rows are fewer.
	if st.Pairs <= len(got) {
		t.Fatalf("two bands examined %d pairs for %d rows; the second band filters within "+
			"the first's window", st.Pairs, len(got))
	}
}

// A band in the middle of a chain. The sides before it supply the bounds, which is the only
// thing that makes a band different from a constraint on one table.
func TestBandInAChainReadsTheSidesBeforeIt(t *testing.T) {
	cat := intervalFixture(t, 10, 200, 5)
	got, st := drainBand(t, cat, JoinSpec{
		Left: JoinSide{Table: "jobs", Alias: "j"},
		Steps: []JoinStep{
			{Kind: JoinInner, Side: JoinSide{Table: "jobs", Alias: "k"},
				Equi: []JoinEqui{{Left: "j.ClusterId", Right: "ClusterId"}}},
			{Kind: JoinInner, Side: JoinSide{Table: "ev", Alias: "e"},
				Band: []JoinBand{{Attr: "Ts", Lo: "k.Lo", Hi: "k.Hi"}}},
		},
	}, JoinLimits{})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if want := wantBand(10, 200, 5, false, false); !equalStrs(got, want) {
		t.Fatalf("the chain returned %d rows, want %d", len(got), len(want))
	}
	if st.BandWindows == 0 {
		t.Fatalf("the chain's band step did not run banded")
	}
}

// A parallel walk shares one sorted side between its goroutines and each searches its own
// window in it. The side is read-only once built, which is what makes that safe -- and a
// disagreement here is what it would look like if it were not.
func TestBandAgreesSerialAndParallel(t *testing.T) {
	const jobs, events, width = 60, 800, 8
	cat := intervalFixture(t, jobs, events, width)
	forceSerialJoinWalk = true
	serial, sst := drainBand(t, cat, bandSpec(JoinInner, wholeBand()), JoinLimits{})
	forceSerialJoinWalk = false
	par, pst := drainBand(t, cat, bandSpec(JoinInner, wholeBand()), JoinLimits{})
	if sst.Err != nil || pst.Err != nil {
		t.Fatalf("serial %v, parallel %v", sst.Err, pst.Err)
	}
	if !equalStrs(serial, par) {
		t.Fatalf("serial returned %d rows and parallel %d", len(serial), len(par))
	}
	if sst.BandWindows != pst.BandWindows {
		t.Fatalf("serial searched %d windows and parallel %d", sst.BandWindows, pst.BandWindows)
	}
}

// THE PLAN.

func TestExplainReportsTheBandAndWhetherItIsSearched(t *testing.T) {
	cat := intervalFixture(t, 10, 100, 5)
	ex, err := cat.ExplainJoin(bandSpec(JoinInner, wholeBand()), JoinLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Steps) != 1 {
		t.Fatalf("explain reported %d steps", len(ex.Steps))
	}
	st := ex.Steps[0]
	if !st.Banded {
		t.Fatalf("explain says the step is not banded; it has a band and no equality")
	}
	if len(st.Band) != 1 || st.Band[0].Attr != "Ts" {
		t.Fatalf("explain reported bands %v", st.Band)
	}
	// Explain and execution must not disagree about this: a plan that claims a window and
	// then scans is the failure this pairing exists to catch.
	_, rst := drainBand(t, cat, bandSpec(JoinInner, wholeBand()), JoinLimits{})
	if st.Banded != (rst.BandWindows > 0) {
		t.Fatalf("explain says banded=%t and the run searched %d windows",
			st.Banded, rst.BandWindows)
	}
	// With an equality the step keeps its band and is narrowed by the bucket instead.
	hashed := bandSpec(JoinInner, wholeBand())
	hashed.Equi = []JoinEqui{{Left: "j.ClusterId", Right: "ClusterId"}}
	hex, err := cat.ExplainJoin(hashed, JoinLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if hex.Steps[0].Banded || len(hex.Steps[0].Band) != 1 {
		t.Fatalf("a hashed step reported banded=%t with %d bands",
			hex.Steps[0].Banded, len(hex.Steps[0].Band))
	}
}

// REFUSALS.

func TestBandRefusalsNameWhatIsWrong(t *testing.T) {
	cat := intervalFixture(t, 4, 20, 2)
	for _, c := range []struct {
		name string
		spec JoinSpec
		want string
	}{
		{"no attribute", bandSpec(JoinInner, JoinBand{Lo: "j.Lo"}), "names no attribute"},
		{"no bounds", bandSpec(JoinInner, JoinBand{Attr: "Ts"}), "neither end"},
		{"cross", bandSpec(JoinCross, wholeBand()), "cross join"},
		{"bound reads its own side",
			bandSpec(JoinInner, JoinBand{Attr: "Ts", Lo: "e.Ts"}), "band bound"},
		{"bound does not parse",
			bandSpec(JoinInner, JoinBand{Attr: "Ts", Lo: "j.Lo +"}), "lower bound"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var st JoinStats
			_, err := cat.Join(c.spec, JoinLimits{}, &st)
			if err == nil {
				t.Fatalf("the spec was accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("refused with %q, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestBandRefusesBothSpellingsAtOnce(t *testing.T) {
	cat := intervalFixture(t, 4, 20, 2)
	spec := JoinSpec{
		Left: JoinSide{Table: "jobs", Alias: "j"},
		Band: []JoinBand{wholeBand()},
		Steps: []JoinStep{{Kind: JoinInner, Side: JoinSide{Table: "ev", Alias: "e"},
			Band: []JoinBand{wholeBand()}}},
	}
	var st JoinStats
	if _, err := cat.Join(spec, JoinLimits{}, &st); err == nil ||
		!strings.Contains(err.Error(), "both the two-table spelling") {
		t.Fatalf("a spec with a band in both spellings was refused with %v", err)
	}
}

func first(s []string, n int) []string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

// THE READ.

// A band's attribute is in no ON term and in no output projection, so the narrowed read is
// the one place it can be dropped -- and dropping it is silent: the window reads undefined,
// admits nothing, and the join returns too few rows.
//
// This is not hypothetical. The first working version of the band failed exactly here, for a
// semi join, whose side is projected to nothing by construction.
func TestBandReadsTheWindowedAttributeThoughNothingAsksToSeeIt(t *testing.T) {
	const jobs, events, width = 20, 300, 5
	cat := intervalFixture(t, jobs, events, width)
	spec := bandSpec(JoinInner, wholeBand())
	// The caller wants the tag and the cluster. Nothing wants Ts, and the window is over it.
	spec.Left.Project = []string{"ClusterId", "Lo", "Hi"}
	spec.Right.Project = []string{"Tag"}

	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for row := range seq {
		got = append(got, evalText(row, "j.ClusterId")+":"+evalText(row, "e.Tag"))
	}
	sort.Strings(got)
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	var want []string
	for _, p := range wantBand(jobs, events, width, false, false) {
		cl, ts, _ := strings.Cut(p, ":")
		want = append(want, cl+":e"+ts)
	}
	sort.Strings(want)
	if !equalStrs(got, want) {
		t.Fatalf("the projected band join returned %d rows, want %d; first %v vs %v",
			len(got), len(want), first(got, 6), first(want, 6))
	}
}

// The same join with every side read whole. The projection is an optimization, so the two
// must answer identically -- this is the switch the read's own oracle uses, pointed at a band.
func TestBandAnswersTheSameWithWholeReads(t *testing.T) {
	const jobs, events, width = 20, 300, 5
	cat := intervalFixture(t, jobs, events, width)
	spec := bandSpec(JoinInner, wholeBand())
	spec.Left.Project = []string{"ClusterId", "Lo", "Hi"}
	spec.Right.Project = []string{"Tag"}

	narrow, nst := drainBand(t, cat, spec, JoinLimits{})
	forceWholeRead = true
	whole, wst := drainBand(t, cat, spec, JoinLimits{})
	forceWholeRead = false
	if nst.Err != nil || wst.Err != nil {
		t.Fatalf("narrow %v, whole %v", nst.Err, wst.Err)
	}
	if !equalStrs(narrow, whole) {
		t.Fatalf("the narrowed read returned %d rows and the whole read %d",
			len(narrow), len(whole))
	}
	if nst.BandWindows == 0 || wst.BandWindows == 0 {
		t.Fatalf("one arm did not run banded: narrow %d, whole %d",
			nst.BandWindows, wst.BandWindows)
	}
}

// THE BUILD SIDE.

// swapTemptingFixture makes the as-written orientation the one that HOLDS MORE: a small,
// order-free FROM side and a large table folded into it.
//
// That is the asymmetry the planner acts on here. Without an equality neither orientation
// reads less -- streaming A and hashing B costs the same records as the reverse -- so the two
// totals are equal and the decision falls to the tiebreak, which is the side held. Swapping
// would hold 40 rows instead of 4,000, and without a band that is what the planner does.
func swapTemptingFixture(t testing.TB, jobs, events int) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	job, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 4096, Unordered: true})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := cat.CreateArchiveTable("ev", ArchiveConfig{SegmentSize: 4096, Unordered: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= jobs; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("Lo", int64(i))
		ad.InsertAttr("Hi", int64(i+2))
		if aerr := job.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	for ts := 0; ts < events; ts++ {
		ad := classad.New()
		ad.InsertAttr("Ts", int64(ts))
		if aerr := ev.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	return cat
}

// A band names an attribute of ONE side, and swapSides moves the sides without moving it.
// Swapping a banded join would window the wrong table -- so the planner compares no second
// orientation at all, and says so.
func TestABandedJoinIsNotSwapped(t *testing.T) {
	const jobs, events = 40, 4000
	cat := swapTemptingFixture(t, jobs, events)

	// The premise: the same shape with the window written as a condition IS swapped. Without
	// this the test passes on a fixture the planner was never going to swap, and proves
	// nothing about the band.
	var sst JoinStats
	seq, err := cat.Join(JoinSpec{
		Kind:  JoinInner,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "ev", Alias: "e"},
		Cond:  []string{"e.Ts >= j.Lo && e.Ts <= j.Hi"},
	}, JoinLimits{}, &sst)
	if err != nil {
		t.Fatal(err)
	}
	for range seq {
	}
	if !sst.BuildSideSwapped {
		t.Fatalf("the unbanded shape was not swapped either (%s); the fixture does not "+
			"test the guard", sst.BuildSideWhy)
	}

	var bst JoinStats
	bseq, err := cat.Join(bandSpec(JoinInner, wholeBand()), JoinLimits{}, &bst)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range bseq {
		n++
	}
	if bst.Err != nil {
		t.Fatal(bst.Err)
	}
	if bst.BuildSideSwapped {
		t.Fatalf("the banded join was swapped; its window is over %s's attribute", "ev")
	}
	if !strings.Contains(bst.BuildSideWhy, "windowed on Ts") {
		t.Fatalf("the planner kept the orientation for %q, which does not name the band",
			bst.BuildSideWhy)
	}
	if bst.BandWindows != jobs {
		t.Fatalf("the banded join searched %d windows for %d jobs", bst.BandWindows, jobs)
	}
	// And it answered: every event in [i, i+2] for each job, which the small side bounds.
	want := 0
	for i := 1; i <= jobs; i++ {
		for ts := i; ts <= i+2 && ts < events; ts++ {
			want++
		}
	}
	if want == 0 {
		t.Fatalf("the fixture admits nothing; it cannot tell a swap from a window")
	}
	if n != want {
		t.Fatalf("the banded join returned %d rows, want %d", n, want)
	}
}

// A re-centred chain is rebuilt from its join keys: each step is constructed fresh and only
// the conditions are carried across. A band is neither, so a chain carrying one must be left
// in the order it was written -- the failure otherwise is a step that quietly lost a conjunct
// and a join that returns rows it should not have.
//
// The step below has an equality too, which is what makes this worth a test of its own: a
// band-only step is declined for having no join key, so it never reaches the rebuild. A step
// with one key AND a window passes that check and is exactly the one that would lose it.
func TestAChainCarryingABandIsNotRecentred(t *testing.T) {
	side := func(table, alias string) JoinSide { return JoinSide{Table: table, Alias: alias} }
	spec := JoinSpec{
		Left: side("jobs", "j"),
		Steps: []JoinStep{
			{Kind: JoinInner, Side: side("ev", "e"),
				Equi: []JoinEqui{{Left: "j.ClusterId", Right: "ClusterId"}}},
			{Kind: JoinInner, Side: side("ev2", "f"),
				Equi: []JoinEqui{{Left: "j.ClusterId", Right: "ClusterId"}}},
		},
	}
	// The premise: without a band this chain is a star the planner is willing to re-centre.
	if star, why := chainToStar(spec); star == nil {
		t.Fatalf("the unbanded chain was declined for %q; it is supposed to be re-centrable", why)
	}
	// With one, on a step that also carries a join key, it is declined and the reason says so.
	spec.Steps[1].Band = []JoinBand{{Attr: "Ts", Lo: "j.Lo", Hi: "j.Hi"}}
	star, why := chainToStar(spec)
	if star != nil {
		t.Fatalf("a chain with a windowed step was re-centred; the window would be dropped")
	}
	if !strings.Contains(why, "windowed on Ts") {
		t.Fatalf("the chain was declined for %q, which does not name the window", why)
	}
}

// A bound that is not a number admits nothing, the way an undefined hash key matches no
// bucket. The case that matters is the MIXED one -- one bound a number and the other not --
// because a window with neither bound usable declines for either reason and cannot tell
// which rule is being followed.
func TestABoundThatIsNotANumberAdmitsNothing(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	job, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := cat.CreateArchiveTable("ev", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	// Job 1 is ordinary. Job 2's lower bound is a string and its upper bound a number;
	// job 3 is the mirror. Neither may admit anything -- a half-usable window is not a
	// one-sided band, it is a window whose other end could not be read.
	add := func(id int64, mk func(*classad.ClassAd)) {
		ad := classad.New()
		ad.InsertAttr("ClusterId", id)
		mk(ad)
		if aerr := job.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	add(1, func(a *classad.ClassAd) { a.InsertAttr("Lo", 0); a.InsertAttr("Hi", 100) })
	add(2, func(a *classad.ClassAd) { a.InsertAttrString("Lo", "x"); a.InsertAttr("Hi", 100) })
	add(3, func(a *classad.ClassAd) { a.InsertAttr("Lo", 0); a.InsertAttrString("Hi", "x") })
	for ts := int64(0); ts < 5; ts++ {
		ad := classad.New()
		ad.InsertAttr("Ts", ts)
		if aerr := ev.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	got, st := drainBand(t, cat, bandSpec(JoinInner, wholeBand()), JoinLimits{})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	want := []string{"1:0", "1:1", "1:2", "1:3", "1:4"}
	if !equalStrs(got, want) {
		t.Fatalf("a half-readable window returned %v, want only job 1's rows %v", got, want)
	}
}

// Bounds that cross -- Lo above Hi -- admit nothing. The window's two searches then land with
// the lower index ABOVE the upper one, and a slice taken between them in that order does not
// come back empty, it panics.
func TestCrossedBoundsAdmitNothing(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	job, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := cat.CreateArchiveTable("ev", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range []struct{ id, lo, hi int64 }{{1, 80, 20}, {2, 20, 80}} {
		ad := classad.New()
		ad.InsertAttr("ClusterId", j.id)
		ad.InsertAttr("Lo", j.lo)
		ad.InsertAttr("Hi", j.hi)
		if aerr := job.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	for ts := int64(0); ts < 100; ts++ {
		ad := classad.New()
		ad.InsertAttr("Ts", ts)
		if aerr := ev.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	got, st := drainBand(t, cat, bandSpec(JoinInner, wholeBand()), JoinLimits{})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	// Job 1 admits nothing; job 2 admits 20..80.
	if len(got) != 61 {
		t.Fatalf("crossed and ordinary bounds returned %d rows, want 61", len(got))
	}
	for _, p := range got {
		if strings.HasPrefix(p, "1:") {
			t.Fatalf("a window whose bounds cross admitted %s", p)
		}
	}
}

// A NaN in the band's attribute is kept OUT of the sorted side, not merely skipped once the
// window has been searched.
//
// Two reasons, and the second is the one this can pin. NaN compares false against everything
// including itself, so `vals[i] < vals[j]` is not an ordering with one present and the sort
// is entitled to leave the slice in any arrangement -- an argument about what Go's sort may
// do, which no fixture here provokes it into doing. What a fixture DOES show is the simpler
// half: a window is a range of INDEXES, and band 0 is not re-checked inside it, so a NaN
// lying between the two searches is emitted as a match. A window open at both ends puts
// every row between them.
func TestANaNNeverJoins(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	job, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := cat.CreateArchiveTable("ev", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	// One job whose window admits every number there is, so nothing is excluded by being
	// out of range and the only rows left out are the ones the rule leaves out.
	ad := classad.New()
	ad.InsertAttr("ClusterId", 1)
	ad.InsertAttr("Lo", -1_000_000_000)
	ad.InsertAttr("Hi", 1_000_000_000)
	if aerr := job.Append(ad); aerr != nil {
		t.Fatal(aerr)
	}
	const numeric = 64
	for k := 0; k < numeric; k++ {
		e := classad.New()
		e.InsertAttr("Ts", int64((k*7919)%numeric))
		e.InsertAttrString("Tag", "n")
		if aerr := ev.Append(e); aerr != nil {
			t.Fatal(aerr)
		}
	}
	for k := 0; k < 8; k++ {
		e := classad.New()
		e.InsertAttrFloat("Ts", math.NaN())
		e.InsertAttrString("Tag", "nan")
		if aerr := ev.Append(e); aerr != nil {
			t.Fatal(aerr)
		}
	}
	// Bounded ABOVE only. With both ends bounded the lower search happens to step over a
	// NaN that sorted below the numbers, so a two-ended window hides the rule rather than
	// testing it; with only an upper bound the window starts at index zero and whatever
	// sorted there is in it.
	var st JoinStats
	seq, err := cat.Join(bandSpec(JoinInner, JoinBand{Attr: "Ts", Hi: "j.Hi"}), JoinLimits{}, &st)
	if err != nil {
		t.Fatal(err)
	}
	rows, nans := 0, 0
	for row := range seq {
		rows++
		if evalText(row, "e.Tag") == "nan" {
			nans++
		}
	}
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if nans != 0 {
		t.Fatalf("a window bounded only above admitted %d NaN rows", nans)
	}
	if rows != numeric {
		t.Fatalf("the open window returned %d rows, want the %d numbers", rows, numeric)
	}
	// And the sorted side itself holds only the numbers: the NaNs were not searched past,
	// they were never in it.
	if st.BandCandidates != numeric {
		t.Fatalf("the window admitted %d candidates, want the %d numbers",
			st.BandCandidates, numeric)
	}
}

// The twin site. A band that is not the one searched -- a second band, or any band on a step
// that hashes -- is evaluated as a plain comparison instead, and a NaN passes a plain
// comparison in BOTH directions: it is neither below the lower bound nor above the upper one,
// because every comparison against it is false. So it needs the same rule stated again, and
// this is the shape that reaches it.
func TestANaNDoesNotPassABandCheckedAsAComparison(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	job, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := cat.CreateArchiveTable("ev", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ad := classad.New()
	ad.InsertAttr("ClusterId", 1)
	ad.InsertAttr("Lo", 0)
	ad.InsertAttr("Hi", 100)
	if aerr := job.Append(ad); aerr != nil {
		t.Fatal(aerr)
	}
	for _, e := range []struct {
		ts  float64
		nan bool
	}{{50, false}, {0, true}, {150, false}} {
		a := classad.New()
		a.InsertAttr("ClusterId", 1)
		if e.nan {
			a.InsertAttrFloat("Ts", math.NaN())
		} else {
			a.InsertAttrFloat("Ts", e.ts)
		}
		if aerr := ev.Append(a); aerr != nil {
			t.Fatal(aerr)
		}
	}
	// The equality hashes the step, so the band is not searched and every band is checked
	// as a comparison -- which is the path under test.
	spec := bandSpec(JoinInner, wholeBand())
	spec.Equi = []JoinEqui{{Left: "j.ClusterId", Right: "ClusterId"}}
	got, st := drainBand(t, cat, spec, JoinLimits{})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if st.BandWindows != 0 {
		t.Fatalf("the step searched %d windows; it is supposed to hash", st.BandWindows)
	}
	if want := []string{"1:50"}; !equalStrs(got, want) {
		t.Fatalf("the hashed band returned %v, want %v", got, want)
	}
}

// A bound that is NaN admits nothing, at EITHER end.
//
// The two ends do not fail the same way if this is left to the comparisons. Every comparison
// against NaN is false, so the lower search finds no row at or above the bound and comes back
// empty -- the right answer by accident -- while the upper search finds no row above it and
// comes back with the slice's length, which is the whole side. A window bounded above by a
// value nobody can read is not a window over everything.
func TestANaNBoundAdmitsNothingAtEitherEnd(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	job, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := cat.CreateArchiveTable("ev", ArchiveConfig{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ad := classad.New()
	ad.InsertAttr("ClusterId", 1)
	ad.InsertAttrFloat("Lo", math.NaN())
	ad.InsertAttrFloat("Hi", math.NaN())
	if aerr := job.Append(ad); aerr != nil {
		t.Fatal(aerr)
	}
	for ts := int64(0); ts < 32; ts++ {
		e := classad.New()
		e.InsertAttr("Ts", ts)
		if aerr := ev.Append(e); aerr != nil {
			t.Fatal(aerr)
		}
	}
	for _, c := range []struct {
		name string
		band JoinBand
	}{
		{"upper only", JoinBand{Attr: "Ts", Hi: "j.Hi"}},
		{"lower only", JoinBand{Attr: "Ts", Lo: "j.Lo"}},
		{"both", wholeBand()},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, st := drainBand(t, cat, bandSpec(JoinInner, c.band), JoinLimits{})
			if st.Err != nil {
				t.Fatal(st.Err)
			}
			if len(got) != 0 {
				t.Fatalf("a NaN bound admitted %d rows: %v", len(got), first(got, 5))
			}
		})
	}
	// And the same bound checked as a comparison rather than searched, which is the other
	// site that has to agree.
	spec := bandSpec(JoinInner, wholeBand())
	spec.Band = append(spec.Band, JoinBand{Attr: "Ts", Lo: "j.Lo"})
	got, st := drainBand(t, cat, spec, JoinLimits{})
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if len(got) != 0 {
		t.Fatalf("a NaN bound in a second band admitted %d rows", len(got))
	}
}

// A bound that reads nothing -- a constant -- still has to leave the projection intact. The
// read accounts for a bound by the sides it names, and a bound that names none must come
// back as "reads nothing", not as "cannot be accounted for", which would read every side
// whole and quietly undo the narrowing on every other query shape.
func TestAConstantBoundDoesNotWidenTheRead(t *testing.T) {
	const jobs, events, width = 10, 200, 5
	cat := intervalFixture(t, jobs, events, width)
	spec := bandSpec(JoinInner, JoinBand{Attr: "Ts", Lo: "j.Lo", Hi: "60"})
	_ = width
	spec.Left.Project = []string{"ClusterId", "Lo"}
	spec.Right.Project = []string{"Tag"}

	proj := planJoinRead(spec)
	if proj[1] == nil {
		t.Fatalf("the folded side is read whole; a constant bound widened the read")
	}
	var hasTs bool
	for _, a := range proj[1] {
		if strings.EqualFold(a, "Ts") {
			hasTs = true
		}
	}
	if !hasTs {
		t.Fatalf("the read of the folded side is %v, which leaves out the windowed attribute",
			proj[1])
	}
	// Rendered through Tag, because the OUTPUT projection does not carry Ts: the read
	// carries it so the window can be searched, and the row does not carry it because
	// nothing asked to see it. Those are two different projections and this is what tells
	// them apart.
	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for row := range seq {
		got = append(got, evalText(row, "j.ClusterId")+":"+evalText(row, "e.Tag"))
	}
	sort.Strings(got)
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	// The window is [j.Lo, 60]: the fixture's width bounds nothing here, because this band
	// carries no j.Hi. Jobs past 60 admit nothing at all.
	var want []string
	for i := 1; i <= jobs; i++ {
		for ts := i * 10; ts <= 60 && ts < events; ts++ {
			want = append(want, fmt.Sprintf("%d:e%d", i, ts))
		}
	}
	sort.Strings(want)
	if !equalStrs(got, want) {
		t.Fatalf("a constant upper bound returned %d rows, want %d; first %v vs %v",
			len(got), len(want), first(got, 4), first(want, 4))
	}
}
