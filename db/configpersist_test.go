package db

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/collections"
)

// goldenArchiveConfig and goldenIndexConfig are every persisted field set to a distinct,
// recognisable value, so a renamed or dropped JSON tag shows up as a byte difference
// against the committed fixtures rather than as a field that silently stops round-tripping.
func goldenArchiveConfig() ArchiveConfig {
	return ArchiveConfig{
		SegmentSize:          1 << 20,
		RowGroupBytes:        1 << 16,
		HotAttrs:             []string{"Owner", "JobStatus"},
		CategoricalAttrs:     []string{"Owner"},
		ValueAttrs:           []string{"ClusterId"},
		ZoneAttrs:            []string{"CompletionDate"},
		AutoAttrs:            []string{"ClusterId"},
		AutoHotAttrs:         []string{"GlobalJobId"},
		IndexBackfillBytes:   1 << 24,
		SchemaRequireAttrs:   []string{"DiskUsage"},
		SchemaRequireRuntime: []string{"ImageSize"},
		GroupSchemaCount:     3,
		GroupStabilityRuns:   2,
		GroupMergeJaccard:    0.75,
		GroupMaxPartialFrac:  0.25,
		Retention:            collections.Retention{MaxSegments: 7, MaxBytes: 1 << 30, MaxAge: 3600},
	}
}

func goldenIndexConfig() persistedIndexConfig {
	return persistedIndexConfig{
		Categorical:               []string{"Owner"},
		Value:                     []string{"ClusterId"},
		Auto:                      []string{"ClusterId"},
		Hot:                       []string{"Owner", "JobStatus"},
		Encrypted:                 []string{"Arguments"},
		SchemaRequire:             []string{"ImageSize"},
		TimeTravel:                true,
		TimeTravelMaxSeconds:      600,
		TimeTravelCheckpointSeced: 60,
	}
}

// TestConfigFileFormatUnchanged pins the bytes. Both files are read by deployments that
// predate this code: a reopen onto an existing directory takes its tuned index set, hot
// set and required attributes from them, so a changed tag name or a dropped field does not
// fail -- it silently reverts someone's tuning to the creation-time configuration, and the
// only trace is that queries get slower.
//
// The fixtures under testdata were produced by the code that wrote these files before they
// shared one writer, and are committed as-is.
func TestConfigFileFormatUnchanged(t *testing.T) {
	t.Run("archive", func(t *testing.T) {
		dir := t.TempDir()
		if err := newConfigFile(dir, archiveConfigFile, goldenArchiveConfig()).save(); err != nil {
			t.Fatal(err)
		}
		compareFixture(t, archiveConfigFile, filepath.Join(dir, archiveConfigFile))
	})
	t.Run("mutable", func(t *testing.T) {
		dir := t.TempDir()
		if err := newConfigFile(dir, indexConfigFile, goldenIndexConfig()).save(); err != nil {
			t.Fatal(err)
		}
		compareFixture(t, indexConfigFile, filepath.Join(dir, indexConfigFile))
	})
}

// compareFixture checks the file at path byte-for-byte against testdata/name.
func compareFixture(t *testing.T, name, path string) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s bytes changed.\n--- want (committed fixture) ---\n%s\n--- got ---\n%s", name, want, got)
	}
}

// TestArchiveReopensCommittedConfig opens an archive over nothing but the committed
// archiveconfig.json -- what a deployment upgrading into this code has on disk -- and
// checks the tuned configuration takes effect, including both provenance tiers.
func TestArchiveReopensCommittedConfig(t *testing.T) {
	dir := t.TempDir()
	copyFixture(t, archiveConfigFile, filepath.Join(dir, archiveConfigFile))

	a, err := openArchiveTable(dir, ArchiveConfig{})
	if err != nil {
		t.Fatalf("reopen over the committed config: %v", err)
	}
	defer a.Close()

	if got := a.RowGroupBytes(); got != 1<<16 {
		t.Errorf("RowGroupBytes = %d, want %d", got, 1<<16)
	}
	if got := a.Retention(); got.MaxSegments != 7 || got.MaxBytes != 1<<30 || got.MaxAge != 3600 {
		t.Errorf("Retention = %+v, want the fixture's", got)
	}
	cats, vals := a.IndexedAttrs()
	if !containsFold(cats, "Owner") {
		t.Errorf("categorical = %v, want Owner", cats)
	}
	if !containsFold(vals, "ClusterId") {
		t.Errorf("value = %v, want ClusterId", vals)
	}
	// Provenance: the fixture lists ClusterId as auto-created and Owner as not, and the
	// distinction has to survive the reopen or an auto index becomes permanently exempt
	// from trimming.
	if auto := a.AutoIndexNames(); !containsFold(auto, "ClusterId") || containsFold(auto, "Owner") {
		t.Errorf("auto index names = %v, want ClusterId only", auto)
	}
	for _, want := range []string{"Owner", "JobStatus", "GlobalJobId"} {
		if !containsFold(a.HotAttrs(), want) {
			t.Errorf("hot attrs = %v, want %s (HotAttrs + AutoHotAttrs)", a.HotAttrs(), want)
		}
	}
	for _, want := range []string{"diskusage", "imagesize"} {
		if !containsFold(a.SchemaRequireAttrs(), want) {
			t.Errorf("required attrs = %v, want %s (configured + runtime)", a.SchemaRequireAttrs(), want)
		}
	}
}

// TestDBReopensCommittedConfig is the mutable-table half: open over nothing but the
// committed indexcfg.json and check the tuned configuration takes effect, auto-index
// provenance included.
func TestDBReopensCommittedConfig(t *testing.T) {
	dir := t.TempDir()
	copyFixture(t, indexConfigFile, filepath.Join(dir, indexConfigFile))

	d, err := OpenConfig(Config{Dir: dir})
	if err != nil {
		t.Fatalf("open over the committed config: %v", err)
	}
	defer d.Close()

	cats, vals := d.IndexedAttrs()
	if !containsFold(cats, "Owner") {
		t.Errorf("categorical = %v, want Owner", cats)
	}
	if !containsFold(vals, "ClusterId") {
		t.Errorf("value = %v, want ClusterId", vals)
	}
	if auto := d.c.AutoIndexNames(); !containsFold(auto, "ClusterId") || containsFold(auto, "Owner") {
		t.Errorf("auto index names = %v, want ClusterId only", auto)
	}
	for _, want := range []string{"Owner", "JobStatus"} {
		if !containsFold(d.HotAttrs(), want) {
			t.Errorf("hot attrs = %v, want %s", d.HotAttrs(), want)
		}
	}
	if !containsFold(d.SchemaRequireAttrs(), "imagesize") {
		t.Errorf("required attrs = %v, want the runtime tier's ImageSize", d.SchemaRequireAttrs())
	}
	maxD, ckpt, on := d.TimeTravel()
	if !on || maxD != 600*time.Second || ckpt != 60*time.Second {
		t.Errorf("TimeTravel = (%v, %v, %v), want (10m, 1m, true)", maxD, ckpt, on)
	}
}

// TestConfigFileInMemoryWritesNothing: a table with no directory keeps its configuration
// live for the run and persists none of it, which is how an in-memory table has always
// behaved and what every caller's "" dir relies on.
func TestConfigFileInMemoryWritesNothing(t *testing.T) {
	f := newConfigFile("", indexConfigFile, persistedIndexConfig{})
	if got := f.path(); got != "" {
		t.Errorf("path() = %q, want empty for an in-memory table", got)
	}
	if err := f.update(func(cfg *persistedIndexConfig) { cfg.Hot = []string{"Owner"} }); err != nil {
		t.Errorf("update on an in-memory table: %v", err)
	}
	if err := f.save(); err != nil {
		t.Errorf("save on an in-memory table: %v", err)
	}
	var held persistedIndexConfig
	f.with(func(cfg *persistedIndexConfig) { held = *cfg })
	if !slices.Equal(held.Hot, []string{"Owner"}) {
		t.Errorf("held config = %v, want the update to be live for the run", held.Hot)
	}
	if _, exists, ok := readConfigFile[persistedIndexConfig]("", indexConfigFile); exists || ok {
		t.Error("readConfigFile reported a file for an in-memory table")
	}
}

// TestConfigFileUnparseableIsNotCreate: a config file that exists but will not parse still
// says "this directory is an existing table". Treating it as a create would re-create an
// archive on top of its own data.
func TestConfigFileUnparseableIsNotCreate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, archiveConfigFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, exists, ok := readConfigFile[ArchiveConfig](dir, archiveConfigFile)
	if !exists {
		t.Error("exists = false for a file that is present but corrupt")
	}
	if ok {
		t.Error("ok = true for a file that does not parse")
	}
	if cfg.SegmentSize != 0 {
		t.Errorf("cfg = %+v, want the zero value when parsing failed", cfg)
	}
}

func copyFixture(t *testing.T, name, dst string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestViewArchiveConfigPersists covers the archive nothing enumerates: a continuous
// aggregate view opens its own ArchiveTable under <catalog>/views/<name>/, outside the
// catalog's archive map. It reaches openArchiveTable by a different route from every other
// archive, so a change to where the config is located or how create is told from open
// breaks it without touching a catalog test.
//
// Checked here: the config file lands beside the view's archive, the create/open
// distinction survives a reload (a second create over existing data would reset the
// configuration), and a runtime change on that archive persists like any other.
func TestViewArchiveConfigPersists(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := cat.CreateTable("jobs")
	if perr := base.Put("1", jobTS(t, 3600)); perr != nil {
		t.Fatal(perr)
	}
	if verr := cat.CreateView("jobs_ts", continuousSpec()); verr != nil {
		t.Fatal(verr)
	}
	waitSeries(t, cat, "jobs_ts", 1)
	v, ok := cat.View("jobs_ts")
	if !ok {
		t.Fatal("view missing")
	}
	seal(v, 10800)

	cfgPath := filepath.Join(dir, viewsSubdir, "jobs_ts", viewArchiveSubdir, archiveConfigFile)
	if _, serr := os.Stat(cfgPath); serr != nil {
		t.Fatalf("the view archive wrote no config: %v", serr)
	}
	// A runtime change on an archive the catalog does not know about.
	if rerr := v.archive.SetRowGroupBytes(1 << 15); rerr != nil {
		t.Fatalf("SetRowGroupBytes on the view archive: %v", rerr)
	}
	if cerr := cat.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	cat2, oerr := OpenCatalog(dir)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer cat2.Close()
	v2, ok := cat2.View("jobs_ts")
	if !ok {
		t.Fatal("view did not survive reload")
	}
	// Reopened, not re-created: the zone attribute the view configures at create time is
	// still in effect, and so is the runtime change made above.
	if got := v2.archive.ZoneAttrs(); !containsFold(got, "time") {
		t.Errorf("view archive zone attrs after reload = %v, want the configured time alias", got)
	}
	if got := v2.archive.RowGroupBytes(); got != 1<<15 {
		t.Errorf("view archive RowGroupBytes after reload = %d, want %d", got, 1<<15)
	}
	if got := len(archiveAds(t, v2)); got != 1 {
		t.Errorf("view archive holds %d sealed buckets after reload, want 1", got)
	}
}
