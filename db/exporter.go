package db

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ExporterDef defines an external sink that mirrors this database's change stream
// elsewhere (for example, a Kafka change-data exporter that federates several instances
// through a broker). The catalog is a passive registry: it stores the definition and an
// opaque resume-state blob and never runs, interprets, or validates Config -- that is the
// job of the out-of-process exporter (which reads these over dbrpc). Keeping the DB free
// of any sink-specific dependency is deliberate.
type ExporterDef struct {
	Name string `json:"name"`
	// Kind selects the exporter implementation (e.g. "kafka"). The catalog does not
	// enumerate valid kinds; an unknown kind is simply ignored by any exporter that does
	// not recognize it.
	Kind string `json:"kind"`
	// Config is the exporter's own configuration, opaque to the catalog. It is stored and
	// returned verbatim.
	Config json.RawMessage `json:"config,omitempty"`
}

// CreateExporter registers a new exporter definition. The name must be a valid identifier
// and must not already be registered. Exporters occupy their own namespace, so an exporter
// may share a name with a table (an exporter named "jobs" that mirrors the "jobs" table is
// expected). On a persistent catalog the definition is written to disk before it is
// returned; on an in-memory catalog it lives only in memory.
func (cat *Catalog) CreateExporter(def ExporterDef) error {
	if !ValidTableName(def.Name) {
		return fmt.Errorf("invalid exporter name %q", def.Name)
	}
	if def.Kind == "" {
		return fmt.Errorf("exporter %q: kind must be set", def.Name)
	}
	cat.mu.Lock()
	defer cat.mu.Unlock()
	if _, ok := cat.exporters[def.Name]; ok {
		return fmt.Errorf("exporter %q already exists", def.Name)
	}
	if cat.dir != "" {
		if err := saveExporterDef(cat.dir, def); err != nil {
			return fmt.Errorf("exporter %q: persisting definition: %w", def.Name, err)
		}
	}
	cat.exporters[def.Name] = def
	return nil
}

// DropExporter removes an exporter's definition and its resume state. It is not an error to
// drop an exporter that does not exist (idempotent teardown).
func (cat *Catalog) DropExporter(name string) error {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	if _, ok := cat.exporters[name]; !ok {
		return nil
	}
	if cat.dir != "" {
		if err := removeExporterDir(cat.dir, name); err != nil {
			return fmt.Errorf("exporter %q: removing on-disk state: %w", name, err)
		}
	}
	delete(cat.exporters, name)
	delete(cat.memExporterState, name)
	return nil
}

// Exporters returns all registered exporter definitions, sorted by name.
func (cat *Catalog) Exporters() []ExporterDef {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	out := make([]ExporterDef, 0, len(cat.exporters))
	for _, def := range cat.exporters {
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Exporter returns a single exporter definition.
func (cat *Catalog) Exporter(name string) (ExporterDef, bool) {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	def, ok := cat.exporters[name]
	return def, ok
}

// SaveExporterState durably records an exporter's opaque resume-state blob, replacing any
// prior state. The exporter calls this after its data has been accepted downstream, so that
// on restart it resumes just past the last delivered change (at-least-once). The exporter
// must already exist.
func (cat *Catalog) SaveExporterState(name string, state []byte) error {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	if _, ok := cat.exporters[name]; !ok {
		return fmt.Errorf("exporter %q does not exist", name)
	}
	if cat.dir == "" {
		cat.memExporterState[name] = append([]byte(nil), state...)
		return nil
	}
	if err := saveExporterStateFile(cat.dir, name, state); err != nil {
		return fmt.Errorf("exporter %q: persisting state: %w", name, err)
	}
	return nil
}

// LoadExporterState returns an exporter's last checkpointed resume-state blob. The bool is
// false when the exporter has never checkpointed (a fresh exporter with no state yet),
// which callers treat as "start from the beginning", not as an error.
func (cat *Catalog) LoadExporterState(name string) ([]byte, bool, error) {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	if _, ok := cat.exporters[name]; !ok {
		return nil, false, fmt.Errorf("exporter %q does not exist", name)
	}
	if cat.dir == "" {
		state, ok := cat.memExporterState[name]
		return state, ok, nil
	}
	return loadExporterStateFile(cat.dir, name)
}

// recoverExporters loads exporter definitions from <dir>/exporters/ on catalog open. A
// definition that cannot be read or parsed is skipped rather than failing catalog open,
// mirroring recoverViews; its resume state (if any) stays on disk and is picked up if the
// definition is later restored. Resume-state blobs are not loaded here -- the exporter
// reads them on demand via LoadExporterState.
func (cat *Catalog) recoverExporters() error {
	root := filepath.Join(cat.dir, exportersSubdir)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("catalog: reading exporters dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || !ValidTableName(e.Name()) {
			continue
		}
		def, err := loadExporterDef(cat.dir, e.Name())
		if err != nil {
			continue // skip a corrupt/unreadable definition rather than fail open
		}
		cat.exporters[def.Name] = def
	}
	return nil
}
