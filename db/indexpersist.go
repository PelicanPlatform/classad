package db

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Runtime index/hot-set configuration is persisted next to the ClassAd log so
// that indexes and hot attributes created at runtime (AddIndex / AddHotAttrs /
// RefreshHotSet, e.g. via a daemon's admin commands) survive a restart. The
// file records the *current* set; on open it is reconciled so the live
// configuration matches exactly what was last persisted, and the indexes are
// rebuilt over the loaded ads. In-memory databases (no directory) do not persist.
const indexConfigFile = "indexcfg.json"

type persistedIndexConfig struct {
	Categorical []string `json:"categorical,omitempty"`
	Value       []string `json:"value,omitempty"`
	// Auto lists the indexes created by the auto-tuner (a subset of Categorical/Value);
	// restoring this provenance keeps the memory-budget trimmer able to remove them and
	// keeps human-created indexes exempt across restarts.
	Auto []string `json:"auto,omitempty"`
	Hot  []string `json:"hot,omitempty"`
}

func (db *DB) indexConfigPath() string {
	if db.dir == "" {
		return ""
	}
	return filepath.Join(db.dir, indexConfigFile)
}

// saveIndexConfig writes the current index + hot-set configuration atomically.
// Best-effort: a write failure is ignored (the configuration is still live for
// this run; only its persistence is lost).
func (db *DB) saveIndexConfig() {
	path := db.indexConfigPath()
	if path == "" {
		return
	}
	cat, val := db.c.IndexedAttrs()
	cfg := persistedIndexConfig{Categorical: cat, Value: val, Auto: db.c.AutoIndexNames(), Hot: db.c.HotAttrNames()}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// loadIndexConfig reconciles the live index configuration to the persisted set
// and rebuilds the indexes over the loaded ads, then pins the persisted hot
// attributes. A missing/unreadable file is a no-op (first run, or in-memory).
func (db *DB) loadIndexConfig() {
	path := db.indexConfigPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var cfg persistedIndexConfig
	if json.Unmarshal(data, &cfg) != nil {
		return
	}

	// Desired kind per attribute (categorical wins if listed as both).
	want := map[string]string{}
	for _, n := range cfg.Value {
		want[n] = "val"
	}
	for _, n := range cfg.Categorical {
		want[n] = "cat"
	}
	// Current kind per attribute.
	curCat, curVal := db.c.IndexedAttrs()
	cur := map[string]string{}
	for _, n := range curCat {
		cur[n] = "cat"
	}
	for _, n := range curVal {
		cur[n] = "val"
	}

	autoSet := map[string]bool{}
	for _, n := range cfg.Auto {
		autoSet[n] = true
	}

	var drop []string
	var addCatH, addCatA, addValH, addValA []string // human vs auto adds, to restore provenance
	for n, k := range cur {                         // drop anything not wanted, or wanted as a different kind
		if want[n] != k {
			drop = append(drop, n)
		}
	}
	for n, k := range want { // add anything wanted that is missing or the wrong kind
		if cur[n] == k {
			continue
		}
		switch {
		case k == "cat" && autoSet[n]:
			addCatA = append(addCatA, n)
		case k == "cat":
			addCatH = append(addCatH, n)
		case autoSet[n]:
			addValA = append(addValA, n)
		default:
			addValH = append(addValH, n)
		}
	}

	changed := false
	if len(drop) > 0 {
		db.c.DropIndex(drop...)
		changed = true
	}
	if len(addCatH) > 0 || len(addValH) > 0 {
		db.c.AddIndex(addCatH, addValH)
		changed = true
	}
	if len(addCatA) > 0 || len(addValA) > 0 {
		db.c.AddAutoIndex(addCatA, addValA)
		changed = true
	}
	if changed {
		db.c.Reindex() // build the reconciled indexes over the already-loaded ads
	}
	if len(cfg.Hot) > 0 {
		db.c.AddHotAttrs(cfg.Hot...)
	}
}
