package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
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

	a, err := openArchiveTable(dir, ArchiveConfig{}, nil)
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

// TestIndexConfigConcurrentPersist is the mutable-table counterpart of
// TestArchiveConfigConcurrentPersist: several runtime changes persisting at once, each
// rewriting indexcfg.json through the same temp path and each re-deriving the whole file
// from the collection.
//
// The mutable table shares no config struct between its savers, so -race has nothing to
// report here and the collision looked like it could not happen. Two ways it can, both
// measured against the writers this replaced:
//
//   - The shared temp path. Unserialized, 7 of 20 runs left indexcfg.json unparseable or
//     carrying none of the index set.
//   - A lost update. With the file write serialized but the derive outside the lock, two
//     savers snapshot the collection and write in the opposite order, so the older
//     snapshot lands last and the index the other one just built is simply absent.
//
// Neither leaves a trace: the next open reverts to the creation-time configuration and
// the indexes are rebuilt from scratch, or not at all.
//
// This is the end-to-end form and it finds the collision readily (7 of 20 single rounds).
// It finds the lost update only sometimes, so TestIndexConfigSaveDerivesUnderTheLock
// pins that one down without depending on an interleaving.
func TestIndexConfigConcurrentPersist(t *testing.T) {
	// Each round races one saver per attribute, then checks the file holds all of them.
	attrs := []string{"Owner", "Universe", "AccountingGroup", "Cmd", "RemoteHost", "GlobalJobId"}
	for round := 0; round < 12; round++ {
		dir := t.TempDir()
		d, err := OpenConfig(Config{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			if perr := d.Put(fmt.Sprintf("k%d", i), mustAd(t, fmt.Sprintf("ClusterId = %d\nOwner = \"u\"\nRequestMemory = %d", i, i))); perr != nil {
				t.Fatal(perr)
			}
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		run := func(fn func()) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				fn()
			}()
		}
		for _, a := range attrs {
			run(func() { d.AddIndex([]string{a}, nil) })
		}
		run(func() { d.AddIndex(nil, []string{"RequestMemory"}) })
		run(func() { d.AddHotAttrs("Owner") })
		run(func() { d.SetTimeTravel(10*time.Minute, time.Minute) })
		run(func() {
			if _, rerr := d.AddSchemaRequireAttrs("ImageSize"); rerr != nil {
				t.Errorf("AddSchemaRequireAttrs: %v", rerr)
			}
		})
		close(start)
		wg.Wait()
		if cerr := d.Close(); cerr != nil {
			t.Fatal(cerr)
		}

		raw, rerr := os.ReadFile(filepath.Join(dir, indexConfigFile))
		if rerr != nil {
			t.Fatalf("round %d: reading the config the storm left behind: %v", round, rerr)
		}
		var saved persistedIndexConfig
		if uerr := json.Unmarshal(raw, &saved); uerr != nil {
			t.Fatalf("round %d: config left unparseable by concurrent writers: %v\n%s", round, uerr, raw)
		}

		for _, a := range attrs {
			if !containsFold(saved.Categorical, a) {
				t.Errorf("round %d: categorical index on %s missing from the saved config: %v", round, a, saved.Categorical)
			}
		}
		if !containsFold(saved.Value, "RequestMemory") {
			t.Errorf("round %d: value index on RequestMemory missing from the saved config: %v", round, saved.Value)
		}
		if !containsFold(saved.Hot, "Owner") {
			t.Errorf("round %d: hot attrs missing from the saved config: %v", round, saved.Hot)
		}
		if !containsFold(saved.SchemaRequire, "ImageSize") {
			t.Errorf("round %d: required attrs missing from the saved config: %v", round, saved.SchemaRequire)
		}
		if !saved.TimeTravel {
			t.Errorf("round %d: time-travel setting missing from the saved config", round)
		}
		if t.Failed() {
			return
		}

		// Once, end to end: what the file says is what the table comes back with.
		if round == 0 {
			d2, oerr := OpenConfig(Config{Dir: dir})
			if oerr != nil {
				t.Fatal(oerr)
			}
			cats, vals := d2.IndexedAttrs()
			for _, a := range attrs {
				if !containsFold(cats, a) {
					t.Errorf("categorical index on %s lost across reopen: %v", a, cats)
				}
			}
			if !containsFold(vals, "RequestMemory") {
				t.Errorf("value index on RequestMemory lost across reopen: %v", vals)
			}
			if !containsFold(d2.HotAttrs(), "Owner") {
				t.Errorf("hot attrs lost across reopen: %v", d2.HotAttrs())
			}
			if !containsFold(d2.SchemaRequireAttrs(), "imagesize") {
				t.Errorf("required attrs lost across reopen: %v", d2.SchemaRequireAttrs())
			}
			if _, _, on := d2.TimeTravel(); !on {
				t.Error("time-travel setting lost across reopen")
			}
			d2.Close()
		}
	}
}

// TestIndexConfigSaveDerivesUnderTheLock pins the lost update down deterministically,
// where the concurrent storm above finds it only by luck.
//
// A saver that reads the collection BEFORE taking the config lock writes a snapshot of a
// configuration that has since moved on, so a change made while it waited is absent from
// the file although it is live in the table -- and the next open reverts to what the file
// says. Here the second change is made with the first saver provably still waiting, so
// the two orderings give different files every time rather than occasionally.
func TestIndexConfigSaveDerivesUnderTheLock(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenConfig(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Hold the config store's lock, so a save arriving now cannot reach the file.
	release, holding := make(chan struct{}), make(chan struct{})
	go d.cfg.with(func(*persistedIndexConfig) { close(holding); <-release })
	<-holding

	// A saver arrives. Its index goes live immediately; its write waits on the lock.
	done := make(chan struct{})
	go func() { defer close(done); d.AddIndex([]string{"Alpha"}, nil) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cats, _ := d.IndexedAttrs(); containsFold(cats, "Alpha") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the saver never made Alpha live")
		}
		time.Sleep(time.Millisecond)
	}
	// Alpha is live, so the saver is at or past the point where it would read the
	// collection. Give it room to get there and block. Sleeping too little would let this
	// test pass against the ordering it is here to reject, never fail against the right
	// one, so the margin is generous and costs one fixed pause.
	time.Sleep(100 * time.Millisecond)

	// A second change lands while that saver is still waiting.
	d.c.AddIndex([]string{"Beta"}, nil)

	close(release)
	<-done

	// The saver wrote after Beta became live. Reading the collection under the lock, it
	// wrote both; reading it beforehand, it wrote a snapshot that predates Beta.
	cfg, _, ok := readConfigFile[persistedIndexConfig](dir, indexConfigFile)
	if !ok {
		t.Fatal("no config file written")
	}
	if !containsFold(cfg.Categorical, "Alpha") {
		t.Errorf("saved categorical = %v, want Alpha", cfg.Categorical)
	}
	if !containsFold(cfg.Categorical, "Beta") {
		t.Errorf("saved categorical = %v, want Beta -- it was live before the save wrote, "+
			"so a save that read the collection before taking the lock dropped it", cfg.Categorical)
	}
}

// TestArchiveIndexSaveDerivesUnderTheLock is the archive's half of the invariant above.
// Its savers read the live index set, hot set and required set the same way, so the same
// ordering drops the same kind of change -- and on an archive the dropped change is an
// index whose backfill read the whole history to build.
func TestArchiveIndexSaveDerivesUnderTheLock(t *testing.T) {
	dir := t.TempDir()
	a, err := openArchiveTable(dir, ArchiveConfig{IndexBackfillBytes: archiveTestHorizon}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 20; i++ {
		if aerr := a.AppendOld(fmt.Sprintf("ClusterId = %d\nOwner = \"u\"", i)); aerr != nil {
			t.Fatal(aerr)
		}
	}

	release, holding := make(chan struct{}), make(chan struct{})
	go a.cfg.with(func(*ArchiveConfig) { close(holding); <-release })
	<-holding

	done := make(chan struct{})
	go func() { defer close(done); a.AddIndex([]string{"Owner"}, nil) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cats, _ := a.IndexedAttrs(); containsFold(cats, "Owner") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the saver never made Owner live")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // see the note in the mutable-table test

	a.a.AddIndex(nil, []string{"ClusterId"})

	close(release)
	<-done

	cfg, _, ok := readConfigFile[ArchiveConfig](dir, archiveConfigFile)
	if !ok {
		t.Fatal("no config file written")
	}
	if !containsFold(cfg.CategoricalAttrs, "Owner") {
		t.Errorf("saved categorical = %v, want Owner", cfg.CategoricalAttrs)
	}
	if !containsFold(cfg.ValueAttrs, "ClusterId") {
		t.Errorf("saved value = %v, want ClusterId -- it was live before the save wrote", cfg.ValueAttrs)
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

// hotProvenanceDB builds a table whose ads give a refresh something to choose, with read
// demand on one attribute that is not configured hot, and returns its directory.
func hotProvenanceDB(t *testing.T, dir string, cfg Config) *DB {
	t.Helper()
	cfg.Dir = dir
	d, err := OpenConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 400 {
		ad := mustAd(t, fmt.Sprintf("ClusterId = %d\nOwner = \"u%d\"\nGlobalJobId = \"ap1#%d.0#17\"\nExitCode = %d",
			i, i%5, i, i%3))
		if err := d.Put(fmt.Sprintf("%d.0", i), ad); err != nil {
			d.Close()
			t.Fatal(err)
		}
	}
	for range 10 {
		seq, err := d.Query(`GlobalJobId == "ap1#3.0#17"`)
		if err != nil {
			d.Close()
			t.Fatal(err)
		}
		for range seq { //nolint:revive // draining the iterator is the point
		}
	}
	return d
}

// readIndexConfig reads dir's persisted index config, failing if it is not there.
func readIndexConfig(t *testing.T, dir string) persistedIndexConfig {
	t.Helper()
	cfg, _, ok := readConfigFile[persistedIndexConfig](dir, indexConfigFile)
	if !ok {
		t.Fatal("no readable indexcfg.json")
	}
	return cfg
}

// TestHotProvenanceSplitSurvivesReopen: the mutable table persists WHICH hot attributes a
// human asked for and which a demand-driven refresh added, and restores the two separately.
//
// It used to persist the whole live set under one field, so after a restart a sampled
// attribute was indistinguishable from a configured one -- and the operator's list was
// whatever the last sample happened to agree with. The reopen is the whole point of the
// test: in one process the live set looks correct either way.
func TestHotProvenanceSplitSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	d := hotProvenanceDB(t, dir, Config{HotAttrs: []string{"Owner"}})
	d.AddHotAttrs("Pinned")
	if n := d.RefreshHotSet(2000, 4); n == 0 {
		d.Close()
		t.Skip("the refresh chose nothing, so there is no auto tier to measure")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	saved := readIndexConfig(t, dir)
	for _, want := range []string{"Owner", "Pinned"} {
		if !containsFold(saved.Hot, want) {
			t.Errorf("persisted human hot set = %v, want %s", saved.Hot, want)
		}
	}
	if len(saved.AutoHot) == 0 {
		t.Fatalf("nothing recorded as refresh-added; hot = %v, autoHot = %v", saved.Hot, saved.AutoHot)
	}
	for _, human := range []string{"Owner", "Pinned"} {
		if containsFold(saved.AutoHot, human) {
			t.Errorf("autoHot = %v holds %s, which a human asked for", saved.AutoHot, human)
		}
	}
	for _, auto := range saved.AutoHot {
		if containsFold(saved.Hot, auto) {
			t.Errorf("hot = %v holds %s, which the refresh added", saved.Hot, auto)
		}
	}

	// Reopen: the live set is the union again, and a refresh over a sample that need not
	// read them keeps the human tier, because the file still says who asked for it.
	d2, err := OpenConfig(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	for _, want := range append([]string{"Owner", "Pinned"}, saved.AutoHot...) {
		if !containsFold(d2.HotAttrs(), want) {
			t.Errorf("hot set after reopen = %v, want %s", d2.HotAttrs(), want)
		}
	}
	d2.RefreshHotSet(2000, 1)
	for _, want := range []string{"Owner", "Pinned"} {
		if !containsFold(d2.HotAttrs(), want) {
			t.Errorf("hot set after a post-restart refresh = %v, want %s kept: the pin did not "+
				"survive the restart as a pin", d2.HotAttrs(), want)
		}
	}
}

// TestHotProvenanceLegacyFileLoadsAsHuman pins the first-load decision: a file written
// before the split has `hot` and no `autoHot`, and every entry in it is read as
// human-configured.
//
// The alternative -- read them as auto -- would discard an operator's pin the first time a
// sample disagreed with it, on an upgrade, with nothing to say so. Reading them as human
// keeps an attribute hot that a refresh could otherwise have dropped, which is visible and
// is undone by naming it.
func TestHotProvenanceLegacyFileLoadsAsHuman(t *testing.T) {
	dir := t.TempDir()
	copyFixture(t, indexConfigFile, filepath.Join(dir, indexConfigFile))
	// The committed fixture predates the split: it has `hot` and no `autoHot`.
	if legacy := readIndexConfig(t, dir); len(legacy.AutoHot) != 0 {
		t.Fatalf("the fixture already carries an auto tier (%v), so it no longer stands in "+
			"for a pre-split file", legacy.AutoHot)
	}

	d := hotProvenanceDB(t, dir, Config{})
	for _, want := range []string{"Owner", "JobStatus"} {
		if !containsFold(d.HotAttrs(), want) {
			t.Errorf("hot set from the pre-split file = %v, want %s", d.HotAttrs(), want)
		}
	}
	d.RefreshHotSet(2000, 1)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	saved := readIndexConfig(t, dir)
	for _, want := range []string{"Owner", "JobStatus"} {
		if !containsFold(saved.Hot, want) {
			t.Errorf("human hot set after a refresh over a pre-split file = %v, want %s: the "+
				"file's entries were not read as human", saved.Hot, want)
		}
	}
	d2, err := OpenConfig(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	for _, want := range []string{"Owner", "JobStatus"} {
		if !containsFold(d2.HotAttrs(), want) {
			t.Errorf("hot set after reopening the upgraded file = %v, want %s", d2.HotAttrs(), want)
		}
	}
}

// TestMaintainPersistsHotRefresh: the scheduled pass's hot-set refresh has to reach the
// file, as the archive pass's does. It went straight to the collection, so the one piece of
// hot-set tuning a daemon does unattended was live for that process and gone on restart --
// with the segments written in between as the only trace that it ever ran.
func TestMaintainPersistsHotRefresh(t *testing.T) {
	dir := t.TempDir()
	d := hotProvenanceDB(t, dir, Config{})
	d.Maintain(MaintainOptions{HotTopN: 4})
	live := append([]string(nil), d.HotAttrs()...)
	if len(live) == 0 {
		d.Close()
		t.Skip("the refresh chose nothing")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d2, err := OpenConfig(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	for _, want := range live {
		if !containsFold(d2.HotAttrs(), want) {
			t.Errorf("hot set after a restart = %v, want %s: the maintenance refresh was "+
				"never persisted", d2.HotAttrs(), want)
		}
	}
}

// TestIndexConfigAutoHotTagged pins the new field's JSON tag the way
// TestConfigFileFormatUnchanged pins the others. The committed fixture is deliberately left
// as the pre-split writer produced it -- that is what makes it evidence of backward
// compatibility -- so it cannot carry this field, and the tag is pinned here instead.
func TestIndexConfigAutoHotTagged(t *testing.T) {
	data, err := json.Marshal(persistedIndexConfig{Hot: []string{"Owner"}, AutoHot: []string{"GlobalJobId"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != `{"hot":["Owner"],"autoHot":["GlobalJobId"]}` {
		t.Errorf("persistedIndexConfig JSON = %s, want the hot/autoHot tags", got)
	}
	// And omitted when empty, which is why adding it left the committed fixture's bytes alone.
	data, err = json.Marshal(persistedIndexConfig{Hot: []string{"Owner"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != `{"hot":["Owner"]}` {
		t.Errorf("persistedIndexConfig JSON without an auto tier = %s, want autoHot omitted", got)
	}
}
