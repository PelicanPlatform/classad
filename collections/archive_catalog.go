package collections

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The catalog is the archive's recovered state: a small JSON manifest listing the
// sealed segments (id, record count, byte length, zone maps) and the interned
// attribute names in id order. Restart loads it and mmaps each segment's data +
// sidecar index — O(segments), not O(records). It is rewritten (atomically) on
// every seal and rotation; a segment's own bytes and sidecar index are the bulk
// data and are never rewritten.
const catalogName = "catalog.json"

type catalog struct {
	Version  int          `json:"version"`
	NextID   uint32       `json:"next_id"`
	Names    []string     `json:"names"`    // interned names in id order
	Segments []catalogSeg `json:"segments"` // sealed segments, oldest first
}

type catalogSeg struct {
	ID      uint32               `json:"id"`
	RecN    int                  `json:"rec_n"`
	ByteLen int                  `json:"byte_len"`
	Zones   map[uint32]zoneRange `json:"zones,omitempty"`
}

// writeCatalog atomically rewrites the catalog from the current sealed-segment set.
// Caller holds a.mu (at least read); seal/rotate hold it for write.
func (a *Archive) writeCatalog() error {
	names := make([]string, a.intern.Len())
	for i := range names {
		names[i], _ = a.intern.Name(uint32(i))
	}
	cat := catalog{Version: 1, NextID: a.nextID, Names: names}
	for _, as := range a.segs {
		cat.Segments = append(cat.Segments, catalogSeg{
			ID:      as.seg.id,
			RecN:    as.recN,
			ByteLen: as.seg.used,
			Zones:   as.zones,
		})
	}
	b, err := json.MarshalIndent(cat, "", "  ")
	if err != nil {
		return err
	}
	// Write to a temp file and rename, so a crash never leaves a half-written
	// catalog: recovery sees either the old or the new one.
	tmp := filepath.Join(a.dir, catalogName+".tmp")
	if err := writeFileSync(tmp, b); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(a.dir, catalogName))
}

func readCatalog(dir string) (*catalog, error) {
	b, err := os.ReadFile(filepath.Join(dir, catalogName))
	if err != nil {
		return nil, err
	}
	var cat catalog
	if err := json.Unmarshal(b, &cat); err != nil {
		return nil, fmt.Errorf("archive: corrupt catalog: %w", err)
	}
	return &cat, nil
}

// OpenArchive reopens an existing Archive from opts.Dir, recovering its segments and
// indexes. The index/hot/zone options should match those the archive was created
// with. A segment left un-sealed by a crash (present on disk but absent from the
// catalog) is recovered by scanning its CRC-framed records and sealing it.
func OpenArchive(opts ArchiveOptions) (*Archive, error) {
	a, err := newArchiveShell(opts)
	if err != nil {
		return nil, err
	}
	cat, err := readCatalog(a.dir)
	if err != nil {
		return nil, err
	}
	// Restore the intern table in id order, then resolve options against it so the
	// same names map to the same ids the sidecar indexes were built with.
	for _, name := range cat.Names {
		a.intern.Intern(name)
	}
	a.configure(opts)
	a.nextID = cat.NextID

	loaded := map[uint32]bool{}
	for _, cs := range cat.Segments {
		as, err := a.loadSegment(cs)
		if err != nil {
			a.Close()
			return nil, err
		}
		a.segs = append(a.segs, as)
		loaded[cs.ID] = true
	}
	a.sortSegsByID()

	if err := a.recoverOrphans(loaded); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

// loadSegment mmaps a sealed segment's data file and its sidecar index.
func (a *Archive) loadSegment(cs catalogSeg) (*archiveSeg, error) {
	path := a.segPath(cs.ID)
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	seg, err := openMmapSegment(cs.ID, a.codec, f, int(info.Size()))
	if err != nil {
		f.Close()
		return nil, err
	}
	seg.used = cs.ByteLen
	seg.synced = cs.ByteLen
	si, err := readSidecarIndex(a.idxPath(cs.ID))
	if err != nil {
		seg.unmap()
		return nil, err
	}
	seg.idx.Store(si)
	return &archiveSeg{seg: seg, recN: cs.RecN, zones: cs.Zones}, nil
}

// recoverOrphans finds segment data files not present in the catalog (a crash left
// the active segment un-sealed), scans their committed CRC-framed prefix, and seals
// them so their records become queryable and catalogued.
func (a *Archive) recoverOrphans(loaded map[uint32]bool) error {
	matches, err := filepath.Glob(filepath.Join(a.dir, "seg-*.dat"))
	if err != nil {
		return err
	}
	var orphanIDs []uint32
	for _, m := range matches {
		id, ok := parseSegID(filepath.Base(m))
		if !ok || loaded[id] {
			continue
		}
		orphanIDs = append(orphanIDs, id)
	}
	sort.Slice(orphanIDs, func(i, j int) bool { return orphanIDs[i] < orphanIDs[j] })

	for _, id := range orphanIDs {
		if err := a.recoverOrphan(id); err != nil {
			return err
		}
	}
	if len(orphanIDs) > 0 {
		if err := a.writeCatalog(); err != nil {
			return err
		}
	}
	return nil
}

// recoverOrphan maps one orphan segment, finds its committed record prefix by CRC,
// and either seals it (if it holds records) or discards it (empty).
func (a *Archive) recoverOrphan(id uint32) error {
	path := a.segPath(id)
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	seg, err := openMmapSegment(id, a.codec, f, int(info.Size()))
	if err != nil {
		f.Close()
		return err
	}
	used, recN := scanCommitted(seg.data)
	if used == 0 {
		// Nothing committed: drop the empty file.
		seg.unmap()
		os.Remove(path)
		return nil
	}
	seg.used = used
	as := &archiveSeg{seg: seg, recN: recN}
	idx := buildSegIndex(seg.data, used, a.codec, a.spec)
	seg.idx.Store(idx)
	as.zones = a.computeZones(seg.data, used)
	if err := a.writeSidecarIndex(as); err != nil {
		return err
	}
	a.segs = append(a.segs, as)
	if id >= a.nextID {
		a.nextID = id + 1
	}
	a.sortSegsByID()
	return nil
}

// scanCommitted returns the length and record count of a segment's committed prefix:
// records whose framing is intact and whose trailing CRC verifies. It stops at the
// first zeroed/torn record (the crash point or the mmap's zero tail).
func scanCommitted(data []byte) (used, recN int) {
	off := 0
	for off < len(data) {
		o := uint32(off)
		total := recTotalLen(data, o)
		if total == 0 || off+int(total) > len(data) {
			break
		}
		if !recVerifyCRC(data, o) {
			break
		}
		off += int(total)
		recN++
	}
	return off, recN
}

// parseSegID extracts the numeric id from a "seg-000123.dat" basename.
func parseSegID(base string) (uint32, bool) {
	s := strings.TrimSuffix(strings.TrimPrefix(base, "seg-"), ".dat")
	if s == base {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}
