package db

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// A Catalog is a set of named tables, each an independent ClassAd store (its own
// keyspace, indexes, hot set, and persisted index config). Tables give the
// database more than one collection to work with -- e.g. separate machine and
// job ads -- without joins. A persistent catalog keeps each table in its own
// subdirectory (<dir>/tables/<name>), so tables are isolated on disk and the set
// is recovered by enumerating those subdirectories on open.
type Catalog struct {
	dir string // "" = in-memory (tables are ephemeral)

	mu     sync.Mutex
	tables map[string]*DB
}

// tablesSubdir is where a persistent catalog keeps its per-table directories.
const tablesSubdir = "tables"

// OpenCatalog opens the catalog rooted at dir, recovering any tables from
// <dir>/tables/. dir == "" makes an in-memory catalog whose tables do not
// persist.
func OpenCatalog(dir string) (*Catalog, error) {
	cat := &Catalog{dir: dir, tables: map[string]*DB{}}
	if dir == "" {
		return cat, nil
	}
	root := filepath.Join(dir, tablesSubdir)
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("catalog: creating tables dir: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading tables dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !ValidTableName(name) {
			continue // ignore stray directories
		}
		d, err := OpenConfig(Config{Dir: filepath.Join(root, name)})
		if err != nil {
			cat.closeAll()
			return nil, fmt.Errorf("catalog: opening table %q: %w", name, err)
		}
		cat.tables[name] = d
	}
	return cat, nil
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

// CreateTable creates (or returns the existing) table named name. Its data
// persists under <dir>/tables/<name> for a persistent catalog.
func (cat *Catalog) CreateTable(name string) (*DB, error) {
	if !ValidTableName(name) {
		return nil, fmt.Errorf("catalog: invalid table name %q", name)
	}
	cat.mu.Lock()
	defer cat.mu.Unlock()
	if d, ok := cat.tables[name]; ok {
		return d, nil
	}
	cfgDir := ""
	if cat.dir != "" {
		cfgDir = filepath.Join(cat.dir, tablesSubdir, name)
	}
	d, err := OpenConfig(Config{Dir: cfgDir})
	if err != nil {
		return nil, fmt.Errorf("catalog: creating table %q: %w", name, err)
	}
	cat.tables[name] = d
	return d, nil
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

// Tables returns the table names, sorted.
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
	return first
}
