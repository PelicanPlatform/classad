package collections

// A sealed segment's index is the immutable sidecar read over an mmap (mmapSegIndex), not
// the in-RAM segIndex the active segment uses. Two backings, one reader:
//
//   - persistent: the sidecar is a file beside the segment (reclaimable from disk);
//   - in-memory:  the sidecar bytes live in an anonymous mapping (off the Go heap, not
//     persisted, MADV_FREE-able).
//
// Both return an unmap closer the segment reaps with its data, so a rotation/compaction that
// drops a segment tears down its index mapping at the same pin-drained moment (no scan reads
// a torn mapping). The active segment keeps its mutable in-RAM segIndex.

// snapshotPath is where a persistent segment's sidecar lives: beside the segment file,
// sharing its name so compaction/rotation drops it with the segment (reap unlinks it). "" for
// a RAM segment (in-memory collection), which has no on-disk sidecar.
func snapshotPath(seg *segment) string {
	if seg.path == "" {
		return ""
	}
	return seg.path + ".idx"
}

// publishSidecar maps a just-written or existing container file at path and publishes
// its sections into the segment: keyIdx always (the primary-key sidecar, phase 1 of
// the pageable index), and msidx when a valid attribute-index section covering the
// segment is present. One mapping backs both, released by a single reap hook. spec, if
// non-nil, gates the attribute section on its generation (a reopen); a nil spec accepts
// any (a fresh seal). Returns false (unmapping) on any mismatch; the sidecar is derived,
// so any doubt is left unpublished and rebuilt later.
func (c *Collection) publishSidecar(seg *segment, path string, spec *indexSpec) bool {
	data, closer, err := mapFile(path)
	if err != nil {
		return false
	}
	attr, key, col, zone, ok := splitSegmentSidecar(data)
	if !ok {
		_ = closer()
		return false
	}
	adoptPersistedZones(seg, zone)
	ki, err := parseKeyIndex(key)
	if err != nil || int(ki.upto) != seg.used {
		_ = closer()
		return false
	}
	var mm *mmapSegIndex
	if len(attr) > 0 && sidecarCRCValid(attr) {
		// Publish the sidecar whatever configuration it was built under, provided it covers
		// the whole segment. Which probes it can actually answer is decided per probe by
		// coversProbe, which looks the attribute up in the categorical or value directory
		// according to the probe's kind -- so an attribute the sidecar does not hold, or
		// holds under the other kind, simply reads as uncovered and that segment is scanned.
		//
		// Requiring the configuration to MATCH instead threw away a whole segment's index
		// over a single unrelated attribute being added elsewhere, and made it impossible for
		// segments to sit on different index configurations at once -- which is what bounding
		// index backfill to recent data needs. The upto check stays: an index that stops
		// short of the segment's records would silently hide the ones past it.
		if m, e := parseMmapSidecar(attr); e == nil && m != nil && int(m.upto) == seg.used {
			mm = m
		}
	}
	// Columnar accelerator block (v2 container). Derived like the attribute index: any
	// doubt (unparseable / wrong record count) leaves it unpublished, so the segment simply
	// row-scans until the block is rebuilt. The unmarshal COPIES its streams, so the block is
	// independent of this mapping.
	var cs *colSegment
	if body := readColSection(col, seg.used); body != nil { // rejects truncated/corrupt/stale -> row-scan
		cs = unmarshalColSegment(body, c.regionCodec(), c.intern.Intern)
	} else {
		c.seedSchemaFromSection(col, seg.used)
	}
	// keyIdx is set exactly once per seal and is the "sealed" marker; CAS so a
	// concurrent seal cannot leak a mapping.
	if !seg.keyIdx.CompareAndSwap(nil, ki) {
		_ = closer()
		return false
	}
	if cs != nil {
		seg.colblk.Store(cs)
	}
	// Phase 2: a resident key Bloom over the segment's key-hashes, built from the key
	// index, gates the sealed-segment probe. Published after keyIdx so a probe that
	// observes the Bloom always finds the index behind it.
	seg.keyBloom.Store(bloomFromKeyIndex(ki))
	if mm != nil {
		seg.msidx.Store(mm)
		seg.idx.Store(nil) // free the heap attr index; readIdx now returns msidx
	}
	seg.setOnReapKey(func() { _ = closer() })
	return true
}

// bloomFromKeyIndex builds a membership filter over a key index's hashes. It over-
// counts multi-version keys (harmless; only slightly over-sizes the filter).
func bloomFromKeyIndex(ki *mmapKeyIndex) *bloomFilter {
	bf := newBloom(int(ki.count))
	for i := uint32(0); i < ki.count; i++ {
		bf.addHash(ki.hashAt(i))
	}
	return bf
}

// sealSegmentIndex writes a sealed persistent segment's combined sidecar container --
// the key index (always) plus the attribute index (si, if any) -- maps it, and
// publishes both. Best-effort and idempotent: a no-op for a RAM segment or one already
// sealed (keyIdx set), and on any error the segment is left unsealed for a later pass.
func (c *Collection) sealSegmentIndex(seg *segment, si *segIndex) {
	path := snapshotPath(seg)
	if path == "" || seg.keyIdx.Load() != nil {
		return
	}
	var attrBlob []byte
	if si != nil {
		b, err := buildSidecarIndex(si)
		if err != nil {
			return
		}
		attrBlob = b
	}
	container := buildSegmentSidecar(attrBlob, buildKeyIndex(seg.data, seg.used, c.h), c.colBlobForSeg(seg),
		buildZoneBlob(seg.zones), seg.used, seg.dictRecOff())
	if err := writeFileAtomic(path, container); err != nil {
		return
	}
	c.publishSidecar(seg, path, nil)
}

// reindexSealed rebuilds a sealed segment's index sidecar in place, under the current
// index spec, WITHOUT touching the segment's data. This is what separates a re-index from
// a rewrite: the arena bytes are immutable and already correct, so only the derived index
// needs rebuilding -- an added or dropped index reaches an existing archive for the cost of
// decompressing its records once, not re-encoding and rewriting every segment.
//
// A sealed sidecar is itself immutable and may be mapped under a live scan, so it is never
// edited: a whole new mapping is built beside it, published atomically, and the displaced
// one is released once the segment's scan pins drain (segment.releaseStale) -- the same
// safety rule compaction's retire/reap already follows.
//
// Returns true if the segment now carries an index built under spec. Best-effort: on any
// error the existing sidecar is left in place and false is returned, so the segment keeps
// serving queries under its older (still correct, just less selective) index.
func (c *Collection) reindexSealed(sh *shard, seg *segment, spec *indexSpec) bool {
	si := buildSegIndex(c, seg, seg.used, spec)
	if si == nil {
		return false
	}
	c.rezoneSealed(sh, seg)
	if seg.persistent {
		return c.reindexSealedFile(sh, seg, si)
	}
	return c.reindexSealedAnon(sh, seg, si)
}

// rezoneSealed recomputes a sealed segment's zone map against the shard's CURRENT zone
// attributes, so an attribute zoned after the segment was sealed (a runtime value index --
// see Collection.addZoneAttrs) gains whole-segment pruning on the same pass that rebuilds
// its postings. The records are being decompressed for the index anyway, so this costs
// little beyond the extra lookups.
//
// The new map REPLACES the old rather than mutating it: a scan window captured the old map
// by reference under the read lock and reads it without further synchronization.
func (c *Collection) rezoneSealed(sh *shard, seg *segment) {
	sh.mu.RLock()
	attrs, inline, appendOnly := sh.zoneAttrs, sh.zoneInline, sh.appendOnly
	sh.mu.RUnlock()
	if !appendOnly || len(attrs) == 0 {
		return
	}
	zones := computeSegZones(c, seg, seg.used, attrs, inline)
	sh.mu.Lock()
	seg.zones = zones
	sh.mu.Unlock()
}

// reindexSealedFile rebuilds a persistent segment's combined sidecar (attribute index +
// key index) and swaps the whole mapping. The key index is rebuilt from the same immutable
// bytes, so it is byte-identical to the one being replaced -- it is regenerated rather than
// copied out of the live mapping so the new container is self-contained.
func (c *Collection) reindexSealedFile(sh *shard, seg *segment, si *segIndex) bool {
	path := snapshotPath(seg)
	if path == "" {
		return false
	}
	attrBlob, err := buildSidecarIndex(si)
	if err != nil {
		return false
	}
	container := buildSegmentSidecar(attrBlob, buildKeyIndex(seg.data, seg.used, c.h), c.colBlobPreserve(seg),
		buildZoneBlob(c.zonesForSeg(sh, seg)), seg.used, seg.dictRecOff())
	return c.installSidecar(sh, seg, path, container)
}

// installSidecar atomically rewrites seg's sidecar to container, remaps it, and publishes the key
// index (always), the attribute index (when the container carries a valid one), and the columnar
// block (when present) -- all aliasing the ONE new mapping, swapped in under the shard lock and
// reaped together with the displaced mapping once scan pins drain. Shared by the reindex rewrite
// and the enable-time columnar persist. Best-effort: on any malformed remap it drops the new
// mapping and leaves the old sidecar in place. It never reaps the old mapping while leaving the
// segment's live attribute index dangling: if the segment has an msidx but the new container has
// no valid one, it aborts.
func (c *Collection) installSidecar(sh *shard, seg *segment, path string, container []byte) bool {
	// Rename over the live file: the existing mapping is unaffected (it holds the old inode), so
	// readers keep working until they are swapped over below.
	if err := writeFileAtomic(path, container); err != nil {
		return false
	}
	data, closer, err := mapFile(path)
	if err != nil {
		return false
	}
	attr, key, col, zone, ok := splitSegmentSidecar(data)
	if !ok {
		_ = closer()
		return false
	}
	adoptPersistedZones(seg, zone)
	ki, err := parseKeyIndex(key)
	if err != nil || int(ki.upto) != seg.used {
		_ = closer()
		return false
	}
	var mm *mmapSegIndex
	if len(attr) > 0 && sidecarCRCValid(attr) {
		if m, e := parseMmapSidecar(attr); e == nil && m != nil && int(m.upto) == seg.used {
			mm = m
		}
	}
	if mm == nil && seg.msidx.Load() != nil {
		_ = closer() // would drop the segment's live attribute index: abort, keep the old sidecar
		return false
	}
	// The columnar block, if present, ALIASES this new mapping (zero-copy) like msidx, so it is
	// published on the same swap; the displaced mapping (which backed the old block) is reaped
	// together by swapSidecarHook + releaseStale once scan pins drain.
	var cs *colSegment
	if body := readColSection(col, seg.used); body != nil {
		cs = unmarshalColSegment(body, c.regionCodec(), c.intern.Intern)
	} else {
		c.seedSchemaFromSection(col, seg.used)
	}
	// Publish under the shard write lock: the readers that touch a sidecar without a scan pin
	// (SidecarSizes, IndexSizes) hold the read lock while they do; pinned scan readers are handled
	// by the pin drain instead.
	sh.mu.Lock()
	seg.keyIdx.Store(ki)
	seg.keyBloom.Store(bloomFromKeyIndex(ki))
	if mm != nil {
		seg.msidx.Store(mm)
		seg.idx.Store(nil) // the heap copy stays dropped; msidx serves queries
	}
	if cs != nil {
		seg.colblk.Store(cs)
	}
	seg.swapSidecarHook(func() { _ = closer() }, true)
	sh.mu.Unlock()
	seg.releaseStale() // free the displaced mapping now if no scan is reading it
	return true
}

// reindexSealedAnon is reindexSealedFile for an in-memory collection's sealed RAM segment,
// whose sidecar is an anonymous mapping holding only the attribute index (there is no key
// sidecar to rebuild).
func (c *Collection) reindexSealedAnon(sh *shard, seg *segment, si *segIndex) bool {
	mm, closer, err := sealedIndexAnon(si)
	if err != nil {
		return false
	}
	sh.mu.Lock()
	seg.msidx.Store(mm)
	seg.idx.Store(nil)
	seg.swapSidecarHook(func() { _ = closer() }, false)
	sh.mu.Unlock()
	seg.releaseStale()
	return true
}

// sealAndEvictShard realizes the pageable-directory RAM win (phase 3) during operation,
// not only at reopen: for every sealed (non-active) persistent segment it ensures a key
// sidecar exists, then evicts that segment's keys from the resident directory so they are
// served by the sealed probe instead. Without it, a long-running process keeps the full
// directory resident until its next Close/reopen; with it, compaction and periodic Reindex
// bring the directory back down to O(active-segment). A no-op for RAM collections, and
// idempotent (an already-sealed/-evicted segment is skipped).
//
// Safe alongside concurrent writes: the active segment is never touched, and a write that
// supersedes an evicted key's sealed record and re-appends it to the active segment moves
// its directory entry off the segment being evicted, so the phase-C check declines to drop
// it (dir[h].seg != seg.id). Reads/writes of an evicted key already go dir-then-probe.
func (c *Collection) sealAndEvictShard(sh *shard) {
	if c.dir == "" {
		return // RAM collection: no file sidecars, the directory stays resident
	}
	// Phase A: snapshot the sealed persistent segments, PINNED. The active segment is skipped
	// (still appended to); its keys stay resident. The pin matters because Phase B reads each
	// segment's bytes off the shard lock to build its key index, and a concurrent compaction would
	// otherwise munmap them mid-build; it is released only after Phase C, since unpin is what
	// performs a deferred reap and Phase C still touches the segments.
	segs := sh.pinSealed(nil)
	defer unpinAll(segs)

	// Phase B: build any missing key sidecars off-lock (file I/O). sealSegmentIndex is
	// idempotent and folds in the attribute index if one is already built for the segment,
	// so a segment Reindex already sealed is left untouched here.
	for _, seg := range segs {
		if seg.keyIdx.Load() == nil {
			c.sealSegmentIndex(seg, seg.idx.Load())
		}
	}

	// Phase C: evict, under the write lock, every segment now carrying a key index. A
	// segment whose sidecar failed to build keeps its keys resident (still correct).
	sh.mu.Lock()
	for _, seg := range segs {
		if seg.keyIdx.Load() != nil {
			sh.evictSegKeys(seg)
		}
	}
	sh.mu.Unlock()
}

// loadSealedIndex maps a sealed persistent segment's sidecar container on Open,
// publishing its key index (always) and, if valid and matching spec, its attribute
// index. A miss leaves the segment unsealed so the reopen rebuilds it.
func (c *Collection) loadSealedIndex(seg *segment, spec *indexSpec) bool {
	path := snapshotPath(seg)
	if path == "" {
		return false
	}
	return c.publishSidecar(seg, path, spec)
}

// sealSegmentIndexAnon is the in-memory analogue of sealSegmentIndex: it converts a sealed
// RAM segment's in-RAM index (si) to an anonymous mmap sidecar (off the Go heap, GC-invisible,
// MADV_FREE-able), publishes it as msidx via CAS, registers its unmap with reap (onReap), and
// drops the heap copy. Best-effort -- on any error the in-RAM index stays. No-op for a segment
// already sealed. The mapping is process-lifetime (no file); it is released when the segment is
// reaped (compaction) or on Close. The segment must be pin/reap-eligible (pinReap set at
// creation) so a concurrent scan reading the mapping keeps it alive until the scan's unpin.
func (c *Collection) sealSegmentIndexAnon(seg *segment, si *segIndex) {
	if si == nil || seg.msidx.Load() != nil {
		return
	}
	mm, closer, err := sealedIndexAnon(si)
	if err != nil {
		return
	}
	if !seg.msidx.CompareAndSwap(nil, mm) {
		_ = closer() // lost the race to a concurrent conversion; unmap our own and bail
		return
	}
	seg.setOnReap(func() { _ = closer() })
	seg.idx.Store(nil)
}

// sealedIndexAnon builds si's sidecar bytes into an anonymous mapping and returns the view
// plus an unmap closer (in-memory sealed segment).
func sealedIndexAnon(si *segIndex) (*mmapSegIndex, func() error, error) {
	b, err := buildSidecarIndex(si)
	if err != nil {
		return nil, nil, err
	}
	data, closer, err := mapAnon(b)
	if err != nil {
		return nil, nil, err
	}
	mm, err := parseMmapSidecar(data)
	if err != nil {
		_ = closer()
		return nil, nil, err
	}
	return mm, closer, nil
}

// zonesForSeg returns the zone map to persist alongside a sealed segment's index: the one it
// already carries, or a freshly computed one if it has none yet. Computing here is not extra
// work overall -- it is the same computation reopen would otherwise redo on every start.
func (c *Collection) zonesForSeg(sh *shard, seg *segment) map[uint32]zoneRange {
	if seg.zones != nil {
		return seg.zones
	}
	sh.mu.RLock()
	attrs, inline, appendOnly := sh.zoneAttrs, sh.zoneInline, sh.appendOnly
	sh.mu.RUnlock()
	if !appendOnly || len(attrs) == 0 {
		return nil
	}
	return computeSegZones(c, seg, seg.used, attrs, inline)
}

// adoptPersistedZones restores a sealed segment's zone map from its sidecar, so recovery does
// not have to rebuild it by decoding every record -- which was ~95% of reopen time and made
// startup scale with the archive rather than with its segment count.
//
// A segment that already has zones keeps them, and an absent or malformed section is simply
// ignored: the caller's existing recompute path then fills it in. Being wrong here is worse
// than being slow, since a too-narrow range would prune segments that hold matches.
func adoptPersistedZones(seg *segment, zone []byte) {
	if seg == nil || seg.zones != nil || len(zone) == 0 {
		return
	}
	if z, ok := parseZoneBlob(zone); ok {
		seg.zones = z
	}
}

// seedSchemaFromSection recovers just the schema from a section this build cannot read as blocks, so a
// columnar-format bump costs a block rebuild rather than the accelerator itself.
//
// A rejected section used to mean no schema at all, because the schema is only recovered from a section that
// loaded -- so v5 turned `.schema` into "off, no derived schema" on every table until something re-derived
// one. First seed wins; any segment's schema is a valid seed, which is the same rule adopt uses.
func (c *Collection) seedSchemaFromSection(col []byte, upto int) {
	if c.schemaSeed.Load() != nil {
		return
	}
	body := readColSectionSchemaOnly(col, upto)
	if body == nil {
		return
	}
	schema, hot := unmarshalColSchemaOnly(body, c.intern.Intern)
	if schema == nil {
		return
	}
	c.schemaSeed.CompareAndSwap(nil, &schemaSeedState{schema: schema, hot: hot})
}
