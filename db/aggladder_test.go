package db

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
)

// fakeRung is a strategy whose answer is dictated by the test, recording that it was consulted.
type fakeRung struct {
	name string
	log  *[]string
	rows []AggRow
	ok   bool
	err  error
}

func (r *fakeRung) Name() string { return r.name }

func (r *fakeRung) Answer(AggRequest) ([]AggRow, bool, error) {
	*r.log = append(*r.log, r.name)
	return r.rows, r.ok, r.err
}

func ladderRow(v string) []AggRow { return []AggRow{{Values: []string{v}}} }

// TestAggLadderFallsThrough is the ladder's own contract, independent of either table kind: a
// declining rung is tried and passed over, the first rung that answers wins, and no rung below it
// is consulted. This is the property both aggregate entry points rest on -- every fast path is
// written assuming that declining is free and total.
func TestAggLadderFallsThrough(t *testing.T) {
	var log []string
	ladder := []AggStrategy{
		&fakeRung{name: "a", log: &log},
		&fakeRung{name: "b", log: &log},
		&fakeRung{name: "c", log: &log, rows: ladderRow("42"), ok: true},
		&fakeRung{name: "d", log: &log, rows: ladderRow("99"), ok: true},
	}
	rows, err := runAggLadder(ladder, AggRequest{})
	if err != nil {
		t.Fatalf("runAggLadder: %v", err)
	}
	if len(rows) != 1 || rows[0].Values[0] != "42" {
		t.Fatalf("rows = %+v, want the first answering rung's", rows)
	}
	if got := strings.Join(log, ","); got != "a,b,c" {
		t.Fatalf("consulted %q, want a,b,c (d must not be reached)", got)
	}
}

// TestAggLadderDiscardsDeclinedRows is the no-partial-answers rule made structural. A rung that
// gets halfway and finds it cannot finish must throw its partial result away; the runner does not
// take that on trust, because a half-built group set is exactly the failure that would look like a
// plausible answer rather than an error.
func TestAggLadderDiscardsDeclinedRows(t *testing.T) {
	var log []string
	ladder := []AggStrategy{
		// Declines, but hands back the rows it had built before giving up.
		&fakeRung{name: "partial", log: &log, rows: ladderRow("half"), ok: false},
		&fakeRung{name: "scan", log: &log, rows: ladderRow("whole"), ok: true},
	}
	rows, err := runAggLadder(ladder, AggRequest{})
	if err != nil {
		t.Fatalf("runAggLadder: %v", err)
	}
	if len(rows) != 1 || rows[0].Values[0] != "whole" {
		t.Fatalf("rows = %+v, want the declining rung's partial result discarded", rows)
	}
}

// TestAggLadderLastMustAnswer: the final rung is the reference and is not allowed to decline. If
// it does, that is a bug in the ladder rather than a query the system cannot serve, so the runner
// says so and names the rung -- an empty result would read as "no matching rows", which is a wrong
// answer rather than a reported failure.
func TestAggLadderLastMustAnswer(t *testing.T) {
	var log []string
	ladder := []AggStrategy{
		&fakeRung{name: "fast", log: &log},
		&fakeRung{name: "reference", log: &log},
	}
	rows, err := runAggLadder(ladder, AggRequest{})
	if err == nil {
		t.Fatalf("runAggLadder = %+v, nil; want an error when the last rung declines", rows)
	}
	if rows != nil {
		t.Fatalf("rows = %+v, want nil alongside the error", rows)
	}
	if !strings.Contains(err.Error(), "reference") {
		t.Fatalf("error %q does not name the offending rung", err)
	}
	if got := strings.Join(log, ","); got != "fast,reference" {
		t.Fatalf("consulted %q, want every rung tried", got)
	}
}

// TestAggLadderErrorStopsLadder: an error is not a decline. A malformed request (COUNT(DISTINCT *),
// a filter that does not parse) or a failed read must surface as itself, not send the ladder on to
// a rung that might answer a question nobody asked.
func TestAggLadderErrorStopsLadder(t *testing.T) {
	var log []string
	boom := errors.New("boom")
	ladder := []AggStrategy{
		&fakeRung{name: "fast", log: &log},
		&fakeRung{name: "broken", log: &log, err: boom},
		&fakeRung{name: "scan", log: &log, rows: ladderRow("x"), ok: true},
	}
	rows, err := runAggLadder(ladder, AggRequest{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the rung's own error", err)
	}
	if rows != nil {
		t.Fatalf("rows = %+v, want nil alongside the error", rows)
	}
	if got := strings.Join(log, ","); got != "fast,broken" {
		t.Fatalf("consulted %q, want the ladder to stop at the error", got)
	}
}

// TestAggLaddersAreOrderedLists pins the shape of the two real ladders: the rungs each table kind
// tries, in order. It is a description, not a preference -- it exists so that reordering or
// dropping a rung has to be a deliberate edit to this list rather than a side effect of a change
// somewhere in the fan-out.
//
// The archive's index rung appearing twice, split on match-all with the columnar rungs between the
// halves, is the measured part: the index wins the unconstrained count from postings and loses the
// constrained grouped count to the columns by an order of magnitude, because it answers that one
// from zone verdicts that read straddling segments record by record.
func TestAggLaddersAreOrderedLists(t *testing.T) {
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	names := func(ladder []AggStrategy) string {
		var out []string
		for _, s := range ladder {
			out = append(out, s.Name())
		}
		return strings.Join(out, ",")
	}
	if got, want := names(d.aggLadder(nil, nil, false)),
		"live-count,count-constraint,columnar-aggregate,grouped-columns,projected-scan"; got != want {
		t.Errorf("mutable ladder = %q, want %q", got, want)
	}
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(a.aggLadder(nil, false)),
		"match-none,index-unconstrained,count-constraint,grouped-columns,index-constrained,columnar-aggregate,projected-scan"; got != want {
		t.Errorf("archive ladder = %q, want %q", got, want)
	}
}

// TestAggLadderEveryRungMatchesTheReference is the invariant the whole arrangement rests on: a
// fast rung's answer must equal the rung below's for the same request. It drives each real rung
// directly, in isolation, and compares whatever it answers against the projected scan -- so a rung
// that renders a value, partitions a group or drops a row differently from the reference fails
// here rather than returning a plausible wrong number.
//
// It also pins the tier each archive rung reports, since that is the caller-visible statement
// about which strategy ran and several rungs share one tier.
func TestAggLadderEveryRungMatchesTheReference(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	d, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	// Two archives over identical records, differing only in whether the columnar accelerator is
	// built. The accelerated one is where the columns win; the plain one is the only place the
	// LOWER half of the split index rung can be reached at all, because grouped-columns sits
	// above it and takes every constrained grouped count the columns can serve.
	newArchive := func(name string) *ArchiveTable {
		a, err := cat.CreateArchiveTable(name, ArchiveConfig{
			SegmentSize:      1 << 14,
			CategoricalAttrs: []string{"Owner", "JobStatus"},
			ZoneAttrs:        []string{"QDate", "ProcId"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a, plain := newArchive("history"), newArchive("history_plain")
	owners := []string{"alice", "bob", "carol"}
	tx := d.Begin()
	for i := range 1200 {
		ad := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nOwner = %q\nQDate = %d\nWallClock = %d.5",
			i/10, i%10, []int{1, 2, 4, 5}[i%4], owners[(i/61)%len(owners)], 1700000000+i*37, i%100)
		tx.NewClassAd(fmt.Sprintf("k%d", i), mustAd(t, ad))
		for _, a := range []*ArchiveTable{a, plain} {
			if err := a.AppendOld(ad); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	d.EnableSchemaScan(1000, 8)
	a.BuildAndEnableSchemaScan(1000, 8)

	reqs := []AggRequest{
		{Constraint: "true", Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
		{Constraint: "JobStatus == 4", Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
		{Constraint: "false", Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
		{Constraint: "true", Aggs: []AggSpec{{Func: AggMax, Arg: "ProcId"}}},
		{Constraint: "true", Aggs: []AggSpec{{Func: AggMin, Arg: "ProcId"}}},
		{Constraint: "JobStatus == 2", Aggs: []AggSpec{{Func: AggSum, Arg: "ProcId"}}},
		{Constraint: "true", Aggs: []AggSpec{{Func: AggAvg, Arg: "WallClock"}}},
		{Constraint: "true", GroupCols: []GroupCol{{Attr: "JobStatus"}}, Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
		{Constraint: "true", GroupCols: []GroupCol{{Attr: "Owner"}}, Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
		{Constraint: "JobStatus == 4", GroupCols: []GroupCol{{Attr: "Owner"}}, Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
		{Constraint: "ProcId >= 2", GroupCols: []GroupCol{{Attr: "Owner"}}, Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
		{Constraint: "true", GroupCols: []GroupCol{{Attr: "ProcId"}}, Aggs: []AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggMax, Arg: "QDate"}}},
		{Constraint: "true", GroupCols: []GroupCol{{Attr: "QDate", BucketWidth: 86400}}, Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
		{Constraint: "true", GroupCols: []GroupCol{{Attr: "Owner"}, {Attr: "QDate", BucketWidth: 86400}}, Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
	}
	// Row order carries no meaning for a GROUP BY, and the rungs legitimately differ on it (the
	// index rung sorts; the scan is first-seen), so compare the row SET.
	asSet := func(rows []AggRow) map[string]int {
		out := map[string]int{}
		for _, r := range rows {
			out[strings.Join(r.Group, "\x00")+"|"+strings.Join(r.Values, "\x00")]++
		}
		return out
	}
	same := func(a, b map[string]int) bool {
		if len(a) != len(b) {
			return false
		}
		for k, v := range a {
			if b[k] != v {
				return false
			}
		}
		return true
	}
	check := func(t *testing.T, kind string, ladder []AggStrategy) {
		t.Helper()
		ref := ladder[len(ladder)-1]
		answered := map[string]bool{}
		for _, req := range reqs {
			want, ok, err := ref.Answer(req)
			if err != nil {
				t.Fatalf("%s reference %+v: %v", kind, req, err)
			}
			if !ok {
				t.Fatalf("%s reference declined %+v; the last rung must always answer", kind, req)
			}
			for _, s := range ladder[:len(ladder)-1] {
				got, ok, err := s.Answer(req)
				if err != nil {
					t.Fatalf("%s/%s %+v: %v", kind, s.Name(), req, err)
				}
				if !ok {
					continue
				}
				answered[s.Name()] = true
				if !same(asSet(got), asSet(want)) {
					t.Errorf("%s/%s %+v = %+v, reference = %+v", kind, s.Name(), req, got, want)
				}
			}
		}
		// A run in which every fast rung declined would pass vacuously, having compared the
		// reference against nothing.
		if len(answered) == 0 {
			t.Fatalf("%s: no fast rung answered any request; the comparison tested nothing", kind)
		}
		t.Logf("%s: rungs exercised %v", kind, answered)
	}
	check(t, "mutable", d.aggLadder(nil, nil, false))
	check(t, "archive", a.aggLadder(nil, false))
	check(t, "archive-plain", plain.aggLadder(nil, false))
	check(t, "archive-redacted", a.aggLadder(nil, true))

	// The tier each archive rung reports: the caller-visible statement about which strategy ran,
	// which several rungs share (three report TierColumnar, and the two halves of the split index
	// rung report the same TierIndex -- the split is about where each wins, not about what it is).
	// Each rung is driven on its own ladder bound to its own stats block, so a tier is
	// attributable to the rung that set it rather than to whichever one happened to answer.
	tiers := map[string]collections.AggTier{
		"match-none":          collections.TierMatchNone,
		"index-unconstrained": collections.TierIndex,
		"index-constrained":   collections.TierIndex,
		"count-constraint":    collections.TierColumnar,
		"grouped-columns":     collections.TierColumnar,
		"columnar-aggregate":  collections.TierColumnar,
		"projected-scan":      collections.TierScan,
	}
	tagged := map[string]bool{}
	for _, tbl := range []*ArchiveTable{a, plain} {
		for i := range tbl.aggLadder(nil, false) {
			for _, req := range reqs {
				var st collections.ScanStats
				s := tbl.aggLadder(&st, false)[i]
				if _, ok, err := s.Answer(req); err != nil || !ok {
					continue
				}
				tagged[s.Name()] = true
				if want, known := tiers[s.Name()]; !known {
					t.Errorf("archive rung %q has no expected tier", s.Name())
				} else if st.Tier != want {
					t.Errorf("archive/%s %+v reported tier %v, want %v", s.Name(), req, st.Tier, want)
				}
			}
		}
	}
	if len(tagged) != len(tiers) {
		t.Errorf("tier checked for %d of %d archive rungs (%v); the untested ones assert nothing",
			len(tagged), len(tiers), tagged)
	}
}
