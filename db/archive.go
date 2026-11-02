package db

import (
	"context"
	"fmt"
	"iter"
	"os"
	"sort"
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
	a *collections.Archive

	// cfg is the persisted config (archiveconfig.json); its Retention, index set, hot set,
	// row-group budget and required-attribute set are all mutable at runtime.
	//
	// Several methods fold live state back into it and persist it, and a caller is free to
	// drive them from different goroutines (a background index backfill while the foreground
	// asserts a config, say). cfg is one struct and the whole of it is marshalled, so an
	// unguarded pair is a torn write as well as a data race. configFile holds its lock across
	// the update and the file write for that reason; see db/configpersist.go, which the
	// mutable table's indexcfg.json goes through too.
	cfg *configFile[ArchiveConfig]

	// enc is the archive's encryption state, resolved for its own directory exactly as a mutable
	// table's is (see resolveCrypto), or nil for an archive opened with no sealing at all -- a
	// continuous view's, whose rows are built with no key for every reader.
	enc *dbCrypto
	// sealedIndexes names the configured indexes this open left out because the sealing policy
	// names their attribute (see openArchiveTable).
	sealedIndexes []string
}

// StopMaintenance asks any maintenance pass running on this archive to stop at its next safe
// boundary. See collections.Collection.StopMaintenance.
func (a *ArchiveTable) StopMaintenance() { a.a.StopMaintenance() }

// MaintProgress reports the maintenance pass running on this archive, or the zero value when
// none is. It takes no lock, so it answers while the pass it describes holds the maintenance
// lock. See collections.Collection.MaintProgress.
func (a *ArchiveTable) MaintProgress() collections.MaintProgress { return a.a.MaintProgress() }

// MaintenanceStopping reports whether this archive has been asked to stop maintenance. See
// collections.Collection.MaintenanceStopping.
func (a *ArchiveTable) MaintenanceStopping() bool { return a.a.MaintenanceStopping() }

// ArchiveConfig configures an archive table. Dir is set by the catalog.
type ArchiveConfig struct {
	// SegmentSize is the sealed-segment file size in bytes (default 8 MiB).
	SegmentSize int
	// RowGroupBytes is the uncompressed record-bytes budget for one columnar row group. 0 takes the
	// default (see collections.Options.RowGroupBytes).
	//
	// Like the rest of this config it is read at CREATE time only -- a reopened archive uses the
	// persisted value. Use SetRowGroupBytes to change it on a live archive: unlike the index set, this
	// needs no reconciliation, because each block records its own layout, so a new budget governs
	// segments sealed from then on and older ones keep theirs.
	//
	// It trades point-lookup cost against how far compression can see across records: a larger group
	// stores the same data in fewer bytes and makes reading one record decompress more of it. The
	// default is measured on real ads, but the right point depends on the read mix and on how much of
	// the working set fits the block cache, which only a production-sized archive can answer.
	RowGroupBytes int
	// HotAttrs / CategoricalAttrs / ValueAttrs tune the per-segment hot header and
	// indexes; ZoneAttrs names numeric attributes to keep per-segment min/max on for
	// whole-segment query pruning (value-indexed attributes are included automatically).
	HotAttrs                     []string
	CategoricalAttrs, ValueAttrs []string
	ZoneAttrs                    []string
	// AutoAttrs names the subset of the above created by the auto-tuner rather than by a
	// human. Provenance has to be persisted with the index set, or an auto index returns
	// from a restart indistinguishable from one someone asked for -- exempt from trimming
	// and from any future auto-drop, permanently, on the strength of nothing.
	AutoAttrs []string
	// AutoHotAttrs names the hot attributes the demand-driven refresh (RefreshHotSet)
	// installed on top of HotAttrs, and is the hot-set counterpart of AutoAttrs: on reopen
	// the live set is HotAttrs plus these.
	//
	// It is a separate field rather than an update of HotAttrs because this file is
	// authoritative on reopen, so folding a sampled set back into HotAttrs would make the
	// tuner's guess indistinguishable from what an operator asked for -- and would quietly
	// discard the operator's list the first time a sample disagreed with it. HotAttrs stays
	// whatever a human configured; this records what the samples added.
	//
	// Without persisting it at all, a refreshed set would reset to the creation-time
	// HotAttrs on every restart, and the hot headers of the segments written since would be
	// the only trace that it ever ran.
	AutoHotAttrs []string
	// IndexBackfillBytes bounds how far back an index configuration change is carried at the
	// moment it is made: only segments within the newest N bytes have their existing index
	// rebuilt by AddIndex and Reindex; older ones keep the one they have until the
	// maintenance pass's budgeted backfill (MaintainOptions.ArchiveIndexBackfillBytes) or an
	// explicit ReindexAll reaches them (see collections.Options.IndexBackfillBytes).
	//
	// Zero takes defaultIndexBackfillBytes. A NEGATIVE value means no bound: the change is
	// carried across the whole archive in one go, which for history means decompressing every
	// record to add one index -- hours at scale, in a call that holds up whatever made it.
	// That is a reasonable thing to ask for deliberately and a bad thing to get by leaving a
	// field unset, which is why it is not what zero means.
	IndexBackfillBytes int64
	// SchemaRequireAttrs names attributes that enter the derived columnar schema whatever their
	// presence rate (see collections.Options.SchemaRequireAttrs). It is the CONFIGURED tier:
	// read at create, persisted here, and authoritative on reopen like the rest of this config,
	// so an application whose own configuration changed re-asserts it with SetSchemaRequireAttrs.
	//
	// An archive is where this matters most. Its records are written once, already in their
	// final state, so an attribute a record acquires only late in its life -- the resource
	// figures a job has only after it has run -- is simultaneously the thing history queries read
	// and the thing the presence floor excludes, because the records that never reached that
	// state are in the same table.
	SchemaRequireAttrs []string
	// SchemaRequireRuntime names the subset added at runtime rather than configured, kept apart
	// for the reason AutoAttrs is: a name that comes back from a restart should say where it came
	// from. SetSchemaRequireAttrs replaces the configured tier and leaves this one alone, so an
	// operator's experiment is not erased by the application re-asserting its config.
	SchemaRequireRuntime []string
	// GroupSchemaCount is how many secondary columnar schemas to derive for attributes the base
	// schema does not carry (see db.Config). 0 takes the default; a NEGATIVE value builds none.
	// An archive's maintenance cadence is long, so the stability gate's several derivations span
	// days rather than minutes -- the intended shape for a table whose structure changes on that
	// scale, and the reason an archive builds none for its first few days.
	GroupSchemaCount    int
	GroupStabilityRuns  int
	GroupMergeJaccard   float64
	GroupMaxPartialFrac float64
	// Retention bounds what rotation keeps (max segments / bytes / age). Zero keeps all.
	Retention collections.Retention
	// EncryptedAttrs is the explicit set of attributes sealed at rest (private attributes are always
	// sealed), as for a mutable table (see Config.EncryptedAttrs). Empty takes the catalog's default
	// set. A non-empty set is authoritative, and it is what SetEncryptedAttrs persists, so a runtime
	// change survives a restart exactly as a mutable table's does.
	EncryptedAttrs []string `json:",omitempty"`
}

// archiveSealing is what an archive table seals with: the catalog's pool keys, which protect the
// master key resolved for the archive's directory, and its default explicit set.
type archiveSealing struct {
	poolKeys []KEK
	encAttrs []string
}

// defaultIndexBackfillBytes is how far back an index change is carried when the config does
// not say. At the 8 MiB default segment size this is ~128 segments, a few seconds of
// decompression -- small enough to be unremarkable on a maintenance pass, and large enough
// to cover the recent history where newest-first queries actually land.
const defaultIndexBackfillBytes = 1 << 30

// backfillHorizon resolves IndexBackfillBytes: 0 takes the default, negative means
// explicitly unbounded (which the storage layer spells as 0).
func (cfg ArchiveConfig) backfillHorizon() int64 {
	switch {
	case cfg.IndexBackfillBytes < 0:
		return 0
	case cfg.IndexBackfillBytes == 0:
		return defaultIndexBackfillBytes
	default:
		return cfg.IndexBackfillBytes
	}
}

// archiveConfigFile persists the ArchiveConfig so a reopen rebuilds the same
// indexes/zone maps/retention (the archive needs its option names re-supplied to
// re-derive interned ids on recovery). Its presence also marks the directory as an
// already-created archive, distinguishing "open" from "create".
const archiveConfigFile = "archiveconfig.json"

// archiveDirFresh reports whether dir holds no archive yet: it is absent, empty, or holds only the
// master key a creation that stopped part way resolved before anything else.
func archiveDirFresh(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true // absent (or unreadable, which the open then reports)
	}
	for _, e := range entries {
		switch strings.TrimSuffix(e.Name(), ".tmp") {
		case masterKeysFile, unprotectedMasterFile:
		default:
			return false
		}
	}
	return true
}

// sealingPolicy is the predicate a collection with a key seals by: every private attribute, and the
// explicit set, case-insensitively (see collections.Collection.shouldEncrypt).
func sealingPolicy(explicit []string) func(string) bool {
	set := make(map[string]struct{}, len(explicit))
	for _, n := range explicit {
		set[strings.ToLower(n)] = struct{}{}
	}
	return func(name string) bool {
		if classad.IsPrivateAttribute(name) {
			return true
		}
		_, ok := set[strings.ToLower(name)]
		return ok
	}
}

// splitSealed returns names without the ones sealed reports, appending those to dropped.
func splitSealed(names []string, sealed func(string) bool, dropped []string) (kept, allDropped []string) {
	for _, n := range names {
		if sealed(n) {
			dropped = append(dropped, n)
			continue
		}
		kept = append(kept, n)
	}
	return kept, dropped
}

// openArchiveTable creates or reopens an archive under dir. On reopen the persisted
// config is authoritative (cfg is ignored). Archives always use a dictless ZSTD codec
// (deterministic, so recovery needs no persisted codec identity).
//
// seal, when non-nil, gives the archive a data key, resolved for dir exactly as a mutable table's is,
// so it seals private attributes always and the explicit set too, as a mutable table does. nil opens
// it with none.
func openArchiveTable(dir string, cfg ArchiveConfig, seal *archiveSealing) (*ArchiveTable, error) {
	// archiveconfig.json is written on create and is authoritative on reopen, so its
	// presence distinguishes "open" from "create" (the backing store keeps no separate
	// catalog file of its own).
	saved, exists, ok := readConfigFile[ArchiveConfig](dir, archiveConfigFile)
	create := !exists
	if ok {
		cfg = saved // reopen with the config the archive was created with
	}
	// The store refuses to be created over existing files, and the key resolved below is one, so
	// that check is made here first. A directory holding nothing but a key is one whose creation
	// stopped between the two, and is created as if empty.
	if create && !archiveDirFresh(dir) {
		return nil, fmt.Errorf("archive: %s already initialized (use OpenArchive)", dir)
	}
	var enc *dbCrypto
	encAttrs := cfg.EncryptedAttrs
	if seal != nil {
		var err error
		if enc, err = resolveCrypto(dir, seal.poolKeys); err != nil {
			return nil, err
		}
		if len(encAttrs) == 0 {
			encAttrs = seal.encAttrs
		}
	}
	// A sealed value is stored opaque and can never satisfy an index, so an attribute both sealed and
	// indexed is refused (collections.Options.EncryptedAttrs). Here the sealing wins, as it does when a
	// mutable table reloads its persisted indexes: an archive created before it had a key, or before
	// the attribute was sealed, may have indexed it, and refusing the open over it would take the
	// archive -- and the catalog opening it -- down with it.
	var sealedIndexes []string
	categorical, value := cfg.CategoricalAttrs, cfg.ValueAttrs
	if enc != nil {
		sealed := sealingPolicy(encAttrs)
		categorical, sealedIndexes = splitSealed(categorical, sealed, sealedIndexes)
		value, sealedIndexes = splitSealed(value, sealed, sealedIndexes)
	}

	codec, err := collections.NewZSTDCodec(nil)
	if err != nil {
		return nil, err
	}
	opts := collections.ArchiveOptions{
		Dir:                 dir,
		SegmentSize:         cfg.SegmentSize,
		RowGroupBytes:       cfg.RowGroupBytes,
		Codec:               codec,
		HotAttrs:            cfg.HotAttrs,
		GroupSchemaCount:    cfg.GroupSchemaCount,
		GroupStabilityRuns:  cfg.GroupStabilityRuns,
		GroupMergeJaccard:   cfg.GroupMergeJaccard,
		GroupMaxPartialFrac: cfg.GroupMaxPartialFrac,
		CategoricalAttrs:    categorical,
		ValueAttrs:          value,
		ZoneAttrs:           cfg.ZoneAttrs,
		SchemaRequireAttrs:  cfg.SchemaRequireAttrs,
		IndexBackfillBytes:  cfg.backfillHorizon(),
		Retention:           cfg.Retention,
		DataKey:             enc.data(),
		EncryptedAttrs:      encAttrs,
	}
	// Created or reopened alike: the emptiness CreateArchive would check was checked above.
	a, err := collections.OpenArchive(opts)
	if err != nil {
		return nil, err
	}
	t := &ArchiveTable{a: a, cfg: newConfigFile(dir, archiveConfigFile, cfg), enc: enc, sealedIndexes: sealedIndexes}
	// The index set above was rebuilt from the config as human-created; re-mark the ones
	// the tuner made. This adds no index (they are already present) -- it only restores
	// provenance.
	if len(cfg.AutoAttrs) > 0 {
		a.MarkAutoIndexes(cfg.AutoAttrs)
	}
	// Restore the demand-refreshed hot attributes on top of the human-configured ones, so a
	// refresh does not silently reset to the creation-time set on every restart. Merged, not
	// installed over: HotAttrs is what someone asked for and stays hot.
	if len(cfg.AutoHotAttrs) > 0 {
		a.AddHotAttrs(cfg.AutoHotAttrs...)
	}
	// The configured tier went in through the options above; restore the runtime tier on top of
	// it. Nothing is re-derived here: the next maintenance pass adopts what the set produces.
	if len(cfg.SchemaRequireRuntime) > 0 {
		a.AddSchemaRequireAttrs(cfg.SchemaRequireRuntime...)
	}
	if create {
		// Persist the config so a later reopen rebuilds the same indexes/retention.
		if err := t.cfg.save(); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// Append adds one ad to the archive (append-only; there is no update or per-key delete).
// The ad is durable when Append returns.
func (t *ArchiveTable) Append(ad *classad.ClassAd) error { return t.a.Append(ad) }

// AppendDeferred adds one ad without waiting for it to reach disk: it is readable at once
// and durable at the next Flush. A caller persisting its own progress through the source
// calls Flush first. See collections.Archive.AppendDeferred.
func (t *ArchiveTable) AppendDeferred(ad *classad.ClassAd) error { return t.a.AppendDeferred(ad) }

// Flush returns once every ad appended before it is durable, or reports why not. See
// collections.Archive.Flush.
func (t *ArchiveTable) Flush() error { return t.a.Flush() }

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
	return gatedArch(t, t.a.QueryLimit(q, limit)), nil
}

// QueryLimitRedacted is QueryLimit for a caller NOT entitled to sealed values, as DB.QueryRedacted is
// for a mutable table: the constraint is matched and the ads decoded with no key, so a sealed attribute
// is undefined in both. An attribute the archive seals reads as undefined in its older records too,
// where it may be stored in the clear (see SetEncryptedAttrs).
func (t *ArchiveTable) QueryLimitRedacted(constraint string, limit int) (iter.Seq[*classad.ClassAd], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("archive: parsing constraint: %w", err)
	}
	return gatedArch(t, t.a.QueryLimitRedacted(q, limit)), nil
}

// QueryProjectStatsRedacted is QueryProjectStats with QueryLimitRedacted's entitlement.
func (t *ArchiveTable) QueryProjectStatsRedacted(constraint string, attrs []string, stats *collections.ScanStats) (iter.Seq[[]classad.Value], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("archive: parsing constraint: %w", err)
	}
	return gatedArch(t, t.a.QueryProjectStatsRedacted(q, attrs, stats)), nil
}

// QueryProject scans the matching ads and yields each projected to just attrs' values, read
// wire-native where possible -- so an aggregate reads only the attributes it needs instead
// of fully decoding every record. Errors only on a malformed constraint.
func (t *ArchiveTable) QueryProject(constraint string, attrs []string) (iter.Seq[[]classad.Value], error) {
	return t.QueryProjectStats(constraint, attrs, nil)
}

// QueryProjectStats is QueryProject that also fills stats (may be nil) with the scan's work
// breakdown, so an EXPLAIN ANALYZE of an aggregate that falls to this projected scan can show the
// large RecordsReassembled that marks the slow per-record path.
func (t *ArchiveTable) QueryProjectStats(constraint string, attrs []string, stats *collections.ScanStats) (iter.Seq[[]classad.Value], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("archive: parsing constraint: %w", err)
	}
	return gatedArch(t, t.a.QueryProjectStats(q, attrs, stats)), nil
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
	return gatedArch(t, t.a.QueryRawProjected(q, projection, false, redact)), nil
}

// QueryRawProjectedStats is QueryRawProjected that also fills stats (may be nil) with the
// per-scan work breakdown for EXPLAIN ANALYZE (segments scanned/pruned, records decided from
// columns vs reassembled, rows matched).
func (t *ArchiveTable) QueryRawProjectedStats(constraint string, projection []string, redact bool, stats *collections.ScanStats) (iter.Seq[collections.RawAd], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("archive: parsing constraint: %w", err)
	}
	return gatedArch(t, t.a.QueryRawProjectedStats(q, projection, false, redact, stats)), nil
}

// QueryRawProjectedRefs is QueryRawProjected with the projected expressions' attribute
// references resolved too, so each yielded ad evaluates self-contained. See
// db.DB.QueryRawProjectedRefs for why that is a separate call rather than the default.
func (t *ArchiveTable) QueryRawProjectedRefs(constraint string, projection []string, redact bool) (iter.Seq[collections.RawAd], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("archive: parsing constraint: %w", err)
	}
	return gatedArch(t, t.a.QueryRawProjected(q, projection, true, redact)), nil
}

// QueryRawProjectedRefsStats is QueryRawProjectedRefs that also fills stats (may be nil) with the
// per-scan work breakdown for EXPLAIN ANALYZE.
func (t *ArchiveTable) QueryRawProjectedRefsStats(constraint string, projection []string, redact bool, stats *collections.ScanStats) (iter.Seq[collections.RawAd], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("archive: parsing constraint: %w", err)
	}
	return gatedArch(t, t.a.QueryRawProjectedStats(q, projection, true, redact, stats)), nil
}

// WIRE-FORM ROWS FROM AN ARCHIVE.
//
// The relay op that reads wire rows used to refuse an archive, which made `history` -- the
// table most queries are about -- the one table no consumer could read the cheap way. The text
// projection it fell back to renders every record and makes the consumer parse it back, which a
// downstream measurement put at roughly two fifths of a read's CPU and 86 allocations a row.
//
// Shaped exactly like DB's four, deliberately: same gate, same empty/true-constraint scan path,
// same error for a bad constraint. A caller should not have to know which kind of table it has
// to know what these return, and the two are easy to let drift apart.

// QueryRawWire yields each matching record as a self-contained WIRE-FORM ROW, newest first
// (see DB.QueryRawWire and collections.Archive.QueryRawWire). projection restricts the entries
// (empty = whole record); redact strips private attributes at the source.
func (t *ArchiveTable) QueryRawWire(constraint string, projection []string, redact bool) (iter.Seq[[]byte], error) {
	return t.QueryRawWireStats(constraint, projection, redact, nil)
}

// QueryRawWireStats is QueryRawWire that also fills stats (may be nil) with the per-scan work
// breakdown. An empty/true constraint takes the no-WHERE scan path and leaves stats zero.
func (t *ArchiveTable) QueryRawWireStats(constraint string, projection []string, redact bool, stats *collections.ScanStats) (iter.Seq[[]byte], error) {
	if !t.a.SupportsRawWire() {
		return nil, ErrRawWireUnsupported
	}
	if s := strings.TrimSpace(constraint); s == "" || strings.EqualFold(s, "true") {
		return gatedArch(t, t.a.ScanRawWire(projection, redact)), nil
	}
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("archive: parsing constraint: %w", err)
	}
	return gatedArch(t, t.a.QueryRawWireStats(q, projection, redact, stats)), nil
}

// QueryRawWireRefs is QueryRawWire whose projection also carries the attributes the projected
// expressions reference, transitively, so each yielded row EVALUATES self-contained. Which of
// the two a caller wants is decided as it is for a mutable table (see DB.QueryRawWireRefs): the
// plain form reproduces HTCondor's projection protocol, this one serves a consumer that will
// evaluate what it receives, for which the references are not optional.
func (t *ArchiveTable) QueryRawWireRefs(constraint string, projection []string, redact bool) (iter.Seq[[]byte], error) {
	return t.QueryRawWireRefsStats(constraint, projection, redact, nil)
}

// QueryRawWireRefsStats is QueryRawWireRefs that also fills stats (may be nil).
func (t *ArchiveTable) QueryRawWireRefsStats(constraint string, projection []string, redact bool, stats *collections.ScanStats) (iter.Seq[[]byte], error) {
	if !t.a.SupportsRawWire() {
		return nil, ErrRawWireUnsupported
	}
	if s := strings.TrimSpace(constraint); s == "" || strings.EqualFold(s, "true") {
		return gatedArch(t, t.a.ScanRawWireRefs(projection, redact)), nil
	}
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("archive: parsing constraint: %w", err)
	}
	return gatedArch(t, t.a.QueryRawWireRefsStats(q, projection, redact, stats)), nil
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
	return t.AggregateCols(constraint, groupCols, aggs)
}

// AggregateCols is Aggregate where a group column may carry a bucket width, so a numeric
// attribute can be grouped into fixed-width buckets (the "per day" dimension) server-side.
func (t *ArchiveTable) AggregateCols(constraint string, groupCols []GroupCol, aggs []AggSpec) ([]AggRow, error) {
	return t.AggregateColsStats(constraint, groupCols, aggs, nil)
}

// AggregateColsStats is AggregateCols that also fills stats (may be nil) with the scan's work
// breakdown for EXPLAIN ANALYZE. Only the fall-through projected scan -- the slow path taken when
// the aggregated attribute is not a numeric field the columnar accelerator covers -- fills the
// per-record counts; the columnar/index fast paths do not reassemble records and leave them zero.
// So a nonzero, large RecordsReassembled here is the signature of the slow aggregate this trailer
// exists to surface.
//
// Every return below also TAGS stats with the tier that served it (stats.Tier), because the
// strategies here differ by more than two orders of magnitude and zero counts were never a
// statement that a fast path ran -- only the absence of a scan, which a provably-empty constraint
// and a columnar answer produce alike. Tag at every return, including the ones that read nothing:
// an untagged return reports TierUnreported, which reads as "did not say" and cannot be told from
// a peer too old to report a tier at all.
func (t *ArchiveTable) AggregateColsStats(constraint string, groupCols []GroupCol, aggs []AggSpec, stats *collections.ScanStats) ([]AggRow, error) {
	return runAggLadder(t.aggLadder(stats, false), AggRequest{Constraint: constraint, GroupCols: groupCols, Aggs: aggs})
}

// AggregateColsStatsRedacted is AggregateColsStats for a caller NOT entitled to sealed values, as
// DB.AggregateColsStatsRedacted is for a mutable table: every rung answers as if the archive had no
// key, so a sealed value is undefined to the WHERE clause, the group key, the aggregate and its FILTER
// alike. A sealed attribute whose older records hold it in the clear reads as undefined in those too.
func (t *ArchiveTable) AggregateColsStatsRedacted(constraint string, groupCols []GroupCol, aggs []AggSpec, stats *collections.ScanStats) ([]AggRow, error) {
	return runAggLadder(t.aggLadder(stats, true), AggRequest{Constraint: constraint, GroupCols: groupCols, Aggs: aggs})
}

// keylessWithholds reports whether a redacted aggregate reads, anywhere -- the constraint, a group
// column, an aggregate or its filter -- an attribute the archive withholds from that reader by name
// (see collections.Archive.KeylessWithholds). Records written before such an attribute was sealed
// hold it in the clear, and so do the index postings, zone maps and columns built from them; only the
// keyless scan reads it as that reader must, as undefined.
func (t *ArchiveTable) keylessWithholds(req AggRequest) bool {
	if t.a.KeylessWithholdsIn(req.Constraint) {
		return true
	}
	for _, g := range req.GroupCols {
		if t.a.KeylessWithholds(g.Attr) {
			return true
		}
	}
	for _, a := range req.Aggs {
		if a.Arg != "*" && t.a.KeylessWithholds(a.Arg) {
			return true
		}
		if a.Filter != "" && t.a.KeylessWithholdsIn(a.Filter) {
			return true
		}
	}
	return false
}

// aggLadder is the archive's ordered list of aggregate strategies: each serves the whole request
// or declines to the next, and the projected scan at the end always answers. Built per call
// because its rungs close over the caller's stats block, which they tag with the tier that served
// the request, and because the match-all memo they share is per constraint.
//
// Order is measured, not incidental, and the index rung appears TWICE on purpose. The index
// answers an unconstrained count from postings, which nothing beats, so it goes above the columnar
// rungs. A CONSTRAINED grouped count it answers from zone verdicts instead, reading every
// straddling segment's records one at a time, so there the columns go first: on a 400k-record
// archive the columnar GROUP BY took 4 ms for a day's window where the zone verdicts took 51 ms
// (42 ms against 117 ms for 30 days). Splitting the rung on match-all is what lets each half sit
// where it wins. Match-none stays first because it is the only rung that answers without
// consulting storage at all; the rest are mutually exclusive on the number of group columns, so
// their relative order is the order they were added.
//
// Every rung that answers tags stats with its tier, including the ones that read nothing: an
// untagged return reports TierUnreported, which reads as "did not say" and cannot be told from a
// peer too old to report a tier at all. A rung that declines tags nothing, so the tier a caller
// sees is always the tier of the rung that actually answered.
//
// redact is the caller's entitlement, as in the mutable ladder. The rungs that read only stored counts
// and columns serve both readers, since neither ever holds a sealed value; those that evaluate a record
// take the entitlement down to it. The index rung for a constrained count is skipped for a redacted
// caller: it decides straddling segments by evaluating records with the key. And a redacted request
// that reads a sealed attribute at all goes straight to the scan (see keylessWithholds).
func (t *ArchiveTable) aggLadder(stats *collections.ScanStats, redact bool) []AggStrategy {
	var matchAll matchAllMemo
	// Asked per rung rather than memoized: a ladder may be driven with several requests, and the
	// answer is a parse of the request's expressions, and only on an archive that seals.
	scanOnly := func(req AggRequest) bool { return redact && t.keylessWithholds(req) }
	return []AggStrategy{
		aggRung{"match-none", func(req AggRequest) ([]AggRow, bool, error) {
			// A constraint that can never match needs no data read at all. Answered by running the
			// ordinary aggregation over an EMPTY sequence rather than by constructing a result
			// here, so the shape is whatever a scan that matched nothing would have produced -- 0
			// for a count, undefined for MIN -- by construction rather than by a second
			// implementation that could disagree. It is also the one fast rung that can fail on a
			// malformed request before any storage is touched, because it runs the same reduce
			// engine the scan does.
			if !IsMatchNone(req.Constraint) {
				return nil, false, nil
			}
			attrs, groupCol, aggCol := AggProjection(req.GroupCols, req.Aggs)
			stats.SetTier(collections.TierMatchNone)
			rows, err := AggregateValues(func(func([]classad.Value) bool) {}, attrs, req.GroupCols, req.Aggs, groupCol, aggCol, nil)
			if err != nil {
				return nil, false, err
			}
			return rows, true, nil
		}},
		aggRung{"index-unconstrained", func(req AggRequest) ([]AggRow, bool, error) {
			// Postings and stored counts, reading no record at all -- the one strategy that is not
			// a scan of anything. Unbeatable for the unconstrained shapes, which is why this half
			// of the rung sits above the columnar ones.
			if scanOnly(req) || !matchAll.is(req.Constraint) {
				return nil, false, nil
			}
			rows, ok := t.aggregateFromIndex(req.Constraint, req.GroupCols, req.Aggs)
			if !ok {
				return nil, false, nil
			}
			stats.SetTier(collections.TierIndex)
			return rows, true, nil
		}},
		aggRung{"count-constraint", func(req AggRequest) ([]AggRow, bool, error) {
			// A CONSTRAINED COUNT(*) with no grouping: read the predicated columns out of the
			// columnar blocks instead of scanning records. The index rung above only answers the
			// UNCONSTRAINED case (a constraint cannot be attributed to a whole segment's stored
			// count), and columnar-aggregate below declines a COUNT(*) because it has no attribute
			// to aggregate -- so this shape reached neither, and fell to the scan even on an
			// archive that carries an accelerator. It is the shape a history table is asked most.
			//
			// Not gated on match-all, unlike its mutable counterpart: an unconstrained plain count
			// has already been taken by the index rung above unless that declined, and this is a
			// better answer than the scan when it does.
			if !req.isPlainCount() || scanOnly(req) {
				return nil, false, nil
			}
			count := t.a.CountConstraint
			if redact {
				count = t.a.CountConstraintRedacted
			}
			n, ok := count(req.Constraint)
			if !ok {
				return nil, false, nil
			}
			stats.SetTier(collections.TierColumnar)
			return []AggRow{{Values: []string{strconv.Itoa(n)}}}, true, nil
		}},
		aggRung{"grouped-columns", func(req AggRequest) ([]AggRow, bool, error) {
			// A COUNT(*) GROUPED BY one or more schema columns: a histogram of them, computed
			// in the same columnar pass the ungrouped count above makes. The index rung answers a
			// grouped count only from the CATEGORICAL index, so a group column that is a hot
			// numeric (`GROUP BY NumJobStarts`) reached neither it nor the ungrouped fast path and
			// fell to a full row scan -- the same predicate costing ~200x its ungrouped count,
			// constrained or not. Several columns is the "per owner per day" shape: the index rung
			// serves its unconstrained COUNT(*) form over a categorically indexed column and nothing
			// else, so the same two columns under a WHERE clause or beside a SUM reached no fast
			// rung at all. Declines, among other reasons, for a predicate not proven free of
			// CurrentTime: a window whose bounds move between the two passes is not one segment
			// statistics can be attributed to.
			if scanOnly(req) {
				return nil, false, nil
			}
			var src GroupStatsSource = t
			if redact {
				src = redactedArchiveGroupStats{t}
			}
			rows, ok := GroupedFromColumns(src, req.Constraint, req.GroupCols, req.Aggs)
			if !ok {
				return nil, false, nil
			}
			stats.SetTier(collections.TierColumnar)
			return rows, true, nil
		}},
		aggRung{"index-constrained", func(req AggRequest) ([]AggRow, bool, error) {
			// The same index strategy, below the columns rather than above them, for the
			// constrained shapes it answers from zone verdicts -- which read every straddling
			// segment record by record, and so lose to the columnar pass by an order of magnitude
			// on exactly the windowed grouped counts a dashboard asks. See the measurement above.
			if redact || matchAll.is(req.Constraint) {
				return nil, false, nil
			}
			rows, ok := t.aggregateFromIndex(req.Constraint, req.GroupCols, req.Aggs)
			if !ok {
				return nil, false, nil
			}
			stats.SetTier(collections.TierIndex)
			return rows, true, nil
		}},
		aggRung{"columnar-aggregate", func(req AggRequest) ([]AggRow, bool, error) {
			// A single MIN/MAX/COUNT(attr) over a numeric column reads that column out of the
			// per-segment columnar blocks instead of decoding every record. Declines (and falls
			// through to the scan) unless the archive carries an accelerator and the aggregate is
			// in scope.
			if scanOnly(req) {
				return nil, false, nil
			}
			numStats := t.NumStats
			if redact {
				numStats = t.numStatsRedacted
			}
			rows, ok := ColumnarAggregate(func(attr string) (NumStats, bool) {
				return numStats(req.Constraint, attr)
			}, req.GroupCols, req.Aggs)
			if !ok {
				return nil, false, nil
			}
			stats.SetTier(collections.TierColumnar)
			return rows, true, nil
		}},
		aggRung{"projected-scan", func(req AggRequest) ([]AggRow, bool, error) {
			// The reference, and the rung that must never decline. Scan wire-native, reading only
			// the attributes the aggregation projects (empty for a pure COUNT), so it does not
			// fully decode every matched record -- mirroring the mutable table's aggregate. Zone
			// maps still prune segments no matching record can fall in. This is the path whose
			// stats matter: an attribute the columnar accelerator does not cover forces a
			// per-record reassembly here, which the trailer surfaces as a large
			// RecordsReassembled.
			//
			// The tier is tagged BEFORE the read, as it was: a scan that then fails has still
			// reported which strategy was attempted, and the caller discards the stats anyway.
			//
			// An empty constraint is match-all to every rung above and a PARSE ERROR here, so the
			// same query succeeds or fails depending on which rung serves it. Left alone: the
			// fast rungs above take every empty-constraint shape they can, and making this one
			// agree with them would change what the server answers.
			//
			// No stop callback, unlike the mutable scan: the archive path takes none, and giving
			// it one would be a new behaviour rather than a description of this one.
			attrs, groupCol, aggCol := AggProjection(req.GroupCols, req.Aggs)
			stats.SetTier(collections.TierScan)
			project := t.QueryProjectStats
			if redact {
				project = t.QueryProjectStatsRedacted
			}
			seq, err := project(req.Constraint, attrs, stats)
			if err != nil {
				return nil, false, err
			}
			rows, err := AggregateValues(seq, attrs, req.GroupCols, req.Aggs, groupCol, aggCol, nil)
			if err != nil {
				return nil, false, err
			}
			return rows, true, nil
		}},
	}
}

// redactedArchiveGroupStats is the GroupStatsSource of a redacted archive aggregate (see
// collections.Archive.GroupStatsConstraintAggsRedacted), the archive's counterpart of
// redactedGroupStats.
type redactedArchiveGroupStats struct{ t *ArchiveTable }

func (r redactedArchiveGroupStats) GroupStatsConstraintAggs(constraint string, groups []collections.GroupSpec,
	aggCols []collections.GroupAggCol, countFilters []string) ([]collections.GroupStats, bool) {
	return r.t.a.GroupStatsConstraintAggsRedacted(constraint, groups, aggCols, countFilters)
}

func (r redactedArchiveGroupStats) GroupStatsAllAggs(groups []collections.GroupSpec, aggCols []collections.GroupAggCol,
	countFilters []string) ([]collections.GroupStats, bool) {
	return r.t.a.GroupStatsAllAggsRedacted(groups, aggCols, countFilters)
}

// numStatsRedacted is NumStats for a caller not entitled to sealed values.
func (t *ArchiveTable) numStatsRedacted(constraint, attr string) (NumStats, bool) {
	q, ok := numStatsQuery(constraint)
	if !ok {
		return NumStats{}, false
	}
	return t.a.NumStatsQueryRedacted(q, attr)
}

// aggregateFromIndex answers the aggregate without reading records where the per-segment
// indexes already contain the answer, returning ok=false when they do not so the caller
// scans. Every path here is restricted to an unconstrained COUNT(*): a WHERE clause would
// have to be proven to hold for a whole segment before its counts could be attributed, and
// the probe set a constraint decomposes into is a conservative under-approximation, not a
// proof.
//
// Row order differs from the scan path's first-seen order: rows come back sorted by group
// value (then bucket). GROUP BY without ORDER BY has no defined order either way, and
// sorted is at least deterministic.
func (t *ArchiveTable) aggregateFromIndex(constraint string, groupCols []GroupCol, aggs []AggSpec) ([]AggRow, bool) {
	if len(aggs) != 1 || aggs[0].Func != AggCount || aggs[0].Arg != "*" {
		return nil, false
	}
	// A per-aggregate FILTER rules out every path here: postings and the stored record
	// count know nothing about the filter, so answering from them would silently report
	// unfiltered totals as if they were filtered.
	if aggs[0].Filter != "" {
		return nil, false
	}
	matchAll := IsMatchAll(constraint)
	switch len(groupCols) {
	case 0:
		if !matchAll {
			return nil, false
		}
		// The retained record count, which the archive tracks in O(1).
		return []AggRow{{Values: []string{strconv.Itoa(t.a.Count())}}}, true

	case 1:
		if groupCols[0].BucketWidth != 0 {
			return nil, false // a lone bucketed column: nothing to read counts from
		}
		// A constrained count is only index-answerable when the constraint's shape can be
		// verified as a conjunction of zone-decidable range conditions; otherwise
		// CategoricalGroupCountsWhere declines and we scan.
		var counts map[string]int64
		var ok bool
		if matchAll {
			counts, ok = t.a.CategoricalGroupCounts(groupCols[0].Attr)
		} else {
			counts, ok = t.a.CategoricalGroupCountsWhere(groupCols[0].Attr, constraint)
		}
		if !ok {
			return nil, false
		}
		vals := make([]string, 0, len(counts))
		for v := range counts {
			vals = append(vals, v)
		}
		sort.Strings(vals)
		rows := make([]AggRow, 0, len(vals))
		for _, v := range vals {
			rows = append(rows, AggRow{
				Group:  []string{v},
				Values: []string{strconv.FormatInt(counts[v], 10)},
			})
		}
		return rows, true

	case 2:
		// One raw categorical column and one bucketed numeric column, in either order --
		// "jobs per owner per day". The bucket dimension comes from segment zone maps, so
		// whole segments are attributed without being read.
		//
		// Unconstrained only: the bucketed helper takes no constraint, so answering a
		// WHERE-bearing query from it would silently ignore the filter.
		if !matchAll {
			return nil, false
		}
		cat, bucket := 0, 1
		if groupCols[0].BucketWidth != 0 {
			cat, bucket = 1, 0
		}
		if groupCols[cat].BucketWidth != 0 || groupCols[bucket].BucketWidth <= 0 {
			return nil, false
		}
		byBucket, ok := t.a.CategoricalGroupCountsBucketed(
			groupCols[cat].Attr, groupCols[bucket].Attr, groupCols[bucket].BucketWidth)
		if !ok {
			return nil, false
		}
		buckets := make([]int64, 0, len(byBucket))
		for b := range byBucket {
			buckets = append(buckets, b)
		}
		sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })
		var rows []AggRow
		for _, b := range buckets {
			counts := byBucket[b]
			vals := make([]string, 0, len(counts))
			for v := range counts {
				vals = append(vals, v)
			}
			sort.Strings(vals)
			for _, v := range vals {
				group := make([]string, 2)
				group[cat] = v
				group[bucket] = strconv.FormatInt(b, 10)
				rows = append(rows, AggRow{
					Group:  group,
					Values: []string{strconv.FormatInt(counts[v], 10)},
				})
			}
		}
		return rows, true
	}
	return nil, false
}

// IsMatchAll reports whether a constraint imposes no filter (an empty string or a literal
// "true"), so an aggregate over it covers every record. Shared with the mutable-table
// COUNT(*) fast path in dbrpc.
func IsMatchAll(constraint string) bool {
	c := strings.TrimSpace(constraint)
	if c == "" || strings.EqualFold(c, "true") {
		return true
	}
	// Fold a constant tautology (e.g. "1 == 1", "true && true") to the same match-all
	// fast paths as "true": a constraint that references no attribute and evaluates to
	// TRUE matches every record. The no-reference check is essential -- a record-
	// dependent expression must never fold even if it happens to evaluate true against
	// an empty ad (e.g. "JobStatus =!= 2" is TRUE when JobStatus is absent but is not
	// match-all), so anything referencing an attribute is left to the normal scan.
	expr, err := classad.ParseExpr(c)
	if err != nil {
		return false
	}
	empty := classad.New()
	if len(empty.ExternalRefs(expr)) != 0 {
		return false
	}
	// A visible reference is not the whole question: the read set has to be statically VISIBLE at
	// all. eval() parses its argument at run time, so an expression with no attribute reference in
	// its syntax may still read every attribute of every record -- and deciding it against an empty
	// ad then answers for a read that never happened. vm.SelfRefsSafe is the predicate for exactly
	// that, and eval() is the one construct that clears it.
	if _, staticReads := vm.SelfRefsSafe(expr.AST()); !staticReads {
		return false
	}
	b, err := expr.Eval(empty).BoolValue()
	return err == nil && b
}

// IsMatchNone reports whether a constraint can never match: it references no attribute, so its value is
// the same for every record, and that value is not TRUE.
//
// The dual of IsMatchAll, and missing until `select min(ProcId) from history where false` was measured at
// 3.5s -- a full scan to establish that nothing matches. FALSE, UNDEFINED and ERROR all match nothing,
// since a record matches only when the constraint is boolean TRUE, so this folds `where false`,
// `where 1 == 2`, `where undefined` and `where error` alike.
//
// The no-reference check is the same load-bearing one IsMatchAll documents, in the other direction: a
// record-dependent expression must never fold even when it evaluates non-TRUE against an empty ad.
// `JobStatus == 2` is UNDEFINED with JobStatus absent and matches nothing HERE, but matches plenty of
// records, so anything referencing an attribute is left to the scan.
func IsMatchNone(constraint string) bool {
	c := strings.TrimSpace(constraint)
	if c == "" {
		return false // no filter at all is match-ALL
	}
	expr, err := classad.ParseExpr(c)
	if err != nil {
		return false
	}
	empty := classad.New()
	if len(empty.ExternalRefs(expr)) != 0 {
		return false
	}
	// A visible reference is not the whole question: the read set has to be statically VISIBLE at
	// all. eval() parses its argument at run time, so an expression with no attribute reference in
	// its syntax may still read every attribute of every record -- and deciding it against an empty
	// ad then answers for a read that never happened. vm.SelfRefsSafe is the predicate for exactly
	// that, and eval() is the one construct that clears it.
	if _, staticReads := vm.SelfRefsSafe(expr.AST()); !staticReads {
		return false
	}
	b, err := expr.Eval(empty).BoolValue()
	return err != nil || !b
}

// CategoricalGroupCounts returns the exact per-value record counts for a categorically
// indexed attribute, read from the per-segment indexes without scanning any record. ok is
// false when the indexes cannot fully account for every record (see the collections-level
// doc), in which case the caller must scan. Aggregate uses this for GROUP BY COUNT(*);
// it is exported so a planner can ask whether the cheap path is available.
func (t *ArchiveTable) CategoricalGroupCounts(attr string) (map[string]int64, bool) {
	return t.a.CategoricalGroupCounts(attr)
}

// CategoricalGroupCountsBucketed is CategoricalGroupCounts split by a numeric bucket
// (floor(bucketAttr/width)*width) -- the "per group per day" shape. bucketAttr must be
// zone-mapped; ok is false when the counts cannot be established from the indexes.
//
// Not yet reachable from SQL: the archive aggregate crosses dbrpc as a []string group list,
// which has no room for a bucket width, so a bucketed GROUP BY over an archive still falls
// back to client-side reduction. Wiring it through is a protocol change.
func (t *ArchiveTable) CategoricalGroupCountsBucketed(attr, bucketAttr string, width int64) (map[int64]map[string]int64, bool) {
	return t.a.CategoricalGroupCountsBucketed(attr, bucketAttr, width)
}

// CategoricalGroupCountsWhere is CategoricalGroupCounts restricted to records matching
// constraint. ok is false unless the constraint is a pure conjunction of numeric
// comparisons on zone-mapped attributes and the indexes account for every record.
func (t *ArchiveTable) CategoricalGroupCountsWhere(attr, constraint string) (map[string]int64, bool) {
	return t.a.CategoricalGroupCountsWhere(attr, constraint)
}

// MergeOptions tunes a merge pass over an archive; see collections.MergeOptions. The zero
// value is usable and fills in defaults.
// AttrTypeProfile is one attribute's measured type profile (see collections.AttrTypeProfile).
type AttrTypeProfile = collections.AttrTypeProfile

type MergeOptions = collections.MergeOptions

// AutoTuneOptions and AutoTuneResult are re-exported so a caller can tune an archive's
// indexes without importing collections directly.
type AutoTuneOptions = collections.AutoTuneOptions
type AutoTuneResult = collections.AutoTuneResult

// IndexBackfillResult reports one index backfill pass (see collections.IndexBackfillResult).
type IndexBackfillResult = collections.IndexBackfillResult

// MergePass merges cold segment runs when the archive has reached its trigger watermark,
// down to its target. Returns the number of merges performed.
//
// An append-only table never compacts, so its segment count only grows, and every sealed
// segment costs a memory mapping at open. This is what bounds that; without it running, the
// count climbs until the daemon cannot start.
// Explain reports how a query against the archive would be executed (see
// collections.Archive.ExplainQuery), so `.explain` works on a history table rather than
// reporting it as a nonexistent table.
func (t *ArchiveTable) Explain(constraint string) (QueryExplain, error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return QueryExplain{}, fmt.Errorf("archive: parsing constraint: %w", err)
	}
	return t.a.ExplainQuery(q), nil
}

// BuildAndEnableSchemaScan builds or extends the archive's columnar accelerator (see
// collections.Archive.BuildAndEnableSchemaScan).
func (t *ArchiveTable) BuildAndEnableSchemaScan(sampleMax, hotTopN int) bool {
	return t.a.BuildAndEnableSchemaScan(sampleMax, hotTopN)
}

// SchemaFit measures the archive's derived schema against a fresh sample (see DB.SchemaFit).
func (t *ArchiveTable) SchemaFit(sampleMax int) ([]SchemaFieldFit, int) {
	return t.a.SchemaFit(sampleMax)
}

// ReschemaScan re-derives the archive's schema and adopts it (see DB.ReschemaScan).
func (t *ArchiveTable) ReschemaScan(sampleMax, hotTopN int) bool {
	return t.a.ReschemaScan(sampleMax, hotTopN)
}

// SchemaScanInfo reports the archive's columnar accelerator state.
func (t *ArchiveTable) SchemaScanInfo() SchemaScanInfo { return t.a.SchemaScanInfo() }

func (t *ArchiveTable) MergePass(opts MergeOptions) int { return t.a.MergePass(opts) }

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
	return t.watchAs(ctx, cursor, false)
}

// WatchRedacted is Watch for a watcher NOT entitled to sealed values: each event's ad is decoded
// with no key, so a sealed attribute arrives undefined. See DB.WatchRedacted.
func (t *ArchiveTable) WatchRedacted(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error) {
	return t.watchAs(ctx, cursor, true)
}

func (t *ArchiveTable) watchAs(ctx context.Context, cursor []byte, redact bool) (iter.Seq[WatchEvent], error) {
	watch := t.a.Watch
	if redact {
		watch = t.a.WatchRedacted
	}
	seq, err := watch(ctx, cursor)
	if err != nil {
		return nil, err
	}
	return archiveWatchEvents(seq), nil
}

// archiveWatchEvents converts an archive's change stream to db watch events.
func archiveWatchEvents(seq iter.Seq[collections.WatchEvent]) iter.Seq[WatchEvent] {
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
	}
}

// WatchCursor returns an opaque cursor at the current head of the archive's change log.
// Watch(cursor) then streams only what is appended after it, which is how a client tails
// an archive without first replaying everything rotation still retains.
func (t *ArchiveTable) WatchCursor() ([]byte, error) { return t.a.WatchCursor() }

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
// A change is persisted (see saveIndexConfig).
func (t *ArchiveTable) AddIndex(categorical, value []string) bool {
	changed := t.a.AddIndex(categorical, value)
	if changed {
		t.saveIndexConfig()
	}
	return changed
}

// DropIndex removes the named per-segment indexes. Returns false if none matched.
// A change is persisted (see saveIndexConfig).
func (t *ArchiveTable) DropIndex(names ...string) bool {
	changed := t.a.DropIndex(names...)
	if changed {
		t.saveIndexConfig()
	}
	return changed
}

// saveIndexConfig folds the archive's live index set back into the saved config, so a
// runtime AddIndex/DropIndex survives a restart. Without it the reopen path (which treats
// archiveconfig.json as authoritative) would resurrect the creation-time index set, and the
// rebuild AddIndex just paid for -- a full decompress of the archive -- would be discarded
// and the sidecars rebuilt back down to the older, narrower spec.
//
// The live set is read back from the archive rather than merged from the caller's names,
// because AddIndex normalizes them (a name given as both kinds, or already indexed as the
// other kind, lands as categorical). Mirrors DB.saveIndexConfig, including its best-effort
// contract: a write failure is ignored, leaving the change live for this run but unpersisted.
func (t *ArchiveTable) saveIndexConfig() {
	_ = t.cfg.update(func(cfg *ArchiveConfig) {
		cfg.CategoricalAttrs, cfg.ValueAttrs = t.a.IndexedAttrs()
		cfg.AutoAttrs = t.a.AutoIndexNames()
	})
}

// Reindex rebuilds the per-segment indexes of segments sealed since the last build, and of
// segments within the backfill horizon whose index predates the current configuration.
func (t *ArchiveTable) Reindex() { t.a.Reindex() }

// ReindexAll is Reindex carried past the backfill horizon to every sealed segment, so each
// holds postings for every indexed attribute. It decompresses every segment that lacks one --
// on a large archive, a long time -- and is meant for an operator who asked for it; the
// maintenance pass gets there incrementally through BackfillIndexes.
func (t *ArchiveTable) ReindexAll() IndexBackfillResult { return t.a.ReindexAll() }

// BackfillIndexes rebuilds, newest first and up to maxBytes of segment data (<= 0: no bound),
// the sidecars of sealed segments lacking postings for an indexed attribute.
func (t *ArchiveTable) BackfillIndexes(maxBytes int64) IndexBackfillResult {
	return t.a.BackfillIndexes(maxBytes)
}

// IndexBackfillPending reports the sealed segments, and their stored bytes, still lacking
// postings for some indexed attribute: the backfill's remaining work.
func (t *ArchiveTable) IndexBackfillPending() (segments int, bytes int64) {
	return t.a.IndexBackfillPending()
}

// --- diagnostics (mirrors the mutable-table stat surface for symmetry) ---

// Stats reports storage accounting (records, segments, arena/used/dead bytes).
func (t *ArchiveTable) Stats() Stats { return t.a.Stats() }

// OpStats reports cumulative operational timings. An archive has no DB-level snapshot lock,
// so SnapshotLock is zero.
func (t *ArchiveTable) OpStats() OpStats { return OpStats{OpStats: t.a.OpStats()} }

// CountConstraint counts the rows matching constraint via the columnar accelerator, or reports
// ok=false so the caller scans. See collections.Archive.CountConstraint.
func (t *ArchiveTable) CountConstraint(constraint string) (int, bool) {
	return t.a.CountConstraint(constraint)
}

// GroupCountConstraint answers a per-value COUNT(*) of groupAttr over the rows matching constraint
// via the columnar accelerator, or reports ok=false so the caller scans. AggregateCols uses it for
// the single-numeric-column GROUP BY shape; it is exported so a caller (and a test) can tell which
// tier answered. See collections.Archive.GroupCountConstraint.
func (t *ArchiveTable) GroupCountConstraint(constraint, groupAttr string) ([]collections.GroupCount, bool) {
	return t.a.GroupCountConstraint(constraint, groupAttr)
}

// GroupCountAll is GroupCountConstraint over every row: the whole column's histogram. AggregateCols
// uses it for a grouped COUNT(*) whose constraint is match-all, which the predicate analysis behind
// GroupCountConstraint cannot serve because there is no predicate to analyze. See
// collections.Archive.GroupCountAll.
func (t *ArchiveTable) GroupCountAll(groupAttr string) ([]collections.GroupCount, bool) {
	return t.a.GroupCountAll(groupAttr)
}

// GroupStatsConstraint answers a per-group record count plus the aggregate inputs for each aggAttr over
// the rows matching constraint, via the columnar accelerator, or reports ok=false so the caller scans.
// AggregateCols uses it for the single-numeric-column GROUP BY shape; it is exported so a caller (and a
// test) can tell which tier answered. See collections.Archive.GroupStatsConstraint.
func (t *ArchiveTable) GroupStatsConstraint(constraint, groupAttr string, aggAttrs []string) ([]collections.GroupStats, bool) {
	return t.a.GroupStatsConstraint(constraint, groupAttr, aggAttrs)
}

// GroupStatsAll is GroupStatsConstraint over every row, for a constraint this layer has established is
// match-all (the predicate analysis behind GroupStatsConstraint has no predicate to work with there).
func (t *ArchiveTable) GroupStatsAll(groupAttr string, aggAttrs []string) ([]collections.GroupStats, bool) {
	return t.a.GroupStatsAll(groupAttr, aggAttrs)
}

// GroupStatsConstraintBucketed is GroupStatsConstraint with the group column floored into buckets of
// bucket (0 = the raw value). See collections.Collection.GroupStatsQueryBucketed.
func (t *ArchiveTable) GroupStatsConstraintBucketed(constraint, groupAttr string, bucket int64, aggAttrs []string) ([]collections.GroupStats, bool) {
	return t.a.GroupStatsConstraintBucketed(constraint, groupAttr, bucket, aggAttrs)
}

// GroupStatsAllBucketed is GroupStatsAll with the group column floored into buckets of bucket
// (0 = the raw value). See collections.Collection.GroupStatsQueryBucketed.
func (t *ArchiveTable) GroupStatsAllBucketed(groupAttr string, bucket int64, aggAttrs []string) ([]collections.GroupStats, bool) {
	return t.a.GroupStatsAllBucketed(groupAttr, bucket, aggAttrs)
}

// GroupStatsConstraintCols is GroupStatsConstraintBucketed over SEVERAL group columns: the group is
// the tuple of their (individually bucketed) values, which is the "per owner per day" shape.
// See collections.Collection.GroupStatsQueryCols.
func (t *ArchiveTable) GroupStatsConstraintCols(constraint string, groups []collections.GroupSpec, aggAttrs []string) ([]collections.GroupStats, bool) {
	return t.a.GroupStatsConstraintCols(constraint, groups, aggAttrs)
}

// GroupStatsAllCols is GroupStatsAllBucketed over SEVERAL group columns, for a constraint this layer
// has established is match-all. See collections.Collection.GroupStatsQueryCols.
func (t *ArchiveTable) GroupStatsAllCols(groups []collections.GroupSpec, aggAttrs []string) ([]collections.GroupStats, bool) {
	return t.a.GroupStatsAllCols(groups, aggAttrs)
}

// GroupStatsConstraintAggs is GroupStatsConstraintCols where an aggregate column may carry a
// per-aggregate FILTER. See collections.Collection.GroupStatsConstraintAggs.
func (t *ArchiveTable) GroupStatsConstraintAggs(constraint string, groups []collections.GroupSpec,
	aggCols []collections.GroupAggCol, countFilters []string) ([]collections.GroupStats, bool) {
	return t.a.GroupStatsConstraintAggs(constraint, groups, aggCols, countFilters)
}

// GroupStatsAllAggs is GroupStatsConstraintAggs over every row, for a constraint this layer has
// established is match-all. See collections.Collection.GroupStatsAllAggs.
func (t *ArchiveTable) GroupStatsAllAggs(groups []collections.GroupSpec,
	aggCols []collections.GroupAggCol, countFilters []string) ([]collections.GroupStats, bool) {
	return t.a.GroupStatsAllAggs(groups, aggCols, countFilters)
}

// CodecStats reports the archive's compression (codec, dict size, last retrain, sampled ratio).
func (t *ArchiveTable) CodecStats(sampleMax int) CodecStats { return t.a.CodecStats(sampleMax) }

// IndexSizes reports the per-attribute index byte footprint.
func (t *ArchiveTable) IndexSizes() IndexSizes { return t.a.IndexSizes() }

// SidecarSizes reports the sealed-segment sidecar index bytes (mmap-backed, evictable).
func (t *ArchiveTable) SidecarSizes() SidecarSizes { return t.a.SidecarSizes() }

// IndexedAttrs returns the archive's categorical and value index attributes.
func (t *ArchiveTable) IndexedAttrs() (categorical, value []string) { return t.a.IndexedAttrs() }

// ZoneAttrs returns the attributes carrying per-segment [min,max] zone maps, on which a
// range query prunes whole segments rather than only postings.
func (t *ArchiveTable) ZoneAttrs() []string { return t.a.ZoneAttrs() }

// HotAttrs reports the archive's hot-header attributes, matching DB.HotAttrs for a mutable table.
func (t *ArchiveTable) HotAttrs() []string { return t.a.HotAttrNames() }

// RefreshHotSet recomputes the archive's hot set from accumulated query demand, installs it
// for future writes, and persists it. Returns how many attributes the sample chose.
//
// The refreshed set governs records appended from now on and changes nothing already sealed.
// Older records pick it up as the merge pass folds their segments together, which re-encodes
// them under the hot set in effect at merge time; see collections.Archive.RefreshHotSet for
// why none of that needs a rewrite, and what a merge never reaches.
//
// The configured HotAttrs are re-pinned afterwards: RefreshHotSet replaces the set outright,
// and an attribute an operator asked to keep hot should not be dropped because a 4096-record
// sample happened not to read it.
func (t *ArchiveTable) RefreshHotSet(sampleMax, topN int) int {
	n := t.a.RefreshHotSet(sampleMax, topN)
	if n == 0 {
		return 0
	}
	var pinned []string
	t.cfg.with(func(cfg *ArchiveConfig) { pinned = append(pinned, cfg.HotAttrs...) })
	if len(pinned) > 0 {
		t.a.AddHotAttrs(pinned...)
	}
	t.saveHotConfig()
	return n
}

// AddHotAttrs pins the named attributes into the archive's hot set for future writes and
// persists them as human-configured, returning the resulting hot attributes.
func (t *ArchiveTable) AddHotAttrs(names ...string) []string {
	hot := t.a.AddHotAttrs(names...)
	// A named add is someone asking, so it joins HotAttrs rather than the auto set -- which
	// keeps it from being attributed to, or recomputed away by, a later refresh.
	t.cfg.with(func(cfg *ArchiveConfig) { cfg.HotAttrs = mergeNames(cfg.HotAttrs, names) })
	t.saveHotConfig()
	return hot
}

// saveHotConfig folds the live hot set back into the saved config, split by provenance:
// HotAttrs keeps whatever was configured or explicitly added, AutoHotAttrs records what the
// demand-driven refresh put on top. Best-effort, like saveIndexConfig: a write failure
// leaves the change live for this run but unpersisted.
func (t *ArchiveTable) saveHotConfig() {
	_ = t.cfg.update(func(cfg *ArchiveConfig) {
		cfg.AutoHotAttrs = autoHotNames(cfg.HotAttrs, t.a.HotAttrNames())
	})
}

// mergeNames appends to base the names of add it does not already hold (case-insensitively),
// preserving base's order and each new name's spelling.
func mergeNames(base, add []string) []string {
	have := make(map[string]bool, len(base))
	for _, n := range base {
		have[strings.ToLower(n)] = true
	}
	for _, n := range add {
		if f := strings.ToLower(n); !have[f] {
			have[f] = true
			base = append(base, n)
		}
	}
	return base
}

// EncryptionEnabled reports ENCRYPTION AT REST, as DB.EncryptionEnabled does: the archive's master key
// is wrapped under pool keys. Without them the archive still seals (see SealsValues), but its master
// sits beside the data in the clear, which protects nothing and is reported here as false.
func (t *ArchiveTable) EncryptionEnabled() bool { return t.enc.protected() }

// SealsValues reports whether this archive seals any attribute values at all -- private attributes
// always, and the explicit set -- as DB.SealsValues does. A table that seals nothing has nothing to
// withhold from a reader not entitled to sealed values.
func (t *ArchiveTable) SealsValues() bool { return t.a.SealsValues() }

// EncryptedAttrNames reports the attributes this archive seals, matching DB.EncryptedAttrNames.
func (t *ArchiveTable) EncryptedAttrNames() []string { return t.a.EncryptedAttrNames() }

// SealedIndexes names the configured indexes this open left out because the sealing policy names
// their attribute. Normally empty; see openArchiveTable.
func (t *ArchiveTable) SealedIndexes() []string { return append([]string(nil), t.sealedIndexes...) }

// SetEncryptedAttrs replaces the explicit set of attributes sealed at rest and persists it
// (archiveconfig.json), as DB.SetEncryptedAttrs does for a mutable table. It errors if the archive has
// no key or a named attribute is indexed.
//
// Appends from now on seal the new set. Records already appended keep the form they were written in
// until maintenance next rewrites their segment (a merge, a retrain, interning), which seals them; a
// history is not rewritten just for this. Until then a read not entitled to sealed values withholds
// the attribute by name, so what such a reader sees does not depend on when a record was written.
func (t *ArchiveTable) SetEncryptedAttrs(attrs []string) error {
	if err := t.a.SetEncryptedAttrs(attrs); err != nil {
		return err
	}
	names := t.a.EncryptedAttrNames()
	return t.cfg.update(func(cfg *ArchiveConfig) { cfg.EncryptedAttrs = names })
}

// StaleIndexSegments reports how many sealed segments within the backfill horizon still carry
// an index built under an older configuration, and how many are sealed in total. Normally zero;
// segments beyond the horizon are counted by StaleIndexSegmentsByPolicy and, the ones missing
// an indexed attribute, by IndexBackfillPending.
func (t *ArchiveTable) StaleIndexSegments() (stale, sealed int) { return t.a.StaleIndexSegments() }

// StaleIndexSegmentsByPolicy reports segments deliberately left on an older index
// configuration because they fall outside the backfill horizon -- expected, and not a sign of
// anything failing. Reported apart from StaleIndexSegments so an alert on the latter stays
// meaningful once a horizon is configured.
func (t *ArchiveTable) StaleIndexSegmentsByPolicy() int { return t.a.StaleIndexSegmentsByPolicy() }

// Retention returns the archive's current retention bounds.
func (t *ArchiveTable) Retention() collections.Retention { return t.a.Retention() }

// SetRetention updates the retention bounds and persists them (archiveconfig.json), so they
// take effect on the next Rotate and survive a restart.
func (t *ArchiveTable) SetRetention(r collections.Retention) error {
	t.a.SetRetention(r)
	return t.cfg.update(func(cfg *ArchiveConfig) { cfg.Retention = r })
}

// SetRowGroupBytes changes the columnar row-group budget and persists it (archiveconfig.json), so a
// value tried on a live archive survives a restart. 0 restores the default.
//
// Unlike the index set this needs no reconciliation pass: every block records the layout it was
// written with, so the new budget governs segments sealed from now on while everything already on disk
// keeps reading as before. That is what makes it worth exposing -- the right value depends on the read
// mix and on how much of the working set fits the block cache, which only a production-sized archive
// can answer, and finding it must not mean rebuilding the archive.
func (t *ArchiveTable) SetRowGroupBytes(n int) error {
	t.a.SetRowGroupBytes(n)
	return t.cfg.update(func(cfg *ArchiveConfig) { cfg.RowGroupBytes = n })
}

// RowGroupBytes reports the budget currently in effect (0 meaning the default).
func (t *ArchiveTable) RowGroupBytes() int { return t.a.RowGroupBytes() }

// SchemaRequireAttrs returns the archive's effective required-attribute set, and
// SchemaRequireSkipped the members of it the last derivation could not make a field of.
func (t *ArchiveTable) SchemaRequireAttrs() []string { return t.a.SchemaRequireAttrs() }
func (t *ArchiveTable) SchemaRequireSkipped() []SchemaRequireSkip {
	return t.a.SchemaRequireSkipped()
}

// SetSchemaRequireAttrs replaces the CONFIGURED required-attribute set and persists it
// (archiveconfig.json), which is how an application re-asserts its own configuration onto an
// archive that was created with a different one -- archiveconfig.json is authoritative on reopen,
// so the ArchiveConfig field alone takes effect only at create.
//
// It reports whether the effective set changed, and re-derives nothing itself. The next maintenance
// pass adopts the schema the new set produces and rewrites the sealed segments onto it a
// ColumnarSegmentBudget per pass, so at history scale the change converges over several passes
// rather than reading the whole archive back at once.
func (t *ArchiveTable) SetSchemaRequireAttrs(names []string) (bool, error) {
	if !t.a.SetSchemaRequireAttrs(names) {
		return false, nil
	}
	return true, t.saveRequireConfig()
}

// AddSchemaRequireAttrs / DropSchemaRequireAttrs change the set by name and persist it. Add goes
// to the runtime tier (it survives a restart); Drop removes from both, and dropping a configured
// attribute lasts only until the application re-asserts its configuration.
func (t *ArchiveTable) AddSchemaRequireAttrs(names ...string) (bool, error) {
	if !t.a.AddSchemaRequireAttrs(names...) {
		return false, nil
	}
	return true, t.saveRequireConfig()
}

func (t *ArchiveTable) DropSchemaRequireAttrs(names ...string) (bool, error) {
	if !t.a.DropSchemaRequireAttrs(names...) {
		return false, nil
	}
	return true, t.saveRequireConfig()
}

// saveRequireConfig folds the archive's live required set back into the saved config, keeping the
// two tiers apart, and rewrites archiveconfig.json.
func (t *ArchiveTable) saveRequireConfig() error {
	return t.cfg.update(func(cfg *ArchiveConfig) {
		runtime := t.a.SchemaRequireRuntimeAttrs()
		runtimeSet := make(map[string]bool, len(runtime))
		for _, n := range runtime {
			runtimeSet[n] = true
		}
		var configured []string
		for _, n := range t.a.SchemaRequireAttrs() {
			if !runtimeSet[n] {
				configured = append(configured, n)
			}
		}
		cfg.SchemaRequireAttrs = configured
		cfg.SchemaRequireRuntime = runtime
	})
}

// SetGCFloor installs a runtime GC watermark (in the archive's MinAgeAttr units) so the next
// Rotate may reclaim already-consumed records early: a change-feed source passes the feed's
// GC floor (min ack over live subscribers) here to drain records every subscriber has read.
// It only shortens retention -- it never keeps data past the configured Retention ceilings,
// and never drops anything younger than Retention.MinAge. Unlike SetRetention this is NOT
// persisted -- the caller re-asserts it from the current live floor each pass, so a stale
// value can never GC data across a restart. Passing floor <= 0 clears it.
func (t *ArchiveTable) SetGCFloor(floor float64) { t.a.SetGCFloor(floor) }

// GCFloor returns the current runtime GC watermark (0 when unset).
func (t *ArchiveTable) GCFloor() float64 { return t.a.GCFloor() }

// SaveDemand checkpoints the archive's recorded query demand so index decisions survive a
// restart, ageing it by the time since the last checkpoint. See collections.SaveDemand.
func (t *ArchiveTable) SaveDemand() { t.a.SaveDemand() }

// AutoTune adds indexes the observed query demand justifies, persisting the result so the
// backfill it just paid for is not thrown away and re-paid on the next restart.
//
// Adds only: opts.DropUnused is forced off. On a mutable table an auto-drop is reversible at
// roughly the cost of the index; on an archive, re-adding means reading history back through
// the decompressor, so the cheap direction and the expensive direction are the wrong way
// round for acting on an absence of evidence.
func (t *ArchiveTable) AutoTune(opts AutoTuneOptions) AutoTuneResult {
	opts.DropUnused = false
	// Refuse to tune an archive whose backfill is explicitly unbounded. Adding an index
	// rebuilds every segment within the horizon, so with no horizon the tuner's first
	// decision would decompress the entire archive. A human AddIndex may ask for that,
	// having asked; a background pass nobody watched decide may not.
	//
	// Declining is the conservative direction: the cost of not indexing is slower queries,
	// the cost of indexing unbounded is hours of I/O on a schedule.
	var horizon int64
	t.cfg.with(func(cfg *ArchiveConfig) { horizon = cfg.backfillHorizon() })
	if horizon <= 0 {
		return AutoTuneResult{}
	}
	res := t.a.AutoTune(opts)
	if res.Changed {
		// Without this the archive reopens on its creation-time index set: the added index
		// disappears, the backfill is discarded, and the tuner adds it again as soon as
		// demand re-accumulates -- paying the same backfill once per restart, forever.
		t.saveIndexConfig()
	}
	return res
}

// AutoIndexNames returns the names of the archive's auto-created indexes.
func (t *ArchiveTable) AutoIndexNames() []string { return t.a.AutoIndexNames() }

// GroupSchemas derives and reports candidate group schemas for the archive (see DB.GroupSchemas).
func (t *ArchiveTable) GroupSchemas(sampleMax, k int) GroupSchemaInfo {
	return t.a.GroupSchemas(sampleMax, k)
}

// AttrTypeProfiles profiles what this archive's attributes hold (see DB.AttrTypeProfiles).
func (t *ArchiveTable) AttrTypeProfiles(sampleMax int) ([]AttrTypeProfile, int) {
	return t.a.AttrTypeProfiles(sampleMax)
}

// GroupSchemaDrift reports how the archive's derived groups have moved.
func (t *ArchiveTable) GroupSchemaDrift() GroupSchemaDrift { return t.a.GroupSchemaDrift() }

// GroupSchemaAgreement reports how well per-segment derivations agree with the archive-wide one.
func (t *ArchiveTable) GroupSchemaAgreement(sampleMax, k int) GroupSchemaAgreement {
	return t.a.GroupSchemaAgreement(sampleMax, k)
}

// GroupSchemaChanges returns the archive's committed-group change log (see DB.GroupSchemaChanges).
func (t *ArchiveTable) GroupSchemaChanges() []GroupSchemaChange { return t.a.GroupSchemaChanges() }

// GroupSchemaLastAgreement returns the archive's last persisted per-segment agreement, if any.
func (t *ArchiveTable) GroupSchemaLastAgreement() (GroupSchemaLastAgreement, bool) {
	return t.a.GroupSchemaLastAgreement()
}
