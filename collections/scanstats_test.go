package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestScanStatsColumnarVsReassembled verifies the counters distinguish the two shapes the
// archive puzzle is about: a columnarized scan decides the WHERE from columns and reassembles
// only matches, while an un-columnarized scan reassembles what it visits.
func TestScanStatsColumnarVsReassembled(t *testing.T) {
	build := func(columnar bool) *Archive {
		a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir(), SegmentSize: 1 << 16, ValueAttrs: []string{"ClusterId"}})
		if err != nil {
			t.Fatal(err)
		}
		a.c.colBudget = 1 << 30 // columnarize everything in one pass when enabled
		for i := 0; i < 20000; i++ {
			proc := i % 5
			if i%1000 == 0 {
				proc = 5 // rare match
			}
			ad, _ := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\n", i, proc, i%50))
			_ = a.Append(ad)
		}
		if columnar {
			for _, e := range []string{"ProcId >= 0"} {
				q, _ := vm.Parse(e)
				for k := 0; k < 20; k++ {
					for range a.c.Query(q) {
					}
				}
			}
			if !a.c.BuildAndEnableSchemaScan(4000, 8) {
				t.Fatal("schema scan")
			}
		}
		return a
	}

	q, _ := vm.Parse("ProcId == 5")
	run := func(a *Archive) ScanStats {
		var st ScanStats
		for range a.QueryRawProjectedStats(q, []string{"ClusterId", "ProcId"}, false, false, &st) {
		}
		return st
	}

	col := run(build(true))
	t.Logf("columnar: %+v", col)
	if col.RecordsColumnDecided < col.RecordsVisited/2 {
		t.Errorf("columnar scan: expected most records decided from columns, got %d/%d", col.RecordsColumnDecided, col.RecordsVisited)
	}
	// A columnar scan reassembles only matches plus the still-in-RAM active segment (not yet
	// columnarized) -- a small fraction, not the whole visited set.
	if col.RecordsReassembled > col.RecordsVisited/10 {
		t.Errorf("columnar scan reassembled %d of %d visited -- expected a small fraction", col.RecordsReassembled, col.RecordsVisited)
	}

	un := run(build(false))
	t.Logf("un-columnar: %+v", un)
	if un.RecordsColumnDecided != 0 {
		t.Errorf("un-columnar scan should decide nothing from columns, got %d", un.RecordsColumnDecided)
	}
	if un.RecordsReassembled < un.RecordsVisited/2 {
		t.Errorf("un-columnar scan should reassemble most records, got %d/%d", un.RecordsReassembled, un.RecordsVisited)
	}
	if col.RowsMatched != un.RowsMatched {
		t.Errorf("both should match the same rows: columnar=%d un=%d", col.RowsMatched, un.RowsMatched)
	}
}
