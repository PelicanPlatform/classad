package db

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// GROUP BY a STRING column fell to a record scan however it was written: the storage layer read the
// group value as a number and declined anything else. On a production history window that was 21.1 s
// against 90 ms for the same query grouped by a numeric column -- a 200x difference in the column's
// TYPE, not in the work.

// groupStrFixture is an archive whose string columns cover the cases the column read has to get right,
// all in one table so a single query exercises several at once:
//
//   - Owner: 40 values repeating in runs of 50, which is what a submit burst looks like and what makes
//     a block dictionary-encode the field. A few records drop it entirely (the group is `undefined`), a
//     few hold a NUMBER in it (the value escapes the string column into the cold tail), and a few hold
//     the literal text "undefined" -- which renders identically to the absent ones, so the scan puts
//     them in ONE group and the columnar path has to as well.
//   - Cmd: ~500 distinct values over a larger row group, so no block clears the dictionary's repeat
//     threshold and the positional string walk answers instead.
//   - Iwd: distinct per record, the high-cardinality end of the same reader.
//
// (Which reader a block uses is a property of the data, not of the query; the assertions that each
// fixture takes the reader it means to live in the collections tests, which can see the blocks.)
//
// The odd moduli keep the three Owner exceptions from lining up with each other or with the predicates.
func groupStrFixture(tb testing.TB, n int) (*Catalog, *ArchiveTable) {
	tb.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: tb.TempDir()})
	if err != nil {
		tb.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 16})
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\n"+
			"RequestCpus = %d\nCmd = \"/home/user%d/run.sh\"\nIwd = \"/scratch/run%d\"\n",
			i, i%10, 1+i%5, 1024+(i%32)*512, 1+i%8, i%500, i)
		switch {
		case i%97 == 3: // absent: the group is `undefined`
		case i%97 == 11:
			src += "Owner = 42\n" // a number in a string column: escapes into the cold tail
		case i%89 == 5:
			src += "Owner = \"undefined\"\n" // renders like the absent ones: one group, not two
		default:
			src += fmt.Sprintf("Owner = \"user%d\"\n", (i/50)%40)
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

// TestArchiveGroupedStringColumn is both halves of the contract in one loop: every case must be SERVED
// from the columns (ok=true from GroupedFromColumns, which is the only way to tell a pushdown from a
// silent fall back to the scan) and must give exactly what the scan gives.
func TestArchiveGroupedStringColumn(t *testing.T) {
	cat, a := groupStrFixture(t, 6000)
	defer cat.Close()

	countStar := []AggSpec{{Func: AggCount, Arg: "*"}}
	withStats := []AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggMin, Arg: "RequestMemory"},
		{Func: AggMax, Arg: "RequestMemory"}, {Func: AggAvg, Arg: "RequestMemory"}}
	for _, tc := range []struct {
		constraint string
		group      string
		aggs       []AggSpec
		why        string
	}{
		{"RequestMemory > 4096", "Owner", countStar, "numeric predicate: the predicate-analysis tier"},
		{"", "Owner", countStar, "match-all: the GroupStatsAll entry point, which has no predicate"},
		{"true", "Owner", countStar, "match-all spelled as a predicate: the vector tier"},
		{`Owner == "user3"`, "Cmd", countStar, "a string predicate routes through the vector tier"},
		{"RequestMemory > RequestCpus", "Owner", countStar, "attribute to attribute: the vector tier"},
		{"RequestMemory > 4096", "Iwd", countStar, "distinct per record: no dictionary, positional read"},
		{"", "Cmd", countStar, "a second dictionary column, never absent"},
		{"RequestCpus >= 4", "Owner", withStats, "value aggregates beside the string group"},
		{"ClusterId > 99999999", "Owner", countStar, "nothing matches: no groups at all"},
	} {
		groupCols := []GroupCol{{Attr: tc.group}}
		got, served := a.groupedFromColumns(tc.constraint, groupCols, tc.aggs)
		if !served {
			t.Errorf("%q GROUP BY %s: declined, but this is served from the columns (%s)",
				tc.constraint, tc.group, tc.why)
			continue
		}
		want := scanAggregate(t, a, tc.constraint, groupCols, tc.aggs)
		gotM, wantM := groupedRows(t, got), groupedRows(t, want)
		if len(gotM) != len(wantM) {
			t.Errorf("%q GROUP BY %s: %d groups, scan found %d",
				tc.constraint, tc.group, len(gotM), len(wantM))
		}
		for g, v := range wantM {
			if fmt.Sprint(gotM[g]) != fmt.Sprint(v) {
				t.Errorf("%q GROUP BY %s: group %q = %v, scan = %v", tc.constraint, tc.group, g, gotM[g], v)
			}
		}
		for g := range gotM {
			if _, ok := wantM[g]; !ok {
				t.Errorf("%q GROUP BY %s: group %q is not in the scan's result at all",
					tc.constraint, tc.group, g)
			}
		}
	}
}

// TestArchiveGroupedStringColumnEdgeGroups pins the three groups that are easy to get wrong and that a
// comparison of totals would not notice: the records with no Owner at all, the ones holding a number
// where a string was expected, and the merge of the absent ones with the literal text "undefined".
func TestArchiveGroupedStringColumnEdgeGroups(t *testing.T) {
	cat, a := groupStrFixture(t, 6000)
	defer cat.Close()
	countStar := []AggSpec{{Func: AggCount, Arg: "*"}}
	rows, served := a.groupedFromColumns("", []GroupCol{{Attr: "Owner"}}, countStar)
	if !served {
		t.Fatal("GROUP BY Owner over every record declined")
	}
	got := rowMap(t, rows)

	// 6000 records: one in 97 has no Owner, and of the rest one in 89 holds the text "undefined".
	// Both render "undefined", and the scan makes ONE group of them -- if the columnar path emitted
	// two rows under one label, rowMap would already have failed on the duplicate.
	absent, literal, numeric := 0, 0, 0
	for i := 0; i < 6000; i++ {
		switch {
		case i%97 == 3:
			absent++
		case i%97 == 11:
			numeric++
		case i%89 == 5:
			literal++
		}
	}
	if absent == 0 || literal == 0 || numeric == 0 {
		t.Fatalf("fixture lost its edge cases: absent=%d literal=%d numeric=%d", absent, literal, numeric)
	}
	if got["undefined"] != strconv.Itoa(absent+literal) {
		t.Errorf("group \"undefined\" counted %q, want %d (%d records with no Owner merged with %d "+
			"holding the text)", got["undefined"], absent+literal, absent, literal)
	}
	// A number stored in a string column escapes the column; it still groups, under its own text.
	if got["42"] != strconv.Itoa(numeric) {
		t.Errorf("group \"42\" counted %q, want %d", got["42"], numeric)
	}
	// And the whole thing still partitions the table.
	total := 0
	for _, v := range got {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if total != 6000 {
		t.Errorf("groups sum to %d, want 6000", total)
	}
}

// TestArchiveGroupedStringColumnDeclinesExpressions is the inverse pin: a group value that only
// evaluation can produce has to give the query back, because the scan makes a group out of it and a
// column read cannot. Serving it would return a result with groups silently missing.
func TestArchiveGroupedStringColumnDeclinesExpressions(t *testing.T) {
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6000; i++ {
		src := fmt.Sprintf("ClusterId = %d\nRequestMemory = %d\nCmd = \"/home/user%d/run.sh\"\n",
			i, 1024+(i%32)*512, i%500)
		if i%97 == 3 {
			src += "Owner = strcat(\"user\", ClusterId % 7)\n"
		} else {
			src += fmt.Sprintf("Owner = \"user%d\"\n", i%500)
		}
		ad, err := classad.ParseOld(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	if !a.BuildAndEnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
	countStar := []AggSpec{{Func: AggCount, Arg: "*"}}
	if _, served := a.groupedFromColumns("true", []GroupCol{{Attr: "Owner"}}, countStar); served {
		t.Error("served a group column holding expressions; the groups they form would be missing")
	}
	// The decline is about the VALUES, not about the shape: the same query on the column next to it,
	// in the same table, is served.
	if _, served := a.groupedFromColumns("true", []GroupCol{{Attr: "Cmd"}}, countStar); !served {
		t.Error("declined a plain string group column in the same table, so the decline above says " +
			"nothing about the expressions")
	}
	// Whichever way it is answered, the answer is the scan's.
	rows, err := a.AggregateCols("true", []GroupCol{{Attr: "Owner"}}, countStar)
	if err != nil {
		t.Fatal(err)
	}
	want := scanAggregate(t, a, "true", []GroupCol{{Attr: "Owner"}}, countStar)
	gotM, wantM := rowMap(t, rows), rowMap(t, want)
	if len(gotM) != len(wantM) {
		t.Fatalf("%d groups, scan found %d", len(gotM), len(wantM))
	}
	for g, v := range wantM {
		if gotM[g] != v {
			t.Errorf("group %q = %q, scan %q", g, gotM[g], v)
		}
	}
}

// BenchmarkArchiveGroupedStringColumn is the measurement the change exists for: the same GROUP BY on a
// string column, from the columns and from the scan behind them.
func BenchmarkArchiveGroupedStringColumn(b *testing.B) {
	cat, a := groupStrFixture(b, 60000)
	defer cat.Close()
	groupCols := []GroupCol{{Attr: "Owner"}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}
	if _, ok := a.groupedFromColumns("RequestMemory > 4096", groupCols, aggs); !ok {
		b.Fatal("not served columnar")
	}
	b.Run("columnar", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, ok := a.groupedFromColumns("RequestMemory > 4096", groupCols, aggs); !ok {
				b.Fatal("declined")
			}
		}
	})
	b.Run("scan", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			attrs, groupCol, aggCol := AggProjection(groupCols, aggs)
			seq, err := a.QueryProject("RequestMemory > 4096", attrs)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := AggregateValues(seq, attrs, groupCols, aggs, groupCol, aggCol, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}
