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

// sealedIndexFromFile writes si's sidecar to path, maps the file, and returns the read-only
// mmap view plus an unmap closer (persistent sealed segment).
func sealedIndexFromFile(path string, si *segIndex) (*mmapSegIndex, func() error, error) {
	if err := writeSidecarIndex(path, si); err != nil {
		return nil, nil, err
	}
	data, closer, err := mapFile(path)
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

// snapshotPath is where a persistent segment's sidecar index lives: beside the segment file,
// sharing its name so compaction/rotation drops it with the segment (reap unlinks it). "" for
// a RAM segment (in-memory collection), which has no on-disk sidecar.
func snapshotPath(seg *segment) string {
	if seg.path == "" {
		return ""
	}
	return seg.path + ".idx"
}

// sealSegmentIndex converts a sealed persistent segment's in-RAM index (si) to the pageable
// mmap sidecar: write+map the sidecar file, publish it as msidx, register its unmap with reap
// (onReap), and drop the heap copy. Best-effort -- on any error the in-RAM index stays and
// the segment is simply not converted this pass. No-op for a RAM segment or one already
// sealed. Once converted the segment's index config is frozen (a later index add/drop does
// not rebuild it; the new attribute is full-scanned via covers()=false), which is correct --
// only less selective for that attribute on already-sealed data.
func (c *Collection) sealSegmentIndex(seg *segment, si *segIndex) {
	path := snapshotPath(seg)
	if path == "" || si == nil || seg.msidx.Load() != nil {
		return
	}
	mm, closer, err := sealedIndexFromFile(path, si)
	if err != nil {
		return
	}
	// CAS so a concurrent conversion of the same segment can't leak a mapping: the loser
	// unmaps its own and bails.
	if !seg.msidx.CompareAndSwap(nil, mm) {
		_ = closer()
		return
	}
	seg.setOnReap(func() { _ = closer() }) // unmap the sidecar when the segment's scan pins drain (reap)
	seg.idx.Store(nil)                     // free the heap index; readIdx now returns msidx
}

// loadSealedIndex maps an existing on-disk sidecar for a sealed persistent segment (on Open),
// returning true if a valid, current sidecar was mapped into msidx. A miss -- no file,
// unreadable, bad CRC, wrong version, a different spec generation, or coverage != the
// segment's recovered extent -- leaves msidx nil so Reindex rebuilds and re-seals it. The
// sidecar is derived, so any doubt is a rebuild, never a wrong index.
func (c *Collection) loadSealedIndex(seg *segment, spec *indexSpec) bool {
	path := snapshotPath(seg)
	if path == "" {
		return false
	}
	data, closer, err := mapFile(path)
	if err != nil {
		return false
	}
	if !sidecarCRCValid(data) {
		_ = closer()
		return false
	}
	mm, err := parseMmapSidecar(data)
	if err != nil || mm == nil || mm.specGen != spec.gen || int(mm.upto) != seg.used {
		_ = closer()
		return false
	}
	seg.setOnReap(func() { _ = closer() })
	seg.msidx.Store(mm)
	return true
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
