package db

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// A materialized view persists only its DEFINITION (ViewSpec) at
// <dir>/views/<name>/view.json; its data is in-memory and is rebuilt from the base table
// on reload (see Catalog.recoverViews). The definition goes through the same atomic write
// as the tuned-configuration files (db/configpersist.go), and needs no lock of its own:
// it is written once when the view is created and never folded into afterwards.

const viewDefFile = "view.json"

// saveViewDef writes a view's definition under <dir>/views/<name>/view.json.
func saveViewDef(dir, name string, spec ViewSpec) error {
	vdir := filepath.Join(dir, viewsSubdir, name)
	if err := os.MkdirAll(vdir, 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(vdir, viewDefFile), data)
}

// loadViewDef reads the persisted definition for a view.
func loadViewDef(dir, name string) (ViewSpec, error) {
	path := filepath.Join(dir, viewsSubdir, name, viewDefFile)
	data, err := os.ReadFile(path) //nolint:gosec // G304 - path is catalog-internal
	if err != nil {
		return ViewSpec{}, err
	}
	var spec ViewSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return ViewSpec{}, fmt.Errorf("view %q: %w", name, err)
	}
	return spec, nil
}
