package db

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

// A Catalog is a set of named tables, each an independent ClassAd store (its own
// keyspace, indexes, hot set, and persisted index config). Tables give the
// database more than one collection to work with -- e.g. separate machine and
// job ads -- without joins. A persistent catalog keeps each table in its own
// subdirectory (<dir>/tables/<name>), so tables are isolated on disk and the set
// is recovered by enumerating those subdirectories on open.
type Catalog struct {
	dir string // "" = in-memory (tables are ephemeral)

	// Encryption at rest applied uniformly to every table: each table is an independent
	// encrypted store whose own master key is wrapped by these pool keys, and whose
	// default explicitly-encrypted attributes are encAttrs. Empty poolKeys ⇒ no encryption AT REST;
	// private attributes are sealed either way.
	poolKeys []KEK
	encAttrs []string

	// onSealMigration and sealWorkers are carried from CatalogConfig so every table opened through this
	// catalog reports its one-off migration, named (see CatalogConfig.OnSealMigration).
	onSealMigration func(table string, segments int)
	sealWorkers     int

	mu       sync.Mutex
	tables   map[string]*DB
	archives map[string]*ArchiveTable
	views    map[string]*View

	// exporters are external-sink definitions (e.g. a Kafka change-data exporter). The
	// catalog only persists each one's opaque per-kind config and an opaque resume-state
	// blob; it never runs an exporter or interprets its config/state. See db/exporter.go.
	exporters map[string]ExporterDef
	// memExporterState holds exporter resume-state blobs for an in-memory catalog
	// (dir == ""), where there is no disk to persist to. Unused when dir is set.
	memExporterState map[string][]byte
}

// tablesSubdir / archivesSubdir are where a persistent catalog keeps its per-table
// directories, split by table type.
const (
	tablesSubdir    = "tables"
	archivesSubdir  = "archives"
	viewsSubdir     = "views"
	exportersSubdir = "exporters"
)

// CatalogConfig configures a catalog, including encryption at rest applied to every
// table. Dir empty is in-memory.
type CatalogConfig struct {
	// OnOpenStep, when set, is called once per table and archive opened by OpenCatalogConfig, with how long
	// that one took. Catalog open is a loop over directories, so without it the whole thing is a single
	// number -- which is how a 15s startup stayed unattributed: every other phase reported 0s and this one
	// reported all of it, with nothing inside.
	//
	// kind is "table" or "archive". MAY BE CALLED CONCURRENTLY, from whichever goroutine opened that table --
	// opens run in parallel -- so a handler must be safe for concurrent use and must not block. It is still
	// called once per open, which is what keeps per-table attribution intact under parallelism: only the SUM
	// stops matching the wall clock.
	OnOpenStep func(kind, name string, d time.Duration)
	Dir        string
	// PoolKeys enables encryption at rest for every table (each table's master key is
	// wrapped under these keys). EncryptedAttrs is the default explicit encrypted-attr
	// set for each table (private attributes are always encrypted). See db/encrypt.go.
	PoolKeys       []KEK
	EncryptedAttrs []string

	// OnSealMigration, when set, is called for each table whose open rewrote segments to seal private
	// attributes that predate sealing, with how many segments it rewrote. It carries the TABLE NAME
	// because a catalog opens many, and "some table rewrote 400 segments" does not tell an operator
	// which one to expect the cost from.
	//
	// The per-table Config hook exists too, but a catalog builds those configs itself, so without this
	// the hook was unreachable for every table a daemon actually opens -- the only place the one-off
	// startup cost needs explaining.
	//
	// Called from the goroutine that opened that table, so it may run concurrently (see OnOpenStep).
	OnSealMigration func(table string, segments int)

	// SealMigrationWorkers bounds the concurrent segment rewrites within one table's migration (see
	// collections.MigrateSealedAttrs). 0 takes a per-table default. Tables are already opened in
	// parallel, so this multiplies with that.
	SealMigrationWorkers int
}

// OpenCatalog opens the catalog rooted at dir with no encryption. See OpenCatalogConfig
// to enable encryption at rest.
func OpenCatalog(dir string) (*Catalog, error) {
	return OpenCatalogConfig(CatalogConfig{Dir: dir})
}

// OpenCatalogConfig opens the catalog rooted at cfg.Dir, recovering any tables from
// <dir>/tables/ and applying cfg's encryption at rest to each. Dir == "" makes an
// in-memory catalog whose tables do not persist.
// openConcurrently opens each name with open, bounded, and returns the results in input order.
//
// Catalog open used to be a serial loop, and it was the whole of one deployment's 15s startup. Nothing in the
// loop depends on the previous iteration, and each open is I/O plus mmap plus a key-directory load -- latency
// that overlaps rather than CPU that contends -- so the wall clock is the slowest table instead of the sum.
//
// Bounded rather than one goroutine per table: a few hundred tables opening at once would storm the page cache
// and the file-descriptor table for no gain, since past a handful the work is queued at the device anyway.
//
// On failure it closes whatever it already opened, because those handles are not in the catalog's maps yet and
// closeAll cannot reach them, and it returns the error from the LOWEST index so the message a caller sees does
// not depend on which goroutine lost the race.
func openConcurrently[T any](names []string, kind string, onStep func(kind, name string, d time.Duration),
	open func(name string) (T, error), closeOne func(T)) ([]T, error) {
	out := make([]T, len(names))
	errs := make([]error, len(names))
	limit := runtime.GOMAXPROCS(0)
	if limit > 8 {
		limit = 8
	}
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := time.Now()
			v, err := open(name)
			if onStep != nil {
				onStep(kind, name, time.Since(start))
			}
			out[i], errs[i] = v, err
		}(i, name)
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			continue
		}
		for j, v := range out {
			if errs[j] == nil {
				closeOne(v)
			}
		}
		return nil, fmt.Errorf("%s %q: %w", kind, names[i], err)
	}
	return out, nil
}

func OpenCatalogConfig(cfg CatalogConfig) (*Catalog, error) {
	cat := &Catalog{
		dir: cfg.Dir, tables: map[string]*DB{}, archives: map[string]*ArchiveTable{},
		views:            map[string]*View{},
		exporters:        map[string]ExporterDef{},
		memExporterState: map[string][]byte{},
		poolKeys:         cfg.PoolKeys, encAttrs: cfg.EncryptedAttrs,
		onSealMigration: cfg.OnSealMigration,
		sealWorkers:     cfg.SealMigrationWorkers,
	}
	if cfg.Dir == "" {
		return cat, nil
	}
	root := filepath.Join(cfg.Dir, tablesSubdir)
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("catalog: creating tables dir: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading tables dir: %w", err)
	}
	var tableNames []string
	for _, e := range entries {
		if !e.IsDir() || !ValidTableName(e.Name()) {
			continue // ignore stray directories
		}
		tableNames = append(tableNames, e.Name())
	}
	openedTables, err := openConcurrently(tableNames, "table", cfg.OnOpenStep,
		func(name string) (*DB, error) { return OpenConfig(cat.tableConfig(filepath.Join(root, name))) },
		func(d *DB) { _ = d.Close() })
	if err != nil {
		cat.closeAll()
		return nil, fmt.Errorf("catalog: opening %w", err)
	}
	for i, name := range tableNames {
		cat.tables[name] = openedTables[i]
	}
	// Recover archive (history) tables from <dir>/archives/.
	aroot := filepath.Join(cfg.Dir, archivesSubdir)
	aentries, err := os.ReadDir(aroot)
	if err != nil && !os.IsNotExist(err) {
		cat.closeAll()
		return nil, fmt.Errorf("catalog: reading archives dir: %w", err)
	}
	var archiveNames []string
	for _, e := range aentries {
		if !e.IsDir() || !ValidTableName(e.Name()) {
			continue
		}
		archiveNames = append(archiveNames, e.Name())
	}
	openedArchives, err := openConcurrently(archiveNames, "archive", cfg.OnOpenStep,
		func(name string) (*ArchiveTable, error) {
			return openArchiveTable(filepath.Join(aroot, name), ArchiveConfig{})
		},
		func(a *ArchiveTable) { _ = a.Close() })
	if err != nil {
		cat.closeAll()
		return nil, fmt.Errorf("catalog: opening %w", err)
	}
	for i, name := range archiveNames {
		cat.archives[name] = openedArchives[i]
	}
	// Recover materialized views from <dir>/views/. A view's DATA is not persisted; only
	// its definition is, so each view is rebuilt from its base table here. This runs after
	// tables are loaded so a view's base table exists. A view whose rebuild fails (e.g.
	// cardinality) or whose base table is absent does NOT fail catalog open -- it loads in
	// the failed/stale state and can be fixed by the operator.
	if err := cat.recoverViews(); err != nil {
		cat.closeAll()
		return nil, err
	}
	// Recover external-sink exporter definitions from <dir>/exporters/. These are inert
	// records: the catalog holds each definition (and, on disk, its opaque resume state)
	// but never runs the exporter. A corrupt definition is skipped, not fatal.
	if err := cat.recoverExporters(); err != nil {
		cat.closeAll()
		return nil, err
	}
	return cat, nil
}

// tableConfig builds a per-table Config carrying the catalog-wide encryption settings.
func (cat *Catalog) tableConfig(dir string) Config {
	cfg := Config{
		Dir:                  dir,
		PoolKeys:             cat.poolKeys,
		EncryptedAttrs:       cat.encAttrs,
		SealMigrationWorkers: cat.sealWorkers,
	}
	if cat.onSealMigration != nil {
		// Name the table the count belongs to: the per-table hook cannot know it, and the catalog can.
		name := filepath.Base(dir)
		if dir == "" {
			name = "(in-memory)"
		}
		cfg.OnSealMigration = func(segments int) { cat.onSealMigration(name, segments) }
	}
	return cfg
}

// ValidTableName reports whether name is usable as a table (and a directory):
// it must start with a letter or underscore and contain only letters, digits,
// underscores, and hyphens.
func ValidTableName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		case c >= '0' && c <= '9' || c == '-':
			if i == 0 {
				return false // must not start with a digit or hyphen
			}
		default:
			return false
		}
	}
	return true
}

// Table returns the table named name.
func (cat *Catalog) Table(name string) (*DB, bool) {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	d, ok := cat.tables[name]
	return d, ok
}

// TableOptions tunes how a table is created. The zero value creates a normal
// table (persistent when the catalog has a directory).
type TableOptions struct {
	// InMemory makes the table's data live only in RAM even in a persistent
	// catalog: no <dir>/tables/<name> directory is created, so the table is not
	// recovered across restarts (it reappears empty). Useful for high-churn,
	// reconstructible data (e.g. frequently-replaced ads) that is not worth the
	// disk I/O of persistence. In an already-in-memory catalog it is a no-op.
	InMemory bool
}

// CreateTable creates (or returns the existing) table named name. Its data
// persists under <dir>/tables/<name> for a persistent catalog.
func (cat *Catalog) CreateTable(name string) (*DB, error) {
	return cat.CreateTableOpts(name, TableOptions{})
}

// CreateTableOpts is CreateTable with options; see TableOptions. If the table
// already exists it is returned as-is (options are not re-applied).
func (cat *Catalog) CreateTableOpts(name string, opts TableOptions) (*DB, error) {
	if !ValidTableName(name) {
		return nil, fmt.Errorf("catalog: invalid table name %q", name)
	}
	cat.mu.Lock()
	defer cat.mu.Unlock()
	if d, ok := cat.tables[name]; ok {
		return d, nil
	}
	if _, ok := cat.archives[name]; ok {
		return nil, fmt.Errorf("catalog: %q already exists as an archive table", name)
	}
	if _, ok := cat.views[name]; ok {
		return nil, fmt.Errorf("catalog: %q already exists as a materialized view", name)
	}
	cfgDir := ""
	if cat.dir != "" && !opts.InMemory {
		cfgDir = filepath.Join(cat.dir, tablesSubdir, name)
	}
	d, err := OpenConfig(cat.tableConfig(cfgDir))
	if err != nil {
		return nil, fmt.Errorf("catalog: creating table %q: %w", name, err)
	}
	cat.tables[name] = d
	return d, nil
}

// CreateTableInMemory creates (or returns the existing) table named name as RAM-only,
// even in a persistent catalog -- shorthand for CreateTableOpts with InMemory set. If the
// table already exists its backing is unchanged (options are not re-applied); use
// ConvertTableToMemory to drop an existing persistent table's on-disk backing.
func (cat *Catalog) CreateTableInMemory(name string) (*DB, error) {
	return cat.CreateTableOpts(name, TableOptions{InMemory: true})
}

// ConvertTableToMemory drops the on-disk backing of an existing persistent table, keeping
// its current contents live in RAM only. It copies the table through a consistent snapshot
// into a fresh in-memory table (preserving ads, index configuration, hot set, codec, and
// encryption policy), swaps that in, then closes and deletes the on-disk original.
//
// It is a no-op when the catalog itself is in-memory or the table is already RAM-only. Like
// Rewrite, it takes a consistent snapshot but does not globally quiesce writers, so a write
// that races the swap can be lost -- run it during low write activity. DAEMON-level: it
// changes a table's durability, so callers gate it above ordinary WRITE.
func (cat *Catalog) ConvertTableToMemory(name string) error {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	old, ok := cat.tables[name]
	if !ok {
		if _, isArch := cat.archives[name]; isArch {
			return fmt.Errorf("catalog: %q is an archive table, not a mutable table", name)
		}
		return fmt.Errorf("catalog: no such table %q", name)
	}
	if cat.dir == "" || old.InMemory() {
		return nil // whole catalog is in-memory, or this table is already RAM-only
	}

	// Build the RAM replacement with the same table configuration (crucially the same
	// pool keys, so the encrypted snapshot round-trips), then copy the full contents in
	// via a snapshot -- the same consistent mechanism backups/HA use.
	mem, err := OpenConfig(cat.tableConfig(""))
	if err != nil {
		return fmt.Errorf("catalog: creating in-memory replacement for %q: %w", name, err)
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(old.Snapshot(pw)) }()
	if err := mem.Restore(pr); err != nil {
		_ = pr.CloseWithError(err)
		_ = mem.Close()
		return fmt.Errorf("catalog: copying %q into memory: %w", name, err)
	}

	// Swap in the RAM table and retire the on-disk original.
	cat.tables[name] = mem
	_ = old.Close()
	if err := os.RemoveAll(filepath.Join(cat.dir, tablesSubdir, name)); err != nil {
		return fmt.Errorf("catalog: removing on-disk data for %q: %w", name, err)
	}
	return nil
}

// DropTable closes and removes the table named name, deleting its on-disk data.
func (cat *Catalog) DropTable(name string) error {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	d, ok := cat.tables[name]
	if !ok {
		return fmt.Errorf("catalog: no such table %q", name)
	}
	delete(cat.tables, name)
	_ = d.Close()
	if cat.dir != "" {
		if err := os.RemoveAll(filepath.Join(cat.dir, tablesSubdir, name)); err != nil {
			return fmt.Errorf("catalog: removing table %q data: %w", name, err)
		}
	}
	return nil
}

// Tables returns the mutable table names, sorted.
func (cat *Catalog) Tables() []string {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	names := make([]string, 0, len(cat.tables))
	for n := range cat.tables {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ArchiveTable returns the archive (history) table named name.
func (cat *Catalog) ArchiveTable(name string) (*ArchiveTable, bool) {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	a, ok := cat.archives[name]
	return a, ok
}

// ArchiveTables returns the archive table names, sorted.
func (cat *Catalog) ArchiveTables() []string {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	names := make([]string, 0, len(cat.archives))
	for n := range cat.archives {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// CreateArchiveTable creates (or returns the existing) append-only archive table named
// name, persisted under <dir>/archives/<name>. cfg configures indexes / zone maps /
// retention on first creation and is ignored for an existing archive.
func (cat *Catalog) CreateArchiveTable(name string, cfg ArchiveConfig) (*ArchiveTable, error) {
	if !ValidTableName(name) {
		return nil, fmt.Errorf("catalog: invalid archive name %q", name)
	}
	cat.mu.Lock()
	defer cat.mu.Unlock()
	if a, ok := cat.archives[name]; ok {
		return a, nil
	}
	if _, ok := cat.tables[name]; ok {
		return nil, fmt.Errorf("catalog: %q already exists as a mutable table", name)
	}
	if cat.dir == "" {
		return nil, fmt.Errorf("catalog: archive tables require a persistent catalog")
	}
	at, err := openArchiveTable(filepath.Join(cat.dir, archivesSubdir, name), cfg)
	if err != nil {
		return nil, fmt.Errorf("catalog: creating archive %q: %w", name, err)
	}
	cat.archives[name] = at
	return at, nil
}

// DropArchiveTable closes and removes the archive named name, deleting its on-disk data.
func (cat *Catalog) DropArchiveTable(name string) error {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	a, ok := cat.archives[name]
	if !ok {
		return fmt.Errorf("catalog: no such archive %q", name)
	}
	delete(cat.archives, name)
	_ = a.Close()
	if cat.dir != "" {
		if err := os.RemoveAll(filepath.Join(cat.dir, archivesSubdir, name)); err != nil {
			return fmt.Errorf("catalog: removing archive %q data: %w", name, err)
		}
	}
	return nil
}

// EnsureTable creates the table if it does not exist, returning it.
func (cat *Catalog) EnsureTable(name string) (*DB, error) { return cat.CreateTable(name) }

// Close closes every table.
func (cat *Catalog) Close() error {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	return cat.closeAll()
}

func (cat *Catalog) closeAll() error {
	var first error
	for name, d := range cat.tables {
		if err := d.Close(); err != nil && first == nil {
			first = err
		}
		delete(cat.tables, name)
	}
	for name, a := range cat.archives {
		if err := a.Close(); err != nil && first == nil {
			first = err
		}
		delete(cat.archives, name)
	}
	for name, v := range cat.views {
		v.stop() // cancels the live updater and closes the in-memory backing
		delete(cat.views, name)
	}
	// Exporter definitions hold no runtime resources (the catalog never runs them), so
	// closing just drops the in-memory registry.
	for name := range cat.exporters {
		delete(cat.exporters, name)
	}
	return first
}
