package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// Composite provenance across a restart, on both table types. What is under test is not
// that the composite comes back -- it is that it comes back as the TUNER's, so the memory
// budget may still trim it and an unused-drop may still remove it. A composite restored
// unmarked is exempt from both for the rest of its life on the strength of a guess, and
// nothing downstream can tell it from a tuple an operator asked for.

// seedTupleAds fills a mutable table with ads whose conjunctions are worth a composite:
// Owner and Arch, both strings, queried together.
func seedTupleAds(t *testing.T, d *DB, n int) {
	t.Helper()
	arches := []string{"X86_64", "aarch64"}
	tx := d.Begin()
	for i := 0; i < n; i++ {
		ad, err := classad.ParseOld(fmt.Sprintf("Owner = \"user%d\"\nArch = %q", i%7, arches[i%2]))
		if err != nil {
			t.Fatal(err)
		}
		tx.NewClassAd(fmt.Sprintf("%d.0", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// tupleLabels renders tuples for a failure message, since a [][]string prints badly.
func tupleLabels(tuples [][]string) []string {
	out := make([]string, 0, len(tuples))
	for _, t := range tuples {
		out = append(out, fmt.Sprint(t))
	}
	return out
}

// TestAutoCompositeProvenancePersistsAcrossReopen: a mutable table's composite is created
// by the tuner, which is the only way one exists there, and both the composite and its
// provenance have to be in indexcfg.json for it to survive.
func TestAutoCompositeProvenancePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedTupleAds(t, d, 300)
	for i := 0; i < 20; i++ {
		seq, qerr := d.Query(`Owner == "user1" && Arch == "X86_64"`)
		if qerr != nil {
			t.Fatal(qerr)
		}
		for range seq {
		}
	}

	// Two passes: a tuple is suggested only once every component is indexed in its own
	// right, so the first pass adds Owner and Arch and the second creates the composite.
	d.Maintain(MaintainOptions{MinIndexDemand: 5})
	d.Maintain(MaintainOptions{MinIndexDemand: 5})
	auto := d.c.AutoCompositeNames()
	if len(auto) != 1 {
		t.Fatalf("the tuner created %v, want one auto composite; nothing below would be testing a restore", tupleLabels(auto))
	}

	// The file is what the reopen reads, so assert on it directly: a save that wrote the
	// composite set and not its provenance would still reopen with the composite present.
	cfg, _, ok := readConfigFile[persistedIndexConfig](dir, indexConfigFile)
	if !ok {
		t.Fatal("no index config file written")
	}
	if len(cfg.AutoComposites) != 1 {
		t.Fatalf("saved autoComposites = %v, want the one tuple", tupleLabels(cfg.AutoComposites))
	}
	if cerr := d.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	d2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if got := d2.c.CompositeAttrs(); len(got) != 1 {
		t.Fatalf("composites after reopen = %v, want the one restored", tupleLabels(got))
	}
	if got := d2.c.AutoCompositeNames(); len(got) != 1 {
		t.Fatalf("auto composites after reopen = %v, want the restored one still marked auto", tupleLabels(got))
	}
	// The mark is not decoration: IndexSizes is what the memory-budget trim ranks and
	// drops, and it is the Auto flag there that makes a composite eligible at all.
	sizes := d2.c.IndexSizes()
	var comp *IndexSize
	for i := range sizes.PerIndex {
		if sizes.PerIndex[i].Kind == "composite" {
			comp = &sizes.PerIndex[i]
		}
	}
	if comp == nil {
		t.Fatal("no composite in IndexSizes after reopen; the budget trim could not see one either")
	}
	if !comp.Auto {
		t.Errorf("IndexSizes reports %s as human after reopen, so the budget trim would never touch it", comp.Attr)
	}
}

// TestArchiveAutoCompositeProvenancePersistsAcrossReopen is the same thing on an archive,
// which restores it the other way: the composite is rebuilt from the saved config as a
// configured one, and only then is its provenance marked back on.
func TestArchiveAutoCompositeProvenancePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{IndexBackfillBytes: archiveTestHorizon})
	if err != nil {
		t.Fatal(err)
	}
	arches := []string{"X86_64", "aarch64"}
	for i := range 500 {
		ad := classad.New()
		ad.Set("ClusterId", int64(i))
		ad.Set("Owner", fmt.Sprintf("user%d", i%7))
		ad.Set("Arch", arches[i%2])
		if aerr := a.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	for range 10 {
		drainArchive(t, a, `Owner == "user3" && Arch == "X86_64"`)
	}
	a.AutoTune(AutoTuneOptions{MinDemand: 5, Reindex: true}) // components
	a.AutoTune(AutoTuneOptions{MinDemand: 5, Reindex: true}) // the tuple
	if auto := a.a.AutoCompositeNames(); len(auto) != 1 {
		t.Fatalf("the tuner created %v, want one auto composite; the reopen below would prove nothing", tupleLabels(auto))
	}

	cfg, _, ok := readConfigFile[ArchiveConfig](a.cfg.dir, archiveConfigFile)
	if !ok {
		t.Fatal("no archive config file written")
	}
	if len(cfg.CompositeAttrs) != 1 || len(cfg.AutoComposites) != 1 {
		t.Fatalf("saved composites = %v, auto = %v, want one of each",
			tupleLabels(cfg.CompositeAttrs), tupleLabels(cfg.AutoComposites))
	}
	if cerr := cat.Close(); cerr != nil {
		t.Fatal(cerr)
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
	if got := a2.a.CompositeAttrs(); len(got) != 1 {
		t.Fatalf("composites after reopen = %v, want the one rebuilt from the config", tupleLabels(got))
	}
	if got := a2.a.AutoCompositeNames(); len(got) != 1 {
		t.Fatalf("auto composites after reopen = %v, want the rebuilt one marked auto again", tupleLabels(got))
	}
	// And the rows still come back: the restore changed provenance, not the index.
	seq, err := a2.Query(`Owner == "user3" && Arch == "X86_64"`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	if n == 0 {
		t.Error("the reopened archive returned no rows for the composite's own conjunction")
	}
}

// TestArchiveHumanCompositeStaysHuman: the saved provenance has to be a SUBSET, not a
// restatement of the set. A composite someone configured must come back exempt from the
// trim, which is the error the auto mark would make if it were applied to everything the
// config lists.
func TestArchiveHumanCompositeStaysHuman(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{
		IndexBackfillBytes: archiveTestHorizon,
		CategoricalAttrs:   []string{"Owner", "Arch"},
		CompositeAttrs:     [][]string{{"Owner", "Arch"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.a.CompositeAttrs(); len(got) != 1 {
		t.Fatalf("configured composites = %v, want the one asked for", tupleLabels(got))
	}
	if got := a.a.AutoCompositeNames(); len(got) != 0 {
		t.Fatalf("a configured composite was recorded as auto: %v", tupleLabels(got))
	}
	// Force a save, so the reopen below reads a file this process wrote rather than the
	// one creation wrote.
	a.AddIndex(nil, []string{"ClusterId"})
	if cerr := cat.Close(); cerr != nil {
		t.Fatal(cerr)
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
	if got := a2.a.CompositeAttrs(); len(got) != 1 {
		t.Fatalf("composites after reopen = %v, want the configured one", tupleLabels(got))
	}
	if got := a2.a.AutoCompositeNames(); len(got) != 0 {
		t.Errorf("a configured composite came back marked auto: %v", tupleLabels(got))
	}
}
