package collections

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// errNoMmap is returned when a persistent collection is requested on a platform
// without mmap support (persistence is currently unix-only).
var errNoMmap = errors.New("collections: persistent collections are unix-only")

// dictReg maps ZSTD dictionaries to small integer ids and back, so every persistent
// segment can record (in its file name) the dictionary its records were compressed
// under. This lets a collection re-train its dictionary over its lifetime (each
// RetrainDict assigns a fresh id and recompacts) while recovery still decodes every
// segment with the exact codec it was written under.
//
// Id 0 is the base codec supplied at New/Open (opts.Codec, default identity). It
// carries no persisted dictionary bytes — it is reconstructed from Options when the
// collection is reopened, so a reopen must pass the same base Codec (the identity
// default needs nothing). Ids > 0 are trained dictionaries whose raw bytes live at
// <dir>/dicts/<id>.zst; recovery loads them and rebuilds the codecs.
type dictReg struct {
	mu   sync.Mutex
	dir  string           // <collection Dir>/dicts, or "" for an in-memory collection
	byID map[uint32]Codec // dictionary id -> codec
	idOf map[Codec]uint32 // codec -> dictionary id
	next uint32           // next id to assign
}

// newDictReg creates a registry with base registered as dictionary id 0.
func newDictReg(base Codec) *dictReg {
	return &dictReg{
		byID: map[uint32]Codec{0: base},
		idOf: map[Codec]uint32{base: 0},
		next: 1,
	}
}

// idFor returns the dictionary id a codec is registered under.
func (r *dictReg) idFor(c Codec) (uint32, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.idOf[c]
	return id, ok
}

// codecFor returns the codec registered for a dictionary id.
func (r *dictReg) codecFor(id uint32) (Codec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.byID[id]
	return c, ok
}

// register assigns the next id to codec, whose compression dictionary is dict, and
// (for a persistent collection) writes the dictionary bytes durably to
// <dir>/dicts/<id>.zst so recovery can reconstruct the codec. Returns the id.
func (r *dictReg) register(codec Codec, dict []byte) (uint32, error) {
	// Reserve the id under the lock (so concurrent registers never collide), but do
	// NOT register the codec in byID/idOf yet: for a persistent collection we must
	// persist the dictionary bytes FIRST, so a failed write never leaves a codec
	// whose backing dict file is missing (a recovery hazard) -- segments would be
	// tagged with a dictionary id whose .zst does not exist. A failed write leaves a
	// hole in the id sequence, which is harmless (loadDicts resumes at max+1).
	r.mu.Lock()
	id := r.next
	r.next++
	dir := r.dir
	r.mu.Unlock()
	if dir != "" {
		if err := writeDictFile(filepath.Join(dir, fmt.Sprintf("%d.zst", id)), dict); err != nil {
			return 0, err
		}
	}
	r.mu.Lock()
	r.byID[id] = codec
	r.idOf[codec] = id
	r.mu.Unlock()
	return id, nil
}

// prune removes every registered dictionary except id 0 (the base codec) and those
// keep returns true for, unlinking each removed id's on-disk .zst. Without pruning the
// registry grows by one codec per retrain FOREVER -- each an un-Closed zstd
// encoder/decoder pair whose state pools inflated while it was the hot codec -- the
// dominant live-heap leak in a long-running daemon. The removed codecs are only
// dereferenced, never Closed: an in-flight scan may still be decompressing through one
// (its pinned segment holds the codec reference), so the GC reclaims each codec when
// its last user finishes.
func (r *dictReg) prune(keep func(Codec) bool) []uint32 {
	r.mu.Lock()
	var removed []uint32
	for id, c := range r.byID {
		if id == 0 || keep(c) {
			continue
		}
		delete(r.byID, id)
		delete(r.idOf, c)
		removed = append(removed, id)
	}
	dir := r.dir
	r.mu.Unlock()
	for _, id := range removed {
		if dir != "" {
			_ = os.Remove(filepath.Join(dir, fmt.Sprintf("%d.zst", id)))
		}
	}
	return removed
}

// writeDictFile writes a dictionary's bytes to path and fsyncs it (so a codec that
// segments already reference cannot be lost across a crash).
func writeDictFile(path string, dict []byte) error {
	// Recreate the dicts directory if it went missing at runtime. It is created at
	// Open, but a retrain must not fail with ENOENT (open ".../dicts/N.zst: no such
	// file or directory") just because the directory disappeared underneath us.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(dict); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// loadDicts loads every <dir>/dicts/<id>.zst into the registry (rebuilding each
// codec from its dictionary bytes) and points the registry at dir for future
// RetrainDict writes. Returns the id of the most recently trained dictionary (the
// highest id present), or 0 if none — that codec is what new writes should use.
func (c *Collection) loadDicts(dictsDir string) (latest uint32, err error) {
	c.dicts.mu.Lock()
	c.dicts.dir = dictsDir
	c.dicts.mu.Unlock()
	entries, err := os.ReadDir(dictsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // no dictionaries trained yet
		}
		return 0, err
	}
	for _, e := range entries {
		var id uint32
		if _, err := fmt.Sscanf(e.Name(), "%d.zst", &id); err != nil || id == 0 {
			continue
		}
		dict, err := os.ReadFile(filepath.Join(dictsDir, e.Name()))
		if err != nil {
			return 0, err
		}
		codec, err := NewZSTDCodec(dict)
		if err != nil {
			return 0, fmt.Errorf("rebuild codec for dict %d: %w", id, err)
		}
		c.dicts.mu.Lock()
		c.dicts.byID[id] = codec
		c.dicts.idOf[codec] = id
		if id >= c.dicts.next {
			c.dicts.next = id + 1
		}
		c.dicts.mu.Unlock()
		if id > latest {
			latest = id
		}
	}
	return latest, nil
}

// Open opens a persistent collection under opts.Dir, whose arenas are memory-mapped
// files. Committed data is flushed to disk on Close (per-commit msync durability is
// added in a later milestone). If opts.Dir is empty, Open is equivalent to New (an
// in-memory collection). Persistence is unix-only.
//
// NOTE (P2): this creates a fresh persistent collection; recovering an existing
// directory (rebuilding the directory + index from the segment files) is the next
// milestone.
func Open(opts Options) (*Collection, error) {
	if opts.Dir == "" {
		return New(opts), nil
	}
	if !mmapSupported {
		return nil, errNoMmap
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}
	c := New(opts)
	c.dir = opts.Dir
	c.inline = true
	// Records are inline-encoded (names, not interned ids); zone-map value extraction
	// must look up by name. New defaulted the shards to id-based lookup for the in-memory
	// case; flip them here now that inline encoding is confirmed.
	for _, sh := range c.shards {
		sh.zoneInline = true
	}
	// Indexes on a persistent collection are in-memory only (rebuilt on recovery, see
	// below). Replace the interned spec New built with an inline one that extracts
	// values by name (records carry no intern ids).
	c.spec.Store(newInlineIndexSpec(opts.CategoricalAttrs, opts.ValueAttrs))
	// Inline mode keys the hot header by (folded) name; install the configured HotAttrs
	// plus the always-hot defaults (Requirements, Rank) in inline form.
	c.installHotNames(opts.HotAttrs)
	// Load any dictionaries trained in a prior lifetime (before recovering segments,
	// which are decoded with the codec they name) and point the registry at the dicts
	// directory for future RetrainDict writes. New writes use the most recent
	// dictionary's codec.
	dictsDir := filepath.Join(opts.Dir, "dicts")
	if err := os.MkdirAll(dictsDir, 0o755); err != nil {
		return nil, err
	}
	latest, err := c.loadDicts(dictsDir)
	if err != nil {
		return nil, fmt.Errorf("load dictionaries: %w", err)
	}
	if latest != 0 {
		if codec, ok := c.dicts.codecFor(latest); ok {
			c.codec.Store(&codecHolder{codec})
		}
	}
	// Give each shard an mmap-segment factory writing to its own subdirectory.
	// Files are named "seg-<counter>.d<dictid>.dat": the counter is a per-shard
	// monotonic sequence (independent of the logical segment id, which is the array
	// index and is reassigned at compaction/recovery, so no rename is needed), and
	// dictid records which dictionary the segment's records were compressed under.
	for i, sh := range c.shards {
		shardDir := filepath.Join(opts.Dir, fmt.Sprintf("%d", i))
		if err := os.MkdirAll(shardDir, 0o755); err != nil {
			return nil, err
		}
		maxNum, err := c.loadShard(sh, shardDir)
		if err != nil {
			return nil, fmt.Errorf("recover shard %d: %w", i, err)
		}
		counter := maxNum
		// allocNamed builds a segment file under prefix. "seg" is the live form recovery
		// loads; any other prefix is invisible to it (loadShard only parses "seg-N.dX.dat"),
		// which is what lets a merge stage its output durably before anything can see it.
		sh.allocNamed = func(id uint32, size int, codec Codec, prefix string) (*segment, error) {
			n := atomic.AddUint64(&counter, 1)
			dictID, ok := c.dicts.idFor(codec)
			if !ok {
				// Every codec new writes/compaction use comes from currentCodec or
				// RetrainDict, both registered; fall back to the base codec's id.
				dictID = 0
			}
			path := filepath.Join(shardDir, fmt.Sprintf("%s-%d.d%d.dat", prefix, n, dictID))
			return newMmapSegment(id, size, codec, path)
		}
		sh.segDir = shardDir
		sh.alloc = func(id uint32, size int, codec Codec) (*segment, error) {
			return sh.allocNamed(id, size, codec, "seg")
		}
	}
	// Recovery is complete: every live segment's codec is known, so dictionaries only
	// history references (loadDicts loads the full on-disk set) can be dropped now
	// instead of holding an inflatable codec each for the life of the process.
	c.pruneDicts()
	// Indexes are derived state, not persisted: build them over the recovered
	// segments so a reopened collection's queries are immediately selective.
	if c.spec.Load().any() {
		c.Reindex()
	}
	// The maintained ordered indexes are likewise derived: rebuild them from the
	// recovered ads so a reopened collection's Ordered() is immediately correct.
	c.rebuildOrdered()
	// If sealed segments recovered persisted columnar blocks, re-enable schema-scan from them
	// (adopt-from-sidecar) so the accelerator is live immediately -- no re-sample, no rebuild.
	c.adoptPersistedSchemaScan()
	// Recorded query demand is checkpointed alongside the data, so index decisions do not
	// restart from zero every time the process does.
	c.loadDemand()
	// Emit the sidecar-adoption summary gathered during recovery (before the Reindex above
	// rebuilds whatever was not adopted), so a slow reopen can be traced to its cause.
	if OpenIndexDiagHook != nil {
		d := c.openIdxDiag
		d.Dir = c.dir
		OpenIndexDiagHook(d)
	}
	return c, nil
}

// loadShard mmaps the existing segment files under shardDir (in file-number order,
// which is commit order), scans each for its written extent, and rebuilds the
// shard's directory. Returns the highest segment file number seen.
// sidecarFileExists reports whether a sealed segment has its .idx sidecar file on disk, to
// distinguish (in OpenIndexDiag) a segment that predates sidecar persistence from one whose
// present sidecar was rejected.
func sidecarFileExists(seg *segment) bool {
	p := snapshotPath(seg)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func (c *Collection) loadShard(sh *shard, shardDir string) (uint64, error) {
	// Complete any merge a crash interrupted, so the directory below is never a half-merged
	// mixture of a merged segment and the sources it replaced.
	finishPendingMerges(shardDir)
	// Each sealed segment's sidecar trailer names the offset of its dictionary record; the
	// hints are collected while mapping and consumed once every segment is in place.
	segDictHint := map[*segment]uint32{}
	entries, err := os.ReadDir(shardDir)
	if err != nil {
		return 0, err
	}
	type segFile struct {
		num    uint64
		dictID uint32
		name   string
	}
	var files []segFile
	var maxNum uint64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Match segment data files only. Sscanf ignores trailing input, so a sidecar
		// (seg-N.dX.dat.idx / .kidx) would otherwise parse as a phantom segment; require
		// the name to end exactly in ".dat".
		if !strings.HasSuffix(e.Name(), ".dat") {
			continue
		}
		var n uint64
		var dictID uint32
		if _, err := fmt.Sscanf(e.Name(), "seg-%d.d%d.dat", &n, &dictID); err == nil {
			files = append(files, segFile{n, dictID, e.Name()})
			if n > maxNum {
				maxNum = n
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].num < files[j].num })

	// Segments are collected first and ordered afterwards by CONTENT (see below), so the
	// id assigned here is provisional.
	type loadedSeg struct {
		seg *segment
		num uint64
	}
	var loaded []loadedSeg

	for _, sf := range files {
		// Decode this segment with the codec it was written under (the dictionary named
		// in its file). Loaded before this by Open via loadDicts.
		codec, ok := c.dicts.codecFor(sf.dictID)
		if !ok {
			return 0, fmt.Errorf("segment %s references unknown dictionary %d", sf.name, sf.dictID)
		}
		f, err := os.OpenFile(filepath.Join(shardDir, sf.name), os.O_RDWR, 0)
		if err != nil {
			return 0, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return 0, err
		}
		seg, err := openMmapSegment(uint32(len(loaded)), codec, f, int(st.Size()))
		if err != nil {
			f.Close()
			return 0, err
		}
		// The write extent is the run of records from offset 0 until an unwritten
		// (zero totalLen) or out-of-bounds record — the file is zero-initialized.
		used := 0
		// A sealed segment records its extent in its sidecar, so recovery can take it
		// instead of walking and CRC-verifying every record to re-derive it. That walk is a
		// full integrity scan of the entire archive on every start -- correct, but it makes
		// startup scale with total records rather than with segment count. The recorded
		// extent is still checked: it must fit the mapping and the record ending exactly
		// there must verify, so a truncated file or a sidecar left over from a different
		// segment falls back to the scan rather than being trusted.
		// The same trailer read also yields the offset of the segment's dictionary record,
		// used below so publishSegDict need not search for it.
		extent, dictRec := readSidecarTrailer(seg.path + ".idx")
		segDictHint[seg] = dictRec
		if rec := extent; rec > 0 && rec <= len(seg.data) &&
			recExtentEndsCleanly(seg.data, rec) {
			used = rec
		} else {
			for used+recHeaderSize <= len(seg.data) {
				total := int(recTotalLen(seg.data, uint32(used)))
				if total <= 0 || used+total > len(seg.data) {
					break // unwritten (zero) tail or an impossible length
				}
				if !recVerifyCRC(seg.data, uint32(used)) {
					break // torn/partial record: the durable data ends here
				}
				used += total
			}
		}
		seg.used = used
		seg.synced = used
		loaded = append(loaded, loadedSeg{seg, sf.num})
	}
	// Order segments by their CONTENT -- the commit sequence of their first record -- rather
	// than by the number in their file name.
	//
	// Segment order is append order, and much depends on it: scans walk newest-first, and
	// QueryLimit stops early on the premise that a later segment holds newer records. The
	// file name only happens to encode that today because files are created in append order;
	// anything that writes a segment out of turn breaks it. Merging several old segments into
	// one is exactly that: the merged file is allocated last, gets the highest number, and
	// would come back as the NEWEST segment while holding the OLDEST records -- "last K jobs"
	// returning ancient ads, silently.
	//
	// The first record's seq is the first 8 bytes of the segment, so this costs one read per
	// segment: no scan, no decompression, and no ordering metadata to keep in the file name
	// or anywhere else that could disagree with the data.
	sort.SliceStable(loaded, func(i, j int) bool {
		si, iok := segFirstSeq(loaded[i].seg)
		sj, jok := segFirstSeq(loaded[j].seg)
		if iok != jok {
			return iok // an empty segment carries no position; sort it last
		}
		if iok && si != sj {
			return si < sj
		}
		return loaded[i].num < loaded[j].num // deterministic tiebreak
	})
	for i := range loaded {
		loaded[i].seg.id = uint32(i)
		sh.segs = append(sh.segs, loaded[i].seg)
	}
	// Fast reopen: a clean Close leaves a directory snapshot; restore from it instead
	// of scanning every record (rebuildDir). It is validated against the loaded
	// segment set and falls back to the full scan on any mismatch. Chained
	// collections are excluded (their per-parent child counts would also need
	// rebuilding); a stale snapshot in that case is discarded, not trusted.
	// Time travel also forces the full scan: rebuildDir is what rebuilds the per-shard
	// time->seq checkpoint index (and the segment scan-pruning counters) from the
	// segment markers, which the directory snapshot does not carry.
	if sh.childParentHash != nil || c.timeTravel() != nil {
		os.Remove(filepath.Join(shardDir, dirSnapName))
		c.rebuildDir(sh)
	} else if !c.tryLoadDirSnapshot(sh, shardDir) {
		c.rebuildDir(sh) // sets sh.act to the last (active) segment
	}
	// Map each sealed segment's existing sidecar directly (skip the active append target,
	// which stays in-RAM): the reopen restores the index by mmapping it instead of
	// re-indexing every record. A missing/invalid/stale sidecar leaves msidx nil so the
	// Reindex that follows Open rebuilds and re-seals that segment.
	// Map each sealed segment's sidecar container: its key index (phase 1 of the
	// pageable primary index, always present on a persistent segment) and, when present
	// and matching the current spec, its attribute index. Runs regardless of whether
	// attribute indexes are configured. A missing/invalid sidecar is left unmapped and
	// rebuilt later; the directory stays authoritative.
	// Restore each segment's attribute dictionary (interned segments only) BEFORE any body
	// decode below, so zone/index rebuilds and scans resolve segment-local ids. Done across
	// ALL segments, including whichever one recovery picked as active. An interned segment is
	// sealed and cannot take new inline writes, so if recovery made one the active target,
	// demote it -- the next write then allocates a fresh inline active segment.
	for _, seg := range sh.segs {
		if seg != nil {
			publishSegDictAt(seg, segDictHint[seg])
		}
	}
	// Then each columnarized segment's columnar payload, which is DURABLE data rather than a cache:
	// its records were written without the attributes it holds, so a segment whose payload is not
	// published reads as an ad missing half its attributes and nothing about the result says so.
	// Publication must follow the dictionary loop above -- an interned segment's payload is
	// translated through its dictionary -- and must cover every segment, because a reader cannot ask
	// whether the format was recognised, only for the ad.
	for _, seg := range sh.segs {
		if seg != nil {
			publishColNative(c, seg)
		}
	}
	if sh.act != nil && sh.act.dict.Load() != nil {
		sh.act = nil
	}
	spec := c.spec.Load()
	for _, seg := range sh.segs {
		if seg != nil && seg != sh.act {
			// Map the sidecar FIRST: a v3 container carries the segment's zone map, which
			// loadSealedIndex adopts. Rebuilding one instead means decoding every record --
			// measured at ~95% of reopen time, i.e. recovery scaling with the size of the
			// archive rather than its segment count.
			c.openIdxDiag.SealedSegments++
			hasSidecar := sidecarFileExists(seg)
			if hasSidecar {
				c.openIdxDiag.SidecarFiles++
			}
			loaded := c.loadSealedIndex(seg, spec)
			// Record how this segment's index was handled: an adopted attribute index needs no
			// rebuild; anything else the post-Open Reindex rebuilds by decoding every record,
			// which is the slow-startup path this diagnostic exists to explain.
			if seg.keyIdx.Load() != nil {
				c.openIdxDiag.KeyIndexAdopted++
			}
			if seg.msidx.Load() != nil {
				c.openIdxDiag.AttrIndexAdopted++
			} else {
				switch {
				case !hasSidecar:
					c.openIdxDiag.note("no-sidecar-file")
				case !loaded:
					c.openIdxDiag.note("sidecar-rejected")
				default:
					c.openIdxDiag.note("attr-section-missing-or-stale")
				}
			}
			// Fall back to rebuilding only when the sidecar carried no zone map (written
			// before v3, or none configured at the time).
			if sh.appendOnly && len(sh.zoneAttrs) > 0 && seg.zones == nil {
				seg.zones = computeSegZones(c, seg, seg.used, sh.zoneAttrs, sh.zoneInline)
			}
			if loaded {
				// Phase 3: the segment's keys are now reachable through the sealed
				// probe, so evict them from the resident directory -- the RAM win. Safe
				// here because loadShard runs single-threaded during Open and every
				// sealed segment is indexed, so no version escapes the probe.
				sh.evictSegKeys(seg)
			}
		}
	}
	return maxNum, nil
}

// rebuildDir reconstructs a shard's directory + commit sequence from its segments
// by replaying records: for each key the record with the greatest seq is its
// current version, live iff it is not superseded (a key whose latest record was
// tombstoned by a delete is absent). Chains are rebuilt fresh.
func (c *Collection) rebuildDir(sh *shard) {
	if sh.appendOnly {
		c.rebuildAppendLog(sh)
		return
	}
	type best struct {
		loc        loc
		seq        uint64
		superseded uint64
	}
	byKey := make(map[string]best)
	var maxSeq uint64
	for _, seg := range sh.segs {
		if seg == nil {
			continue
		}
		for off := 0; off < seg.used; {
			o := uint32(off)
			total := recTotalLen(seg.data, o)
			if total == 0 {
				break
			}
			seq := recSeq(seg.data, o)
			sup := recSuperseded(seg.data, o)
			// commitSeq must cover every sequence ever assigned, including delete
			// sequences (a delete bumps the sequence via supersededBySeq without
			// writing a record); otherwise a deleted record would look current
			// (S0 < its supersededBySeq) to a post-recovery scan.
			if seq > maxSeq {
				maxSeq = seq
			}
			if sup != seqMax && sup > maxSeq {
				maxSeq = sup
			}
			// Rebuild the segment's scan-pruning metadata from disk (zeroed on reopen).
			if seg.minSeq == 0 || seq < seg.minSeq {
				seg.minSeq = seq
			}
			// A time-checkpoint marker feeds the shard time index; it never enters the
			// directory, and its bytes count as dead (as appendMarker does at runtime)
			// so a history-only segment reaches dead >= used.
			if recIsMarker(seg.data, o) {
				sh.tseq.record(seq, recMarkerMillis(seg.data, o))
				seg.dead += int64(total)
				off += int(total)
				continue
			}
			if sup != seqMax {
				seg.dead += int64(total)
				if sup > seg.maxSup {
					seg.maxSup = sup
				}
			}
			key := string(recKey(seg.data, o))
			if b, ok := byKey[key]; !ok || seq >= b.seq {
				byKey[key] = best{loc{seg.id, o}, seq, sup}
			}
			off += int(total)
		}
	}
	sh.commitSeq = maxSeq

	// Enforce the single-current-version invariant: across all segments, at most one
	// record per key may be non-superseded. Two situations violate it on disk:
	//   - a crash between a compaction writing a record's destination copy and
	//     retiring (unlinking) its source segment leaves both files, so the same
	//     record appears current twice; and
	//   - an update or delete whose supersession of the *old* record landed in an
	//     already-synced region (hence was not re-msync'd) is not durable, so the
	//     stale version still looks current.
	// max-seq wins already yields the right value for the directory (Get), but a scan
	// walks every segment's records directly, so any extra current record would be a
	// duplicate. Mark every current record that is not its key's winner superseded.
	for _, seg := range sh.segs {
		if seg == nil {
			continue
		}
		for off := 0; off < seg.used; {
			o := uint32(off)
			total := recTotalLen(seg.data, o)
			if total == 0 {
				break
			}
			if recIsMarker(seg.data, o) {
				off += int(total) // marker, not a data record
				continue
			}
			if recSuperseded(seg.data, o) == seqMax {
				b := byKey[string(recKey(seg.data, o))]
				if b.loc.seg != seg.id || b.loc.off != o {
					// A stale still-current duplicate/older version: retire it at its
					// own seq so seq <= S0 < sup is false for every scan.
					seg.supersedeRec(o, recSeq(seg.data, o))
				}
			}
			off += int(total)
		}
	}

	sh.dir = make(map[uint64]loc, len(byKey))
	count := 0
	for keyStr, b := range byKey {
		if b.superseded != seqMax {
			continue // latest version was superseded (deleted) -> key absent
		}
		h := c.h.Hash([]byte(keyStr))
		setRecNext(sh.segs[b.loc.seg].data, b.loc.off, dirGetOr(sh.dir, h))
		sh.dir[h] = b.loc
		count++
	}
	sh.count = count
	if len(sh.segs) > 0 {
		sh.act = sh.segs[len(sh.segs)-1]
	}
}

// rebuildAppendLog restores an append-only shard on reopen. An append log has no per-key
// supersession or directory, so the single-current-version dedup that rebuildDir performs for a
// mutable shard must be skipped: every on-disk record stays live (duplicate keys are intentional
// here). It only recovers the commit sequence, the per-segment min sequence, the live count, and
// the active (last) segment for continued appends.
func (c *Collection) rebuildAppendLog(sh *shard) {
	var maxSeq uint64
	count := 0
	for _, seg := range sh.segs {
		if seg == nil {
			continue
		}
		for off := 0; off < seg.used; {
			o := uint32(off)
			total := recTotalLen(seg.data, o)
			if total == 0 {
				break
			}
			seq := recSeq(seg.data, o)
			if seq > maxSeq {
				maxSeq = seq
			}
			if seg.minSeq == 0 || seq < seg.minSeq {
				seg.minSeq = seq
			}
			if !recIsMarker(seg.data, o) {
				count++ // append-only records are never superseded, so all are live
			}
			off += int(total)
		}
	}
	sh.commitSeq = maxSeq
	sh.count = count
	if len(sh.segs) > 0 {
		sh.act = sh.segs[len(sh.segs)-1]
	}
}

// Close flushes all committed data to disk and unmaps the collection's segment
// files. It is a no-op for an in-memory collection. The collection must not be used
// after Close.
func (c *Collection) Close() error {
	var firstErr error
	for i, sh := range c.shards {
		sh.mu.Lock()
		// Flush every segment durable BEFORE writing the directory snapshot, so the
		// snapshot never references bytes not yet on disk.
		for _, seg := range sh.segs {
			if seg != nil {
				if err := seg.flush(); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
		// Ensure each sealed (non-active) segment has its sidecar container on disk so a
		// reopen can map it -- the key index (phase 1 of the pageable primary index) plus
		// any in-RAM attribute index. sealSegmentIndex is a no-op for a segment already
		// sealed (by the Reindex pass during operation). Best effort; data is still mapped.
		if c.dir != "" {
			for _, seg := range sh.segs {
				if seg != nil && seg != sh.act {
					c.sealSegmentIndex(seg, seg.idx.Load())
				}
			}
		}
		// Persist the directory for a scan-free reopen (best effort; skipped for a
		// chained collection -- see tryLoadDirSnapshot). Only for a persistent store.
		if c.dir != "" && sh.childParentHash == nil {
			shardDir := filepath.Join(c.dir, fmt.Sprintf("%d", i))
			_ = writeDirSnapshot(sh, shardDir)
		}
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			// closeUnmap unmaps a persistent segment's data file (without unlinking -- it
			// persists for reopen) and, via the reap hook, its sidecar index; for an in-memory
			// segment with an anon sidecar it unmaps that sidecar so a Close short of process
			// exit does not leak the mapping. It is pin-aware (a live scan defers the unmap to
			// its last unpin) and a no-op for a plain RAM segment, so this is safe and cheap
			// for a pure key/value in-memory collection too.
			if err := seg.closeUnmap(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		sh.mu.Unlock()
	}
	// Release the shared columnar-block cache once (its segments shared it, so it is not closed
	// per-segment above). Also closes the schema-scan cache if one was built.
	c.colCache.close()
	if st := c.schemaScan.Load(); st != nil {
		st.cache.close()
	}
	return firstErr
}

// recExtentEndsCleanly sanity-checks a recorded extent in O(1): it must be record-aligned,
// fit the mapping, and point at the end of written data -- either the end of the arena, or a
// zeroed header where the next record would begin.
//
// Records are forward-linked (no back-pointer), so the final record cannot be located from
// the extent without walking; this therefore does NOT re-verify record CRCs. It does not need
// to: the extent is written only at seal, after the segment is complete and flushed, and the
// checks here catch the failures that matter -- a truncated file (extent past the mapping) and
// a sidecar belonging to a different segment (extent landing mid-record). Anything suspicious
// falls back to the full scan, which does verify every record.
func recExtentEndsCleanly(data []byte, used int) bool {
	if used <= 0 || used > len(data) || used%8 != 0 {
		return false
	}
	if used == len(data) {
		return true
	}
	if used+recHeaderSize > len(data) {
		return false // no room for another header: treat as suspicious, rescan
	}
	// Unwritten arena tail reads as a zero-length record.
	return recTotalLen(data, uint32(used)) == 0
}

// segFirstSeq returns the commit sequence of a segment's first record, which is its position
// in append order. ok is false for an empty segment.
func segFirstSeq(seg *segment) (uint64, bool) {
	if seg == nil || seg.used < recHeaderSize {
		return 0, false
	}
	return recSeq(seg.data, 0), true
}
