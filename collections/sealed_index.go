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
