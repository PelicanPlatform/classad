package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// HOW WIDE THE AD IS DECIDES WHETHER THE PROJECTION PLAN MATTERS AT ALL.
//
// BenchmarkRawWireRefsNoWhere measures the same two spellings over archiveCountFixture, whose ads
// carry EIGHT attributes. Projecting three of eight leaves almost nothing to save, so that fixture
// cannot show what the projection plan buys however the arms are arranged -- which is why the
// numbers there came out small and kept inverting on whatever else differed between the paths.
//
// A real completed-job ad is not eight attributes. The job ads this read exists to serve run 45 to
// 80, and projecting three of eighty is where narrowing is the whole cost of the read. This
// benchmark holds the row count and the projection fixed and sweeps ONLY the width, so the thing
// that changes between arms is how much of each record the read has to touch.
//
// Both arms return byte-for-byte the same rows; that is asserted before anything is timed.
//
// MEASURED at width=80, 20k rows, 3 iterations, before and after giving the unconstrained
// spelling a projection plan, with a BEFORE round repeated after the AFTER round:
//
//	arm                     time              B/op       allocs/op
//	true, before            107.4 / 104.2 ms  353.7 MB   1,848,807
//	true, after              13.0 ms            3.72 MB     20,562
//	predicate (control)      13.4 / 14.5 ms     3.70 MB     20,718
//
// The control does not move, so the change reaches only the spelling it was aimed at. Lean on
// the allocation column rather than the times: this machine runs other work, and allocations
// are exact whatever the load is. 1.85M allocations for 20k rows is one per record per
// attribute -- the signature of reassembling all eighty and keeping three.
func BenchmarkRawWireRefsByWidth(b *testing.B) {
	for _, width := range []int{8, 40, 80} {
		cat, a := wideArchiveFixture(b, 20000, width)
		_ = cat
		proj := []string{"ClusterId", "Owner", "RequestMemory"}

		// Agreement first: the two spellings must ship identical bytes, or the faster one is
		// faster for the wrong reason.
		drain := func(constraint string) (rows, bytes int) {
			seq, err := a.QueryRawWireRefs(constraint, proj, false)
			if err != nil {
				b.Fatal(err)
			}
			for row := range seq {
				rows++
				bytes += len(row)
			}
			return
		}
		r1, b1 := drain("true")
		r2, b2 := drain("JobStatus >= 0")
		if r1 != r2 || b1 != b2 {
			b.Fatalf("width=%d: the two spellings disagree: true=%d rows/%d bytes, predicate=%d rows/%d bytes",
				width, r1, b1, r2, b2)
		}

		for _, arm := range []struct{ name, constraint string }{
			{"true-shortcut", "true"},
			{"equivalent-predicate", "JobStatus >= 0"},
		} {
			b.Run(fmt.Sprintf("width=%d/%s", width, arm.name), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					drain(arm.constraint)
				}
			})
		}
	}
}

// wideArchiveFixture is archiveCountFixture with the ad width as a knob, columnarized so the
// projection plan has columns to project FROM. BuildAndEnableSchemaScan is asserted rather than
// assumed: with no sealed segment it declines, and both arms would then measure the record path
// while the name said columnar.
func wideArchiveFixture(tb testing.TB, n, width int) (*Catalog, *ArchiveTable) {
	tb.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: tb.TempDir()})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 20})
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < n; i++ {
		var sb strings.Builder
		fmt.Fprintf(&sb, `[ ClusterId = %d; ProcId = 0; Owner = "user%02d"; JobStatus = 4; RequestMemory = %d`,
			i, i%32, 1024+(i%8)*512)
		for k := 0; k < width-5; k++ {
			fmt.Fprintf(&sb, `; HAttr%02d = "a completed job attribute value"`, k)
		}
		sb.WriteString(" ]")
		ad, perr := classad.Parse(sb.String())
		if perr != nil {
			tb.Fatal(perr)
		}
		if aerr := a.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	if !a.BuildAndEnableSchemaScan(8000, 16) {
		tb.Skip("no sealed segments to columnarize; this benchmark would measure the record path")
	}
	return cat, a
}
