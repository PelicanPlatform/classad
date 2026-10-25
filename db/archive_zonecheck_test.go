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

// The same check over records whose zone attribute is not a literal: an expression evaluating far
// past every literal, and a negative value only evaluation can give. Segment zones once skipped
// both, so the segments holding them were pruned for any range they fell in but the literals did
// not -- here every one, since both sit in the oldest, sealed segment.
//
// The negative one is written as arithmetic on purpose. It used to be the constant `-5`, which was
// an expression only because old-ClassAd text had no negative literal and the encoder left the
// unary minus in place; now that the encoder folds it, `-5` is a literal and the record stopped
// testing the non-literal path without the test noticing.
func TestArchiveZoneCheckExpressionValued(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ArchiveConfig{SegmentSize: 16 << 10, CategoricalAttrs: []string{"Owner"},
		ZoneAttrs: []string{"CompletionDate"}}
	a, err := cat.CreateArchiveTable("history", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 1000 {
		cd := fmt.Sprintf("%d", 1700000000+i)
		switch i {
		case 3:
			cd = "1700000000 + 100000"
		case 5:
			cd = "0 - 5" // -5, as a value no zone can read without evaluating
		}
		if aerr := a.AppendOld(fmt.Sprintf("ID = %d\nOwner = \"u%d\"\nCompletionDate = %s\n"+
			"Pad = \"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx%d\"", i, i%3, cd, i)); aerr != nil {
			t.Fatal(aerr)
		}
	}
	check := func(stage string) {
		t.Helper()
		for _, pair := range [][2]string{
			{"CompletionDate > 1700050000", "CompletionDate + 0 > 1700050000"},
			{"CompletionDate == 1700100000", "CompletionDate + 0 == 1700100000"},
			{"CompletionDate < 0", "CompletionDate + 0 < 0"},
			{"CompletionDate == -5", "CompletionDate + 0 == -5"},
		} {
			pruned, unpruned := zoneCheckCount(t, a, pair[0]), zoneCheckCount(t, a, pair[1])
			if pruned != unpruned || unpruned != "1" {
				t.Errorf("%s: %s counts %s, %s counts %s, want 1", stage, pair[0], pruned, pair[1], unpruned)
			}
			got, gerr := a.Aggregate(pair[0], []string{"Owner"}, []AggSpec{{Func: AggCount, Arg: "*"}})
			if gerr != nil {
				t.Fatal(gerr)
			}
			want, werr := a.Aggregate(pair[1], []string{"Owner"}, []AggSpec{{Func: AggCount, Arg: "*"}})
			if werr != nil {
				t.Fatal(werr)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) || len(want) != 1 {
				t.Errorf("%s: GROUP BY Owner WHERE %s: %v, want %v", stage, pair[0], got, want)
			}
		}
	}
	check("written")
	if cerr := cat.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if cat, err = OpenCatalog(dir); err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	var ok bool
	if a, ok = cat.ArchiveTable("history"); !ok {
		t.Fatal("history not reopened")
	}
	check("reopened")
}
