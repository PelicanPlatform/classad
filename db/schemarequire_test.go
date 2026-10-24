package db

import (
	"slices"
	"testing"
)

// The required-attribute set is only useful if it is still there after a restart. It is read at
// open and then governs every later schema derivation, so a set that silently reset would leave
// the next derivation quietly dropping the column -- queries get slower and nothing says why.
// That failure only ever appears across a reopen, which is what these tests are.

// TestArchiveSchemaRequirePersistsAcrossReopen: a runtime addition survives, and so does the
// configured tier (archiveconfig.json is authoritative on reopen, so it has to).
func TestArchiveSchemaRequirePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{
		IndexBackfillBytes: archiveTestHorizon,
		SchemaRequireAttrs: []string{"ResidentSetSize"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fillArchive(t, a, 50)
	changed, err := a.AddSchemaRequireAttrs("ImageSize")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("AddSchemaRequireAttrs reported no change")
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
	if got, want := a2.SchemaRequireAttrs(), []string{"imagesize", "residentsetsize"}; !slices.Equal(got, want) {
		t.Fatalf("required set after reopen = %v, want %v", got, want)
	}
}

// TestArchiveSchemaRequireReassertKeepsRuntime: re-asserting the configured tier -- what an
// application does from its own configuration on every start -- must not erase what an operator
// added by hand. That is the point of persisting the two tiers apart.
func TestArchiveSchemaRequireReassertKeepsRuntime(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{
		IndexBackfillBytes: archiveTestHorizon,
		SchemaRequireAttrs: []string{"ResidentSetSize"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddSchemaRequireAttrs("ImageSize"); err != nil {
		t.Fatal(err)
	}
	// The application's configuration changed: DiskUsage in, ResidentSetSize out.
	if _, err := a.SetSchemaRequireAttrs([]string{"DiskUsage"}); err != nil {
		t.Fatal(err)
	}
	if got, want := a.SchemaRequireAttrs(), []string{"diskusage", "imagesize"}; !slices.Equal(got, want) {
		t.Fatalf("after re-assert = %v, want %v", got, want)
	}
	if got, want := a.cfg.SchemaRequireRuntime, []string{"imagesize"}; !slices.Equal(got, want) {
		t.Errorf("persisted runtime tier = %v, want %v -- provenance must survive the re-assert", got, want)
	}
	if got, want := a.cfg.SchemaRequireAttrs, []string{"diskusage"}; !slices.Equal(got, want) {
		t.Errorf("persisted configured tier = %v, want %v", got, want)
	}
}

// TestDBSchemaRequirePersistsAcrossReopen: the mutable table's runtime tier comes back from
// indexcfg.json, while the configured tier comes back from Config -- so a configuration change
// still takes effect on the next open instead of being frozen out by the persisted copy.
func TestDBSchemaRequirePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenConfig(Config{Dir: dir, SchemaRequireAttrs: []string{"ResidentSetSize"}})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := d.AddSchemaRequireAttrs("ImageSize"); err != nil || !changed {
		t.Fatalf("AddSchemaRequireAttrs = (%v, %v)", changed, err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopened with a DIFFERENT configured set: the runtime addition must still be there.
	d2, err := OpenConfig(Config{Dir: dir, SchemaRequireAttrs: []string{"DiskUsage"}})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if got, want := d2.SchemaRequireAttrs(), []string{"diskusage", "imagesize"}; !slices.Equal(got, want) {
		t.Fatalf("required set after reopen = %v, want %v", got, want)
	}
}
