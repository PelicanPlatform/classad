package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestTopKCurrentTimeOneInstant: a constraint reading CurrentTime gets its cutoff from the columns
// (its CurrentTime-folded form), and the projection pass must evaluate it at the same instant. Here the
// clock crosses a second between the two passes; recompiling the constraint from text would pin the
// later time, drop the oldest in-window record, and return k-1 rows for an ascending top-k.
func TestTopKCurrentTimeOneInstant(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 12, ValueAttrs: []string{"CompletionDate"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	const n = 200
	for i := 0; i < n; i++ {
		// The oldest records sit just inside a 100-second window.
		if e := hist.AppendOld(fmt.Sprintf("CompletionDate = %d\nId = %d", now-99+int64(i%50), i)); e != nil {
			t.Fatal(e)
		}
	}
	hist.BuildAndEnableSchemaScan(4096, 8)
	const k = 3
	served := false
	q, err := topKAugment(`CompletionDate > CurrentTime - 100`, "CompletionDate", false, k,
		func(q *vm.Query) (float64, int, bool) {
			th, seen, ok := hist.a.TopKOrderThreshold(q, "CompletionDate", false, k, nil)
			served = ok
			time.Sleep(time.Until(time.Unix(time.Now().Unix()+1, 50e6)))
			return th, seen, ok
		})
	if err != nil {
		t.Fatal(err)
	}
	if !served {
		t.Fatal("the columnar cutoff declined a CurrentTime constraint")
	}
	rows := topKResult(hist.a.QueryProjectStats(q, []string{"Id", "CompletionDate"}, nil), 1, false, k, false)
	if len(rows) != k {
		t.Fatalf("got %d rows, want %d", len(rows), k)
	}
}

// TestArchiveConstrainedGroupCountTier: a constrained COUNT(*) GROUP BY on a categorical-indexed
// attribute goes to the columns first and reports TierColumnar; without an accelerator the zone
// verdicts answer it and it reports TierIndex. Both give the same counts.
func TestArchiveConstrainedGroupCountTier(t *testing.T) {
	build := func(columnar bool) *ArchiveTable {
		cat, err := OpenCatalog(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		hist, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 13,
			CategoricalAttrs: []string{"Owner"}, ZoneAttrs: []string{"ClusterId"}})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 1500; i++ {
			if e := hist.AppendOld(fmt.Sprintf("ClusterId = %d\nOwner = \"u%d\"", i, i%7)); e != nil {
				t.Fatal(e)
			}
		}
		hist.Reindex()
		if columnar {
			hist.BuildAndEnableSchemaScan(4096, 8)
		}
		return hist
	}
	groups := []GroupCol{{Attr: "Owner"}}
	count := []AggSpec{{Func: AggCount, Arg: "*"}}
	var rows [2][]AggRow
	for i, tc := range []struct {
		columnar bool
		want     collections.AggTier
	}{{true, collections.TierColumnar}, {false, collections.TierIndex}} {
		var st collections.ScanStats
		r, err := build(tc.columnar).AggregateColsStats("ClusterId >= 600", groups, count, &st)
		if err != nil {
			t.Fatal(err)
		}
		if st.Tier != tc.want {
			t.Errorf("columnar=%v: tier %v, want %v", tc.columnar, st.Tier, tc.want)
		}
		rows[i] = r
	}
	if fmt.Sprint(rows[0]) != fmt.Sprint(rows[1]) {
		t.Errorf("columnar %v, index %v", rows[0], rows[1])
	}
}
