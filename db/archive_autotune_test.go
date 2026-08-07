package db

import (
	"fmt"
	"slices"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func fillArchive(t *testing.T, a *ArchiveTable, n int) {
	t.Helper()
	for i := range n {
		ad := classad.New()
		ad.Set("ClusterId", int64(i))
		ad.Set("Owner", fmt.Sprintf("user%d", i%7))
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
}

func drainArchive(t *testing.T, a *ArchiveTable, constraint string) {
	t.Helper()
	seq, err := a.Query(constraint)
	if err != nil {
		t.Fatalf("query %q: %v", constraint, err)
	}
	for range seq {
	}
}

// TestArchiveAutoTuneAddsFromDemand: an archive that has been queried on an unindexed
// attribute enough times gets the index, without anyone asking.
func TestArchiveAutoTuneAddsFromDemand(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	fillArchive(t, a, 500)

	if cat, val := a.IndexedAttrs(); len(cat) != 0 || len(val) != 0 {
		t.Fatalf("archive starts with indexes %v/%v, want none", cat, val)
	}
	for range 10 {
		drainArchive(t, a, `Owner == "user3"`)
	}
	res := a.AutoTune(AutoTuneOptions{MinDemand: 5, Reindex: true})
	if !res.Changed {
		t.Fatalf("AutoTune made no change; suggestions were %v", res.Changes)
	}
	catAttrs, _ := a.IndexedAttrs()
	if !slices.Contains(catAttrs, "Owner") {
		t.Errorf("categorical indexes = %v, want Owner", catAttrs)
	}
}

// TestArchiveAutoTuneBelowThreshold: demand under the threshold is not acted on. Adding an
// archive index means reading history back through the decompressor, so a handful of
// ad-hoc queries must not be enough to trigger it.
func TestArchiveAutoTuneBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	fillArchive(t, a, 500)
	for range 3 {
		drainArchive(t, a, `Owner == "user3"`)
	}
	if res := a.AutoTune(AutoTuneOptions{MinDemand: 100, Reindex: true}); res.Changed {
		t.Errorf("AutoTune acted on 3 queries against a threshold of 100: %v", res.Changes)
	}
	if c, v := a.IndexedAttrs(); len(c) != 0 || len(v) != 0 {
		t.Errorf("indexes = %v/%v, want none", c, v)
	}
}

// TestArchiveAutoTunePersistsAcrossReopen is the one that matters for cost. An auto-added
// index that is not written to the archive config is discarded on reopen along with the
// backfill that produced it -- and re-added as soon as demand accrues again, paying the
// same backfill once per restart, forever.
func TestArchiveAutoTunePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	fillArchive(t, a, 500)
	for range 10 {
		drainArchive(t, a, `Owner == "user3"`)
	}
	if res := a.AutoTune(AutoTuneOptions{MinDemand: 5, Reindex: true}); !res.Changed {
		t.Fatal("AutoTune made no change")
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	a2, ok := cat2.ArchiveTable("history")
	if !ok {
		t.Fatal("archive table missing after reopen")
	}
	catAttrs, _ := a2.IndexedAttrs()
	if !slices.Contains(catAttrs, "Owner") {
		t.Errorf("categorical indexes after reopen = %v, want Owner retained", catAttrs)
	}
	// And the index actually answers: the reopened archive must still return the rows.
	seq, err := a2.Query(`Owner == "user3"`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	if want := 500 / 7; n < want-1 || n > want+1 {
		t.Errorf("query after reopen returned %d rows, want ~%d", n, want)
	}
}

// autoIndexedArchive builds an archive whose Owner index was added by the tuner, then
// reopens it. The reopen is what makes the index read as UNUSED: demand is not checkpointed
// here, so the fresh process has none, which is the state an auto-drop acts on.
func autoIndexedArchive(t *testing.T, dir string) *ArchiveTable {
	t.Helper()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	fillArchive(t, a, 500)
	for range 10 {
		drainArchive(t, a, `Owner == "user3"`)
	}
	if res := a.AutoTune(AutoTuneOptions{MinDemand: 5, Reindex: true}); !res.Changed {
		t.Fatal("setup: AutoTune added nothing")
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat2.Close() })
	a2, ok := cat2.ArchiveTable("history")
	if !ok {
		t.Fatal("setup: archive missing after reopen")
	}
	return a2
}

// TestArchiveAutoIndexProvenancePersists: an index the tuner added must come back from a
// restart still marked auto. Restored as human-created it would be exempt from trimming and
// from any future auto-drop -- permanently, on the strength of nothing but a restart.
func TestArchiveAutoIndexProvenancePersists(t *testing.T) {
	a := autoIndexedArchive(t, t.TempDir())
	if got := a.AutoIndexNames(); !slices.Contains(got, "Owner") {
		t.Errorf("auto index names after reopen = %v, want Owner", got)
	}
}

// TestArchiveAutoTuneNeverDrops: DropUnused is forced off whatever the caller passes.
// Dropping an archive index is nearly free and re-adding it costs a backfill, so the
// asymmetry runs the wrong way for acting on an absence of queries.
//
// The raw collections path DOES drop in this exact state (verified while writing this), so
// the assertion is about the archive rule, not about the drop being unreachable.
func TestArchiveAutoTuneNeverDrops(t *testing.T) {
	a := autoIndexedArchive(t, t.TempDir())
	a.AutoTune(AutoTuneOptions{
		MinDemand: 5, Reindex: true,
		DropUnused:           true, // asked for, and must be ignored
		DropUnusedMinWindow:  -1,   // waive every generic guard, leaving only the archive rule
		DropUnusedMinQueries: -1,
	})
	if catAttrs, _ := a.IndexedAttrs(); !slices.Contains(catAttrs, "Owner") {
		t.Errorf("Owner index dropped: indexes = %v", catAttrs)
	}
}
