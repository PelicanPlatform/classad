package collections

import (
	"sort"
	"strings"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// colSegmentBuilds counts columnar-block builds from segment records (buildColSegment). A reopen
// that reloads persisted blocks does NOT build any, so a test asserts this stays flat across an
// Open -- proving the accelerator came off disk rather than being re-transcoded.
var colSegmentBuilds atomic.Int64

// colSegment is a sealed segment's columnar accelerator: the segment's PAX row-group blocks (see
// colGroupRows) in record order, plus offs, mapping each record to its arena offset so a scan can
// read the record's live MVCC seq/sup.
//
// offs is FLAT across the blocks: block j's local record k is offs[base_j+k], where base_j is the
// sum of the preceding blocks' record counts. Every block in a segment is built under the same
// schema and hot set, so those are read from the first block.
type colSegment struct {
	// schemaOnly means this payload carries ONLY the attributes its schema covers, because the
	// records were rewritten to drop them (columnar-native, see colnative.go). Everything else
	// lives in the records, so the blocks' COLD TAIL is no longer the whole remainder of an ad:
	// an attribute missing from it is missing from the BLOCK, not from the record. Derived from
	// the segment at publish rather than stored, so the on-disk format is unchanged.
	schemaOnly bool
	blocks     []*columnarBlock
	offs       []uint32
	// groups are the group schemas' selections, one colGroupBlock per group per base block. Empty
	// when the collection carries no group schemas.
	groups []*colGroup
}

// schema returns the schema all of cs's blocks were built under, or nil if it carries none.
func (cs *colSegment) schema() *adSchema {
	if cs == nil || len(cs.blocks) == 0 {
		return nil
	}
	return cs.blocks[0].schema
}

// hotNum returns the hot numeric field set all of cs's blocks were built under.
func (cs *colSegment) hotNum() []int {
	if cs == nil || len(cs.blocks) == 0 {
		return nil
	}
	return cs.blocks[0].hotNum
}

// schemaScanState is a collection's resolved adschema columnar scan configuration.
type schemaScanState struct {
	schema *adSchema
	hot    []int
	cache  *blockCache
	// groups are the secondary schemas a block is also built against, pinned at enable time with
	// the base schema. Pinned for the same reason: every block of a segment is built against ONE
	// set, and re-deriving between segments would leave earlier ones unmatched.
	groups []*colGroup
}

// SchemaScanInfo reports the state of the per-segment columnar (adschema) accelerator, for
// diagnostics. Enabled means a numeric COUNT(*) WHERE can take the columnar fast path; HotFields
// are the demand-hot numeric columns kept uncompressed for O(1) scan; SchemaFields is the schema's
// field count; and CoveredSegments of SealedSegments carry a columnar block (the rest row-fall-back
// until a rewrite/reindex reaches them).
type SchemaScanInfo struct {
	Enabled         bool     `json:"enabled"`
	HotFields       []string `json:"hotFields,omitempty"`
	SchemaFields    int      `json:"schemaFields,omitempty"`
	SealedSegments  int      `json:"sealedSegments,omitempty"`
	CoveredSegments int      `json:"coveredSegments,omitempty"`
	// GroupSchemas is how many SECONDARY schemas are built alongside the base one, and
	// GroupSchemaFields their total field count. Zero when the feature is off, and also when it
	// is on but no group has yet kept its members together long enough to be committed to
	// storage -- which is the normal state for the first few maintenance passes, and the one
	// worth being able to tell apart from "not configured".
	GroupSchemas      int `json:"groupSchemas,omitempty"`
	GroupSchemaFields int `json:"groupSchemaFields,omitempty"`
	// Schema is the derived schema itself, field by field, in layout order. The counts above
	// say how much of the table the accelerator covers; this says what it decided the ads
	// look like -- which is what you need to judge whether the sampling recovered the shape
	// you expected, or picked up something odd.
	Schema []SchemaScanField `json:"schema,omitempty"`
}

// SchemaScanField is one attribute in the derived schema: the name it was recovered under, the
// storable kind chosen for it, and the fixed width its slot occupies. Hot marks the numeric
// columns kept uncompressed for O(1) scan.
//
// Width is bytes for an int (1/2/4/6/8, the narrowest that fits the sampled values), 8 for a
// real or a string slot, and 0 for a bool (bit-packed).
type SchemaScanField struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // bool, int, real, string
	Width    int    `json:"width,omitempty"`
	Unsigned bool   `json:"unsigned,omitempty"`
	Hot      bool   `json:"hot,omitempty"`
}

// String names a schema field's kind for display.
func (k adKind) String() string {
	switch k {
	case akBool:
		return "bool"
	case akInt:
		return "int"
	case akReal:
		return "real"
	case akString:
		return "string"
	}
	return "none"
}

// SchemaScanInfo returns the columnar accelerator's current state (see SchemaScanInfo).
func (c *Collection) SchemaScanInfo() SchemaScanInfo {
	var info SchemaScanInfo
	if st := c.schemaScan.Load(); st != nil {
		info.Enabled = true
		info.SchemaFields = len(st.schema.fields)
		info.GroupSchemas = len(st.groups)
		for _, g := range st.groups {
			info.GroupSchemaFields += len(g.schema.fields)
		}
		hot := make(map[int]bool, len(st.hot))
		for _, idx := range st.hot {
			if idx >= 0 && idx < len(st.schema.fields) {
				hot[idx] = true
				if name, ok := c.schemaFieldName(st.schema.fields[idx].id); ok {
					info.HotFields = append(info.HotFields, name)
				}
			}
		}
		for i, f := range st.schema.fields {
			name, ok := c.schemaFieldName(f.id)
			if !ok {
				// An id with no name in the intern table cannot be reported usefully; say so
				// rather than drop the field, so the count still lines up with SchemaFields.
				name = "?"
			}
			info.Schema = append(info.Schema, SchemaScanField{
				Name:     name,
				Kind:     f.kind.String(),
				Width:    f.width,
				Unsigned: f.unsigned,
				Hot:      hot[i],
			})
		}
	}
	for _, sh := range c.shards {
		sh.mu.RLock()
		act := sh.act
		for _, seg := range sh.segs {
			if seg != nil && seg != act && seg.used > 0 {
				info.SealedSegments++
				if seg.colblk.Load() != nil {
					info.CoveredSegments++
				}
			}
		}
		sh.mu.RUnlock()
	}
	return info
}

// schemaFieldName resolves a schema field's interned id to its attribute name. A collection
// with no intern table cannot have a schema (buildAdSchema reads the id-keyed form), but guard
// it rather than depend on that.
func (c *Collection) schemaFieldName(id uint32) (string, bool) {
	if c.intern == nil {
		return "", false
	}
	return c.intern.Name(id)
}

// EnableSchemaScan builds a columnar block over every currently-sealed segment (skipping the
// active append target) for the given schema and hot field set, publishes it on each segment,
// and records the state so CountQuery can auto-route matching queries. Additive and opt-in:
// with no state/block a query takes the normal row path, so a collection that never calls this
// is unaffected. Reads immutable sealed bytes only.
func (c *Collection) EnableSchemaScan(s *adSchema, hot []int) {
	if !c.installSchemaScan(s, hot) {
		return
	}
	c.coverSealedSegments(s, hot)
}

// installSchemaScan publishes the schema state without covering any segment, so a caller can
// columnarize first and let coverSealedSegments skip whatever no longer needs a sidecar block.
// Reports false if the collection cannot take a columnar accelerator at all.
func (c *Collection) installSchemaScan(s *adSchema, hot []int) bool {
	if c.sealer != nil {
		// A columnar block stores attribute values in the clear; over an encryption-at-rest
		// collection it would materialize sealed values as plaintext on disk. The two are
		// mutually exclusive -- an encrypted collection always takes the row path.
		return false
	}
	st := c.schemaScan.Load()
	if st == nil || st.schema != s {
		bc, err := newBlockCache(256 << 20) // ~256 MiB of decompressed blocks
		if err != nil {
			return false
		}
		st = &schemaScanState{schema: s, hot: hot, cache: bc, groups: c.groupSchemasFor(s)}
		c.schemaScan.Store(st)
	}
	return true
}

// coverSealedSegments builds a sidecar columnar block for each sealed segment that has none.
//
// A columnarized segment already publishes its own block from inside the segment, so pinSealed's
// filter skips it and colBlobForSeg would decline anyway -- which is why columnarizing runs BEFORE
// this rather than after. Covering first would build, compress and write a sidecar block only for
// the rewrite to make it redundant.
func (c *Collection) coverSealedSegments(s *adSchema, hot []int) {
	for _, sh := range c.shards {
		// PINNED: transcoding reads the segment's bytes off the shard lock, and a concurrent
		// Compact/merge/rotation would otherwise munmap them mid-read. The pin defers the reap.
		segs := sh.pinSealed(func(seg *segment) bool { return seg.colblk.Load() == nil })
		// Unpin each segment as soon as its block is built, rather than holding every pin until this
		// returns: a pin DEFERS a concurrent compaction's reap, and a whole-archive transcode takes
		// long enough that holding them all would keep every replaced segment's file on disk for the
		// duration. The deferred sweep is a safety net for a panic mid-loop.
		done := 0
		defer func() { unpinAll(segs[done:]) }()
		for i, seg := range segs {
			if cs := c.buildColSegment(seg, s, hot); cs != nil {
				seg.colblk.Store(cs)
				c.persistColSeg(sh, seg) // write it to the sidecar so a reopen reloads it (no-op for RAM)
			}
			seg.unpin()
			done = i + 1
		}
	}
}

// persistColSeg writes seg's freshly-built columnar block into its sidecar container (preserving
// the existing attribute + key sections) and remaps it, so a block built at EnableSchemaScan time
// over an ALREADY-sealed segment survives a reopen instead of being re-transcoded. Without it a
// segment sealed BEFORE schema-scan was enabled would keep an in-RAM-only block and row-fall-back
// after reopen until a reindex touched it. No-op for an in-memory (RAM) segment or one whose
// sidecar cannot be read. Holds reindexMu to serialize the sidecar rewrite against a concurrent
// Reindex, which rewrites the same container.
func (c *Collection) persistColSeg(sh *shard, seg *segment) {
	path := snapshotPath(seg)
	if path == "" {
		return // in-memory segment: the block stays in RAM, nothing to persist
	}
	colBlob := c.colBlobPreserve(seg)
	if colBlob == nil {
		return
	}
	c.reindexMu.Lock()
	defer c.reindexMu.Unlock()
	// Copy the current attribute + key sections out of the live mapping before the rewrite.
	data, closer, err := mapFile(path)
	if err != nil {
		return
	}
	attr, key, _, zone, ok := splitSegmentSidecar(data)
	if !ok {
		_ = closer()
		return
	}
	container := buildSegmentSidecar(append([]byte(nil), attr...), append([]byte(nil), key...), colBlob,
		append([]byte(nil), zone...), seg.used, seg.dictRecOff())
	_ = closer()
	c.installSidecar(sh, seg, path, container)
}

// regionCodec is the codec a columnar block's REGIONS are compressed with: ZSTD with NO trained
// dictionary, created once per collection and cached.
//
// WHY NO DICTIONARY. The trained dictionary is an ARENA optimization and does not pay for itself on
// the block regions. Measured on real OSPool slot ads (dictcpu_test.go, decompression throughput):
// the dictionary costs 20% on the cold-numeric region for ZERO size benefit (it compresses that
// region 0.0% to +0.7% WORSE), 24% on the strings region for -3.5%, and 25% on the cold tail for
// -5.3%. Since row groups bounded the blocks, regions are only about a fifth of stored bytes, so
// dropping the dictionary across all three costs under 1% of total stored size and returns roughly a
// quarter of the decompression throughput on a path taken by every cold-column scan, escaped-value
// read and full-ad reconstruct.
//
// WHY NOT the segment's codec (what it used to be), and why not dictionary id 0 either. A block's
// region codec has to be recoverable at reload time without being recorded per block, so it must
// depend only on state that never changes for the life of the collection. The segment's codec fails
// that -- it is whatever dictionary generation the segment was written under, and a reseal changes
// it. currentCodec fails it too, since a retrain swaps identity for ZSTD mid-life, which would leave
// blocks written earlier in the process undecodable. Dictionary id 0 is stable, but on a store whose
// base codec is identity (the historical default that db.chooseBaseCodec preserves for existing
// stores) it would leave the regions UNCOMPRESSED, which for such a store is a real size regression
// against today, where a retrain gives them dictionary compression.
//
// A dictionary-less ZSTD codec satisfies all of it: identical for every collection, never affected by
// a retrain or a reseal, and compressing. Note that on an identity-base store this means the block
// regions are compressed while the records are not. That is fine -- a block is derived state with its
// own version stamp, and nothing assumes it shares the record encoding.
func (c *Collection) regionCodec() Codec {
	if h := c.regionCodecCache.Load(); h != nil {
		return h.c
	}
	cd, err := NewZSTDCodec(nil)
	if err != nil {
		// Cannot happen for a nil dictionary; degrade to the base codec rather than fail a seal.
		// Cached so the choice is still stable for this process.
		cd = c.currentCodec()
	}
	c.regionCodecCache.CompareAndSwap(nil, &codecHolder{cd})
	return c.regionCodecCache.Load().c
}

// colBuildStallHook is a test seam: called at the start of each segment's transcode, while the
// caller holds that segment pinned. It lets a test force a compaction (which retires and reaps
// sealed segments) at exactly the moment a build is reading one, which is the production race that
// crashed -- an admin Maintain transcoding segments while the background maintenance goroutine
// compacted them. Production leaves it nil.
var colBuildStallHook func()

// buildColSegment transcodes a sealed segment into its columnar accelerator block under schema s
// and hot tier hot (resolving an interned segment's local ids on the way). Returns nil if the
// block cannot be built. Off the write lock (reads immutable sealed bytes).
func (c *Collection) buildColSegment(seg *segment, s *adSchema, hot []int) *colSegment {
	if seg.columnarized() || seg.colDamaged.Load() {
		// The segment already holds its own columnar payload, and its records are remnants -- so
		// building a block from them would produce a block covering only the attributes the schema
		// does NOT carry, which is the opposite of a schema block. publishColNative installs the
		// in-segment copy as the read-path block instead.
		return nil
	}
	colSegmentBuilds.Add(1)
	if colBuildStallHook != nil {
		colBuildStallHook()
	}
	d := seg.dict.Load() // interned segment -> resolve its local ids during transcode
	var groups []*colGroup
	if st := c.schemaScan.Load(); st != nil {
		groups = st.groups
	}
	blocks, gblocks, offs := buildColumnarFromSegmentGrouped(seg.data, seg.used, seg.codec,
		c.regionCodec(), s, hot, groups, defaultColGrouping(),
		func(dst, w []byte) ([]byte, bool) { return c.recordToInternedDict(d, dst, w) })
	if len(blocks) == 0 {
		return nil
	}
	cs := &colSegment{blocks: blocks, offs: offs}
	// Re-key the pinned groups onto this segment's selections: the schema and members are shared,
	// the per-block bitmaps are not.
	for gi, g := range groups {
		sel := &colGroup{schema: g.schema, ids: g.ids}
		for bi := range blocks {
			if gi < len(gblocks[bi]) {
				sel.blocks = append(sel.blocks, gblocks[bi][gi])
			}
		}
		if len(sel.blocks) == len(blocks) {
			cs.groups = append(cs.groups, sel)
		}
	}
	return cs
}

// colBlobForSeg builds and marshals seg's columnar block under the collection's CURRENT schema-scan
// schema, for embedding in the segment's sidecar container so the block persists. Returns nil when
// schema-scan is not enabled (nothing to persist) -- and never runs for an encryption-at-rest
// collection, since EnableSchemaScan refuses those (a columnar block would store values in the
// clear). Written at seal (sealSegmentIndex) and rebuilt on reindex (reindexSealedFile).
func (c *Collection) colBlobForSeg(seg *segment) []byte {
	if colNativeBlobRedundant(seg) {
		// The segment carries the authoritative copy; a sidecar copy would be the duplication
		// this format exists to remove.
		return nil
	}
	st := c.schemaScan.Load()
	if st == nil {
		return nil
	}
	cs := c.buildColSegment(seg, st.schema, st.hot)
	if cs == nil {
		return nil
	}
	body := marshalColSegment(cs, c.intern.Name)
	if body == nil {
		return nil
	}
	return wrapColSection(body, seg.used)
}

// colBlobPreserve returns the sidecar columnar section for seg on a REINDEX (sidecar rewrite),
// preserving the segment's existing block by re-marshaling it -- a reindex changes the attribute
// index spec, not the schema the block was built under, so there is no need to re-transcode the
// records. Falls back to building the block when the segment has none yet but schema-scan is
// enabled (an index added just after enabling schema-scan). nil when there is nothing to persist.
func (c *Collection) colBlobPreserve(seg *segment) []byte {
	cs := seg.colblk.Load()
	if cs == nil {
		return c.colBlobForSeg(seg)
	}
	body := marshalColSegment(cs, c.intern.Name)
	if body == nil {
		return nil
	}
	return wrapColSection(body, seg.used)
}

// adoptPersistedSchemaScan enables schema-scan on reopen from persisted columnar blocks: if any
// sealed segment recovered a block (publishSidecar unmarshaled it from the sidecar's col section),
// the collection adopts one block's schema + hot tier as the current schema-scan state, so
// CountQuery routes and future seals build under it. Existing segments keep their OWN per-segment
// schemas -- the scan resolves the field per block -- so this only seeds the "current" schema; it
// never rewrites anything. A no-op when schema-scan is already enabled or no block was persisted
// (so an encryption-at-rest collection, which never persists a block, never enables it here).
func (c *Collection) adoptPersistedSchemaScan() {
	if c.schemaScan.Load() != nil {
		return
	}
	var adopt *colSegment
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil {
				if cs := seg.colblk.Load(); cs != nil && cs.schema() != nil {
					adopt = cs // last recovered segment wins; any segment's schema is a valid seed
				}
			}
		}
		sh.mu.RUnlock()
	}
	schema, hot := (*adSchema)(nil), []int(nil)
	if adopt != nil {
		schema, hot = adopt.schema(), adopt.hotNum()
	} else if seed := c.schemaSeed.Load(); seed != nil {
		// No block loaded, but a sidecar's SCHEMA was still readable -- its block format was from an older
		// version. Seeding from it keeps the accelerator enabled so the blocks rebuild under the schema the
		// table already had, instead of the accelerator switching off until something re-derives one.
		//
		// Only the schema is adopted. Nothing is attached to the segment: a colSegment carrying a schema and
		// no blocks would read as "accelerator present" and then iterate zero blocks, silently skipping every
		// record in the segment. Absent blocks must look absent.
		schema, hot = seed.schema, seed.hot
	}
	if schema == nil {
		return
	}
	bc, err := newBlockCache(256 << 20)
	if err != nil {
		return
	}
	c.schemaScan.Store(&schemaScanState{schema: schema, hot: hot, cache: bc})
}

// BuildAndEnableSchemaScan samples the collection, builds an adschema, chooses the hot numeric
// tier as the top-hotTopN int/real fields by accumulated read demand (c.demand -- the same
// signal RefreshHotSet uses; the schema's hot tier IS the hot set), and enables the columnar
// scan over the sealed segments. Returns false if there is nothing to sample. Re-callable to
// pick up newly-sealed segments (existing blocks are kept).
func (c *Collection) BuildAndEnableSchemaScan(sampleMax, hotTopN int) bool {
	if c.sealer != nil {
		return false // see EnableSchemaScan: incompatible with encryption at rest
	}
	// Already enabled: keep the stable schema and hot set chosen at first enable, and just
	// extend coverage to any segments sealed since (their blocks are built against this same
	// schema, so nothing is orphaned). Rebuilding a fresh schema here would give every new
	// block a different schema pointer, leaving earlier segments' blocks unmatched and
	// silently demoting them to the brute-scan fallback. Re-schema-ing (with a full block
	// rebuild) is a separate, heavier operation, not part of a routine maintenance refresh.
	if st := c.schemaScan.Load(); st != nil {
		c.schemaScanPass(st.schema, st.hot)
		return true
	}
	s, hot, ok := c.deriveSchema(sampleMax, hotTopN)
	if !ok {
		return false
	}
	c.schemaScanPass(s, hot)
	return true
}

// schemaScanPass is what a routine schema review does: publish the schema, move each sealed
// segment's schema'd attributes INTO the segment (columnar-native, bounded by
// ColumnarSegmentBudget), then build sidecar blocks for whatever the rewrite did not take.
//
// The order is the point. Columnarizing makes a sidecar block redundant, so covering first
// would compress and write one only to strand it; and columnarizing needs the schema, so it
// cannot go before the state is published. A collection with the rewrite disabled sees exactly
// the old behaviour, because the middle step returns 0 and covering does all the work.
func (c *Collection) schemaScanPass(s *adSchema, hot []int) {
	if !c.installSchemaScan(s, hot) {
		return
	}
	c.ColumnarizeSealed()
	c.coverSealedSegments(s, hot)
}

// deriveSchema samples the collection and derives a schema plus its hot numeric tier, without
// touching the current state. Shared by BuildAndEnableSchemaScan (first enable) and
// ReschemaScan (a deliberate rebuild). Returns false if there is nothing to sample or no field
// survived the presence threshold.
func (c *Collection) deriveSchema(sampleMax, hotTopN int) (*adSchema, []int, bool) {
	samples := c.CollectSamplesRecentN(sampleMax)
	if len(samples) == 0 {
		return nil, nil, false
	}
	// buildAdSchema reads the id-keyed wire form; a persistent (inline) collection's records
	// store names, so canonicalize them to interned wire first (else the schema is empty and
	// the accelerator silently never enables).
	if c.inline {
		interned := make([][]byte, 0, len(samples))
		for _, w := range samples {
			if iw, ok := c.recordToInterned(nil, w); ok {
				interned = append(interned, iw)
			}
		}
		samples = interned
		if len(samples) == 0 {
			return nil, nil, false
		}
	}
	s := buildAdSchema(samples, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	if len(s.fields) == 0 {
		return nil, nil, false
	}
	type fieldDemand struct {
		idx   int
		reads int64
	}
	var nums []fieldDemand
	for i := range s.fields {
		if k := s.fields[i].kind; k != akInt && k != akReal {
			continue
		}
		var reads int64
		if name, ok := c.intern.Name(s.fields[i].id); ok {
			if v, ok := c.demand.m.Load(strings.ToLower(name)); ok {
				reads = v.(*demandCounts).reads.Load()
			}
		}
		nums = append(nums, fieldDemand{i, reads})
	}
	sort.Slice(nums, func(a, b int) bool {
		if nums[a].reads != nums[b].reads {
			return nums[a].reads > nums[b].reads
		}
		return nums[a].idx < nums[b].idx
	})
	hot := make([]int, 0, hotTopN)
	for i := 0; i < len(nums) && i < hotTopN; i++ {
		hot = append(hot, nums[i].idx)
	}
	// Re-fit the hot int columns to (near-)zero escapes: an escaped value on a queried column
	// forces the block's cold-stream decompression + a cold-tail walk, so a hot field wants a
	// width that covers essentially all its values. Cold fields keep the tight base width. See
	// refitHotIntWidths.
	s, hot = refitHotIntWidths(s, samples, hot, hotIntFit)
	return s, hot, true
}

// hotIntFit is the width fit percentile for HOT (demand-queried) int columns: high enough to
// eliminate escapes on real data, but below 1.0 so a single sampled outlier does not widen the
// column for every record (0.999 and 1.0 pick identical widths on the OSPool corpus, but 0.999
// caps an outlier's blast radius at 0.1%). Cold columns keep the base Fit (0.95).
const hotIntFit = 0.999

// refitHotIntWidths widens each hot INT field to cover ~all its sampled values (hotFit), so a
// queried column rarely escapes to the cold path. It re-collects the hot fields' sampled int
// values (buildAdSchema discards them), re-chooses their widths at hotFit, and re-lays-out the
// schema; cold fields and non-int hot fields are untouched. Returns the re-fitted schema and the
// hot field indices remapped into its (re-sorted) layout. A no-op when no hot int field exists.
func refitHotIntWidths(s *adSchema, samples [][]byte, hot []int, hotFit float64) (*adSchema, []int) {
	hotByID := make(map[uint32]bool, len(hot)) // all hot fields (int+real), for index remap
	hotIntIDs := map[uint32]bool{}             // hot int fields, for width re-fit
	for _, i := range hot {
		hotByID[s.fields[i].id] = true
		if s.fields[i].kind == akInt {
			hotIntIDs[s.fields[i].id] = true
		}
	}
	if len(hotIntIDs) == 0 {
		return s, hot
	}
	vals := map[uint32][]int64{}
	for _, w := range samples {
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			if hotIntIDs[id] {
				if k, lit := nodeKind(node); k == akInt {
					vals[id] = append(vals[id], lit.Int)
				}
			}
			return true
		})
	}
	fs := make([]adField, len(s.fields))
	copy(fs, s.fields)
	for i := range fs {
		if hotIntIDs[fs[i].id] {
			if vs := vals[fs[i].id]; len(vs) > 0 {
				fs[i].width, fs[i].unsigned = chooseIntWidth(vs, hotFit)
			}
		}
	}
	s2 := layoutSchema(fs)
	var hot2 []int
	for i := range s2.fields {
		if hotByID[s2.fields[i].id] {
			hot2 = append(hot2, i)
		}
	}
	return s2, hot2
}

// CountConstraint parses a constraint string and, if it is columnar-eligible, counts via the
// columnar scan (see CountQuery). ok=false ⇒ use the normal count path.
func (c *Collection) CountConstraint(constraint string) (int, bool) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return 0, false
	}
	return c.CountQuery(q)
}

// CountQuery counts the records matching q using the columnar scan, when q is exactly (Native)
// one or more numeric comparisons on a single INT schema field -- the common count-where. It
// returns (count, true) on that fast path, or (0, false) to signal the caller to use the normal
// scan (schema-scan not enabled, or the predicate is not columnar-eligible). Correctness: the
// per-record numeric comparison matches the store's evaluation of `field OP number`; a value
// escaped out of the hot column is read from the cold tail, and Native() rules out any residual
// program the columnar path would miss.
func (c *Collection) CountQuery(q *vm.Query) (int, bool) {
	st := c.schemaScan.Load()
	if st == nil {
		return 0, false
	}
	if c.intern == nil {
		return 0, false
	}
	// The predicate analysis is shared with NumStatsQuery (see numPredOnField). fieldID is only
	// the routing decision -- the attr is a numeric field in the CURRENT schema; the scan
	// resolves the field per block against each block's own schema.
	// Multi-field first: it subsumes the single-field case (one combined predicate) and additionally
	// serves conjunctions across several columns, which used to fall off the cliff into a full row
	// scan even though the shape is still `Attr OP literal`, just repeated.
	if preds, ok := c.numPredsOnFields(q, st.schema); ok {
		names := make([]string, 0, len(preds))
		for _, p := range preds {
			if name, ok := c.schemaFieldName(p.fieldID); ok {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			c.demand.recordReads(names)
		}
		return c.schemaScanCountMulti(preds, st.cache), true
	}
	fieldID, eval, ok := c.numPredOnField(q, st.schema)
	if !ok {
		// Not a scalar comparison. A lone presence probe (`attr is undefined`) is still
		// columnar-servable straight from the escape bitmap -- see PresenceCountQuery.
		if n, served := c.PresenceCountQuery(q); served {
			return n, true
		}
		// Last tier: evaluate the query itself against the columns, a COLUMN at a time where the
		// expression allows it (VectorEvalCount) and per record where it does not. It serves any
		// NATIVE query -- string comparisons, attribute-to-attribute, arithmetic, disjunctions.
		//
		// VectorEvalCount rather than ColumnarEvalCount because it cannot be worse: a block it cannot
		// vectorize is served by exactly the per-record path ColumnarEvalCount would have used, and a
		// block that is mostly superseded is too. Where it does vectorize -- the arithmetic,
		// disjunction and boolean shapes the hand-written scans above refuse -- it is ~10x, which is
		// the hand-written scans' speed generalized to expressions nobody hand-wrote.
		//
		// Skipped when an index could prune the row path instead: this evaluates EVERY visible record,
		// so against a selective indexed constraint the scan wins, and a routing that ignored that was
		// measured 2.9x slower on exactly such a query.
		// Skipped only when an index could prune the row path instead.
		if !c.indexCanPrune(q) {
			return c.VectorEvalCount(q)
		}
		return 0, false
	}
	// As NumStatsQuery: record the predicate's attribute so the hot tier keeps tracking the
	// columns queries actually read, instead of freezing once the accelerator starts serving them.
	if name, ok := c.schemaFieldName(fieldID); ok {
		c.demand.recordReads([]string{name})
	}
	return c.schemaScanCount(fieldID, st.cache, eval), true
}

func numCmp(op string, t float64) (func(float64) bool, bool) {
	switch op {
	case "<":
		return func(v float64) bool { return v < t }, true
	case "<=":
		return func(v float64) bool { return v <= t }, true
	case ">":
		return func(v float64) bool { return v > t }, true
	case ">=":
		return func(v float64) bool { return v >= t }, true
	case "==":
		return func(v float64) bool { return v == t }, true
	case "!=":
		return func(v float64) bool { return v != t }, true
	}
	return nil, false
}

// schemaScanCount counts the records whose numeric attribute fieldID satisfies eval, over the
// shared columnar value scan -- see scanNumValues for the per-block schema resolution, the MVCC
// visibility rule and the row fallback. A record whose value is absent is skipped, not counted.
func (c *Collection) schemaScanCount(fieldID uint32, bc *blockCache, eval func(float64) bool) int {
	count := 0
	c.scanNumValues(fieldID, bc, func(nv colVal) {
		if eval(nv.f) {
			count++
		}
	})
	return count
}

// schemaScanIntCount is the int-typed convenience wrapper (used by tests/benchmarks).
func (c *Collection) schemaScanIntCount(s *adSchema, fieldIdx int, match func(int64) bool) int {
	bc := (*blockCache)(nil)
	if st := c.schemaScan.Load(); st != nil {
		bc = st.cache
	}
	return c.schemaScanCount(s.fields[fieldIdx].id, bc, func(f float64) bool { return match(int64(f)) })
}

// The row-scan fallback now lives with the shared value scan, as bruteNumValues in colagg.go.
