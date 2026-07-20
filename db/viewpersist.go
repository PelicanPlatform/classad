package db

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// A materialized view persists only its DEFINITION (ViewSpec) at
// <dir>/views/<name>/view.json; its data is in-memory and is rebuilt from the base table
// on reload (see Catalog.recoverViews). The definition is written atomically (tmp+rename),
// mirroring db/indexpersist.go.

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
	path := filepath.Join(vdir, viewDefFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
