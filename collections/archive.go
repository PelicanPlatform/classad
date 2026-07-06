package collections

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Archive is an append-only, larger-than-RAM, rotated store of ClassAds — the
// "condor history file" use case. Ads are appended once and never updated; old data
// is dropped in bulk by rotation. Unlike Collection it has no MVCC, no compaction,
// no per-key delete, and no in-RAM key directory: a segment is sealed once, its
// index persisted as an immutable sidecar, and dropped as a unit on rotation.
//
// See docs/history-archive.md for the design. This is the H1–H4 core: sealed
// segments with persisted sidecar indexes, a segment catalog with zone maps for
// whole-segment pruning, O(segments) recovery, newest-first + LIMIT queries, and
// age/size/count rotation. Indexes are loaded fully into RAM per sealed segment;
// mmap-paged indexes (H5) are a later optimization.
type Archive struct {
	dir     string
	segSize int
	codec   Codec
	intern  *wire.InternTable
	spec    *indexSpec          // categorical/value indexes (immutable for an Archive)
	hot     map[uint32]struct{} // hot-header attr ids
	zoneIDs []uint32            // attrs kept as per-segment min/max for pruning
	ret     Retention

	mu     sync.RWMutex
	segs   []*archiveSeg // sealed segments, oldest first
	active *archiveSeg   // current append target (unsealed), or nil
	nextID uint32

	// scanned, if set, is called with each segment id a query actually scans (i.e.
	// not pruned by its zone map). Test seam for asserting pruning; nil in production.
	scanned func(uint32)
}

// archiveSeg is one segment of the log: a reused mmap-backed segment plus the
// metadata the catalog persists. A sealed segment is immutable; its seg.idx holds
// the loaded sidecar index and zones holds its per-attribute min/max.
type archiveSeg struct {
	seg   *segment
	recN  int
	zones map[uint32]zoneRange // interned attr id -> [min,max]
}

// zoneRange is one attribute's numeric span within a segment (a zone map entry).
// Fields are exported so the catalog can serialize them.
type zoneRange struct {
	Min, Max float64
}

// Retention bounds how much history is kept. Rotation (Rotate, or automatically
// after a seal) drops the oldest sealed segments until every set bound is met. A
// zero field means "no bound on that axis".
type Retention struct {
	MaxSegments int   // keep at most this many sealed segments
	MaxBytes    int64 // keep at most this many bytes of sealed segment files
	// MaxAgeAttr / MaxAge: drop a segment whose max value of the given numeric attr
	// (e.g. "CompletionDate", unix seconds) is older than Now-MaxAge. Now is supplied
	// per Rotate call so the store needs no clock of its own.
	MaxAgeAttr string
	MaxAge     float64
}

// ArchiveOptions configures an Archive.
type ArchiveOptions struct {
	// Dir is the directory holding segment/index/catalog files. Required.
	Dir string
	// SegmentSize is the sealed-segment (mmap file) size in bytes; a segment rolls
	// over when the next ad will not fit. Default 8 MiB.
	SegmentSize int
	// Codec compresses stored ad bytes. Default identity. For recovery the same codec
	// must be supplied (the archive does not yet persist codec identity per segment).
	Codec Codec
	// HotAttrs front-loads these attributes in each ad's hot header (see Collection).
	HotAttrs []string
	// CategoricalAttrs / ValueAttrs configure the per-segment indexes (see Collection).
	CategoricalAttrs []string
	ValueAttrs       []string
	// ZoneAttrs names numeric attributes to keep per-segment min/max on, so a query
	// with a range/equality constraint on one can skip whole segments without opening
	// them. ValueAttrs are automatically included.
	ZoneAttrs []string
	// Retention bounds what rotation keeps. Zero ⇒ keep everything.
	Retention Retention
}

const defaultArchiveSegmentSize = 8 << 20 // 8 MiB

// archiveSeq is the constant commit sequence stamped on every archive record: the
// framing is shared with Collection, but the archive has no MVCC, so every record
// is permanently current (supersededBySeq stays seqMax).
const archiveSeq = 1

// CreateArchive creates a new, empty Archive. The directory must not already hold a
// catalog (use OpenArchive to reopen one).
func CreateArchive(opts ArchiveOptions) (*Archive, error) {
	if opts.Dir == "" {
		return nil, errors.New("archive: Dir is required")
	}
	if _, err := os.Stat(filepath.Join(opts.Dir, catalogName)); err == nil {
		return nil, fmt.Errorf("archive: %s already exists (use OpenArchive)", catalogName)
	}
	a, err := newArchiveShell(opts)
	if err != nil {
		return nil, err
	}
	a.configure(opts)
	if err := a.writeCatalog(); err != nil {
		return nil, err
	}
	return a, nil
}

// newArchiveShell builds the Archive with an empty intern table and no interned
// options; call configure (after any intern-table restore) to resolve options.
func newArchiveShell(opts ArchiveOptions) (*Archive, error) {
	if opts.Dir == "" {
		return nil, errors.New("archive: Dir is required")
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}
	segSize := opts.SegmentSize
	if segSize <= 0 {
		segSize = defaultArchiveSegmentSize
	}
	var codec Codec = opts.Codec
	if codec == nil {
		codec = identityCodec{}
	}
	return &Archive{
		dir:     opts.Dir,
		segSize: recAlign(segSize),
		codec:   codec,
		intern:  wire.NewInternTable(),
		ret:     opts.Retention,
	}, nil
}

// configure resolves the option attribute names to interned ids (spec, hot header,
// zone maps). On recovery it runs after the persisted intern table is restored, so
// the same names map to the same ids.
func (a *Archive) configure(opts ArchiveOptions) {
	a.spec = newIndexSpec(a.intern, opts.CategoricalAttrs, opts.ValueAttrs)
	if len(opts.HotAttrs) > 0 {
		a.hot = make(map[uint32]struct{}, len(opts.HotAttrs))
		for _, n := range opts.HotAttrs {
			a.hot[a.intern.Intern(n)] = struct{}{}
		}
	}
	// Zone maps cover the explicit ZoneAttrs plus every value-indexed attr.
	a.zoneIDs = nil
	seen := map[uint32]struct{}{}
	addZone := func(id uint32) {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			a.zoneIDs = append(a.zoneIDs, id)
		}
	}
	for _, n := range opts.ZoneAttrs {
		addZone(a.intern.Intern(n))
	}
	for _, id := range a.spec.valIDs {
		addZone(id)
	}
}

// Append adds one ad to the archive. It is appended to the active segment; when the
// ad will not fit, the active segment is sealed (its index + zone maps built and
// persisted) and a new one started. Safe for use by a single writer concurrently
// with queries.
func (a *Archive) Append(ad *classad.ClassAd) error {
	wireBytes := wire.EncodeWithHot(nil, ad.AST(), a.intern, a.hot)
	stored := a.codec.Compress(nil, wireBytes)

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == nil {
		if err := a.openActiveLocked(); err != nil {
			return err
		}
	}
	if _, ok := a.active.seg.append(archiveSeq, noLoc, nil, stored); !ok {
		// Doesn't fit: seal the active segment and start a fresh one.
		if err := a.sealActiveLocked(); err != nil {
			return err
		}
		if err := a.openActiveLocked(); err != nil {
			return err
		}
		if _, ok := a.active.seg.append(archiveSeq, noLoc, nil, stored); !ok {
			return fmt.Errorf("archive: ad of %d bytes exceeds segment size %d", len(stored), a.segSize)
		}
	}
	a.active.recN++
	return nil
}

// openActiveLocked starts a fresh mmap-backed active segment. Caller holds mu.
func (a *Archive) openActiveLocked() error {
	id := a.nextID
	a.nextID++
	seg, err := newMmapSegment(id, a.segSize, a.codec, a.segPath(id))
	if err != nil {
		return err
	}
	a.active = &archiveSeg{seg: seg}
	return nil
}

// sealActiveLocked finalizes the active segment: flush its bytes, build + persist its
// sidecar index and zone maps, move it into the sealed set, and clear active. Caller
// holds mu.
func (a *Archive) sealActiveLocked() error {
	as := a.active
	if as == nil || as.seg.used == 0 {
		return nil
	}
	if err := as.seg.flush(); err != nil {
		return err
	}
	idx := buildSegIndex(as.seg.data, as.seg.used, a.codec, a.spec)
	as.seg.idx.Store(idx)
	as.zones = a.computeZones(as.seg.data, as.seg.used)
	if err := a.writeSidecarIndex(as); err != nil {
		return err
	}
	a.segs = append(a.segs, as)
	a.active = nil
	if err := a.writeCatalog(); err != nil {
		return err
	}
	return nil
}

// computeZones scans a segment's records and returns the per-attribute numeric
// min/max for the configured zone attributes (absent/non-numeric values ignored).
func (a *Archive) computeZones(data []byte, upto int) map[uint32]zoneRange {
	if len(a.zoneIDs) == 0 {
		return nil
	}
	zones := make(map[uint32]zoneRange, len(a.zoneIDs))
	var buf []byte
	for off := 0; off < upto; {
		o := uint32(off)
		total := recTotalLen(data, o)
		if total == 0 {
			break
		}
		if w, err := a.codec.Decompress(buf[:0], recAd(data, o)); err == nil {
			buf = w
			ad := wire.Ad(w)
			for _, id := range a.zoneIDs {
				node, ok := ad.Lookup(id)
				if !ok {
					continue
				}
				f, ok := literalFloat(node)
				if !ok {
					continue
				}
				z, seen := zones[id]
				if !seen {
					zones[id] = zoneRange{Min: f, Max: f}
				} else {
					if f < z.Min {
						z.Min = f
					}
					if f > z.Max {
						z.Max = f
					}
					zones[id] = z
				}
			}
		}
		off += int(total)
	}
	return zones
}

// literalFloat extracts the index-normalized float64 from a literal node (int/real/
// bool), matching how the value index and zone maps key numbers.
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

// Flush seals the active segment (if any), making all appended ads durable and
// indexed. Useful to make recent ads queryable-by-index and recoverable without
// closing.
func (a *Archive) Flush() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sealActiveLocked()
}

// Rotate drops the oldest sealed segments until every configured Retention bound is
// met, and returns the number dropped. now (e.g. the current time as unix seconds)
// is used only to evaluate an age bound, so the Archive needs no clock of its own. A
// dropped segment's data file and sidecar index are unmapped and unlinked; a query
// currently scanning a dropped segment holds a pin, so the data file's unlink is
// deferred until that query finishes (no read-through-a-torn-mapping).
func (a *Archive) Rotate(now float64) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var err error
	dropped := 0
	for a.shouldDropOldest(now) {
		as := a.segs[0]
		a.segs = a.segs[1:]
		if e := os.Remove(a.idxPath(as.seg.id)); e != nil && err == nil {
			err = e
		}
		if as.seg.retire() { // unpinned now ⇒ reap immediately; else last unpin reaps
			if e := as.seg.reap(); e != nil && err == nil {
				err = e
			}
		}
		dropped++
	}
	if dropped > 0 {
		if e := a.writeCatalog(); e != nil && err == nil {
			err = e
		}
	}
	return dropped, err
}

// shouldDropOldest reports whether the oldest sealed segment exceeds a retention
// bound. It is evaluated one segment at a time so count/byte bounds converge.
func (a *Archive) shouldDropOldest(now float64) bool {
	if len(a.segs) == 0 {
		return false
	}
	r := a.ret
	if r.MaxSegments > 0 && len(a.segs) > r.MaxSegments {
		return true
	}
	if r.MaxBytes > 0 {
		var total int64
		for _, as := range a.segs {
			total += int64(as.seg.used)
		}
		if total > r.MaxBytes {
			return true
		}
	}
	if r.MaxAgeAttr != "" && r.MaxAge > 0 {
		if id, ok := a.intern.LookupID(r.MaxAgeAttr); ok {
			if z, ok := a.segs[0].zones[id]; ok && z.Max < now-r.MaxAge {
				return true
			}
		}
	}
	return false
}

// Close seals the active segment and unmaps every segment file. The Archive must not
// be used afterward.
func (a *Archive) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var err error
	if e := a.sealActiveLocked(); e != nil {
		err = e
	}
	for _, as := range a.segs {
		if e := as.seg.unmap(); e != nil && err == nil {
			err = e
		}
	}
	a.segs = nil
	return err
}

func (a *Archive) segPath(id uint32) string {
	return filepath.Join(a.dir, fmt.Sprintf("seg-%06d.dat", id))
}

func (a *Archive) idxPath(id uint32) string {
	return filepath.Join(a.dir, fmt.Sprintf("seg-%06d.idx", id))
}

// sortSegsByID keeps the sealed set ordered oldest-first (ascending id).
func (a *Archive) sortSegsByID() {
	sort.Slice(a.segs, func(i, j int) bool { return a.segs[i].seg.id < a.segs[j].seg.id })
}

// decodeAt decodes the record at offset o in data to a *classad.ClassAd.
func (a *Archive) decodeAt(data []byte, o uint32, buf *[]byte) (*classad.ClassAd, bool) {
	w, err := a.codec.Decompress((*buf)[:0], recAd(data, o))
	if err != nil {
		return nil, false
	}
	*buf = w
	ad, err := wire.Decode(w, a.intern)
	if err != nil {
		return nil, false
	}
	return classad.FromAST(ad), true
}
