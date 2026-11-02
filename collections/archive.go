package collections

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Archive is an append-only, larger-than-RAM, rotated store of ClassAds — the
// "condor history file" use case. Ads are appended once and never updated; old data is
// dropped in bulk by rotation, with whole-segment pruning via zone maps and newest-first
// + LIMIT queries.
//
// It is a thin facade over a Collection configured as an append log (Options.AppendOnly)
// with newest-first scans, retention rotation, and zone maps. All the archive-specific
// machinery — sealed segments, per-segment sidecar indexes, zone pruning, O(segments)
// recovery, retention, and the append-stream watch — now lives in the mainline
// Collection, so the archive is "a persistent store with a few rules and no compaction"
// rather than a fork. See docs/history-archive.md.
type Archive struct {
	c   *Collection
	dir string
	// lastSeal is the shard seal count at the last eager reindex. When Append observes a new
	// seal it reindexes the just-sealed segment, so a categorical/value query on freshly
	// appended history is index-accelerated immediately rather than scanning until the next
	// periodic reindex. Touched only by the single-writer Append.
	lastSeal uint64
}

// zoneRange is one attribute's numeric span within a segment (a zone map entry). Shared
// with the Collection's zone maps (see zonemap.go). Fields are exported for JSON.
//
// A segment zone map has an entry for an attribute only when [Min,Max] holds every value in the
// segment that could satisfy a numeric comparison; values that cannot (absent, string,
// undefined, NaN) lie outside it. That is what pruning needs. Full says more: every record in
// the segment holds a number for the attribute, so all of them lie in [Min,Max], which is what
// attributing a whole segment to a range or bucket needs.
//
// Full is also what a NEGATED ordering comparison needs, and for a reason worth stating here
// because it is the inverse of the one above: a value the range leaves out because no comparison
// can be TRUE of it may still make the NEGATION true. A NaN is such a value -- comparisons with it
// are false rather than undefined -- so it satisfies `!(x <= c)` while lying outside [Min,Max],
// invisible to the probe. Only Full rules a NaN out. See zoneMayMatch.
type zoneRange struct {
	Min, Max float64
	Full     bool
}

// Retention bounds how much history an append-only store keeps. Rotate drops the oldest
// whole segments until every set bound is met. A zero field means "no bound on that
// axis". Shared with Collection (Options.Retention / Collection.Rotate).
type Retention struct {
	MaxSegments int   // keep at most this many sealed segments
	MaxBytes    int64 // keep at most this many bytes of sealed segment files
	// MaxAgeAttr names the numeric attribute (e.g. "CompletionDate", unix seconds) that
	// MaxAge is measured against. It must be a ZoneAttr so its per-segment max is available.
	// Now is supplied per Rotate call so the store needs no clock of its own.
	MaxAgeAttr string
	// MaxAge is a hard ceiling: drop a segment whose newest MaxAgeAttr value is older than
	// Now-MaxAge, regardless of anything else (a slow or absent consumer never keeps data
	// past it). Zero ⇒ no age ceiling.
	MaxAge float64
	// MinAgeAttr names the numeric attribute the minimum-retention floor and the GC floor are
	// measured against (a ZoneAttr). It is independent of MaxAgeAttr -- MinAge works with no
	// MaxAge configured -- though a change-feed queue typically points both at the same
	// entry-time attribute.
	MinAgeAttr string
	// MinAge is the minimum-retention floor, for using the store as a short-lived queue. The
	// GC floor (SetGCFloor) may reclaim already-consumed records early, but never any younger
	// than Now-MinAge -- so a consumer always has at least MinAge to drain a record. MinAge
	// only bounds the GC floor; it never overrides the hard ceilings, so MaxBytes / MaxSegments
	// / MaxAge still evict even younger-than-MinAge data under pressure. Zero ⇒ consumed
	// records may be GC'd as soon as the floor passes them.
	MinAge float64
}

// ArchiveOptions configures an Archive.
type ArchiveOptions struct {
	// Dir is the directory holding the archive's files. Required.
	Dir string
	// SegmentSize is the segment (mmap file) size in bytes; a segment rolls over when
	// the next ad will not fit. Default 8 MiB.
	SegmentSize int
	// Codec compresses stored ad bytes. Default identity. For recovery the codec must
	// match what a segment was written under (recorded per segment in its file).
	Codec Codec
	// HotAttrs front-loads these attributes in each ad's hot header (see Collection).
	HotAttrs []string
	// CategoricalAttrs / ValueAttrs configure the per-segment indexes (see Collection).
	CategoricalAttrs []string
	ValueAttrs       []string
	// ZoneAttrs names numeric attributes to keep per-segment min/max on, so a query with
	// a range/equality constraint on one can skip whole segments. ValueAttrs are
	// automatically included.
	ZoneAttrs []string
	// Retention bounds what rotation keeps. Zero ⇒ keep everything.
	Retention Retention
	// IndexBackfillBytes bounds how far back Reindex carries an index configuration change
	// (see Options.IndexBackfillBytes). 0 carries it across the whole archive.
	IndexBackfillBytes int64

	// RowGroupBytes is the uncompressed record-bytes budget for one columnar row group (see
	// Options.RowGroupBytes). 0 takes the default. It decides how much of a segment has to be
	// decompressed to read a single record, against how far compression can see across records, and
	// an archive is where that trade is worth tuning: its segments are immutable, so the choice is
	// made once per segment and lived with.
	RowGroupBytes int

	// SchemaRequireAttrs names attributes that enter the derived columnar schema whatever their
	// presence rate (see Options.SchemaRequireAttrs). It matters most on an archive: a record is
	// written once, in a final state, so attributes a record only acquires late in its life are
	// exactly the ones a history query reads and exactly the ones the presence floor keeps out.
	SchemaRequireAttrs []string

	// GroupSchemaCount and its companions enable secondary columnar schemas (see Options).
	GroupSchemaCount    int
	GroupStabilityRuns  int
	GroupMergeJaccard   float64
	GroupMaxPartialFrac float64

	// RemnantDictionary stores the values columnarized records repeat once per row group (see
	// Options.RemnantDictionary). On by default; negative turns it off.
	RemnantDictionary int

	// InternAtSeal interns each segment as soon as it seals (see Options.InternAtSeal), so an
	// archive gets the interning density/decode win immediately instead of only after a later
	// RetrainDict/Rewrite. Optional; default off. Trades a per-seal transcode (off the write
	// lock) for the win landing eagerly.
	InternAtSeal bool

	// BlockCacheBytes sets the PROCESS-GLOBAL shared archive block-cache budget (see
	// Options.ArchiveBlockCacheBytes). It is shared by ALL archives in the process, not scoped to
	// this one. 0 keeps the current/default archive budget. Setting it here is equivalent to
	// calling SetArchiveBlockCacheBudget.
	BlockCacheBytes int64

	// DataKey and EncryptedAttrs are encryption at rest, exactly as for a mutable collection (see
	// Options.DataKey and Options.EncryptedAttrs): with a key, every private attribute and every
	// attribute EncryptedAttrs names is sealed as it is appended. An attribute both sealed and indexed
	// is refused as it is there -- the caller must leave it out of CategoricalAttrs/ValueAttrs.
	//
	// Records appended before the archive had a key, or before an attribute joined the set, keep their
	// values in the clear until maintenance next rewrites their segment (a merge, a retrain, interning).
	// A read not entitled to sealed values treats those as sealed too, by name; see the archive's
	// *Redacted reads.
	DataKey        []byte
	EncryptedAttrs []string
}

const defaultArchiveSegmentSize = 8 << 20 // 8 MiB

// archiveWatchCap enables the append-stream watch hub on an archive's Collection (see
// Collection.Watch). An append log keeps no delete journal, so the value only sizes the
// live event machinery; the archive always supports Watch.
const archiveWatchCap = 1 << 10

// archiveCollectionOptions maps ArchiveOptions onto the append-log Collection configuration.
//
// The archive contract is newest-first Query (condor_history order). The Collection honors
// that on both the full-scan and the conjunctive indexed path (ReverseScan is reverse-aware
// there); disjunctive (OR) index queries fall back to a reverse full scan. So per-segment
// categorical/value indexes are configured for acceleration while newest-first is preserved.
func archiveCollectionOptions(opts ArchiveOptions) Options {
	segSize := opts.SegmentSize
	if segSize <= 0 {
		segSize = defaultArchiveSegmentSize
	}
	return Options{
		AppendOnly:          true, // pure append log: no supersession, no compaction, no key dir
		ReverseScan:         true, // newest-first Query, matching condor_history order
		Dir:                 opts.Dir,
		SegmentSize:         segSize,
		Codec:               opts.Codec,
		HotAttrs:            opts.HotAttrs,
		CategoricalAttrs:    opts.CategoricalAttrs,
		ValueAttrs:          opts.ValueAttrs, // numeric ValueAttrs are auto-added to the zone maps
		ZoneAttrs:           opts.ZoneAttrs,
		Retention:           opts.Retention,
		InternAtSeal:        opts.InternAtSeal, // intern each segment eagerly at seal
		IndexBackfillBytes:  opts.IndexBackfillBytes,
		RowGroupBytes:       opts.RowGroupBytes,
		SchemaRequireAttrs:  opts.SchemaRequireAttrs,
		GroupSchemaCount:    opts.GroupSchemaCount,
		GroupStabilityRuns:  opts.GroupStabilityRuns,
		GroupMergeJaccard:   opts.GroupMergeJaccard,
		GroupMaxPartialFrac: opts.GroupMaxPartialFrac,
		RemnantDictionary:   opts.RemnantDictionary,
		WatchHistory:        archiveWatchCap, // enable the append-stream watch
		// Global shared ARCHIVE block-cache budget (0 keeps the current/default).
		ArchiveBlockCacheBytes: opts.BlockCacheBytes,
		DataKey:                opts.DataKey,
		EncryptedAttrs:         opts.EncryptedAttrs,
	}
}

// CreateArchive creates a new, empty Archive. The directory must not already hold one
// (use OpenArchive to reopen).
func CreateArchive(opts ArchiveOptions) (*Archive, error) {
	if opts.Dir == "" {
		return nil, errors.New("archive: Dir is required")
	}
	if dirHasEntries(opts.Dir) {
		return nil, fmt.Errorf("archive: %s already initialized (use OpenArchive)", opts.Dir)
	}
	c, err := Open(archiveCollectionOptions(opts))
	if err != nil {
		return nil, err
	}
	return &Archive{c: c, dir: opts.Dir}, nil
}

// OpenArchive reopens an existing Archive (or creates one if the directory is empty),
// recovering its segments. The same options must be supplied as at creation (the
// Collection recovers each segment with the codec it names).
func OpenArchive(opts ArchiveOptions) (*Archive, error) {
	if opts.Dir == "" {
		return nil, errors.New("archive: Dir is required")
	}
	c, err := Open(archiveCollectionOptions(opts))
	if err != nil {
		return nil, err
	}
	return &Archive{c: c, dir: opts.Dir}, nil
}

// dirHasEntries reports whether dir exists and is non-empty — the "already initialized"
// test for CreateArchive.
func dirHasEntries(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false // does not exist (or unreadable): treat as fresh
	}
	defer f.Close()
	names, _ := f.Readdirnames(1)
	return len(names) > 0
}

// Append adds one ad to the archive: it is appended to the log (never superseding any
// prior record) and, when the active segment fills, a new one is started and the old one
// sealed with its index and zone maps. The ad is durable when Append returns. Safe for
// one writer concurrent with queries.
func (a *Archive) Append(ad *classad.ClassAd) error {
	// An append log has no key; a nil key routes to the single append-only shard and
	// stores a zero-length key. Duplicate appends are all retained.
	if err := a.c.Put(nil, ad); err != nil {
		return err
	}
	a.afterAppend()
	return nil
}

// AppendDeferred is Append without waiting for the ad to reach disk. The ad is readable
// by queries when it returns; it becomes durable at the next Flush, at the next durable
// Append, when its segment seals, or at Close (and, while a watcher is attached, at least
// every 256 appends). Until then a crash of the machine can lose it (a process crash
// leaves it in the page cache). What survives such a crash is always a prefix of the
// appends, since recovery cuts the log at the first torn record.
//
// Watchers see a deferred ad only once it is durable: its event is published by the sync
// that makes it so, and a Watch or WatchCursor syncs before it reads. So nothing a watcher
// has seen, and no cursor it holds, can be lost to a crash.
//
// A caller that records its own progress through the archive's source (a history file
// offset, a remote cursor) calls Flush before persisting it: Flush returning nil means
// every ad appended before it is on disk, so the persisted position never gets ahead of
// the archive. The loss window between Flushes is at most one segment, because a segment
// is synced before it seals.
func (a *Archive) AppendDeferred(ad *classad.ClassAd) error {
	if err := a.c.putDeferred(nil, ad); err != nil {
		return err
	}
	a.afterAppend()
	return nil
}

// afterAppend does the eager per-seal work after an append, off the write lock: if the
// append sealed a segment, intern the just-sealed segment (Options.InternAtSeal) so its
// density/decode win lands immediately, then build its sidecar index so categorical/value
// queries on just-appended history are accelerated instead of scanning until the next
// periodic reindex. Interning runs first so the sidecar is built over the interned segment.
// Both are idempotent, build only what is missing (all older segments are already
// sealed/interned), and are serialized against the periodic passes. Skipped when neither
// is configured (nothing to do).
func (a *Archive) afterAppend() {
	if a.c.internAtSeal || a.c.hasSegmentIndexes() {
		if seal := a.c.appendSealSeq(); seal != a.lastSeal {
			a.lastSeal = seal
			if a.c.internAtSeal {
				a.c.InternSealed()
			}
			if a.c.hasSegmentIndexes() {
				a.c.Reindex()
			}
		}
	}
}

// Flush is the durability barrier for AppendDeferred: it returns once every ad appended
// before the call is on disk and published to watchers, or reports the sticky write or
// sync error that prevented it. The error is sticky: every later Append, AppendDeferred
// and Flush reports it until the archive is truncated or reopened. With no deferred
// appends outstanding Flush is cheap.
func (a *Archive) Flush() error { return a.c.syncAll() }

// Rotate drops whole oldest segments until the archive is back within its Retention
// bounds, returning the number dropped. now is the caller's wall clock (unix seconds) for
// age-based retention.
func (a *Archive) Rotate(now float64) (int, error) { return a.c.Rotate(now) }

// Count returns the number of records currently retained. Rotation reduces it in
// whole-segment steps.
func (a *Archive) Count() int { return a.c.Len() }

// CountConstraint counts the records matching constraint via the columnar accelerator, returning
// ok=false when the columnar path cannot serve it (see Collection.CountConstraint) so the caller
// scans instead.
//
// An archive is the big append-only scan target, and it is exactly where this was missing: a mutable
// table's COUNT(*) WHERE reaches the columnar count through DB.CountConstraint, while an archive's
// went to a wire-native scan of every record, even with an accelerator built over its segments.
func (a *Archive) CountConstraint(constraint string) (int, bool) {
	return a.c.CountConstraint(constraint)
}

// GroupCountConstraint returns the per-value record counts of groupAttr over the records
// matching constraint, read out of the columnar blocks. ok=false ⇒ the shape is not
// columnar-eligible and the caller must group by scanning records.
func (a *Archive) GroupCountConstraint(constraint, groupAttr string) ([]GroupCount, bool) {
	return a.c.GroupCountConstraint(constraint, groupAttr)
}

// GroupStatsConstraint returns the per-group record count plus a NumStats per aggAttr over the records
// matching constraint, read out of the columnar blocks. ok=false ⇒ not columnar-eligible; group by
// scanning records. See Collection.GroupStatsQuery.
func (a *Archive) GroupStatsConstraint(constraint, groupAttr string, aggAttrs []string) ([]GroupStats, bool) {
	return a.GroupStatsConstraintBucketed(constraint, groupAttr, 0, aggAttrs)
}

// GroupStatsConstraintBucketed is GroupStatsConstraint with the group column floored into buckets of
// bucket (0 = the raw value). See Collection.GroupStatsQueryBucketed.
func (a *Archive) GroupStatsConstraintBucketed(constraint, groupAttr string, bucket int64, aggAttrs []string) ([]GroupStats, bool) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, false
	}
	return a.c.GroupStatsQueryBucketed(q, groupAttr, bucket, aggAttrs)
}

// GroupStatsAll is GroupStatsConstraint over EVERY record. The caller must have established that its
// query matches all records.
func (a *Archive) GroupStatsAll(groupAttr string, aggAttrs []string) ([]GroupStats, bool) {
	return a.c.GroupStatsAll(groupAttr, aggAttrs)
}

// GroupStatsAllBucketed is GroupStatsAll with the group column floored into buckets of bucket
// (0 = the raw value). See Collection.GroupStatsQueryBucketed.
func (a *Archive) GroupStatsAllBucketed(groupAttr string, bucket int64, aggAttrs []string) ([]GroupStats, bool) {
	return a.c.GroupStatsAllBucketed(groupAttr, bucket, aggAttrs)
}

// GroupStatsConstraintCols is GroupStatsConstraintBucketed over SEVERAL group columns: the group is
// the tuple of their (individually bucketed) values. See Collection.GroupStatsQueryCols.
func (a *Archive) GroupStatsConstraintCols(constraint string, groups []GroupSpec, aggAttrs []string) ([]GroupStats, bool) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, false
	}
	return a.c.GroupStatsQueryCols(q, groups, aggAttrs)
}

// GroupStatsAllCols is GroupStatsAllBucketed over SEVERAL group columns. The caller must have
// established that its query matches all records. See Collection.GroupStatsQueryCols.
func (a *Archive) GroupStatsAllCols(groups []GroupSpec, aggAttrs []string) ([]GroupStats, bool) {
	return a.c.GroupStatsAllCols(groups, aggAttrs)
}

// GroupStatsConstraintAggs is GroupStatsConstraintCols where an aggregate column may carry a
// per-aggregate FILTER. See Collection.GroupStatsConstraintAggs.
func (a *Archive) GroupStatsConstraintAggs(constraint string, groups []GroupSpec, aggCols []GroupAggCol,
	countFilters []string) ([]GroupStats, bool) {
	return a.c.GroupStatsConstraintAggs(constraint, groups, aggCols, countFilters)
}

// GroupStatsAllAggs is GroupStatsConstraintAggs over every record, for a constraint the caller has
// established is match-all. See Collection.GroupStatsAllAggs.
func (a *Archive) GroupStatsAllAggs(groups []GroupSpec, aggCols []GroupAggCol,
	countFilters []string) ([]GroupStats, bool) {
	return a.c.GroupStatsAllAggs(groups, aggCols, countFilters)
}

// GroupCountAll returns the per-value record counts of groupAttr over EVERY record, read out of the
// columnar blocks. The caller must have established that its query matches all records.
// ok=false ⇒ not columnar-eligible; group by scanning records.
func (a *Archive) GroupCountAll(groupAttr string) ([]GroupCount, bool) {
	return a.c.GroupCountAll(groupAttr)
}

// Truncate drops every record, resetting the archive to empty in place: all segments are
// unmapped and their data + sidecar-index files unlinked (via the backing Collection's
// Truncate). Segment counters keep advancing, so a fresh Append starts a new segment. This
// is the destructive reset a from-scratch history re-sync uses -- empty the store, then
// re-ingest from the source. Retention/index/zone-map configuration is preserved. Safe
// against concurrent queries (a scan holding a pin reads its old data until it finishes);
// callers must serialize Truncate against the single Append writer.
func (a *Archive) Truncate() { a.c.Truncate() }

// Close flushes and unmaps the archive, making every append durable, deferred ones
// included. It must not be used afterward.
func (a *Archive) Close() error { return a.c.Close() }

// Query returns an iterator over the ads matching q, newest first.
func (a *Archive) Query(q *vm.Query) iter.Seq[*classad.ClassAd] { return a.QueryLimit(q, 0) }

// QueryLimit returns an iterator over the ads matching q, newest first, stopping after
// limit results (limit <= 0 ⇒ unlimited). Because the backing Collection scans
// newest-first, stopping early yields the most recent limit matches — a pushed-down LIMIT.
func (a *Archive) QueryLimit(q *vm.Query, limit int) iter.Seq[*classad.ClassAd] {
	return firstN(a.c.Query(q), limit)
}

// firstN yields the first limit ads of seq (limit <= 0: all of them), stopping seq there.
func firstN(seq iter.Seq[*classad.ClassAd], limit int) iter.Seq[*classad.ClassAd] {
	return func(yield func(*classad.ClassAd) bool) {
		n := 0
		for ad := range seq {
			if !yield(ad) {
				return
			}
			n++
			if limit > 0 && n >= limit {
				return
			}
		}
	}
}

// QueryProject scans the ads matching q and yields each one projected to just attrs'
// values (aligned with attrs), read wire-native where possible -- so an aggregate does not
// pay the full-ad decode QueryLimit costs. The yielded slice is reused; copy to retain.
// QueryRawProjected yields each ad matching q as a raw projected subset (only the projection
// attributes, rendered from the stored representation), newest first — the archive-side of the
// server projection relay. It shares the backing Collection's projection walk, so the same
// newest-first ordering and pushed-down LIMIT (via early stop by the caller) apply. chaseRefs
// and redact are as in Collection.QueryRawProjected.
func (a *Archive) QueryRawProjected(q *vm.Query, projection []string, chaseRefs, redact bool) iter.Seq[RawAd] {
	return a.c.QueryRawProjected(q, projection, chaseRefs, redact)
}

// QueryRawProjectedStats is QueryRawProjected that also fills stats (may be nil) with the
// per-scan work breakdown for EXPLAIN ANALYZE.
func (a *Archive) QueryRawProjectedStats(q *vm.Query, projection []string, chaseRefs, redact bool, stats *ScanStats) iter.Seq[RawAd] {
	return a.c.QueryRawProjectedStats(q, projection, chaseRefs, redact, stats)
}

// WIRE-FORM ROWS FROM AN ARCHIVE.
//
// These delegate, and the delegation is the whole implementation, for the same reason
// QueryRawProjected's is: the backing Collection scans newest-first, so the archive's ordering
// and its pushed-down LIMIT (the caller stopping early) fall out of the shared walk. An archive
// is also the storage form wire rows suit best -- its records already carry inline names, so a
// row is assembled by slice copies with no intern table to consult.
//
// They exist because the relay op that reads them refused an archive, which made `history` --
// the table most queries are about -- the one table that could not be read the cheap way. The
// text projection it fell back to renders every record and makes the consumer parse it back,
// measured at roughly two fifths of a read's CPU and 86 allocations a row downstream.

// SupportsRawWire reports whether the wire-form scans can serve this archive
// (Collection.SupportsRawWire).
func (a *Archive) SupportsRawWire() bool { return a.c.SupportsRawWire() }

// ScanRawWire yields every retained record as a self-contained wire-form row, newest first
// (Collection.ScanRawWire).
func (a *Archive) ScanRawWire(projection []string, redact bool) iter.Seq[[]byte] {
	return a.c.ScanRawWire(projection, redact)
}

// QueryRawWire yields each record matching q as a self-contained wire-form row, newest first
// (Collection.QueryRawWire).
func (a *Archive) QueryRawWire(q *vm.Query, projection []string, redact bool) iter.Seq[[]byte] {
	return a.c.QueryRawWire(q, projection, redact)
}

// QueryRawWireStats is QueryRawWire that also fills stats (may be nil) with the per-scan work
// breakdown.
func (a *Archive) QueryRawWireStats(q *vm.Query, projection []string, redact bool, stats *ScanStats) iter.Seq[[]byte] {
	return a.c.QueryRawWireStats(q, projection, redact, stats)
}

// ScanRawWireRefs is ScanRawWire whose projection also carries the attributes the projected
// expressions reference (Collection.ScanRawWireRefs). This is the form a consumer that
// EVALUATES what it receives needs.
func (a *Archive) ScanRawWireRefs(projection []string, redact bool) iter.Seq[[]byte] {
	return a.c.ScanRawWireRefs(projection, redact)
}

// QueryRawWireRefs is QueryRawWire with the reference closure of ScanRawWireRefs.
func (a *Archive) QueryRawWireRefs(q *vm.Query, projection []string, redact bool) iter.Seq[[]byte] {
	return a.c.QueryRawWireRefs(q, projection, redact)
}

// QueryRawWireRefsStats is QueryRawWireRefs that also fills stats (may be nil).
func (a *Archive) QueryRawWireRefsStats(q *vm.Query, projection []string, redact bool, stats *ScanStats) iter.Seq[[]byte] {
	return a.c.QueryRawWireRefsStats(q, projection, redact, stats)
}

// QueryProject scans the matching records and yields each projected to just attrs' values,
// read wire-native where possible. See Collection.QueryProject.
func (a *Archive) QueryProject(q *vm.Query, attrs []string) iter.Seq[[]classad.Value] {
	return a.c.QueryProject(q, attrs)
}

// QueryProjectStats is QueryProject that also fills stats (may be nil) with the scan's work
// breakdown, for an EXPLAIN ANALYZE of an aggregate that falls to the projected scan.
func (a *Archive) QueryProjectStats(q *vm.Query, attrs []string, stats *ScanStats) iter.Seq[[]classad.Value] {
	return a.c.QueryProjectStats(q, attrs, stats)
}

// Reads for a caller NOT entitled to sealed values. Each is the read above it with no key: a sealed
// value is undefined to the constraint, to what is returned and to anything computed from it -- the
// rules a mutable table's *Redacted reads follow. An archive may also hold a sealed attribute's values
// in the clear, written before it was sealed; these reads withhold those by name as well, so such a
// reader sees the same undefined for every record whatever form its value is stored in.

// QueryLimitRedacted is QueryLimit read with no key.
func (a *Archive) QueryLimitRedacted(q *vm.Query, limit int) iter.Seq[*classad.ClassAd] {
	return firstN(a.c.QueryRedacted(q), limit)
}

// QueryProjectRedacted is QueryProject read with no key.
//
// The stats form below had this and the plain one did not, which is a trap for the layer above: a
// history table is read by the same unprivileged peers a mutable table is, and a projected read
// that could only be made entitled would either refuse those peers or serve them the plaintext --
// which is how the paged projection came to ignore its session's privilege.
func (a *Archive) QueryProjectRedacted(q *vm.Query, attrs []string) iter.Seq[[]classad.Value] {
	return a.c.QueryProjectRedacted(q, attrs)
}

// QueryProjectStatsRedacted is QueryProjectStats read with no key.
func (a *Archive) QueryProjectStatsRedacted(q *vm.Query, attrs []string, stats *ScanStats) iter.Seq[[]classad.Value] {
	return a.c.QueryProjectStatsRedacted(q, attrs, stats)
}

// CountConstraintRedacted is CountConstraint read with no key.
func (a *Archive) CountConstraintRedacted(constraint string) (int, bool) {
	return a.c.CountConstraintRedacted(constraint)
}

// NumStatsQueryRedacted is NumStatsQuery read with no key.
func (a *Archive) NumStatsQueryRedacted(q *vm.Query, attr string) (NumStats, bool) {
	return a.c.NumStatsQueryRedacted(q, attr)
}

// TopKOrderThresholdRedacted is TopKOrderThreshold read with no key.
func (a *Archive) TopKOrderThresholdRedacted(q *vm.Query, orderAttr string, desc bool, k int, stats *ScanStats) (float64, int, bool) {
	return a.c.TopKOrderThresholdRedacted(q, orderAttr, desc, k, stats)
}

// GroupStatsConstraintAggsRedacted is GroupStatsConstraintAggs read with no key.
func (a *Archive) GroupStatsConstraintAggsRedacted(constraint string, groups []GroupSpec, aggCols []GroupAggCol,
	countFilters []string) ([]GroupStats, bool) {
	return a.c.GroupStatsConstraintAggsRedacted(constraint, groups, aggCols, countFilters)
}

// GroupStatsAllAggsRedacted is GroupStatsAllAggs read with no key.
func (a *Archive) GroupStatsAllAggsRedacted(groups []GroupSpec, aggCols []GroupAggCol,
	countFilters []string) ([]GroupStats, bool) {
	return a.c.GroupStatsAllAggsRedacted(groups, aggCols, countFilters)
}

// Watch streams the archive as change data: a full replay of retained records (oldest
// first) then live appends, resumable from an opaque cursor. A cursor older than what
// rotation still retains yields a WatchReset and resumes from the current floor. See
// Collection.Watch and docs/WATCH.md.
func (a *Archive) Watch(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error) {
	return a.c.Watch(ctx, cursor)
}

// WatchRedacted is Watch for a watcher NOT entitled to sealed values: every event's ad is decoded
// with no key. See Collection.WatchRedacted.
func (a *Archive) WatchRedacted(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error) {
	return a.c.WatchRedacted(ctx, cursor)
}

// WatchCursor returns an opaque cursor at the current head of the archive's change log,
// so a following Watch streams only subsequent appends rather than replaying what is
// retained. See Collection.WatchCursor.
func (a *Archive) WatchCursor() ([]byte, error) {
	return a.c.WatchCursor()
}

// ExplainQuery reports how the archive would execute q -- which conjuncts are index-usable
// and the resulting access path -- as Collection.ExplainQuery does for a mutable table.
//
// Two archive-specific notes on reading the result. An archive's scan is newest-first over
// sealed segments, and whole segments are additionally skipped by zone map (see ZoneAttrs),
// which the plan string does not name: a "serial-scan" over an archive may still visit far
// fewer records than TotalAds. And TotalAds counts everything retained, so probe selectivity
// is against the whole history rather than a working set.
func (a *Archive) ExplainQuery(q *vm.Query) QueryExplain { return a.c.ExplainQuery(q) }

// BuildAndEnableSchemaScan builds (or extends coverage of) the per-segment columnar
// accelerator over the archive's sealed segments, as Collection.BuildAndEnableSchemaScan does
// for a mutable table. An archive is the natural target for it -- the segments are immutable
// once sealed, so a block never needs invalidating -- and the payoff is large: a numeric
// COUNT over 50k history records drops from ~50ms of row scan to ~1ms.
//
// The first call reads every sealed record once to sample and to build a block per segment,
// which over a long history is the same "paid for by reading history back" cost an archive
// index backfill has. Later calls cover segments sealed since, re-derive the schema and adopt it
// when the change is material, and converge stale segments a budget's worth per call.
func (a *Archive) BuildAndEnableSchemaScan(sampleMax, hotTopN int) bool {
	return a.c.BuildAndEnableSchemaScan(sampleMax, hotTopN)
}

// SchemaFit measures the archive's derived schema against a fresh sample, reporting per-field
// escape rates (see Collection.SchemaFit).
func (a *Archive) SchemaFit(sampleMax int) ([]SchemaFieldFit, int) { return a.c.SchemaFit(sampleMax) }

// ReschemaScan re-derives the archive's schema and adopts it, rebuilding sidecar blocks now and
// converging columnarized segments over maintenance passes (see Collection.ReschemaScan).
func (a *Archive) ReschemaScan(sampleMax, hotTopN int) bool {
	return a.c.ReschemaScan(sampleMax, hotTopN)
}

// SchemaScanInfo reports the columnar accelerator's state for the archive.
func (a *Archive) SchemaScanInfo() SchemaScanInfo { return a.c.SchemaScanInfo() }

// SchemaRequireAttrs returns the archive's effective required-attribute set, and
// SchemaRequireRuntimeAttrs the subset added at runtime (see Collection).
func (a *Archive) SchemaRequireAttrs() []string        { return a.c.SchemaRequireAttrs() }
func (a *Archive) SchemaRequireRuntimeAttrs() []string { return a.c.SchemaRequireRuntimeAttrs() }

// SetSchemaRequireAttrs / AddSchemaRequireAttrs / DropSchemaRequireAttrs change the required set
// (see Collection). Each reports whether the effective set changed; none re-derives anything
// itself -- the next maintenance review adopts the schema the new set produces and converges the
// sealed segments onto it, ColumnarSegmentBudget per pass.
func (a *Archive) SetSchemaRequireAttrs(names []string) bool { return a.c.SetSchemaRequireAttrs(names) }
func (a *Archive) AddSchemaRequireAttrs(names ...string) bool {
	return a.c.AddSchemaRequireAttrs(names...)
}
func (a *Archive) DropSchemaRequireAttrs(names ...string) bool {
	return a.c.DropSchemaRequireAttrs(names...)
}

// SchemaRequireSkipped returns the required attributes the archive's last schema derivation
// refused, with the reason (see Collection.SchemaRequireSkipped).
func (a *Archive) SchemaRequireSkipped() []SchemaRequireSkip { return a.c.SchemaRequireSkipped() }

// NumStatsQuery computes numeric aggregate inputs for attr over the archive's matching records
// via the columnar scan (see Collection.NumStatsQuery). Only meaningful once the archive carries
// a columnar accelerator; ok=false otherwise, and the caller scans.
func (a *Archive) NumStatsQuery(q *vm.Query, attr string) (NumStats, bool) {
	return a.c.NumStatsQuery(q, attr)
}

// TopKOrderThreshold computes the ORDER BY orderAttr {DESC|ASC} LIMIT k cutoff over the archive's
// matching records via the columnar scan (see Collection.TopKOrderThreshold). stats (may be nil)
// records the cutoff scan's work for EXPLAIN ANALYZE.
func (a *Archive) TopKOrderThreshold(q *vm.Query, orderAttr string, desc bool, k int, stats *ScanStats) (float64, int, bool) {
	return a.c.TopKOrderThreshold(q, orderAttr, desc, k, stats)
}

// SidecarSizes reports the archive's sealed-segment sidecar index bytes (mmap-backed,
// evictable page cache), broken out by structure. An operator diagnostic.
func (a *Archive) SidecarSizes() SidecarSizes { return a.c.SidecarSizes() }

// HotAttrNames reports the attributes stored in the hot header, mirroring Collection.HotAttrNames. An
// archive has a hot set exactly as a mutable collection does; only the accessor was missing, which made
// an archive look like it had none.
func (a *Archive) HotAttrNames() []string { return a.c.HotAttrNames() }

// RefreshHotSet recomputes the hot set from accumulated query demand and installs it for
// FUTURE writes, returning how many attributes were chosen. See Collection.RefreshHotSet
// for the ranking.
//
// It takes effect immediately for records appended from now on, and changes nothing already
// sealed. That is safe without any rewrite because the hot header is per RECORD and
// self-describing -- each record states its own hot count and pairs -- so records written
// under different hot sets coexist in one archive, and a reader resolving an attribute that
// is not in a given record's header simply misses and falls back to the entry scan.
//
// Older records are not stranded on the old set forever either, though they converge more
// slowly than a mutable table's ads do. MergePass re-encodes the records it merges
// (resealSegments), under the hot set in effect AT MERGE TIME -- so a cold run picks the
// refreshed set up when it is next folded together. What that leaves behind is whatever the
// merge policy never merges: the KeepRecent tail (which was written under a recent set
// anyway) and any run the watermarks never select. Rewrite() is the way to force the rest,
// at the cost of re-encoding the whole history.
func (a *Archive) RefreshHotSet(sampleMax, topN int) int { return a.c.RefreshHotSet(sampleMax, topN) }

// AddHotAttrs pins the named attributes into the hot set, merging them with the current
// one, and returns the resulting hot attributes. As with RefreshHotSet this governs future
// writes only; existing records keep the header they were written with.
func (a *Archive) AddHotAttrs(names ...string) []string { return a.c.AddHotAttrs(names...) }

// EncryptionEnabled reports whether this archive holds a data key, so that the attributes the sealing
// policy names are sealed as they are appended (mirroring Collection.EncryptionEnabled).
func (a *Archive) EncryptionEnabled() bool { return a.c.EncryptionEnabled() }

// SealsValues reports whether any value this archive stores can be sealed (see
// Collection.SealsValues).
func (a *Archive) SealsValues() bool { return a.c.SealsValues() }

// EncryptedAttrNames reports the attributes this archive seals, mirroring Collection.EncryptedAttrNames.
func (a *Archive) EncryptedAttrNames() []string { return a.c.EncryptedAttrNames() }

// SetEncryptedAttrs replaces the explicit set of sealed attributes, as Collection.SetEncryptedAttrs
// does: it errors without a key, and for an attribute that is currently indexed. Records appended from
// now on are sealed under the new set; earlier ones keep their form until maintenance rewrites their
// segment, and the *Redacted reads withhold an attribute the set names whatever form it is stored in.
func (a *Archive) SetEncryptedAttrs(attrs []string) error { return a.c.SetEncryptedAttrs(attrs) }

// KeylessWithholds reports whether a read not entitled to sealed values withholds any of names --
// reads them as undefined whatever their stored form -- because the sealing policy names it. Such an
// attribute's clear values from before it was sealed can sit in this archive's columns, zone maps and
// indexes, so a caller planning such a read must not answer it from those.
func (a *Archive) KeylessWithholds(names ...string) bool { return a.c.keylessHidesAny(names...) }

// KeylessWithholdsIn reports whether constraint references an attribute KeylessWithholds reports. A
// constraint that does not parse reports true: it reaches no answer either way.
func (a *Archive) KeylessWithholdsIn(constraint string) bool {
	if !a.c.hidesClearSealed() {
		return false
	}
	q, err := vm.Parse(constraint)
	if err != nil {
		return true
	}
	return a.c.keylessQuery(q) != q
}

// RetrainDict trains a fresh ZSTD dictionary from up to sampleMax records and recompresses
// every segment in place under it (an append-only reseal that preserves order), returning
// the new dictionary's size in bytes. This is how an archive's compression adapts to the
// data it has accumulated.
func (a *Archive) RetrainDict(sampleMax int) (int, error) { return a.c.RetrainDict(sampleMax) }

// Rewrite recompresses and re-encodes every segment in place under the current codec and
// hot set (e.g. after a hot-set change), preserving order, and returns the number of
// records rewritten.
func (a *Archive) Rewrite() int { return a.c.Rewrite() }

// AddIndex adds per-segment indexes on the named categorical and/or value attributes and
// rebuilds every segment's index under the new set, so existing records are covered too.
// Returns false if the index set was unchanged.
//
// Cost: the rebuild decompresses every record in the archive once. It does NOT rewrite any
// segment data -- a sealed segment's bytes are already correct and are left untouched; only
// the derived index sidecar beside them is rebuilt (see Collection.reindexSealed). That is
// the difference from Rewrite, which re-encodes and rewrites the whole store.
func (a *Archive) AddIndex(categorical, value []string) bool {
	if !a.c.AddIndex(categorical, value) {
		return false
	}
	a.c.Reindex()
	return true
}

// DropIndex removes the named per-segment indexes. Returns false if none matched.
func (a *Archive) DropIndex(names ...string) bool { return a.c.DropIndex(names...) }

// Reindex rebuilds the per-segment indexes over all segments: those sealed since the last
// build, and those whose sidecar was built under a superseded index configuration and lie
// within ArchiveOptions.IndexBackfillBytes of the newest data. Segment data is never touched.
func (a *Archive) Reindex() { a.c.Reindex() }

// ReindexAll is Reindex carried to every segment regardless of the backfill horizon, so each
// sealed segment ends up holding postings for every indexed attribute. See
// Collection.ReindexAll.
func (a *Archive) ReindexAll() IndexBackfillResult { return a.c.ReindexAll() }

// BackfillIndexes rebuilds, newest first and up to maxBytes of segment data, the sidecars of
// sealed segments that lack postings for an indexed attribute. See Collection.BackfillIndexes.
func (a *Archive) BackfillIndexes(maxBytes int64) IndexBackfillResult {
	return a.c.BackfillIndexes(maxBytes)
}

// IndexBackfillPending reports the sealed segments, and their bytes, still lacking postings
// for some indexed attribute.
func (a *Archive) IndexBackfillPending() (segments int, bytes int64) {
	return a.c.IndexBackfillPending()
}

// SaveDemand checkpoints recorded query demand to the archive's directory, ageing it by the
// time since the last checkpoint, so index decisions are not restarted from zero every time
// the process is. See Collection.SaveDemand.
func (a *Archive) SaveDemand() { a.c.SaveDemand() }

// AutoTune applies demand-driven index changes to the archive. See Collection.AutoTune.
func (a *Archive) AutoTune(opts AutoTuneOptions) AutoTuneResult { return a.c.AutoTune(opts) }

// MarkAutoIndexes records existing indexes as auto-created, restoring the provenance a
// saved config carries without rebuilding anything. See Collection.MarkAutoIndexes.
func (a *Archive) MarkAutoIndexes(names []string) { a.c.MarkAutoIndexes(names) }

// AutoIndexNames returns the names of the archive's auto-created indexes, for persisting
// provenance alongside the index set itself.
func (a *Archive) AutoIndexNames() []string { return a.c.AutoIndexNames() }

// SetRetention updates the retention bounds at runtime; the next Rotate enforces them.
func (a *Archive) SetRetention(r Retention) { a.c.SetRetention(r) }

// Retention returns the archive's current retention bounds.
func (a *Archive) Retention() Retention { return a.c.Retention() }

// SetGCFloor installs a runtime GC watermark (in Retention.MinAgeAttr units) so Rotate may
// reclaim consumed records early -- above MinAge, below the configured ceilings (see
// Collection.SetGCFloor). Not persisted; re-assert it each maintenance pass. floor <= 0
// clears it.
func (a *Archive) SetGCFloor(floor float64) { a.c.SetGCFloor(floor) }

// GCFloor returns the current runtime GC watermark (0 when unset).
func (a *Archive) GCFloor() float64 { return a.c.GCFloor() }

// Stats reports storage accounting: record count, segment count, and arena/used/dead bytes
// (dead is ~0 for an append log, which never supersedes). The same struct the mutable store
// reports, so an archive's storage is visible on the same terms.
func (a *Archive) Stats() Stats { return a.c.Stats() }

// OpStats reports cumulative operational timing counters (write wait/hold, sync, retrain,
// reindex, ...) for the archive's writes and maintenance.
func (a *Archive) OpStats() OpStats { return a.c.OpStats() }

// CodecStats reports the archive's compression: codec, dictionary size, last retrain time,
// and the sampled compression ratio (up to sampleMax records).
func (a *Archive) CodecStats(sampleMax int) CodecStats { return a.c.CodecStats(sampleMax) }

// IndexSizes reports the per-attribute index byte footprint (heap postings) versus data.
func (a *Archive) IndexSizes() IndexSizes { return a.c.IndexSizes() }

// IndexedAttrs returns the categorical and value attributes the archive indexes.
func (a *Archive) IndexedAttrs() (categorical, value []string) { return a.c.IndexedAttrs() }

// ZoneAttrs returns the attributes carrying per-segment [min,max] zone maps -- the ones a
// range query prunes whole segments on, not just postings.
func (a *Archive) ZoneAttrs() []string { return a.c.ZoneAttrs() }

// StaleIndexSegments reports how many of the archive's sealed segments still carry an index
// built under an older configuration, and how many are sealed in total. Normally zero:
// AddIndex/DropIndex rebuild as they go. A non-zero count means a rebuild was interrupted or
// failed (it is best-effort per segment); a Reindex retries those segments.
func (a *Archive) StaleIndexSegments() (stale, sealed int) { return a.c.StaleIndexSegments() }

// StaleIndexSegmentsByPolicy reports segments left on an older index configuration because
// they fall outside the backfill horizon: expected, and not actionable. Those among them that
// lack an indexed attribute are what BackfillIndexes works through (IndexBackfillPending).
func (a *Archive) StaleIndexSegmentsByPolicy() int { return a.c.StaleIndexSegmentsByPolicy() }

// --- zone-map helpers (shared with the Collection's per-segment zone maps) ---

// literalFloat extracts a numeric value from a wire literal node (int/real/bool), for
// building and testing zone maps.
func literalFloat(node []byte) (float64, bool) {
	lit, ok := wire.LiteralValue(node)
	if !ok {
		return 0, false
	}
	switch lit.Kind {
	case wire.LitInt:
		return float64(lit.Int), true
	case wire.LitReal:
		return lit.Real, true
	case wire.LitBool:
		if lit.Bool {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// SetRowGroupBytes changes the archive's columnar row-group budget at runtime (see
// Collection.SetRowGroupBytes). It governs segments sealed from now on; nothing already written is
// rewritten or needs to be.
func (a *Archive) SetRowGroupBytes(n int) { a.c.SetRowGroupBytes(n) }

// RowGroupBytes reports the budget currently in effect (0 meaning the default).
func (a *Archive) RowGroupBytes() int { return a.c.RowGroupBytes() }

// zonePrune reports whether a segment can be skipped entirely: some required conjunct (a
// top-level AND probe) on a zone-mapped attribute cannot be satisfied by any record whose
// value lies in the segment's [min,max] for that attribute.
func zonePrune(zones map[uint32]zoneRange, probes []vm.Probe, intern *wire.InternTable) bool {
	if len(zones) == 0 {
		return false
	}
	for _, p := range probes {
		id, ok := intern.LookupID(p.Attr)
		if !ok {
			continue
		}
		z, ok := zones[id]
		if !ok {
			continue
		}
		vals := probeFloats(p)
		if len(vals) == 0 {
			continue // non-numeric constraint: zone map can't rule it out
		}
		// Zones hold float64 bounds, so an integer probe past 2^53 prunes only on the
		// widened operator (see floatKeyOp).
		op, _, ok := floatKeyOp(p.Op, p.Vals)
		if !ok {
			continue
		}
		if !zoneMayMatch(z, op, vals) {
			return true
		}
	}
	return false
}

// zoneMayMatch reports whether a segment with the given [min,max] for an attribute could
// contain a record satisfying (op, vals). A false result means the segment is prunable for
// this required conjunct.
func zoneMayMatch(z zoneRange, op string, vals []float64) bool {
	switch op {
	case "==", "in":
		for _, v := range vals {
			if v >= z.Min && v <= z.Max {
				return true
			}
		}
		return false
	case "<":
		return z.Min < vals[0]
	case "<=":
		return z.Min <= vals[0]
	case ">":
		return z.Max > vals[0]
	case ">=":
		return z.Max >= vals[0]
	}
	// A NEGATED ordering comparison. Pruning on one is sound ONLY over a Full zone, and the reason
	// is the exact inverse of what makes the positive form prunable.
	//
	// The range deliberately leaves out every value no numeric comparison can be true of -- absent,
	// string, undefined, NaN (zoneValue classes them zoneNever/sparse). For the positive form that
	// is free: such a value cannot satisfy the comparison, so a probe that misses it prunes nothing
	// it should have kept. Under negation it inverts. A NaN makes `x <= c` FALSE -- every
	// comparison with a NaN is false, not undefined -- so it makes `!(x <= c)` TRUE, and it is
	// invisible to the range. Reusing the flipped-operator test would prune a segment whose only
	// matches are NaN: a wrong answer, served faster than the right one, and wrong for the ROW SCAN
	// too, since zonePrune decides which segments the scan reads at all.
	//
	// (Absent/string/undefined do NOT satisfy the negation: the comparison is UNDEFINED or ERROR
	// there, and `!` of either is not TRUE. NaN is the only sparse cause that flips sides, but the
	// zone map does not record WHICH cause made a zone sparse.)
	//
	// Full is exactly the missing proof: it says every record in the segment holds a number for the
	// attribute, which rules out NaN along with absent and non-numeric, so the segment's values are
	// precisely the numbers in [Min,Max]. Over those, the negation IS the flipped comparison and
	// the ordinary test decides it. Without Full the segment may hold a NaN the range cannot show,
	// so the conjunct is undecidable here and the segment must be read.
	if base, neg := negOrderingBase(op); neg {
		if !z.Full {
			return true
		}
		return zoneMayMatch(z, orderingFlip[base], vals)
	}
	return true // != and anything else: not prunable
}

// probeFloats returns the numeric values of a probe, or nil if any value is non-numeric
// (in which case the zone map cannot reason about it).
func probeFloats(p vm.Probe) []float64 {
	out := make([]float64, 0, len(p.Vals))
	for _, v := range p.Vals {
		f, ok := numericFloat(v)
		if !ok {
			return nil
		}
		out = append(out, f)
	}
	return out
}

// GroupSchemas derives and reports candidate group schemas for the archive (see
// Collection.GroupSchemas). Report-only: nothing about storage or reads changes.
func (a *Archive) GroupSchemas(sampleMax, k int) GroupSchemaInfo {
	return a.c.GroupSchemas(sampleMax, k)
}

// AttrTypeProfiles profiles what the archive's attributes hold (see
// Collection.AttrTypeProfiles). History is where attribute variety is widest -- a table that
// has accumulated every shape its submitters ever sent -- so this is the side a consumer
// declaring types most needs.
func (a *Archive) AttrTypeProfiles(sampleMax int) ([]AttrTypeProfile, int) {
	return a.c.AttrTypeProfiles(sampleMax)
}

// AttrTypeProfilesWith is AttrTypeProfiles with the measurement selected by o (see
// Collection.AttrTypeProfilesWith).
func (a *Archive) AttrTypeProfilesWith(o AttrTypeOptions) ([]AttrTypeProfile, int) {
	return a.c.AttrTypeProfilesWith(o)
}

// GroupSchemaDrift reports how the archive's derived groups have moved (see
// Collection.GroupSchemaDrift).
func (a *Archive) GroupSchemaDrift() GroupSchemaDrift { return a.c.GroupSchemaDrift() }

// GroupSchemaAgreement reports how well per-segment derivations agree with the archive-wide one
// (see Collection.GroupSchemaAgreement).
func (a *Archive) GroupSchemaAgreement(sampleMax, k int) GroupSchemaAgreement {
	return a.c.GroupSchemaAgreement(sampleMax, k)
}

// GroupSchemaChanges returns the archive's committed-group change log (see
// Collection.GroupSchemaChanges).
func (a *Archive) GroupSchemaChanges() []GroupSchemaChange { return a.c.GroupSchemaChanges() }

// GroupSchemaLastAgreement returns the archive's last persisted per-segment agreement (see
// Collection.GroupSchemaLastAgreement).
func (a *Archive) GroupSchemaLastAgreement() (GroupSchemaLastAgreement, bool) {
	return a.c.GroupSchemaLastAgreement()
}

// Discard is Close for an archive whose files are being DELETED (a dropped archive table):
// it releases the mappings without flushing or persisting anything. See
// Collection.Discard.
func (a *Archive) Discard() error { return a.c.Discard() }

// AcquireRead registers the caller as an in-flight reader of the archive; see
// Collection.AcquireRead. Balance a true answer with ReleaseRead.
func (a *Archive) AcquireRead() bool { return a.c.AcquireRead() }

// ReleaseRead balances an AcquireRead that returned true.
func (a *Archive) ReleaseRead() { a.c.ReleaseRead() }
