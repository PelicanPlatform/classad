package db

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// An ArchiveTable is an append-only, rotated history store (the "condor history file"
// use case), exposed as a catalog table type alongside the mutable tables. Ads are
// appended once and never updated or deleted individually; old data is dropped in bulk
// by rotation. Queries are newest-first with an optional limit -- condor_history's "last
// K" -- with whole-segment pruning via zone maps. See collections.Archive.
type ArchiveTable struct {
	a   *collections.Archive
	dir string
	cfg ArchiveConfig // the persisted config (archiveconfig.json); its Retention is mutable at runtime
}

// ArchiveConfig configures an archive table. Dir is set by the catalog.
type ArchiveConfig struct {
	// SegmentSize is the sealed-segment file size in bytes (default 8 MiB).
	SegmentSize int
	// HotAttrs / CategoricalAttrs / ValueAttrs tune the per-segment hot header and
	// indexes; ZoneAttrs names numeric attributes to keep per-segment min/max on for
	// whole-segment query pruning (value-indexed attributes are included automatically).
	HotAttrs                     []string
	CategoricalAttrs, ValueAttrs []string
	ZoneAttrs                    []string
	// Retention bounds what rotation keeps (max segments / bytes / age). Zero keeps all.
	Retention collections.Retention
}

// archiveConfigFile persists the ArchiveConfig so a reopen rebuilds the same
// indexes/zone maps/retention (the archive needs its option names re-supplied to
// re-derive interned ids on recovery). Its presence also marks the directory as an
// already-created archive, distinguishing "open" from "create".
const archiveConfigFile = "archiveconfig.json"

// openArchiveTable creates or reopens an archive under dir. On reopen the persisted
// config is authoritative (cfg is ignored). Archives always use a dictless ZSTD codec
// (deterministic, so recovery needs no persisted codec identity).
func openArchiveTable(dir string, cfg ArchiveConfig) (*ArchiveTable, error) {
	// archiveconfig.json is written on create and is authoritative on reopen, so its
	// presence distinguishes "open" from "create" (the backing store keeps no separate
	// catalog file of its own).
	create := true
	if data, rerr := os.ReadFile(filepath.Join(dir, archiveConfigFile)); rerr == nil {
		create = false
		var saved ArchiveConfig
		if json.Unmarshal(data, &saved) == nil {
			cfg = saved // reopen with the config the archive was created with
		}
	}

	codec, err := collections.NewZSTDCodec(nil)
	if err != nil {
		return nil, err
	}
	opts := collections.ArchiveOptions{
		Dir:              dir,
		SegmentSize:      cfg.SegmentSize,
		Codec:            codec,
		HotAttrs:         cfg.HotAttrs,
		CategoricalAttrs: cfg.CategoricalAttrs,
		ValueAttrs:       cfg.ValueAttrs,
		ZoneAttrs:        cfg.ZoneAttrs,
		Retention:        cfg.Retention,
	}
	var a *collections.Archive
	if create {
		a, err = collections.CreateArchive(opts)
	} else {
		a, err = collections.OpenArchive(opts)
	}
	if err != nil {
		return nil, err
	}
	t := &ArchiveTable{a: a, dir: dir, cfg: cfg}
	if create {
		// Persist the config so a later reopen rebuilds the same indexes/retention.
		if err := t.saveConfig(); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// saveConfig persists the archive's config (indexes, zone maps, retention) to
// archiveconfig.json atomically (tmp + rename), so a runtime change survives a restart and
// a reader never sees a torn file. A no-op for an in-memory archive (dir == "").
func (t *ArchiveTable) saveConfig() error {
	if t.dir == "" {
		return nil
	}
	data, err := json.MarshalIndent(t.cfg, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(t.dir, archiveConfigFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Append adds one ad to the archive (append-only; there is no update or per-key delete).
func (t *ArchiveTable) Append(ad *classad.ClassAd) error { return t.a.Append(ad) }

// AppendOld appends an ad parsed from old-ClassAd text (the qmgmt/history line format).
func (t *ArchiveTable) AppendOld(text string) error {
	ad, err := classad.ParseOld(text)
	if err != nil {
		return fmt.Errorf("archive: parsing ad: %w", err)
	}
	return t.a.Append(ad)
}

// Query returns the archived ads matching constraint, newest first. QueryLimit caps the
// result at the newest limit matches (<= 0 = all) -- the scan stops after the newest
// satisfying segments, so "last K" is cheap.
func (t *ArchiveTable) Query(constraint string) (iter.Seq[*classad.ClassAd], error) {
	return t.QueryLimit(constraint, 0)
}

func (t *ArchiveTable) QueryLimit(constraint string, limit int) (iter.Seq[*classad.ClassAd], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("archive: parsing constraint: %w", err)
	}
	return t.a.QueryLimit(q, limit), nil
}

// QueryProject scans the matching ads and yields each projected to just attrs' values, read
// wire-native where possible -- so an aggregate reads only the attributes it needs instead
// of fully decoding every record. Errors only on a malformed constraint.
func (t *ArchiveTable) QueryProject(constraint string, attrs []string) (iter.Seq[[]classad.Value], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("archive: parsing constraint: %w", err)
	}
	return t.a.QueryProject(q, attrs), nil
}

// QueryRawProjected yields each matching ad as a raw projected subset (only the projection
// attributes, rendered from the stored representation), newest first — the archive-side of the
// server-side projection op. redact strips private attributes. It mirrors db.DB.QueryRawProjected
// so the same wire op serves archives and mutable tables uniformly. chaseRefs is false, matching
// HTCondor's projection protocol (exactly the requested attributes).
func (t *ArchiveTable) QueryRawProjected(constraint string, projection []string, redact bool) (iter.Seq[collections.RawAd], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("archive: parsing constraint: %w", err)
	}
	return t.a.QueryRawProjected(q, projection, false, redact), nil
}

// Aggregate runs a server-side GROUP BY over the archive's matches: it applies the
// constraint (using the archive's zone-map pruning, so segments no matching record can
// fall in are never scanned), groups by the raw group columns, and reduces each group
// with COUNT/SUM/AVG/MIN/MAX. It shares the exact grouping/reduce engine (AggregateValues)
// the mutable-table aggregate uses, so an archive aggregate behaves identically to the same
// aggregate over a live table -- only the (small) grouped result is produced, not every
// matched ad. With no group columns it returns a single row over the whole match.
func (t *ArchiveTable) Aggregate(constraint string, groupBy []string, aggs []AggSpec) ([]AggRow, error) {
	// Fast path: an unconstrained COUNT(*) with no grouping is the retained record count,
	// which the archive tracks in O(1). This is the common `SELECT COUNT(*) FROM history`;
	// scanning tens of thousands of records to count them would be needlessly slow.
	if len(groupBy) == 0 && len(aggs) == 1 && aggs[0].Func == AggCount && aggs[0].Arg == "*" &&
		isMatchAll(constraint) {
		return []AggRow{{Values: []string{strconv.Itoa(t.a.Count())}}}, nil
	}

	groupCols := make([]GroupCol, len(groupBy))
	for i, g := range groupBy {
		groupCols[i] = GroupCol{Attr: g}
	}
	attrs, groupCol, aggCol := AggProjection(groupCols, aggs)

	// Scan wire-native, reading only the attributes the aggregation projects (empty for a
	// pure COUNT), so it does not fully decode every matched record -- mirroring the mutable
	// table's aggregate. Zone maps still prune segments no matching record can fall in.
	seq, err := t.QueryProject(constraint, attrs)
	if err != nil {
		return nil, err
	}
	return AggregateValues(seq, groupCols, aggs, groupCol, aggCol, nil), nil
}

// isMatchAll reports whether a constraint imposes no filter (an empty string or a literal
// "true"), so an aggregate over it covers every retained record.
func isMatchAll(constraint string) bool {
	c := strings.TrimSpace(constraint)
	return c == "" || strings.EqualFold(c, "true")
}

// Count is the number of records currently retained (reduced by rotation).
func (t *ArchiveTable) Count() int { return t.a.Count() }

// Rotate drops sealed segments that fall outside the retention policy, given the current
// time (unix seconds, for age-based retention). Returns how many segments were dropped.
func (t *ArchiveTable) Rotate(now float64) (int, error) { return t.a.Rotate(now) }

// Watch streams the archive's change events (append-only: upserts and the catch-up/live
// markers, no deletes), converting collections.WatchEvent to the db WatchEvent used by the
// mutable-table watch so the two are wire-identical. An empty cursor replays the retained
// history, then goes live. See collections.Archive.Watch.
func (t *ArchiveTable) Watch(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error) {
	seq, err := t.a.Watch(ctx, cursor)
	if err != nil {
		return nil, err
	}
	return func(yield func(WatchEvent) bool) {
		for ev := range seq {
			we := WatchEvent{Key: string(ev.Key), Ad: ev.Ad, Cursor: ev.Cursor}
			switch ev.Kind {
			case collections.WatchDelete:
				we.Kind = WatchDelete
			case collections.WatchReset:
				we.Kind = WatchReset
			case collections.WatchSynced:
				we.Kind = WatchSynced
			case collections.WatchResync:
				we.Kind = WatchResync
			}
			if !yield(we) {
				return
			}
		}
	}, nil
}

// Truncate drops every record, resetting the archive to empty in place (see
// collections.Archive.Truncate). It is the destructive reset behind a from-scratch history
// re-sync: empty the table, then re-ingest from the source. The persisted config (indexes,
// zone maps, retention) is retained, so the archive keeps its shape.
func (t *ArchiveTable) Truncate() { t.a.Truncate() }

// Close flushes and closes the archive.
func (t *ArchiveTable) Close() error { return t.a.Close() }

// RetrainDict trains a fresh ZSTD dictionary from up to sampleMax records and recompresses
// every segment in place under it, returning the new dictionary's size in bytes.
func (t *ArchiveTable) RetrainDict(sampleMax int) (int, error) { return t.a.RetrainDict(sampleMax) }

// Rewrite recompresses and re-encodes every segment in place under the current codec and
// hot set, returning the number of records rewritten.
func (t *ArchiveTable) Rewrite() int { return t.a.Rewrite() }

// AddIndex adds per-segment indexes on the named categorical and/or value attributes and
// rebuilds them over existing segments. Returns false if the index set was unchanged.
func (t *ArchiveTable) AddIndex(categorical, value []string) bool {
	return t.a.AddIndex(categorical, value)
}

// DropIndex removes the named per-segment indexes. Returns false if none matched.
func (t *ArchiveTable) DropIndex(names ...string) bool { return t.a.DropIndex(names...) }

// Reindex rebuilds the per-segment indexes over all segments.
func (t *ArchiveTable) Reindex() { t.a.Reindex() }

// --- diagnostics (mirrors the mutable-table stat surface for symmetry) ---

// Stats reports storage accounting (records, segments, arena/used/dead bytes).
func (t *ArchiveTable) Stats() Stats { return t.a.Stats() }

// OpStats reports cumulative operational timings. An archive has no DB-level snapshot lock,
// so SnapshotLock is zero.
func (t *ArchiveTable) OpStats() OpStats { return OpStats{OpStats: t.a.OpStats()} }

// CodecStats reports the archive's compression (codec, dict size, last retrain, sampled ratio).
func (t *ArchiveTable) CodecStats(sampleMax int) CodecStats { return t.a.CodecStats(sampleMax) }

// IndexSizes reports the per-attribute index byte footprint.
func (t *ArchiveTable) IndexSizes() IndexSizes { return t.a.IndexSizes() }

// SidecarSizes reports the sealed-segment sidecar index bytes (mmap-backed, evictable).
func (t *ArchiveTable) SidecarSizes() SidecarSizes { return t.a.SidecarSizes() }

// IndexedAttrs returns the archive's categorical and value index attributes.
func (t *ArchiveTable) IndexedAttrs() (categorical, value []string) { return t.a.IndexedAttrs() }

// Retention returns the archive's current retention bounds.
func (t *ArchiveTable) Retention() collections.Retention { return t.a.Retention() }

// SetRetention updates the retention bounds and persists them (archiveconfig.json), so they
// take effect on the next Rotate and survive a restart.
func (t *ArchiveTable) SetRetention(r collections.Retention) error {
	t.a.SetRetention(r)
	t.cfg.Retention = r
	return t.saveConfig()
}
