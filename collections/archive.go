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
type zoneRange struct {
	Min, Max float64
}

// Retention bounds how much history an append-only store keeps. Rotate drops the oldest
// whole segments until every set bound is met. A zero field means "no bound on that
// axis". Shared with Collection (Options.Retention / Collection.Rotate).
type Retention struct {
	MaxSegments int   // keep at most this many sealed segments
	MaxBytes    int64 // keep at most this many bytes of sealed segment files
	// MaxAgeAttr / MaxAge: drop a segment whose max value of the given numeric attr
	// (e.g. "CompletionDate", unix seconds) is older than Now-MaxAge. Now is supplied
	// per Rotate call so the store needs no clock of its own. MaxAgeAttr must be a
	// ZoneAttr so its per-segment max is available.
	MaxAgeAttr string
	MaxAge     float64
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
		AppendOnly:       true, // pure append log: no supersession, no compaction, no key dir
		ReverseScan:      true, // newest-first Query, matching condor_history order
		Dir:              opts.Dir,
		SegmentSize:      segSize,
		Codec:            opts.Codec,
		HotAttrs:         opts.HotAttrs,
		CategoricalAttrs: opts.CategoricalAttrs,
		ValueAttrs:       opts.ValueAttrs, // numeric ValueAttrs are auto-added to the zone maps
		ZoneAttrs:        opts.ZoneAttrs,
		Retention:        opts.Retention,
		WatchHistory:     archiveWatchCap, // enable the append-stream watch
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
// sealed with its index and zone maps. Safe for one writer concurrent with queries.
func (a *Archive) Append(ad *classad.ClassAd) error {
	// An append log has no key; a nil key routes to the single append-only shard and
	// stores a zero-length key. Duplicate appends are all retained.
	if err := a.c.Put(nil, ad); err != nil {
		return err
	}
	// If this append sealed a segment, build its sidecar index now (off the write lock),
	// so categorical/value queries on just-appended history are accelerated immediately
	// instead of scanning until the next periodic reindex. Reindex builds only the missing
	// sidecar (all older segments are already sealed) and is serialized against the periodic
	// reindex. Skipped when no per-segment indexes are configured (nothing to build).
	if a.c.hasSegmentIndexes() {
		if seal := a.c.appendSealSeq(); seal != a.lastSeal {
			a.lastSeal = seal
			a.c.Reindex()
		}
	}
	return nil
}

// Flush makes prior appends durable and queryable. The Collection keeps appended records
// queryable in place and mmap-durable across a process crash without an explicit seal, so
// this is a no-op retained for API compatibility.
func (a *Archive) Flush() error { return nil }

// Rotate drops whole oldest segments until the archive is back within its Retention
// bounds, returning the number dropped. now is the caller's wall clock (unix seconds) for
// age-based retention.
func (a *Archive) Rotate(now float64) (int, error) { return a.c.Rotate(now) }

// Count returns the number of records currently retained. Rotation reduces it in
// whole-segment steps.
func (a *Archive) Count() int { return a.c.Len() }

// Truncate drops every record, resetting the archive to empty in place: all segments are
// unmapped and their data + sidecar-index files unlinked (via the backing Collection's
// Truncate). Segment counters keep advancing, so a fresh Append starts a new segment. This
// is the destructive reset a from-scratch history re-sync uses -- empty the store, then
// re-ingest from the source. Retention/index/zone-map configuration is preserved. Safe
// against concurrent queries (a scan holding a pin reads its old data until it finishes);
// callers must serialize Truncate against the single Append writer.
func (a *Archive) Truncate() { a.c.Truncate() }

// Close flushes and unmaps the archive. It must not be used afterward.
func (a *Archive) Close() error { return a.c.Close() }

// Query returns an iterator over the ads matching q, newest first.
func (a *Archive) Query(q *vm.Query) iter.Seq[*classad.ClassAd] { return a.QueryLimit(q, 0) }

// QueryLimit returns an iterator over the ads matching q, newest first, stopping after
// limit results (limit <= 0 ⇒ unlimited). Because the backing Collection scans
// newest-first, stopping early yields the most recent limit matches — a pushed-down LIMIT.
func (a *Archive) QueryLimit(q *vm.Query, limit int) iter.Seq[*classad.ClassAd] {
	return func(yield func(*classad.ClassAd) bool) {
		n := 0
		for ad := range a.c.Query(q) {
			if !yield(ad) {
				return
			}
			if limit > 0 {
				if n++; n >= limit {
					return
				}
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

func (a *Archive) QueryProject(q *vm.Query, attrs []string) iter.Seq[[]classad.Value] {
	return a.c.QueryProject(q, attrs)
}

// Watch streams the archive as change data: a full replay of retained records (oldest
// first) then live appends, resumable from an opaque cursor. A cursor older than what
// rotation still retains yields a WatchReset and resumes from the current floor. See
// Collection.Watch and docs/WATCH.md.
func (a *Archive) Watch(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error) {
	return a.c.Watch(ctx, cursor)
}

// SidecarSizes reports the archive's sealed-segment sidecar index bytes (mmap-backed,
// evictable page cache), broken out by structure. An operator diagnostic.
func (a *Archive) SidecarSizes() SidecarSizes { return a.c.SidecarSizes() }

// RetrainDict trains a fresh ZSTD dictionary from up to sampleMax records and recompresses
// every segment in place under it (an append-only reseal that preserves order), returning
// the new dictionary's size in bytes. This is how an archive's compression adapts to the
// data it has accumulated.
func (a *Archive) RetrainDict(sampleMax int) (int, error) { return a.c.RetrainDict(sampleMax) }

// Rewrite recompresses and re-encodes every segment in place under the current codec and
// hot set (e.g. after a hot-set change), preserving order, and returns the number of
// records rewritten.
func (a *Archive) Rewrite() int { return a.c.Rewrite() }

// AddIndex adds per-segment indexes on the named categorical and/or value attributes.
// Returns false if the index set was unchanged.
//
// Reach: it takes effect immediately for segments not yet sealed to an immutable sidecar,
// and for every segment sealed from now on. Segments ALREADY sealed keep the index they
// were sealed with -- Reindex cannot rewrite an immutable sidecar -- so an existing archive
// only picks the new index up everywhere after a Rewrite, which re-encodes the whole store.
// StaleIndexSegments reports how many segments are still on the old configuration.
func (a *Archive) AddIndex(categorical, value []string) bool {
	if !a.c.AddIndex(categorical, value) {
		return false
	}
	a.c.Reindex()
	return true
}

// DropIndex removes the named per-segment indexes. Returns false if none matched.
func (a *Archive) DropIndex(names ...string) bool { return a.c.DropIndex(names...) }

// Reindex rebuilds the per-segment indexes over all segments (e.g. to pick up segments
// sealed since the last build).
func (a *Archive) Reindex() { a.c.Reindex() }

// SetRetention updates the retention bounds at runtime; the next Rotate enforces them.
func (a *Archive) SetRetention(r Retention) { a.c.SetRetention(r) }

// Retention returns the archive's current retention bounds.
func (a *Archive) Retention() Retention { return a.c.Retention() }

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
// built under an older configuration, and how many are sealed in total. See AddIndex.
func (a *Archive) StaleIndexSegments() (stale, sealed int) { return a.c.StaleIndexSegments() }

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
		if !zoneMayMatch(z, p.Op, vals) {
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
	default:
		return true // != and anything else: not prunable
	}
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
