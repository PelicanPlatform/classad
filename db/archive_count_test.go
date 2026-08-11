package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A constrained COUNT(*) on an ARCHIVE reached neither index-answering (which only handles the
// unconstrained case, since a constraint cannot be attributed to a whole segment's stored count) nor
// ColumnarAggregate (which declines COUNT(*) for want of an attribute to aggregate). So the shape a
// history table is asked most fell to a record scan even with an accelerator over its segments.

func archiveCountFixture(tb testing.TB, n int) (*Catalog, *ArchiveTable) {
	tb.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: tb.TempDir()})
	if err != nil {
		tb.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{
		SegmentSize: 1 << 16,
		ValueAttrs:  []string{"ClusterId"},
	})
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ad, err := classad.ParseOld(fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\nRequestCpus = %d\n"+
				"Owner = \"user%d\"\nCmd = \"/home/user%d/run.sh\"\nCompletionDate = %d",
			i, i%10, 1+i%5, 1024+(i%32)*512, 1+i%8, i%512, i%512, 1700000000+i))
		if err != nil {
			tb.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			tb.Fatal(err)
		}
	}
	// Build the accelerator the way archive maintenance does.
	if !a.BuildAndEnableSchemaScan(4000, 8) {
		tb.Skip("no sealed segments to accelerate")
	}
	return cat, a
}

// scanCount is the reference: count by iterating the matching records.
func scanCount(tb testing.TB, a *ArchiveTable, constraint string) int {
	tb.Helper()
	seq, err := a.Query(constraint)
	if err != nil {
		tb.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	return n
}

// TestArchiveConstrainedCountMatchesScan is the equivalence gate over the shapes the columnar count
// now serves, plus shapes it must decline and hand back to the scan.
func TestArchiveConstrainedCountMatchesScan(t *testing.T) {
	cat, a := archiveCountFixture(t, 5000)
	defer cat.Close()

	for _, constraint := range []string{
		"RequestMemory > 4096",
		"RequestMemory > 4096 && RequestCpus >= 4",
		"JobStatus == 4 && RequestMemory > 2048 && ProcId < 5",
		"ClusterId >= 1000 && ClusterId < 1100",
		"ClusterId > 9999999",         // nothing matches
		`Owner == "user3"`,            // a string field: must decline, and still be right
		"RequestMemory > RequestCpus", // no literal: must decline
		"true",                        // match-all: answered from the stored count
	} {
		want := scanCount(t, a, constraint)
		rows, err := a.AggregateCols(constraint, nil, []AggSpec{{Func: AggCount, Arg: "*"}})
		if err != nil {
			t.Fatalf("%s: %v", constraint, err)
		}
		if len(rows) != 1 || len(rows[0].Values) != 1 {
			t.Fatalf("%s: unexpected shape %+v", constraint, rows)
		}
		var got int
		if _, err := fmt.Sscanf(rows[0].Values[0], "%d", &got); err != nil {
			t.Fatalf("%s: unparsable count %q", constraint, rows[0].Values[0])
		}
		if got != want {
			t.Errorf("%s: COUNT(*) = %d, scan = %d", constraint, got, want)
		}
		_, served := a.CountConstraint(constraint)
		t.Logf("%-52s count=%-6d columnar=%v", constraint, got, served)
	}
}

// TestArchiveCountConstraintDeclinesGracefully pins that a declined constraint still produces the
// right answer through the scan, since the whole design rests on falling back rather than guessing.
func TestArchiveCountConstraintDeclinesGracefully(t *testing.T) {
	cat, a := archiveCountFixture(t, 2000)
	defer cat.Close()
	for _, constraint := range []string{`Owner == "user3"`, "RequestMemory > RequestCpus"} {
		if _, served := a.CountConstraint(constraint); served {
			t.Errorf("%s: expected the columnar count to decline", constraint)
		}
		rows, err := a.AggregateCols(constraint, nil, []AggSpec{{Func: AggCount, Arg: "*"}})
		if err != nil {
			t.Fatal(err)
		}
		var got int
		fmt.Sscanf(rows[0].Values[0], "%d", &got)
		if want := scanCount(t, a, constraint); got != want {
			t.Errorf("%s: fell back to the scan and got %d, want %d", constraint, got, want)
		}
	}
}

// TestArchiveCountFilterNotServed guards the FILTER carve-out: a per-aggregate filter must not be
// answered from a count that knows nothing about it.
func TestArchiveCountFilterNotServed(t *testing.T) {
	cat, a := archiveCountFixture(t, 2000)
	defer cat.Close()
	rows, err := a.AggregateCols("RequestMemory > 2048", nil,
		[]AggSpec{{Func: AggCount, Arg: "*", Filter: "RequestCpus >= 4"}})
	if err != nil {
		t.Fatal(err)
	}
	var got int
	fmt.Sscanf(rows[0].Values[0], "%d", &got)
	want := scanCount(t, a, "RequestMemory > 2048 && RequestCpus >= 4")
	if got != want {
		t.Errorf("filtered COUNT(*) = %d, want %d: the filter was ignored", got, want)
	}
}

// BenchmarkArchiveConstrainedCount measures what the archive gains.
func BenchmarkArchiveConstrainedCount(b *testing.B) {
	cat, a := archiveCountFixture(b, 60000)
	defer cat.Close()
	// Multi-field conjunctions become columnar-servable with the N-conjunct column scan; on a base
	// without it they decline, so skip rather than fail -- the point here is the archive routing, not
	// which predicate shapes the collection can serve.
	for _, constraint := range []string{
		"RequestMemory > 4096",
		"ClusterId >= 1000 && ClusterId < 1100",
		"RequestMemory > 4096 && RequestCpus >= 4",
	} {
		if _, ok := a.CountConstraint(constraint); !ok {
			b.Logf("skipping %q: the columnar count declines it on this base", constraint)
			continue
		}
		b.Run("columnar/"+constraint, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				a.CountConstraint(constraint)
			}
		})
		b.Run("scan/"+constraint, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				seq, err := a.Query(constraint)
				if err != nil {
					b.Fatal(err)
				}
				n := 0
				for range seq {
					n++
				}
			}
		})
	}
}
