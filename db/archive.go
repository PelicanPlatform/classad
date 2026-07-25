package db

import (
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"

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
	if create {
		// Persist the config so a later reopen rebuilds the same indexes/retention.
		if data, merr := json.MarshalIndent(cfg, "", "  "); merr == nil {
			_ = os.WriteFile(filepath.Join(dir, archiveConfigFile), data, 0o644)
		}
	}
	return &ArchiveTable{a: a, dir: dir}, nil
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

// Aggregate runs a server-side GROUP BY over the archive's matches: it applies the
// constraint (using the archive's zone-map pruning, so segments no matching record can
// fall in are never scanned), groups by the raw group columns, and reduces each group
// with COUNT/SUM/AVG/MIN/MAX. It shares the exact grouping/reduce engine (AggregateValues)
// the mutable-table aggregate uses, so an archive aggregate behaves identically to the same
// aggregate over a live table -- only the (small) grouped result is produced, not every
// matched ad. With no group columns it returns a single row over the whole match.
func (t *ArchiveTable) Aggregate(constraint string, groupBy []string, aggs []AggSpec) ([]AggRow, error) {
	groupCols := make([]GroupCol, len(groupBy))
	for i, g := range groupBy {
		groupCols[i] = GroupCol{Attr: g}
	}
	attrs, groupCol, aggCol := AggProjection(groupCols, aggs)

	// limit <= 0: scan the whole (zone-pruned) match; the aggregate reduces it server-side.
	seq, err := t.QueryLimit(constraint, 0)
	if err != nil {
		return nil, err
	}
	// Project each matched ad to just the attributes the aggregation reads. The scratch
	// slice is reused across rows (AggregateValues copies a group's key only when created).
	proj := func(yield func([]classad.Value) bool) {
		scratch := make([]classad.Value, len(attrs))
		for ad := range seq {
			for i, n := range attrs {
				scratch[i] = ad.EvaluateAttr(n)
			}
			if !yield(scratch) {
				return
			}
		}
	}
	return AggregateValues(proj, groupCols, aggs, groupCol, aggCol, nil), nil
}

// Count is the number of records currently retained (reduced by rotation).
func (t *ArchiveTable) Count() int { return t.a.Count() }

// Rotate drops sealed segments that fall outside the retention policy, given the current
// time (unix seconds, for age-based retention). Returns how many segments were dropped.
func (t *ArchiveTable) Rotate(now float64) (int, error) { return t.a.Rotate(now) }

// Close flushes and closes the archive.
func (t *ArchiveTable) Close() error { return t.a.Close() }
