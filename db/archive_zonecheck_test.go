package db

import (
	"fmt"
	"testing"
)

// zoneCheckCount is SELECT COUNT(*) FROM history WHERE constraint, as htcondordb-cli runs it.
func zoneCheckCount(t *testing.T, a *ArchiveTable, constraint string) string {
	t.Helper()
	rows, err := a.Aggregate(constraint, nil, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Values) != 1 {
		t.Fatalf("%s: rows %v", constraint, rows)
	}
	return rows[0].Values[0]
}

// The production check: a zone-prunable predicate and the same predicate written so nothing
// can prune it (`attr + 0`, which is no `attr op literal` conjunct) must count alike. They did
// not after the htcondordb shape of configuration change -- a value index added to a live
// history archive, then a restart -- because zone maps persisted by intern id were read back
// against the next process's ids.
func TestArchiveZoneCheckAfterRuntimeIndex(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 16 << 10,
		ZoneAttrs: []string{"CompletionDate", "EnteredHistoryTime"}})
	if err != nil {
		t.Fatal(err)
	}
	app := func(from, to int) {
		for i := from; i < to; i++ {
			if aerr := a.AppendOld(fmt.Sprintf("ID = %d\nClusterId = %d\nProcId = %d\nCompletionDate = %d\n"+
				"EnteredHistoryTime = %d\nPad = \"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx%d\"",
				i, i/40, i%40, 1700000000+i, 1700000005+i, i)); aerr != nil {
				t.Fatal(aerr)
			}
		}
	}
	app(0, 2000)
	if !a.AddIndex(nil, []string{"ClusterId"}) {
		t.Fatal("index not added")
	}
	app(2000, 4000)
	if cerr := cat.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if cat, err = OpenCatalog(dir); err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, ok := cat.ArchiveTable("history")
	if !ok {
		t.Fatal("history not reopened")
	}
	for _, pair := range [][2]string{
		{"ClusterId == 70", "ClusterId + 0 == 70"},
		{"ClusterId >= 90", "ClusterId + 0 >= 90"},
		{"CompletionDate < 1700000300", "CompletionDate + 0 < 1700000300"},
		{"CompletionDate >= 1700003500", "CompletionDate + 0 >= 1700003500"},
		{"EnteredHistoryTime >= 1700003000", "EnteredHistoryTime + 0 >= 1700003000"},
	} {
		if pruned, unpruned := zoneCheckCount(t, a, pair[0]), zoneCheckCount(t, a, pair[1]); pruned != unpruned {
			t.Errorf("%s counts %s, %s counts %s", pair[0], pruned, pair[1], unpruned)
		}
	}
}
