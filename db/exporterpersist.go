package db

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// An exporter persists two things under <dir>/exporters/<name>/:
//   - def.json  : the ExporterDef (name, kind, opaque per-kind config), written on
//     create and never mutated afterward.
//   - state.bin : an opaque resume-state blob the exporter owns (e.g. a watch cursor,
//     a monotonic export sequence, and a per-key version map for delete detection),
//     rewritten on every checkpoint.
//
// Both are written atomically (tmp+rename), mirroring db/viewpersist.go. The catalog
// never interprets either payload.

const (
	exporterDefFile   = "def.json"
	exporterStateFile = "state.bin"
)

func exporterDir(dir, name string) string {
	return filepath.Join(dir, exportersSubdir, name)
}

// saveExporterDef writes an exporter's definition under <dir>/exporters/<name>/def.json.
func saveExporterDef(dir string, def ExporterDef) error {
	edir := exporterDir(dir, def.Name)
	if err := os.MkdirAll(edir, 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(def, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(edir, exporterDefFile), data)
}

// loadExporterDef reads the persisted definition for an exporter.
func loadExporterDef(dir, name string) (ExporterDef, error) {
	path := filepath.Join(exporterDir(dir, name), exporterDefFile)
	data, err := os.ReadFile(path) //nolint:gosec // G304 - path is catalog-internal
	if err != nil {
		return ExporterDef{}, err
	}
	var def ExporterDef
	if err := json.Unmarshal(data, &def); err != nil {
		return ExporterDef{}, fmt.Errorf("exporter %q: %w", name, err)
	}
	return def, nil
}

// saveExporterStateFile atomically writes an exporter's opaque resume-state blob.
func saveExporterStateFile(dir, name string, state []byte) error {
	edir := exporterDir(dir, name)
	if err := os.MkdirAll(edir, 0o750); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(edir, exporterStateFile), state)
}

// loadExporterStateFile reads an exporter's opaque resume-state blob. The bool is false
// when no state has been checkpointed yet (a fresh exporter), which is not an error.
func loadExporterStateFile(dir, name string) ([]byte, bool, error) {
	path := filepath.Join(exporterDir(dir, name), exporterStateFile)
	data, err := os.ReadFile(path) //nolint:gosec // G304 - path is catalog-internal
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// removeExporterDir deletes an exporter's on-disk directory (definition + state).
func removeExporterDir(dir, name string) error {
	return os.RemoveAll(exporterDir(dir, name))
}

// writeAtomic writes data to path via a temp file and rename, so a reader never observes
// a partial write.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // G306 - catalog metadata, not secret
		return err
	}
	return os.Rename(tmp, path)
}
