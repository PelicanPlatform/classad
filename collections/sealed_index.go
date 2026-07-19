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
	attr, key, ok := splitSegmentSidecar(data)
	if !ok {
		_ = closer()
		return false
	}
	ki, err := parseKeyIndex(key)
	if err != nil || int(ki.upto) != seg.used {
		_ = closer()
		return false
	}
	var mm *mmapSegIndex
	if len(attr) > 0 && sidecarCRCValid(attr) {
		if m, e := parseMmapSidecar(attr); e == nil && m != nil && int(m.upto) == seg.used && (spec == nil || m.specGen == spec.gen) {
			mm = m
		}
	}
	// keyIdx is set exactly once per seal and is the "sealed" marker; CAS so a
	// concurrent seal cannot leak a mapping.
	if !seg.keyIdx.CompareAndSwap(nil, ki) {
		_ = closer()
		return false
	}
	if mm != nil {
		seg.msidx.Store(mm)
		seg.idx.Store(nil) // free the heap attr index; readIdx now returns msidx
	}
	seg.setOnReapKey(func() { _ = closer() })
	return true
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
	container := buildSegmentSidecar(attrBlob, buildKeyIndex(seg.data, seg.used, c.h))
	if err := writeFileAtomic(path, container); err != nil {
		return
	}
	c.publishSidecar(seg, path, nil)
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
