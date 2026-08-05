package db

import (
	"fmt"
	"slices"
	"testing"
)

// TestArchiveIndexConfigPersists covers the reopen half of a runtime index change on an
// archive. archiveconfig.json is authoritative on reopen, so an AddIndex that does not
// fold its result back into the saved config is silently undone by a restart -- and
// because the on-disk sidecars were rebuilt under the wider spec, reopening under the
// narrower creation-time spec discards the rebuild AddIndex just paid for (a full
// decompress of the archive). Assert the index set survives, and that re-asserting the
// same index after a restart is a no-op rather than a second backfill.
func TestArchiveIndexConfigPersists(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{
		ValueAttrs: []string{"ClusterId"},
		ZoneAttrs:  []string{"CompletionDate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	const n = 200
	for i := 0; i < n; i++ {
		if err := hist.AppendOld(fmt.Sprintf(
			"ClusterId = %d\nCompletionDate = %d\nOwner = %q", i, 1000+i, fmt.Sprintf("user%d", i%4),
		)); err != nil {
			t.Fatal(err)
		}
	}

	// Owner is not indexed at creation -- this is the state a deployed archive is in.
	if cat0, _ := hist.IndexedAttrs(); slices.Contains(cat0, "Owner") {
		t.Fatalf("Owner unexpectedly indexed at creation: %v", cat0)
	}
	if !hist.AddIndex([]string{"Owner"}, nil) {
		t.Fatal("AddIndex(Owner) reported no change")
	}
	catAttrs, valAttrs := hist.IndexedAttrs()
	if !slices.Contains(catAttrs, "Owner") {
		t.Fatalf("after AddIndex, categorical = %v, want it to contain Owner", catAttrs)
	}
	if !slices.Contains(valAttrs, "ClusterId") {
		t.Fatalf("after AddIndex, value = %v, want it to still contain ClusterId", valAttrs)
	}
	cat.Close()

	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	h2, ok := cat2.ArchiveTable("history")
	if !ok {
		t.Fatal("archive not recovered on reopen")
	}

	catAttrs2, valAttrs2 := h2.IndexedAttrs()
	if !slices.Contains(catAttrs2, "Owner") {
		t.Errorf("after reopen, categorical = %v, want it to contain Owner (index add was not persisted)", catAttrs2)
	}
	if !slices.Contains(valAttrs2, "ClusterId") {
		t.Errorf("after reopen, value = %v, want it to still contain ClusterId", valAttrs2)
	}

	// Re-asserting the same index must be a no-op. If it reports a change, a daemon that
	// reconciles its configured attributes at startup would re-run the full backfill on
	// every restart.
	if h2.AddIndex([]string{"Owner"}, nil) {
		t.Error("re-adding an already-persisted index reported a change (would re-backfill every restart)")
	}

	// The data is still queryable through the recovered index.
	rows, err := h2.Aggregate(`Owner == "user0"`, nil, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Values[0] != fmt.Sprint(n/4) {
		t.Errorf("count for user0 = %v, want %d", rows, n/4)
	}
}

// TestArchiveDropIndexPersists is the mirror: a runtime drop must not be resurrected by a
// restart either.
func TestArchiveDropIndexPersists(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{
		CategoricalAttrs: []string{"Owner", "AccountingGroup"},
		ZoneAttrs:        []string{"CompletionDate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := hist.AppendOld(fmt.Sprintf(
			"CompletionDate = %d\nOwner = %q\nAccountingGroup = \"grp\"", 1000+i, fmt.Sprintf("user%d", i%4),
		)); err != nil {
			t.Fatal(err)
		}
	}
	if !hist.DropIndex("AccountingGroup") {
		t.Fatal("DropIndex(AccountingGroup) reported no change")
	}
	cat.Close()

	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	h2, ok := cat2.ArchiveTable("history")
	if !ok {
		t.Fatal("archive not recovered on reopen")
	}
	catAttrs, _ := h2.IndexedAttrs()
	if slices.Contains(catAttrs, "AccountingGroup") {
		t.Errorf("after reopen, categorical = %v, want AccountingGroup dropped (drop was not persisted)", catAttrs)
	}
	if !slices.Contains(catAttrs, "Owner") {
		t.Errorf("after reopen, categorical = %v, want it to still contain Owner", catAttrs)
	}
}
