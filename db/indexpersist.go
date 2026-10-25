package db

import (
	"sort"
	"strings"
	"time"

	"github.com/PelicanPlatform/classad/collections"
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
	// Hot is the HUMAN tier of the hot set: what Config.HotAttrs asked for at open plus
	// whatever AddHotAttrs named since. AutoHot is what the demand-driven RefreshHotSet put
	// on top. The live set is their union, and each is restored separately on reopen.
	//
	// They were one field, holding the whole live set, which is the same mistake the Auto
	// field above exists to avoid: a sampled attribute came back from a restart
	// indistinguishable from one an operator pinned, so nothing downstream could tell a
	// guess from an instruction -- and the operator's list was whatever the last sample
	// happened to agree with.
	//
	// A file written before the split has `hot` and no `autoHot`, and every entry in it is
	// read as human. That direction is chosen because the two errors are not comparable:
	// reading an auto entry as human leaves an attribute pinned that a refresh could
	// otherwise have dropped, which costs a few bytes in each record's hot header and is
	// undone by naming it; reading a human entry as auto discards an operator's pin the
	// first time a sample disagrees, silently and with no record that it was ever asked
	// for. The first is visible and reversible, the second is neither.
	Hot     []string `json:"hot,omitempty"`
	AutoHot []string `json:"autoHot,omitempty"`
	// Encrypted lists the explicitly encrypted attributes (the human-toggled set, not
	// the always-on private attributes). Restoring it keeps a runtime encryption-policy
	// change consistent across restarts -- and, in HA, lets a follower converge on the
	// policy by reloading, since there is no op-stream replication of config.
	Encrypted []string `json:"encrypted,omitempty"`
	// SchemaRequire lists the required schema attributes added AT RUNTIME (the configured tier
	// comes back from Config.SchemaRequireAttrs on the next open, so persisting it too would
	// freeze a configuration change out -- the same provenance split the Auto field above makes
	// for indexes). Restoring it keeps a runtime addition from silently resetting on restart,
	// which is exactly the kind of loss nothing surfaces: the attribute just stops being a
	// column, and queries get slower with no sign of why.
	SchemaRequire []string `json:"schemaRequire,omitempty"`
	// Time-travel (point-in-time / AS OF) settings, a runtime-toggled per-table policy
	// persisted like Encrypted. TimeTravel off (default) leaves the others inert.
	TimeTravel                bool `json:"timeTravel,omitempty"`
	TimeTravelMaxSeconds      int  `json:"timeTravelMaxSeconds,omitempty"`
	TimeTravelCheckpointSeced int  `json:"timeTravelCheckpointSeconds,omitempty"`
}

// timeTravelOptions converts the persisted seconds to a collections option set, or nil
// when disabled. The checkpoint interval defaults inside collections if left zero.
func (p persistedIndexConfig) timeTravelOptions() *collections.TimeTravelOptions {
	if !p.TimeTravel || p.TimeTravelMaxSeconds <= 0 {
		return nil
	}
	return &collections.TimeTravelOptions{
		MaxDistance:        time.Duration(p.TimeTravelMaxSeconds) * time.Second,
		CheckpointInterval: time.Duration(p.TimeTravelCheckpointSeced) * time.Second,
	}
}

// readPersistedTimeTravel reads just the time-travel settings from dir's index-config
// file, before the collection is opened, so recovery can rebuild the time index. Nil
// when there is no dir, no file, or time travel is off.
func readPersistedTimeTravel(dir string) *collections.TimeTravelOptions {
	cfg, _, ok := readConfigFile[persistedIndexConfig](dir, indexConfigFile)
	if !ok {
		return nil
	}
	return cfg.timeTravelOptions()
}

// saveIndexConfig writes the current index + hot-set configuration atomically.
// Best-effort: a write failure is ignored (the configuration is still live for
// this run; only its persistence is lost).
//
// The whole file is derived from the live collection on every save rather than
// accumulated in the held struct, because the collection is where this table tracks
// index provenance (AutoIndexNames) and the runtime required tier. The archive tracks
// its own provenance in the held struct instead; see ArchiveTable.
//
// The derive runs inside the store's critical section, so that two savers cannot each
// snapshot the collection and then write in the opposite order -- which drops whichever
// change the older snapshot had not seen.
//
// pinHot names attributes to fold into the human hot tier as part of the same write -- what
// AddHotAttrs passes, so the name someone asked for and the save that records it cannot be
// split by another saver. Every other caller passes none.
func (db *DB) saveIndexConfig(pinHot ...string) {
	_ = db.cfg.update(func(cfg *persistedIndexConfig) {
		*cfg = db.deriveIndexConfig(mergeNames(cfg.Hot, pinHot))
	})
}

// deriveIndexConfig reads the collection's current index set, hot set, encryption policy,
// runtime required tier and time-travel setting into the persisted shape. Called with the
// config store's lock held.
//
// humanHot is the human tier carried over from the held config, because the collection does
// not track hot provenance -- it knows only the live set. Everything live and not in that
// tier is what a refresh added. This is the one field the derive cannot take wholly from
// the collection, which is why it is threaded through rather than read here.
func (db *DB) deriveIndexConfig(humanHot []string) persistedIndexConfig {
	cat, val := db.c.IndexedAttrs()
	cfg := persistedIndexConfig{
		Categorical: cat, Value: val, Auto: db.c.AutoIndexNames(),
		Hot: humanHot, AutoHot: autoHotNames(humanHot, db.c.HotAttrNames()),
		Encrypted: db.c.EncryptedAttrNames(), SchemaRequire: db.c.SchemaRequireRuntimeAttrs(),
	}
	if o, on := db.c.TimeTravelConfig(); on {
		cfg.TimeTravel = true
		cfg.TimeTravelMaxSeconds = int(o.MaxDistance / time.Second)
		cfg.TimeTravelCheckpointSeced = int(o.CheckpointInterval / time.Second)
	}
	return cfg
}

// loadIndexConfig reconciles the live index configuration to the persisted set
// and rebuilds the indexes over the loaded ads, then pins the persisted hot
// attributes. A missing/unreadable file is a no-op (first run, or in-memory).
func (db *DB) loadIndexConfig() {
	cfg, _, ok := readConfigFile[persistedIndexConfig](db.dir, indexConfigFile)
	if !ok {
		return
	}
	db.cfg.with(func(held *persistedIndexConfig) {
		// The store was seeded with Config.HotAttrs, which is configured and therefore human
		// however old the file is. Keep it: a name the application asks for at open must not
		// be filed as a sample's doing by the next save just because the file predates it.
		configured := held.Hot
		*held = cfg
		held.Hot = mergeNames(append([]string(nil), configured...), cfg.Hot)
	})

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
	// Restore both hot tiers, human first. Each is added rather than installed over, so the
	// live set comes back as their union and neither can erase the other.
	if len(cfg.Hot) > 0 {
		db.c.AddHotAttrs(cfg.Hot...)
	}
	if len(cfg.AutoHot) > 0 {
		db.c.AddHotAttrs(cfg.AutoHot...)
	}
	// Restore the runtime-required schema attributes on top of the configured tier. Changing the
	// set does not re-derive the schema (that is ReschemaScan's job and costs the whole table),
	// so this only ensures the next derivation sees what the operator asked for.
	if len(cfg.SchemaRequire) > 0 {
		db.c.AddSchemaRequireAttrs(cfg.SchemaRequire...)
	}
	// Restore the runtime encryption policy over the reconciled indexes. Best-effort:
	// ignored if encryption is disabled on this open (no data key) or an attribute has
	// since become indexed. Private attributes are always encrypted regardless.
	if len(cfg.Encrypted) > 0 {
		_ = db.c.SetEncryptedAttrs(cfg.Encrypted)
	}
}

// autoHotNames is the live hot set minus the human tier: what a demand-driven refresh added.
// Case-insensitive, like every other attribute-name comparison here, and sorted so the file
// does not churn on a set that has not changed.
func autoHotNames(human, live []string) []string {
	if len(live) == 0 {
		return nil
	}
	isHuman := make(map[string]bool, len(human))
	for _, n := range human {
		isHuman[strings.ToLower(n)] = true
	}
	var auto []string
	for _, n := range live {
		if !isHuman[strings.ToLower(n)] {
			auto = append(auto, n)
		}
	}
	sort.Strings(auto)
	return auto
}
