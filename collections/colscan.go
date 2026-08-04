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

// colSegment is a sealed segment's columnar accelerator: the PAX block plus offs, mapping each
// block record k to its arena offset so a scan can read the record's live MVCC seq/sup.
type colSegment struct {
	block *columnarBlock
	offs  []uint32
}

// schemaScanState is a collection's resolved adschema columnar scan configuration.
type schemaScanState struct {
	schema *adSchema
	hot    []int
	cache  *blockCache
}

// EnableSchemaScan builds a columnar block over every currently-sealed segment (skipping the
// active append target) for the given schema and hot field set, publishes it on each segment,
// and records the state so CountQuery can auto-route matching queries. Additive and opt-in:
// with no state/block a query takes the normal row path, so a collection that never calls this
// is unaffected. Reads immutable sealed bytes only.
func (c *Collection) EnableSchemaScan(s *adSchema, hot []int) {
	if c.sealer != nil {
		// A columnar block stores attribute values in the clear; over an encryption-at-rest
		// collection it would materialize sealed values as plaintext on disk. The two are
		// mutually exclusive -- an encrypted collection always takes the row path.
		return
	}
	st := c.schemaScan.Load()
	if st == nil || st.schema != s {
		bc, err := newBlockCache(256 << 20) // ~256 MiB of decompressed blocks
		if err != nil {
			return
		}
		st = &schemaScanState{schema: s, hot: hot, cache: bc}
		c.schemaScan.Store(st)
	}
	for _, sh := range c.shards {
		sh.mu.RLock()
		act := sh.act
		segs := make([]*segment, 0, len(sh.segs))
		for _, seg := range sh.segs {
			if seg != nil && seg != act && seg.used > 0 && seg.colblk.Load() == nil {
				segs = append(segs, seg) // sealed ⇒ data/used immutable, safe to read off-lock
			}
		}
		sh.mu.RUnlock()
		for _, seg := range segs {
			if cs := c.buildColSegment(seg, s, hot); cs != nil {
				seg.colblk.Store(cs)
			}
		}
	}
}

// buildColSegment transcodes a sealed segment into its columnar accelerator block under schema s
// and hot tier hot (resolving an interned segment's local ids on the way). Returns nil if the
// block cannot be built. Off the write lock (reads immutable sealed bytes).
func (c *Collection) buildColSegment(seg *segment, s *adSchema, hot []int) *colSegment {
	colSegmentBuilds.Add(1)
	d := seg.dict.Load() // interned segment -> resolve its local ids during transcode
	blk, offs := buildColumnarFromSegment(seg.data, seg.used, seg.codec, s, hot,
		func(dst, w []byte) ([]byte, bool) { return c.recordToInternedDict(d, dst, w) })
	if blk == nil {
		return nil
	}
	return &colSegment{block: blk, offs: offs}
}

// colBlobForSeg builds and marshals seg's columnar block under the collection's CURRENT schema-scan
// schema, for embedding in the segment's sidecar container so the block persists. Returns nil when
// schema-scan is not enabled (nothing to persist) -- and never runs for an encryption-at-rest
// collection, since EnableSchemaScan refuses those (a columnar block would store values in the
// clear). Written at seal (sealSegmentIndex) and rebuilt on reindex (reindexSealedFile).
func (c *Collection) colBlobForSeg(seg *segment) []byte {
	st := c.schemaScan.Load()
	if st == nil {
		return nil
	}
	cs := c.buildColSegment(seg, st.schema, st.hot)
	if cs == nil {
		return nil
	}
	return wrapColSection(marshalColSegment(cs, c.intern.Name), seg.used)
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
				if cs := seg.colblk.Load(); cs != nil {
					adopt = cs // last recovered block wins; any block's schema is a valid seed
				}
			}
		}
		sh.mu.RUnlock()
	}
	if adopt == nil {
		return
	}
	bc, err := newBlockCache(256 << 20)
	if err != nil {
		return
	}
	c.schemaScan.Store(&schemaScanState{schema: adopt.block.schema, hot: adopt.block.hotNum, cache: bc})
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
		c.EnableSchemaScan(st.schema, st.hot)
		return true
	}
	samples := c.CollectSamples(sampleMax)
	if len(samples) == 0 {
		return false
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
			return false
		}
	}
	s := buildAdSchema(samples, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	if len(s.fields) == 0 {
		return false
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
	c.EnableSchemaScan(s, hot)
	return true
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
	s := st.schema
	probes := q.Probes()
	if !q.Native() || len(probes) == 0 {
		return 0, false
	}
	fieldIdx := -1
	var fieldID uint32
	var cmps []func(float64) bool
	for _, p := range probes {
		id, ok := c.intern.LookupID(p.Attr)
		if !ok {
			return 0, false
		}
		idx, ok := s.byID[id]
		if !ok || s.fields[idx].kind != akInt {
			return 0, false // only single-field int comparisons route
		}
		if fieldIdx == -1 {
			fieldIdx, fieldID = idx, id
		} else if idx != fieldIdx {
			return 0, false // more than one field: not this fast path
		}
		up, ok := valUsable(id, p)
		if !ok || len(up.fvals) != 1 {
			return 0, false // present/absent/in/isnt or non-numeric: not a scalar comparison
		}
		cmp, ok := numCmp(up.op, up.fvals[0])
		if !ok {
			return 0, false
		}
		cmps = append(cmps, cmp)
	}
	eval := func(v float64) bool {
		for _, cmp := range cmps {
			if !cmp(v) {
				return false
			}
		}
		return true
	}
	// fieldIdx above is only the routing decision (the attr is an int field in the current
	// schema); the scan resolves the field per block against each block's own schema.
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

// schemaScanCount counts records whose numeric attribute fieldID is present, live, and satisfies
// eval -- using each sealed segment's columnar block (hot column: no decode; cold column: one
// cached decode) and each window's live MVCC visibility. The field is resolved PER BLOCK against
// that block's OWN schema (byID over the shared intern id): a segment sealed under a schema that
// includes fieldID as an int is columnar-scanned; one whose schema lacks it (or types it
// differently) -- including the active segment, which has no block -- row-scans instead. This is
// what lets a heterogeneous set of per-segment schemas coexist: a schema change never forces
// rewriting existing segments; they simply row-fall-back for attributes absent from their schema.
func (c *Collection) schemaScanCount(fieldID uint32, bc *blockCache, eval func(float64) bool) int {
	// The brute fallback reads records straight from the arena, so it must honor the collection's
	// wire mode: look the field up by inline name on a persistent store, by interned id in memory.
	lookup := func(a wire.Ad) ([]byte, bool) { return a.Lookup(fieldID) }
	if c.inline {
		if name, ok := c.intern.Name(fieldID); ok {
			lookup = func(a wire.Ad) ([]byte, bool) { return a.LookupByName(name) }
		}
	}
	count := 0
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			bs := (*adSchema)(nil)
			bidx := -1
			if cs != nil {
				if idx, ok := cs.block.schema.byID[fieldID]; ok && cs.block.schema.fields[idx].kind == akInt {
					bs, bidx = cs.block.schema, idx
				}
			}
			if bidx < 0 {
				count += bruteNumCount(w, s0, lookup, eval) // no block, or field absent/non-int in this segment's schema
				continue
			}
			cs.block.scanInt(bidx, bc, func(k int, present bool, v int64) {
				o := cs.offs[k]
				if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
					return // not visible at this snapshot
				}
				if present {
					if eval(float64(v)) {
						count++
					}
					return
				}
				if rec, err := cs.block.reconstruct(k, bc); err == nil { // escaped: cold tail
					if f, ok := numFieldOf(bs, rec, fieldID); ok && eval(f) {
						count++
					}
				}
			})
		}
		releaseWindows(wins)
	}
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

// bruteNumCount is the row-scan fallback: walk a window's visible records, read the numeric
// field from the wire ad, and count matches.
func bruteNumCount(w segWindow, s0 uint64, lookup func(wire.Ad) ([]byte, bool), eval func(float64) bool) int {
	count := 0
	var buf []byte
	for off := 0; off < w.used; {
		o := uint32(off)
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(w.data, o) && recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
			if ww, err := w.codec.Decompress(buf[:0], recAd(w.data, o)); err == nil {
				buf = ww
				if node, ok := lookup(wire.Ad(ww)); ok {
					if f, ok := literalFloat(node); ok && eval(f) {
						count++
					}
				}
			}
		}
		off += int(total)
	}
	return count
}

// numFieldOf returns the numeric value (int or real) of fieldID in a reconstructed schema
// record, if present as a number (missing or non-numeric ⇒ false).
func numFieldOf(s *adSchema, rec []byte, fieldID uint32) (float64, bool) {
	var out float64
	var found bool
	s.forEach(rec, func(id uint32, node []byte) bool {
		if id != fieldID {
			return true
		}
		out, found = literalFloat(node)
		return false
	})
	return out, found
}
