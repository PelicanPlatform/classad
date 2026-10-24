package db

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// hasFold reports whether names holds s case-insensitively (hot attributes are
// case-insensitive, and the set records whatever spelling it first saw).
func hasFold(names []string, s string) bool {
	return slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, s) })
}

// fillArchiveWide appends records carrying a distinctive attribute so a query against it
// registers read demand the hot-set refresh can rank.
func fillArchiveWide(t *testing.T, a *ArchiveTable, n int) {
	t.Helper()
	for i := range n {
		ad := classad.New()
		ad.Set("ClusterId", int64(i))
		ad.Set("Owner", fmt.Sprintf("user%d", i%7))
		ad.Set("GlobalJobId", fmt.Sprintf("ap1#%d.0#1700000000", i))
		ad.Set("ExitCode", int64(i%3))
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
}

func readArchiveConfig(t *testing.T, dir, table string) ArchiveConfig {
	t.Helper()
	// The catalog lays archives out one directory per table; find the one holding a config.
	var found ArchiveConfig
	var hits int
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() != archiveConfigFile {
			return nil
		}
		if !strings.Contains(path, table) {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if json.Unmarshal(data, &found) == nil {
			hits++
		}
		return nil
	})
	if hits != 1 {
		t.Fatalf("found %d %s files for table %q under %s, want 1", hits, archiveConfigFile, table, dir)
	}
	return found
}

// TestArchiveRefreshHotSetPersistsAcrossReopen is the test that matters: archiveconfig.json
// is authoritative on reopen, so a refreshed hot set that is not written back silently
// resets to the creation-time HotAttrs every time the daemon restarts -- and the refresh
// then has to rediscover the same set, forever, while every restart's worth of new segments
// is written under the wrong one. Only a reopen can show that, so this test closes the
// catalog.
func TestArchiveRefreshHotSetPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	fillArchiveWide(t, a, 300)
	if hasFold(a.HotAttrs(), "GlobalJobId") {
		t.Fatalf("GlobalJobId already hot before any refresh: %v", a.HotAttrs())
	}
	for range 10 {
		drainArchive(t, a, `GlobalJobId == "ap1#3.0#1700000000"`)
	}
	if n := a.RefreshHotSet(1000, 4); n == 0 {
		t.Fatal("RefreshHotSet chose nothing")
	}
	if !hasFold(a.HotAttrs(), "GlobalJobId") {
		t.Fatalf("hot set after refresh = %v, want the queried GlobalJobId", a.HotAttrs())
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
	if !hasFold(a2.HotAttrs(), "GlobalJobId") {
		t.Errorf("hot set after reopen = %v, want the refreshed GlobalJobId retained", a2.HotAttrs())
	}
}

// TestArchiveRefreshHotSetRecordsProvenance: the refreshed attributes are persisted as the
// tuner's (AutoHotAttrs), never folded into the operator's HotAttrs, and the operator's
// stay hot even though the sample's ranking did not reach them.
func TestArchiveRefreshHotSetRecordsProvenance(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{HotAttrs: []string{"ExitCode"}})
	if err != nil {
		t.Fatal(err)
	}
	fillArchiveWide(t, a, 300)
	// Read demand on GlobalJobId only. A refresh capped at one attribute therefore ranks
	// GlobalJobId and would, left alone, drop the configured ExitCode.
	for range 10 {
		drainArchive(t, a, `GlobalJobId == "ap1#3.0#1700000000"`)
	}
	if a.RefreshHotSet(1000, 1) == 0 {
		t.Fatal("RefreshHotSet chose nothing")
	}
	hot := a.HotAttrs()
	if !hasFold(hot, "ExitCode") {
		t.Errorf("hot set = %v, want the configured ExitCode still pinned", hot)
	}
	if !hasFold(hot, "GlobalJobId") {
		t.Errorf("hot set = %v, want the demand-ranked GlobalJobId", hot)
	}

	saved := readArchiveConfig(t, dir, "history")
	if !slices.Equal(saved.HotAttrs, []string{"ExitCode"}) {
		t.Errorf("persisted HotAttrs = %v, want the operator's [ExitCode] untouched", saved.HotAttrs)
	}
	if !hasFold(saved.AutoHotAttrs, "GlobalJobId") {
		t.Errorf("persisted AutoHotAttrs = %v, want the refreshed GlobalJobId", saved.AutoHotAttrs)
	}
	if hasFold(saved.AutoHotAttrs, "ExitCode") {
		t.Errorf("persisted AutoHotAttrs = %v, must not claim the operator's ExitCode", saved.AutoHotAttrs)
	}
}

// TestArchiveAddHotAttrsPersistsAsHuman: a named add is an operator action, so it joins
// HotAttrs (where a later refresh re-pins rather than recomputes it away) and survives a
// reopen.
func TestArchiveAddHotAttrsPersistsAsHuman(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	fillArchiveWide(t, a, 50)
	if !hasFold(a.AddHotAttrs("ExitCode"), "ExitCode") {
		t.Fatal("AddHotAttrs did not install ExitCode")
	}
	saved := readArchiveConfig(t, dir, "history")
	if !hasFold(saved.HotAttrs, "ExitCode") {
		t.Errorf("persisted HotAttrs = %v, want the added ExitCode", saved.HotAttrs)
	}
	if hasFold(saved.AutoHotAttrs, "ExitCode") {
		t.Errorf("persisted AutoHotAttrs = %v, want an explicit add attributed to the operator", saved.AutoHotAttrs)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	a2, _ := cat2.ArchiveTable("history")
	if !hasFold(a2.HotAttrs(), "ExitCode") {
		t.Errorf("hot set after reopen = %v, want the added ExitCode", a2.HotAttrs())
	}
}

// TestArchiveRecordsSurviveAHotSetChange is the correctness premise the whole feature rests
// on: the hot header is written per RECORD, so records appended before and after a refresh
// -- carrying different hot headers in the same archive -- all still read back whole. If
// that were not so, refreshing without a Rewrite would corrupt reads of the older data.
func TestArchiveRecordsSurviveAHotSetChange(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{HotAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	// Written under the creation-time set.
	fillArchiveWide(t, a, 200)
	for range 10 {
		drainArchive(t, a, `GlobalJobId == "ap1#3.0#1700000000"`)
	}
	if a.RefreshHotSet(1000, 3) == 0 {
		t.Fatal("RefreshHotSet chose nothing")
	}
	// Written under the refreshed set, into the same archive.
	for i := 200; i < 400; i++ {
		ad := classad.New()
		ad.Set("ClusterId", int64(i))
		ad.Set("Owner", fmt.Sprintf("user%d", i%7))
		ad.Set("GlobalJobId", fmt.Sprintf("ap1#%d.0#1700000000", i))
		ad.Set("ExitCode", int64(i%3))
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	a2, _ := cat2.ArchiveTable("history")
	seq, err := a2.Query("true")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int64]bool, 400)
	for ad := range seq {
		cid, ok := ad.EvaluateAttrInt("ClusterId")
		if !ok {
			t.Fatalf("record with no readable ClusterId: %v", ad)
		}
		// Every attribute must read back whatever header the record carries.
		owner, ok := ad.EvaluateAttrString("Owner")
		if !ok || owner != fmt.Sprintf("user%d", cid%7) {
			t.Fatalf("ClusterId %d: Owner = %q (ok=%v) after the hot-set change", cid, owner, ok)
		}
		gjid, ok := ad.EvaluateAttrString("GlobalJobId")
		if !ok || gjid != fmt.Sprintf("ap1#%d.0#1700000000", cid) {
			t.Fatalf("ClusterId %d: GlobalJobId = %q (ok=%v) after the hot-set change", cid, gjid, ok)
		}
		ec, ok := ad.EvaluateAttrInt("ExitCode")
		if !ok || ec != cid%3 {
			t.Fatalf("ClusterId %d: ExitCode = %d (ok=%v) after the hot-set change", cid, ec, ok)
		}
		seen[cid] = true
	}
	if len(seen) != 400 {
		t.Errorf("read back %d distinct records, want 400", len(seen))
	}
}
