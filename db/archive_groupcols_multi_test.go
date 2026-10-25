package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// A GROUP BY on MORE THAN ONE column, served from the columnar blocks.
//
// "Per owner per day" -- one raw categorical column beside one bucketed time column -- is the shape
// a dashboard asks constantly, and it reached no fast path at all unless it was an unconstrained
// COUNT(*) over a categorically indexed column, which the index tier answers from postings. Add a
// WHERE clause, or a SUM, or a group column the index does not carry, and the same query decoded
// every matching record.
//
// The tests here are the db half: the storage layer produces one row per STORAGE key and this layer
// merges the keys that render alike, so what must hold is that the merged result is exactly the
// record scan's -- group for group, value for value, over data built to make the two part company.

// multiColFixture is an archive carrying the shapes a multi-column group has to get right at once.
//
//   - Owner: a low-cardinality string, so every block dictionary-encodes it, and spelled in TWO
//     CASES for part of its range. The dictionaries are fold-ORDERED, so a group key taken from a
//     folded form would merge "u07" with "U07"; the record scan groups by exact text and keeps them
//     apart, and the counts below are the exact-bytes counts.
//   - Site: the render-alike column. An absent attribute, the UNDEFINED keyword and the literal text
//     "undefined" are three storage keys and ONE scan group; the number 42 and the string "42"
//     likewise. Put beside a second group column, each of those has to merge within every value of
//     the other column rather than globally.
//   - Ts: a monotone timestamp, bucketed by day.
//   - Mem: a numeric aggregate column.
func multiColFixture(tb testing.TB, n int) (*Catalog, *ArchiveTable) {
	tb.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: tb.TempDir()})
	if err != nil {
		tb.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 16})
	if err != nil {
		tb.Fatal(err)
	}
	const day = 86400
	base := int64(1700000000) / day * day
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nMem = %d\nTs = %d\n",
			i, i%8, 512+(i*97)%60000, base+int64(i%5)*day+int64(i%day))
		src += fmt.Sprintf("Owner = %q\n", multiColOwner(i))
		switch {
		case i%37 == 3: // Site absent entirely
		case i%37 == 11:
			src += "Site = undefined\n"
		case i%41 == 5:
			src += "Site = \"undefined\"\n"
		case i%43 == 7:
			src += "Site = 42\n"
		case i%47 == 9:
			src += "Site = \"42\"\n"
		default:
			src += fmt.Sprintf("Site = %q\n", fmt.Sprintf("site-%02d", (i/3)%8))
		}
		ad, err := classad.ParseOld(src)
		if err != nil {
			tb.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			tb.Fatal(err)
		}
	}
	if !a.BuildAndEnableSchemaScan(4000, 8) {
		tb.Skip("no sealed segments to accelerate")
	}
	return cat, a
}

// multiColOwner is record i's owner: eight lower-case values in runs of 16, two of which are also
// spelled in upper case for part of their run. Two, not eight: a block dictionary only forms while
// the distinct count stays below half the block's record count.
func multiColOwner(i int) string {
	o := (i / 16) % 8
	lower := fmt.Sprintf("u%02d", o)
	if (o == 3 && i%3 == 0) || (o == 6 && i%5 == 0) {
		return strings.ToUpper(lower)
	}
	return lower
}

// TestArchiveGroupedMultipleColumns is the contract in one loop: every shape must be SERVED from the
// columns (ok=true from GroupedFromColumns, the only way to tell a pushdown from a silent fall back)
// and must give exactly what the record scan gives.
func TestArchiveGroupedMultipleColumns(t *testing.T) {
	const day = 86400
	cat, a := multiColFixture(t, 8000)
	defer cat.Close()

	countStar := []AggSpec{{Func: AggCount, Arg: "*"}}
	withStats := []AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggMin, Arg: "Mem"},
		{Func: AggMax, Arg: "Mem"}, {Func: AggSum, Arg: "Mem"}, {Func: AggAvg, Arg: "Mem"}}
	for _, tc := range []struct {
		constraint string
		groups     []GroupCol
		aggs       []AggSpec
		why        string
	}{
		{"", []GroupCol{{Attr: "Owner"}, {Attr: "Ts", BucketWidth: day}}, countStar,
			"per owner per day, unconstrained: the shape the index tier also answers"},
		{"Mem > 30000", []GroupCol{{Attr: "Owner"}, {Attr: "Ts", BucketWidth: day}}, countStar,
			"... under a WHERE clause, which the index tier declines"},
		{"", []GroupCol{{Attr: "Owner"}, {Attr: "Ts", BucketWidth: day}}, withStats,
			"... with value aggregates, which the index tier has no way to produce"},
		{"", []GroupCol{{Attr: "Ts", BucketWidth: day}, {Attr: "Owner"}}, countStar,
			"the same two columns in the other order"},
		{"", []GroupCol{{Attr: "Site"}, {Attr: "ProcId"}}, countStar,
			"the render-alike column beside a second raw column: the merge must be per tuple"},
		{"ProcId < 4", []GroupCol{{Attr: "Site"}, {Attr: "Owner"}}, withStats,
			"two string columns, both dictionary-coded"},
		{"", []GroupCol{{Attr: "ProcId"}, {Attr: "Mem", BucketWidth: 1000}}, countStar,
			"two numeric columns, one bucketed"},
		{"", []GroupCol{{Attr: "Ts", BucketWidth: day}, {Attr: "Mem", BucketWidth: 10000}}, countStar,
			"BOTH columns bucketed, on different widths"},
		{"Mem > 30000 && ProcId < 4", []GroupCol{{Attr: "Owner"}, {Attr: "Site"}, {Attr: "ProcId"}}, countStar,
			"three columns under a conjunction"},
		{"ClusterId > 99999999", []GroupCol{{Attr: "Owner"}, {Attr: "Ts", BucketWidth: day}}, countStar,
			"nothing matches: no groups at all"},
		{`Owner == "u03"`, []GroupCol{{Attr: "Owner"}, {Attr: "Site"}}, countStar,
			"a string predicate routes through the vector tier"},
	} {
		got, served := a.groupedFromColumns(tc.constraint, tc.groups, tc.aggs)
		if !served {
			t.Errorf("%q GROUP BY %s: declined, but this is served from the columns (%s)",
				tc.constraint, groupColNames(tc.groups), tc.why)
			continue
		}
		want := scanAggregate(t, a, tc.constraint, tc.groups, tc.aggs)
		gotM, wantM := tupleRows(t, got), tupleRows(t, want)
		if len(gotM) != len(wantM) {
			t.Errorf("%q GROUP BY %s: %d groups, scan found %d (%s)",
				tc.constraint, groupColNames(tc.groups), len(gotM), len(wantM), tc.why)
		}
		for g, v := range wantM {
			if fmt.Sprint(gotM[g]) != fmt.Sprint(v) {
				t.Errorf("%q GROUP BY %s: group %q = %v, scan = %v",
					tc.constraint, groupColNames(tc.groups), g, gotM[g], v)
			}
		}
		for g := range gotM {
			if _, ok := wantM[g]; !ok {
				t.Errorf("%q GROUP BY %s: group %q is not in the scan's result at all",
					tc.constraint, groupColNames(tc.groups), g)
			}
		}
	}
}

// TestArchiveGroupedMultipleColumnsKeepsCaseApart is the one that would otherwise pass by luck.
//
// The dictionaries this data is read through are fold-ORDERED and the comparison path works in fold
// ranges, so a group key derived from a folded form -- or from the fold range a value lands in -- is
// the natural thing to build and would merge "u03" with "U03". The scan groups by EXACT TEXT and
// gives two groups, and no step downstream could split them again: the merge at this boundary only
// ever joins keys together.
//
// Asserted against counts derived from the fixture's own rule rather than against the scan, so the
// test says what the right answer IS rather than that two implementations agree.
func TestArchiveGroupedMultipleColumnsKeepsCaseApart(t *testing.T) {
	const n = 8000
	const day = 86400
	cat, a := multiColFixture(t, n)
	defer cat.Close()

	base := int64(1700000000) / day * day
	want := map[string]int{}
	for i := 0; i < n; i++ {
		ts := base + int64(i%5)*day + int64(i%day)
		want[multiColOwner(i)+"\x00"+fmt.Sprint(ts/day*day)]++
	}
	rows, served := a.groupedFromColumns("", []GroupCol{{Attr: "Owner"}, {Attr: "Ts", BucketWidth: day}},
		[]AggSpec{{Func: AggCount, Arg: "*"}})
	if !served {
		t.Fatal("per owner per day declined")
	}
	got := map[string]string{}
	for _, r := range rows {
		k := strings.Join(r.Group, "\x00")
		if _, dup := got[k]; dup {
			t.Fatalf("group %q emitted twice", strings.ReplaceAll(k, "\x00", "|"))
		}
		got[k] = r.Values[0]
	}
	if len(got) != len(want) {
		t.Errorf("%d groups, the fixture has %d owner-days: case-folding merges groups the scan keeps apart",
			len(got), len(want))
	}
	for k, n := range want {
		if got[k] != fmt.Sprint(n) {
			t.Errorf("group %q = %q, want %d", strings.ReplaceAll(k, "\x00", "|"), got[k], n)
		}
	}
	// Named explicitly: a count can be right for the wrong reason, and these are the two spellings
	// the whole test is about.
	for _, pair := range [][2]string{{"u03", "U03"}, {"u06", "U06"}} {
		lo, up := 0, 0
		for k, v := range got {
			switch {
			case strings.HasPrefix(k, pair[0]+"\x00"):
				n := 0
				fmt.Sscan(v, &n)
				lo += n
			case strings.HasPrefix(k, pair[1]+"\x00"):
				n := 0
				fmt.Sscan(v, &n)
				up += n
			}
		}
		if lo == 0 || up == 0 {
			t.Errorf("%q counted %d and %q counted %d: one spelling vanished, so the two were merged",
				pair[0], lo, pair[1], up)
		}
	}
}

// TestArchiveGroupedDeclinesPastTheColumnCap pins the refusal. A packed key holds a fixed number of
// components, and truncating past the last slot would merge every group that differs only in a
// dropped component and report the merge as an answer -- so the query goes back to the scan, which
// has no such limit.
func TestArchiveGroupedDeclinesPastTheColumnCap(t *testing.T) {
	cat, a := multiColFixture(t, 4000)
	defer cat.Close()
	all := []GroupCol{{Attr: "Owner"}, {Attr: "Site"}, {Attr: "ProcId"}, {Attr: "ClusterId"}, {Attr: "Mem"}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}
	for n := 1; n <= len(all); n++ {
		rows, served := a.groupedFromColumns("", all[:n], aggs)
		if n > collections.MaxGroupCols {
			if served {
				t.Errorf("%d group columns were SERVED with a %d-component key",
					n, collections.MaxGroupCols)
			}
			continue
		}
		if !served {
			t.Fatalf("%d group columns declined", n)
		}
		for _, r := range rows {
			if len(r.Group) != n {
				t.Fatalf("%d group columns produced a row with %d labels", n, len(r.Group))
			}
		}
	}
	// And the whole dispatcher still answers the declined shape, by scanning. Spelled "true"
	// rather than left empty: an empty constraint is match-all to every fast rung and a parse error
	// to the scan, which is a pre-existing gap and not what this test is about.
	rows, err := a.AggregateCols("true", all, aggs)
	if err != nil {
		t.Fatalf("the dispatcher refused a shape the scan can answer: %v", err)
	}
	if len(rows) == 0 {
		t.Error("the scan answered the over-cap shape with no rows")
	}
}

// TestArchiveGroupedNegativeBucketWidthDeclines pins the one width that cannot mean what it says.
// The scan's own test is `width > 0`, so it reads a negative width as "do not bucket"; agreeing with
// that reading silently rather than refusing a nonsensical request is how the two paths come to
// label a row differently.
func TestArchiveGroupedNegativeBucketWidthDeclines(t *testing.T) {
	cat, a := multiColFixture(t, 2000)
	defer cat.Close()
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}
	for _, groups := range [][]GroupCol{
		{{Attr: "Ts", BucketWidth: -86400}},
		{{Attr: "Owner"}, {Attr: "Ts", BucketWidth: -1}},
	} {
		if _, served := a.groupedFromColumns("", groups, aggs); served {
			t.Errorf("GROUP BY %s with a negative bucket width was served", groupColNames(groups))
		}
	}
}

// tupleRows keys whole rows by their joined group TUPLE, the way AggregateValues keys a group, and
// fails on a duplicated tuple -- which would mean one logical group was emitted as two rows.
func tupleRows(t *testing.T, rows []AggRow) map[string][]string {
	t.Helper()
	out := make(map[string][]string, len(rows))
	for _, r := range rows {
		k := strings.Join(r.Group, "\x00")
		if _, dup := out[k]; dup {
			t.Errorf("group %q appears twice: one group was split into two rows",
				strings.ReplaceAll(k, "\x00", "|"))
		}
		out[k] = r.Values
	}
	return out
}

// groupColNames labels a group column list for a message.
func groupColNames(gs []GroupCol) string {
	parts := make([]string, len(gs))
	for i, g := range gs {
		parts[i] = g.Attr
		if g.BucketWidth != 0 {
			parts[i] = fmt.Sprintf("bucket(%s,%d)", g.Attr, g.BucketWidth)
		}
	}
	return strings.Join(parts, ", ")
}

// TestArchiveGroupedMultipleColumnsReachesTheColumnarTier is the end-to-end statement, and the one
// that does not depend on a clock: the DISPATCHER must pick the columnar rung for the motivating
// shape, and the scan trailer must report that no record was reassembled.
//
// GroupedFromColumns answering when asked directly proves nothing on its own -- the fan-out stops at
// the first rung that answers, and a rung that is eligible but never reached is a rung that does
// nothing. Before several group columns were served, every shape below reported the SCAN tier with
// one reassembly per matching record; a regression that sent them back there would leave every other
// test in this file passing.
func TestArchiveGroupedMultipleColumnsReachesTheColumnarTier(t *testing.T) {
	const day = 86400
	cat, a := multiColFixture(t, 8000)
	defer cat.Close()
	groups := []GroupCol{{Attr: "Owner"}, {Attr: "Ts", BucketWidth: day}}
	for _, tc := range []struct {
		why        string
		constraint string
		aggs       []AggSpec
	}{
		{"match-all COUNT(*)", "true", []AggSpec{{Func: AggCount, Arg: "*"}}},
		{"constrained COUNT(*)", "Mem > 30000", []AggSpec{{Func: AggCount, Arg: "*"}}},
		{"match-all with AVG", "true",
			[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggAvg, Arg: "Mem"}}},
		{"constrained with AVG", "Mem > 30000",
			[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggAvg, Arg: "Mem"}}},
	} {
		var st collections.ScanStats
		rows, err := a.AggregateColsStats(tc.constraint, groups, tc.aggs, &st)
		if err != nil {
			t.Fatalf("%s: %v", tc.why, err)
		}
		// The index tier is also a correct answer for the unconstrained COUNT(*) -- it reads no
		// record at all -- so what is asserted is that the SCAN did not serve it.
		if st.Tier == collections.TierScan {
			t.Errorf("%s: served by the record scan, reassembling %d records",
				tc.why, st.RecordsReassembled)
		}
		if st.RecordsReassembled != 0 {
			t.Errorf("%s: %d records were reassembled; the columnar and index rungs read none",
				tc.why, st.RecordsReassembled)
		}
		if len(rows) == 0 {
			t.Errorf("%s: no rows", tc.why)
		}
	}
}
