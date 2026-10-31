package collections

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
			retireDictFile(dir, id)
		}
	}
	return removed
}

// releaseEncodersExcept drops the lazily-built zstd encoder of every registered dictionary
// codec except keep (the codec new writes currently compress with). A registered dict codec
// is retained so its sealed segments stay decodable, but once it is no longer the write
// codec it only ever Decompresses; its encoder was warmed by the maintenance pass that wrote
// those segments (columnarize/retrain recompress) and then holds a multi-MB match-finder
// history that reads never touch. Releasing it lets the GC reclaim that history; the encoder
// rebuilds lazily if the codec is ever asked to compress again. Non-zstd codecs (the identity
// base) have no encoder and are skipped.
func (r *dictReg) releaseEncodersExcept(keep Codec) {
	r.mu.Lock()
	codecs := make([]Codec, 0, len(r.byID))
	for _, c := range r.byID {
		if c != keep {
			codecs = append(codecs, c)
		}
	}
	r.mu.Unlock()
	for _, c := range codecs {
		if z, ok := c.(*zstdCodec); ok {
			z.releaseEncoder()
		}
	}
}

// retireDictFile moves a no-longer-referenced dictionary into dicts/attic/ instead of
// unlinking it.
//
// Pruning decides a dictionary is dead by asking which ones the LIVE segments name, and
// a segment is live only if its file name parses as "seg-<n>.d<dict>.dat". A segment
// whose name was mangled -- by a restore, a rename, a half-finished move -- is invisible
// to that question, so its dictionary looks unreferenced and used to be deleted. The
// segment's bytes were still perfectly good and its name still repairable, but without
// the dictionary nothing could ever decode them again: a recoverable mistake became
// permanent loss, silently, at the next clean open.
//
// The attic costs one dictionary-sized file per retrain (tens of KB) and makes that
// unrecoverable. Deleting an attic file is an explicit operator decision; see Fsck,
// which reports segments whose dictionary is missing.
func retireDictFile(dir string, id uint32) {
	name := fmt.Sprintf("%d.zst", id)
	attic := filepath.Join(dir, "attic")
	if err := os.MkdirAll(attic, 0o755); err != nil {
		return // leave the dictionary in place rather than removing what we cannot keep
	}
	if err := os.Rename(filepath.Join(dir, name), filepath.Join(attic, name)); err != nil {
		// A rename within one directory should not fail; if it did, the file is still
		// there, which is the safe outcome. Never fall back to removing it.
		return
	}
}

// writeDictFile writes a dictionary's bytes to path and fsyncs it (so a codec that
// segments already reference cannot be lost across a crash).
func writeDictFile(path string, dict []byte) error {
	if writeFaultHook != nil {
		if err := writeFaultHook(path); err != nil {
			return err
		}
	}
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
	// Whether this store CONTAINS deltas is a property of its records, not of the option this
	// process was opened with, so it is recorded on disk and read back here. Without that, the
	// same directory reopened without DeltaMax serves every delta as if it were a whole ad --
	// a job ad coming back with one attribute instead of forty-five, and no error anywhere.
	// Exactly the trap <dir>/basecodec exists to close for the codec, closed the same way.
	if err := c.initDeltaMode(opts.Dir); err != nil {
		return nil, err
	}
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
	dictStart := time.Now()
	latest, err := c.loadDicts(dictsDir)
	if err != nil {
		return nil, fmt.Errorf("load dictionaries: %w", err)
	}
	c.openIdxDiag.Timing.LoadDicts = time.Since(dictStart)
	if latest != 0 {
		if codec, ok := c.dicts.codecFor(latest); ok {
			c.codec.Store(&codecHolder{codec})
		}
	}
	// A persistent append log keeps its watch epoch across a clean restart. The marker is
	// consumed BEFORE any segment is mapped or any seq issued, so a crash from here on
	// rotates the epoch instead of letting recovery reissue seqs a cursor already covers.
	var watchHW []uint64
	if c.hub != nil && opts.AppendOnly {
		if watchHW, err = c.claimWatchEpoch(opts.Dir); err != nil {
			return nil, err
		}
	}
	// Each shard's durable horizon, which its recovery reads stamps against (see horizon.go).
	horizons := readHorizons(opts.Dir, len(c.shards))
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
		sh.horizon.Store(horizons[i])
		maxNum, err := c.loadShard(sh, shardDir)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("recover shard %d: %w", i, err), c.releaseLoadedSegments())
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
	// Never reissue a seq the previous process issued, even if its record is gone from disk
	// (Truncate, or a shard emptied by Rotate): a resumed cursor would skip the new record.
	for i, hw := range watchHW {
		if sh := c.shards[i]; hw > sh.commitSeq {
			sh.commitSeq = hw
		}
	}
	// Nothing records what an earlier process reclaimed, so a paged-read cursor issued before
	// this open cannot be trusted to find the versions it sees: start the reclamation floor at
	// the recovered sequence, which refuses every such cursor except one whose shard has not
	// moved since (see seqlease.go). A transaction never spans an open, so conflictSince is
	// unaffected.
	for _, sh := range c.shards {
		sh.gcFloor = max(sh.gcFloor, sh.commitSeq)
	}
	// The append floor is not persisted; derive it from what recovery found. With the
	// high-water mark applied, a shard Truncated (or rotated empty) before a clean Close
	// gets commitSeq as its floor, so a cursor from before the truncate still Resets.
	for _, sh := range c.shards {
		if sh.appendOnly {
			sh.appendFloor = sh.droppedFloorLocked()
		}
	}
	// Recovery is complete: every live segment's codec is known, so dictionaries only
	// history references (loadDicts loads the full on-disk set) can be dropped now
	// instead of holding an inflatable codec each for the life of the process.
	c.pruneDicts()
	// Indexes are derived state, not persisted: build them over the recovered
	// segments so a reopened collection's queries are immediately selective.
	if c.spec.Load().any() {
		reindexStart := time.Now()
		c.Reindex()
		c.openIdxDiag.Timing.Reindex = time.Since(reindexStart)
	}
	// The maintained ordered indexes are likewise derived: rebuild them from the
	// recovered ads so a reopened collection's Ordered() is immediately correct.
	orderedStart := time.Now()
	c.rebuildOrdered()
	c.openIdxDiag.Timing.RebuildOrdered = time.Since(orderedStart)
	// If sealed segments recovered persisted columnar blocks, re-enable schema-scan from them
	// (adopt-from-sidecar) so the accelerator is live immediately -- no re-sample, no rebuild.
	adoptStart := time.Now()
	c.adoptPersistedSchemaScan()
	c.openIdxDiag.Timing.AdoptSchema = time.Since(adoptStart)
	// Recorded query demand is checkpointed alongside the data, so index decisions do not
	// restart from zero every time the process does.
	demandStart := time.Now()
	c.loadDemand()
	c.openIdxDiag.Timing.LoadDemand = time.Since(demandStart)
	// Verify the seal-collapse invariant rather than continuing to assume it. Every collapse
	// and every pre-compaction pass is built on "a live fragment can only be in an active
	// segment", and nothing has ever checked it -- while a stranded one is invisible to those
	// passes, loses its base to the next compaction, and leaves the key permanently unreadable.
	// Done here because the segments are mapped, no reader is waiting, and the cost is bounded.
	if stranded := c.checkSealCollapseInvariant(openStrandedRepairMax); len(stranded) > 0 {
		c.openIdxDiag.StrandedSealedDeltas = len(stranded)
		c.openIdxDiag.StrandedSealedKeys = stranded
		if len(c.openIdxDiag.StrandedSealedKeys) > openStrandedSampleMax {
			c.openIdxDiag.StrandedSealedKeys = c.openIdxDiag.StrandedSealedKeys[:openStrandedSampleMax]
		}
		// Rescue the ones whose base is still there. Reported first, so the diagnostic reflects
		// what was FOUND rather than what survived the repair -- otherwise a successful repair
		// would erase its own evidence and the count would read zero forever.
		c.repairStrandedSealedDeltas(stranded)
	}
	// From here on every commit leaves the record stream recovery reads stamps against; make what
	// recovery found durable and record it as each shard's horizon (see horizon.go).
	if err := c.persistOpenHorizons(); err != nil {
		return nil, errors.Join(err, c.releaseLoadedSegments())
	}
	// Every phase that walks records has run by now, so the walk count is final.
	c.openIdxDiag.Timing.RecordsWalked = int(c.recWalked.Load())
	// Emit the sidecar-adoption summary gathered during recovery (before the Reindex above
	// rebuilds whatever was not adopted), so a slow reopen can be traced to its cause.
	if OpenIndexDiagHook != nil {
		d := c.openIdxDiag
		d.Dir = c.dir
		OpenIndexDiagHook(d)
	}
	return c, nil
}

// releaseLoadedSegments unmaps every segment recovery has installed so far, for an Open that fails
// part way. The files are untouched; only this process's mappings and descriptors go.
func (c *Collection) releaseLoadedSegments() error {
	var errs []error
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg != nil {
				errs = append(errs, seg.closeUnmap())
			}
		}
		sh.segs = nil
		sh.act = nil
	}
	return errors.Join(errs...)
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
	// Per-shard phase timing, folded into the collection's open diagnostic when the shard finishes,
	// so a slow reopen names the phase (map segments / rebuild directory / recompute zones / ...).
	var tm OpenTiming
	defer func() { c.openIdxDiag.Timing.add(tm) }()
	mapStart := time.Now()
	// Each sealed segment's sidecar trailer names where its trailer records are -- the attribute
	// dictionary and the columnar payload -- and its key-index flags say which of them the segment
	// does not carry at all. All three spare the searches below a walk over the segment's records,
	// and are collected while mapping and consumed once every segment is in place.
	segDictHint := map[*segment]uint32{}
	segColHint := map[*segment]uint32{}
	segFlags := map[*segment]uint16{}
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
			continue
		}
		// A ".dat" that does not parse is a segment whose name was mangled. Recovery
		// cannot load it (the dictionary it needs is named only here), but saying
		// nothing is worse: the data is intact and the name repairable, while silence
		// lets the next prune retire the dictionary that reads it.
		c.openIdxDiag.ignoreFile(filepath.Join(shardDir, e.Name()))
	}
	sort.Slice(files, func(i, j int) bool { return files[i].num < files[j].num })
	mapProg := newOpenProgress(shardDir, OpenMapSegments, len(files))

	// Segments are collected first and ordered afterwards by CONTENT (see below), so the
	// id assigned here is provisional.
	type loadedSeg struct {
		seg *segment
		num uint64
	}
	var loaded []loadedSeg

	for sfi, sf := range files {
		mapProg.step(sfi)
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
		// Everything between here and the end of loadShard that touches this mapping --
		// the extent walk below, rebuildDir, a zone recompute -- reads it front to back,
		// so hint for that now and reset it once the open is done.
		seg.adviseSequential()
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
		// A v5 sidecar also names the segment's damaged records, which are skipped from here on
		// (see damage.go); the set must fit the segment or the extent is not trusted either.
		facts := readSidecarTrailer(seg.path + ".idx")
		extent, dictRec, dmg := facts.used, facts.dictRec, facts.damage
		segDictHint[seg] = dictRec
		segColHint[seg] = facts.colRec
		segFlags[seg] = facts.flags
		if rec := extent; rec > 0 && rec <= len(seg.data) &&
			recExtentEndsCleanly(seg.data, rec) && damageFits(seg.data, rec, dmg) {
			used = rec
			if dmg != nil {
				seg.damage.Store(dmg)
				damagedRecordsSkipped.Add(int64(dmg.len()))
			}
		} else {
			used = c.recoverExtent(seg)
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
	tm.MapSegments = time.Since(mapStart)
	// Fast reopen: a clean Close leaves a directory snapshot; restore from it instead
	// of scanning every record (rebuildDir). It is validated against the loaded
	// segment set and falls back to the full scan on any mismatch. Chained
	// collections are excluded (their per-parent child counts would also need
	// rebuilding); a stale snapshot in that case is discarded, not trusted.
	// Time travel also forces the full scan: rebuildDir is what rebuilds the per-shard
	// time->seq checkpoint index (and the segment scan-pruning counters) from the
	// segment markers, which the directory snapshot does not carry.
	dirStart := time.Now()
	if sh.childParentHash != nil || c.timeTravel() != nil {
		os.Remove(filepath.Join(shardDir, dirSnapName))
		c.rebuildDir(sh)
		tm.DirRebuilt = true
	} else if !c.tryLoadDirSnapshot(sh, shardDir) {
		c.rebuildDir(sh) // sets sh.act to the last (active) segment
		tm.DirRebuilt = true
	}
	c.openIdxDiag.ChainLinksRepaired += c.relinkActiveChains(sh)
	tm.DirRestore = time.Since(dirStart)
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
	dictStart := time.Now()
	for _, seg := range sh.segs {
		if seg != nil {
			seg.ct.opened.Store(true) // its CurrentTime proof, if any, comes from its sidecar (ctproof.go)
			c.recWalked.Add(int64(publishSegDictAt(seg, segDictHint[seg], segFlags[seg])))
			markSegDictNeed(seg)
		}
	}
	tm.PublishDict = time.Since(dictStart)
	// Then each columnarized segment's columnar payload, which is DURABLE data rather than a cache:
	// its records were written without the attributes it holds, so a segment whose payload is not
	// published reads as an ad missing half its attributes and nothing about the result says so.
	// Publication must follow the dictionary loop above -- an interned segment's payload is
	// translated through its dictionary -- and must cover every segment, because a reader cannot ask
	// whether the format was recognised, only for the ad.
	//
	// A payload in a format version this build cannot read fails the open. The alternative is a
	// collection whose scans skip that segment's records without saying so, and nothing in this
	// process could rebuild what they are missing.
	colStart := time.Now()
	for _, seg := range sh.segs {
		if seg != nil {
			if err := publishColNative(c, seg, segColHint[seg], segFlags[seg]); err != nil {
				return 0, err
			}
		}
	}
	tm.PublishColumns = time.Since(colStart)
	if sh.act != nil && sh.act.dict.Load() != nil {
		sh.act = nil
	}
	spec := c.spec.Load()
	sidecarStart := time.Now()
	// Counts sealed segments, not slots: sh.segs is sparse (rotation nils reclaimed slots), so
	// len(sh.segs) would overstate the total and make a finished phase read as half done.
	sidecarTotal := 0
	for _, seg := range sh.segs {
		if seg != nil && seg != sh.act {
			sidecarTotal++
		}
	}
	sidecarProg := newOpenProgress(shardDir, OpenLoadSidecars, sidecarTotal)
	sidecarDone := 0
	for _, seg := range sh.segs {
		if seg != nil && seg != sh.act {
			sidecarProg.step(sidecarDone)
			sidecarDone++
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
			// Damage found by this open, in a segment whose existing sidecar was adopted, goes into
			// that sidecar now: the next open may trust its extent, and must then know what to skip.
			// A segment with no adopted sidecar gets one, with its damage, when it is sealed.
			if seg.damageUnsaved && seg.keyIdx.Load() != nil {
				c.persistSegDamage(seg)
			}
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
			// Fall back to rebuilding only when the sidecar carried no zone map it can trust
			// (written before v3, in an older section format, or with none configured at the
			// time). This decodes every record of the
			// segment, so it is the phase that turns a segment-count reopen into a record-count
			// one -- timed separately (ZoneRecompute) so the log can pin it.
			if sh.appendOnly && len(sh.zoneAttrs) > 0 && seg.zones == nil {
				zoneStart := time.Now()
				seg.zones = computeSegZones(c, seg, seg.used, sh.zoneAttrs, sh.zoneInline)
				tm.ZoneRecompute += time.Since(zoneStart)
				tm.ZoneRecomputed++
				// Write the freshly computed zone map back into the sidecar (a legacy sidecar
				// predating zone persistence), so the next open ADOPTS it instead of decoding
				// every record again -- otherwise this recompute recurs on every start forever
				// (sealSegmentIndex never revisits an already-sealed segment). One-time per
				// segment; this open already paid the decode.
				if c.persistSegZonesUpgrade(seg) {
					tm.ZoneUpgraded++
				} else {
					tm.ZoneUpgradeFailed++
				}
			}
			// Record in the sidecar where this segment's trailer records are, or that it has
			// none, when its sidecar predates those fields. The searches above ran over every
			// record of the segment to establish it; writing it back is what stops the next open
			// doing the same (see persistSegTrailerHintsUpgrade).
			if loaded && segTrailerHintsStale(seg, segColHint[seg], segFlags[seg]) {
				if c.persistSegTrailerHintsUpgrade(seg, segColHint[seg]) {
					tm.TrailerFlagsUpgraded++
				} else {
					tm.TrailerFlagsUpgradeFailed++
				}
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
	tm.LoadSidecars = time.Since(sidecarStart)
	// Every front-to-back pass over these mappings is finished; from here on access is
	// by key, so drop the sequential hint.
	for _, seg := range sh.segs {
		if seg != nil {
			seg.adviseNormal()
		}
	}
	return maxNum, nil
}

// rebuildDir reconstructs a shard's directory + commit sequence from its segments
// by replaying records: for each key the record with the greatest seq is its
// current version, live iff it is not superseded (a key whose latest record was
// tombstoned by a delete is absent). Chains are rebuilt fresh.
//
// It decides from record headers, which a damaged record's are not fit for: a damaged key that
// matches a live one would win it with a damaged seq and have the live record superseded on disk
// below. So when two records claim one key, both are checked against their CRC before either is
// believed, and a damaged one joins its segment's damage set. A walk that does not land exactly on
// its segment's extent was misframed by a damaged totalLen; the segment's damage is then derived
// again (reframeSegment). Either finding restarts the replay, so every decision is made by a walk
// that skips all the damage known.
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
	type mark struct{ seq, millis uint64 }
	// A stamped record whose stamp recovery has to account for, and a delete marker that may have
	// to be replayed (see horizon.go). Collected only when the shard has a horizon; only stamps
	// and markers above it qualify.
	type stamped struct {
		l   loc
		key string
		sup uint64
	}
	type delMark struct {
		key  string
		hash uint64
		seq  uint64
	}
	// byKey holds each key's winning version, keyed by the key's HASH rather than by the key
	// itself: a map[string] pays a string header plus the key bytes for every live key, and on a
	// store with millions of them that is the largest thing recovery allocates. The hash is
	// already what sh.dir is keyed by, so nothing below has to derive it a second time.
	//
	// A hash is not an identity, so collide holds the entries of any key whose hash another key
	// already claimed. Letting a collision merge two keys would silently drop one of them, which
	// is data loss, not a slow path -- so the two are distinguished by comparing the key bytes,
	// which is free: the only place a collision can be introduced already reads the stored
	// record to verify its checksum. collide stays nil on every store that has no collision,
	// which is almost all of them.
	var (
		byKey    map[uint64]best
		collide  map[string]best
		maxSeq   uint64
		marks    []mark
		stamps   []stamped
		delMarks []delMark
		lastSeq  uint64              // newest record of the stream, markers included
		lastKeys map[string]struct{} // keys the commit at lastSeq wrote or deleted
		touched  []supRef            // supersession fields recovery rewrote, flushed at the end
	)
	hz := sh.horizon.Load()
	hzOK := hz != horizonUnknown
	noteLast := func(seq uint64, key string) {
		if seq > lastSeq {
			lastSeq = seq
			clear(lastKeys)
		}
		if seq == lastSeq && key != "" {
			lastKeys[key] = struct{}{}
		}
	}
	walked := 0
	defer func() { c.recWalked.Add(int64(walked)) }()
	intact := map[loc]bool{} // CRC results, by record
	recIntact := func(l loc) bool {
		seg := sh.segs[l.seg]
		if !seg.persistent {
			return true // a RAM segment carries no CRC
		}
		ok, seen := intact[l]
		if !seen {
			ok = recVerifyCRC(seg.data, l.off)
			intact[l] = ok
		}
		return ok
	}
	// keyOf reads the key of the record a best names. Every caller below is already reading
	// that record for another reason, so the hash map costs no extra page touch.
	keyOf := func(b best) []byte { return recKey(sh.segs[b.loc.seg].data, b.loc.off) }
	// bestFor returns the winner recorded for a key, telling a repeat of the same key apart
	// from a different key that merely hashes the same.
	bestFor := func(h uint64, key []byte) (best, bool) {
		b, ok := byKey[h]
		if !ok {
			return best{}, false
		}
		if bytes.Equal(keyOf(b), key) {
			return b, true
		}
		b, ok = collide[string(key)] // not this key's slot: a collision, or nothing
		return b, ok
	}
	// setBest records b as the winner for a key, sending it to the overflow if another key
	// already holds its hash.
	setBest := func(h uint64, key []byte, b best) {
		if cur, ok := byKey[h]; ok && !bytes.Equal(keyOf(cur), key) {
			if collide == nil {
				collide = make(map[string]best)
			}
			collide[string(key)] = b
			return
		}
		byKey[h] = b
	}
	reframed := map[*segment]int{}
	for {
		byKey, collide = make(map[uint64]best), nil
		maxSeq = 0
		marks = marks[:0]
		stamps, delMarks = stamps[:0], delMarks[:0]
		lastSeq, lastKeys = 0, map[string]struct{}{}
		changed := false
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			seg.minSeq, seg.maxSeq, seg.maxSup, seg.dead = 0, 0, 0, 0
			dm := seg.damaged()
			off := 0
			for off < seg.used {
				o := uint32(off)
				if end, ok := dm.skip(o); ok {
					off = int(end)
					continue
				}
				total := recTotalLen(seg.data, o)
				if total == 0 || total%8 != 0 || off+int(total) > seg.used {
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
				// Note the local maxSeq above is the SHARD's high-water mark; seg.maxSeq is
				// this segment's, which resume-from-sequence scans use to skip it whole.
				seg.noteWalkedRec(o, total, false)
				// A time-checkpoint marker feeds the shard time index; it never enters the
				// directory. Only a time checkpoint feeds the index: a dictionary or columnar
				// record is a marker too, and its body read as millis put a sequence-0 checkpoint
				// at an arbitrary time into it.
				walked++
				if recIsMarker(seg.data, o) {
					if recIsTimeMarker(seg.data, o) {
						marks = append(marks, mark{seq, recMarkerMillis(seg.data, o)})
					}
					off += int(total)
					if hzOK && seq > hz {
						if k, ok := recDeleteMarkerKey(seg.data, o); ok {
							noteLast(seq, string(k))
							delMarks = append(delMarks, delMark{string(k), c.h.Hash(k), seq})
						} else if !recIsDict(seg.data, o) && !recIsCol(seg.data, o) {
							noteLast(seq, "") // a time checkpoint carries its commit's sequence
						}
					}
					continue
				}
				k := recKey(seg.data, o)
				h := c.h.Hash(k)
				if hzOK && (seq > hz || (sup != seqMax && sup > hz)) {
					// Only a record above the horizon needs its key as a string. Materializing
					// one for every record walked was the rest of this pass's allocation.
					key := string(k)
					if seq > hz {
						noteLast(seq, key)
					}
					if sup != seqMax && sup > hz {
						stamps = append(stamps, stamped{loc{seg.id, o}, key, sup})
					}
				}
				cand := loc{seg.id, o}
				b, seen := bestFor(h, k)
				if seen {
					// Two records claim the key. Whichever wins supersedes the other on disk, so
					// neither is believed until its checksum is.
					for _, l := range [2]loc{b.loc, cand} {
						if !recIntact(l) {
							c.noteDamagedRecord(sh.segs[l.seg], l.off)
							changed = true
						}
					}
				}
				if !seen || seq >= b.seq {
					setBest(h, k, best{cand, seq, sup})
				}
				off += int(total)
			}
			if off != seg.used {
				// A damaged totalLen stepped the walk off the records' framing.
				reframed[seg]++
				c.reframeSegment(seg, reframed[seg] > 1)
				changed = true
			}
		}
		// Undo every stamp above the horizon whose commit did not survive: one above the newest
		// surviving record, or one at it on a key that commit left nothing for. The key's
		// previous version is then current again, and the next pass sees it so.
		if !changed {
			for _, st := range stamps {
				if st.sup < lastSeq {
					continue
				}
				if _, ok := lastKeys[st.key]; ok && st.sup == lastSeq {
					continue
				}
				seg := sh.segs[st.l.seg]
				setRecSuperseded(seg.data, st.l.off, seqMax)
				touched = append(touched, supRef{seg, st.l.off})
				c.openIdxDiag.SupersessionsUndone++
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	// Replay delete markers whose stamp did not survive: the key's newest surviving version
	// predates the delete and is still current.
	for _, dm := range delMarks {
		b, ok := bestFor(dm.hash, []byte(dm.key))
		if !ok || b.seq >= dm.seq || b.superseded != seqMax {
			continue
		}
		seg := sh.segs[b.loc.seg]
		seg.supersedeRec(b.loc.off, dm.seq)
		touched = append(touched, supRef{seg, b.loc.off})
		b.superseded = dm.seq
		setBest(dm.hash, []byte(dm.key), b)
		c.openIdxDiag.DeletesReplayed++
	}
	// What recovery rewrote must be on disk before the horizon written at the end of Open says
	// so, or a second crash could bring back a stamp this recovery decided against.
	for _, t := range touched {
		if err := t.seg.msyncRange(int(t.off)+recSupOff, int(t.off)+recSupOff+8); err != nil && isDurabilityFailure(err) {
			sh.syncErr.CompareAndSwap(nil, &syncFail{err})
		}
	}
	for _, m := range marks {
		sh.tseq.record(m.seq, m.millis)
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
		dm := seg.damaged()
		for off := 0; off < seg.used; {
			o := uint32(off)
			if end, ok := dm.skip(o); ok {
				off = int(end)
				continue
			}
			total := recTotalLen(seg.data, o)
			if total == 0 {
				break
			}
			if recIsMarker(seg.data, o) {
				off += int(total) // marker, not a data record
				continue
			}
			if recSuperseded(seg.data, o) == seqMax {
				k := recKey(seg.data, o)
				b, ok := bestFor(c.h.Hash(k), k)
				if !ok || b.loc.seg != seg.id || b.loc.off != o {
					// A stale still-current duplicate/older version: retire it at its
					// own seq so seq <= S0 < sup is false for every scan.
					seg.supersedeRec(o, recSeq(seg.data, o))
				}
			}
			off += int(total)
		}
	}

	sh.dir = make(map[uint64]loc, len(byKey)+len(collide))
	// The last segment becomes the write target, and its records' links are not touched here:
	// relinkActiveChains, which runs after either restore path, owns them. Relinking a current
	// record in it to the other current records of its bucket, as is done for the sealed ones,
	// cut it off from the key's older versions in the same segment -- the delta records a read
	// merges -- and wrote that into the file.
	actID := noSeg
	if n := len(sh.segs); n > 0 && sh.segs[n-1] != nil {
		actID = sh.segs[n-1].id
	}
	count := 0
	// The hash is the map key now, so no entry has to be re-hashed on the way into sh.dir --
	// which is keyed by the same hash.
	addDir := func(h uint64, b best) {
		if b.superseded != seqMax {
			return // latest version was superseded (deleted) -> key absent
		}
		count++
		if b.loc.seg == actID {
			return
		}
		setRecNext(sh.segs[b.loc.seg].data, b.loc.off, dirGetOr(sh.dir, h))
		sh.dir[h] = b.loc
	}
	for h, b := range byKey {
		addDir(h, b)
	}
	for keyStr, b := range collide {
		addDir(c.h.Hash([]byte(keyStr)), b) // empty on a store with no hash collision
	}
	sh.count = count
	if len(sh.segs) > 0 {
		sh.act = sh.segs[len(sh.segs)-1]
	}
}

// relinkActiveChains rebuilds the bucket chains inside the write-target segment after either
// restore path, and returns how many links between that segment's records it had to change.
//
// Every by-key walk (findCurrent, findVisible, conflictSince, materializeAtWhy) follows a bucket's
// chain from the directory and then probes every OTHER segment by key index, which skips the
// write target by construction. So the chain is the only route to a record in the write target,
// and it must reach every one of them: a delta record a read merges sits there, usually behind
// the key's current record. Within one segment records are appended in commit order and each was
// linked to its bucket's head when written, so the correct chain is simply the bucket's records
// in descending offset. That is what this writes. The oldest record of each bucket is linked to
// nothing: what lies beyond it is in other segments, which the probe covers, and a link out of the
// segment could lead back into it through a link an older build wrote. The directory head of
// each such bucket becomes its newest record here.
//
// On a store written by this build nothing between records of the segment changes, so a non-zero
// count means an earlier recovery had relinked them -- reads of the affected keys merged too few
// versions until now -- and is reported as ChainLinksRepaired. Links are only written when they
// differ, so a healthy store's pages are not dirtied. Caller holds the shard lock (Open).
func (c *Collection) relinkActiveChains(sh *shard) int {
	act := sh.act
	if act == nil || sh.appendOnly || sh.dir == nil {
		return 0
	}
	newest := make(map[uint64]uint32) // bucket -> offset of its newest record so far
	repaired := 0
	dm := act.damaged()
	for off := 0; off < act.used; {
		o := uint32(off)
		if end, ok := dm.skip(o); ok {
			off = int(end)
			continue
		}
		total := recTotalLen(act.data, o)
		if total == 0 || total%8 != 0 || off+int(total) > act.used {
			break
		}
		off += int(total)
		if recIsMarker(act.data, o) {
			continue
		}
		h := c.h.Hash(recKey(act.data, o))
		want := noLoc
		prev, ok := newest[h]
		if ok {
			want = loc{seg: act.id, off: prev}
		}
		if got := recNext(act.data, o); got != want {
			// Only a link between two of this segment's records is a repair; the bucket's
			// oldest record pointing out of the segment is how a healthy chain looks.
			if ok || got.seg == act.id {
				repaired++
			}
			setRecNext(act.data, o, want)
		}
		newest[h] = o
	}
	for h, o := range newest {
		sh.dir[h] = loc{seg: act.id, off: o}
	}
	return repaired
}

// noteWalkedRec folds the record at o, of length total, into the segment's scan-pruning counters,
// for a walk that rebuilds them from the record headers (the counters start at zero). It derives
// exactly what the append and supersede paths maintain at runtime:
//   - minSeq/maxSeq span every record's sequence, time markers included (appendMarker counts
//     them). A dictionary or columnar record is left out: it is written with sequence 0 whatever
//     the segment holds and is never visible, so counting it would only zero minSeq -- the
//     "unknown" value, which disables every sequence skip for the segment. The append paths
//     that write those records do not count them either.
//   - dead is the bytes of every superseded record plus every keyless one (markers, dictionary,
//     columnar), and maxSup the largest supersession stamped here, as supersedeRec and the
//     keyless appends keep them. An append-only walk leaves both alone, as it always has: its
//     records are never superseded.
func (s *segment) noteWalkedRec(o, total uint32, appendOnly bool) {
	marker := recIsMarker(s.data, o)
	// colFlag alone also marks a columnarized segment's stripped records (recIsStripped), which
	// are ordinary keyed records; only with markerFlag is it the payload.
	if !marker || (!recIsDict(s.data, o) && !recIsCol(s.data, o)) {
		seq := recSeq(s.data, o)
		if s.minSeq == 0 || seq < s.minSeq {
			s.minSeq = seq
		}
		if seq > s.maxSeq {
			s.maxSeq = seq
		}
	}
	if appendOnly {
		return
	}
	if marker {
		s.dead += int64(total)
		return
	}
	if sup := recSuperseded(s.data, o); sup != seqMax {
		s.dead += int64(total)
		if sup > s.maxSup {
			s.maxSup = sup
		}
	}
}

// walkSegBounds derives seg's scan-pruning counters from its record headers alone: the values
// rebuildDir (or, for an append-only shard, rebuildAppendLog) arrives at, without the per-key
// replay and dedup that make those walks expensive. For a shard whose directory came from a
// snapshot that does not carry the counters. The walk steps the way rebuildDir's does, skipping
// the segment's damaged records.
func walkSegBounds(seg *segment, appendOnly bool) {
	seg.minSeq, seg.maxSeq = 0, 0
	if !appendOnly {
		seg.maxSup, seg.dead = 0, 0
	}
	dm := seg.damaged()
	for off := 0; off < seg.used; {
		o := uint32(off)
		if end, ok := dm.skip(o); ok {
			off = int(end)
			continue
		}
		total := recTotalLen(seg.data, o)
		if total == 0 || total%8 != 0 || off+int(total) > seg.used {
			break
		}
		seg.noteWalkedRec(o, total, appendOnly)
		off += int(total)
	}
}

// noteDamagedRecord adds the record at off, found failing its checksum, to its segment's damage set.
// The span ends where the record's own totalLen says: the caller has walked the segment by those
// lengths to its extent (or will, and reframes it if not), which is how every other walk steps too.
func (c *Collection) noteDamagedRecord(seg *segment, off uint32) {
	dm := seg.damaged()
	if dm.covers(off) {
		return
	}
	seg.damage.Store(dm.with(damageSpan{off, off + recTotalLen(seg.data, off)}))
	seg.damageUnsaved = true
	damagedRecordsSkipped.Add(1)
	c.openIdxDiag.notePastDamage(OpenDamageStop{Path: seg.path, Offset: int(off), Damaged: 1, Recovered: true})
}

// reframeSegment re-derives the damage of a segment whose header walk did not land on its extent: a
// damaged totalLen inside a trusted extent misframes every walk that steps over it. The sidecar key
// index, which lists every record's offset, delimits each record; one that does not frame to the next
// or does not verify is damaged. Without a key index that describes the segment, the extent is not
// trusted after all, and the segment is recovered as if it had none. last cuts the segment back to
// the records the recovery walk verifies, for a segment that misframed again after reframing.
func (c *Collection) reframeSegment(seg *segment, last bool) {
	known := seg.damaged()
	if last {
		seg.damage.Store(nil)
		seg.used, _, _ = recoveryExtent(seg.data)
		seg.synced = seg.used
		seg.stoppedAtDamage = true
		return
	}
	if ki := sidecarKeyIndex(seg); ki != nil {
		if spans, ok := keyIndexDamage(seg.data, seg.used, ki, c.h, known); ok && len(spans) > 0 {
			dm := newDamageSet(spans)
			seg.damage.Store(dm)
			seg.damageUnsaved = true
			if n := dm.len() - known.len(); n > 0 {
				damagedRecordsSkipped.Add(int64(n))
			}
			c.openIdxDiag.notePastDamage(OpenDamageStop{Path: seg.path, Offset: int(spans[0].start),
				Damaged: dm.len(), Recovered: true})
			return
		}
	}
	seg.damage.Store(nil)
	seg.used = c.recoverExtent(seg)
	seg.synced = seg.used
}

// rebuildAppendLog restores an append-only shard on reopen. An append log has no per-key
// supersession or directory, so the single-current-version dedup that rebuildDir performs for a
// mutable shard must be skipped: every on-disk record stays live (duplicate keys are intentional
// here). It only recovers the commit sequence, the per-segment min sequence, the live count, and
// the active (last) segment for continued appends. A segment whose walk is misframed is reframed
// and walked again, as in rebuildDir.
func (c *Collection) rebuildAppendLog(sh *shard) {
	var maxSeq uint64
	count := 0
	walked := 0
	defer func() { c.recWalked.Add(int64(walked)) }()
	for _, seg := range sh.segs {
		if seg == nil {
			continue
		}
		for attempt := 1; ; attempt++ {
			segMax, segCount := maxSeq, 0
			seg.minSeq, seg.maxSeq = 0, 0
			dm := seg.damaged()
			off := 0
			for off < seg.used {
				o := uint32(off)
				if end, ok := dm.skip(o); ok {
					off = int(end)
					continue
				}
				total := recTotalLen(seg.data, o)
				if total == 0 || total%8 != 0 || off+int(total) > seg.used {
					break
				}
				seq := recSeq(seg.data, o)
				if seq > segMax {
					segMax = seq // the shard's high-water mark
				}
				seg.noteWalkedRec(o, total, true) // this segment's bounds, for resume-from-sequence pruning
				if !recIsMarker(seg.data, o) {
					segCount++ // append-only records are never superseded, so all are live
				}
				walked++
				off += int(total)
			}
			if off != seg.used {
				c.reframeSegment(seg, attempt > 1)
				continue
			}
			maxSeq = segMax
			count += segCount
			break
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
//
// It does not wait for readers and does not defer: the write-out is what the returned error
// reports on, so it happens before Close returns. A reader in flight keeps the segments it
// has pinned (their unmap is deferred to its last unpin) and finds the rest of the
// collection empty, which is a short answer rather than a fault. A second Close does
// nothing.
func (c *Collection) Close() error {
	if !c.gate.closeNow() {
		return nil
	}
	return c.teardown(false)
}

// Discard is Close for a collection whose files are being DELETED (a dropped table): it
// releases the mappings without flushing anything, writing a directory snapshot, sealing
// sidecar indexes or vouching for the watch epoch.
//
// Skipping that work is not only an optimization. The caller removes the directory right
// after, and the teardown may be deferred behind a reader for an unbounded time, by which
// point a table of the same name may have recreated the very paths a late dir-snapshot
// write would land on. Writing nothing is what makes deferring safe.
func (c *Collection) Discard() error {
	if c.gate.beginClose(func() { _ = c.teardown(true) }) {
		return nil
	}
	return c.teardown(true)
}

// teardown unmaps the collection's segments and empties its shards. discard skips every
// persistence step (see Discard). It runs at most once per collection, either from
// Close/Discard directly or, for a deferred Discard, from the last reader on its way out.
func (c *Collection) teardown(discard bool) error {
	var firstErr error
	// watchSeqs is each shard's commitSeq for the watch-epoch marker; watchClean is cleared by
	// anything that leaves doubt about what reached disk (see watchepoch.go).
	watchSeqs := make([]uint64, len(c.shards))
	watchClean := !discard && c.watchEpochDir != ""
	for i, sh := range c.shards {
		sh.mu.Lock()
		watchSeqs[i] = sh.commitSeq
		if sh.writeErr != nil || sh.syncErr.Load() != nil {
			watchClean = false
		}
		if !discard {
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
			// Delete stamps a sync pass deferred (see horizon.go) must be on disk before the
			// snapshot: its reopen reads no delete markers.
			if err := msyncSups(sh.dirtySup); err != nil && isDurabilityFailure(err) && firstErr == nil {
				firstErr = err
			}
			// Persist the directory for a scan-free reopen (best effort; skipped for a
			// chained collection -- see tryLoadDirSnapshot). Only for a persistent store.
			// Not when a walk lost a segment's framing: restoring the snapshot would skip the record walk
			// that finds and records that damage, and every later scan would come back short again.
			if c.dir != "" && sh.childParentHash == nil && !shardFramingLost(sh) {
				shardDir := filepath.Join(c.dir, fmt.Sprintf("%d", i))
				// Writing the snapshot validates every entry, which reads the record it
				// names; the entries are sorted so that runs forwards through each segment.
				// Hint for it. No reset: these mappings are unmapped a few lines below.
				for _, seg := range sh.segs {
					if seg != nil {
						seg.adviseSequential()
					}
				}
				writeDirSnapshot(sh, shardDir, c.h)
			}
		}
		sh.dirtySup = nil
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
		// Empty the shard under its own lock, the way a failed Open does
		// (releaseLoadedSegments). Unmapping alone leaves every segment still reachable
		// from sh.segs and every key still resolving through sh.dir to one, so a read that
		// arrives after the close -- a point lookup, a scan that started on an earlier
		// shard -- walks straight into the released mapping and takes the process down with
		// a runtime fault. With nothing left to find, such a read answers empty instead.
		// Segments a scan pinned stay alive through their windows until it unpins them.
		sh.dir = nil
		sh.segs = nil
		sh.act = nil
		sh.pendingSeal = nil
		sh.count = 0
		sh.mu.Unlock()
	}
	// Only now, with every segment flushed, may the epoch be vouched for across a restart.
	if watchClean && firstErr == nil {
		c.persistWatchEpoch(watchSeqs)
	}
	// The columnar-block cache is NOT closed here: c.colCache is the process-wide cache for this
	// collection's kind, shared with every other open collection of that kind, and it lives for the
	// life of the process. This collection's entries are not evicted either. Block ids are
	// process-unique, so no later read can ask for them again; they hold no more than the shared
	// byte budget and are displaced by live entries as the cache admits them, the same way the
	// blocks of a compacted or truncated segment age out.
	return firstErr
}

// shardFramingLost reports whether a walk of any of the shard's segments lost its framing.
func shardFramingLost(sh *shard) bool {
	for _, seg := range sh.segs {
		if seg != nil && seg.framingLost.Load() {
			return true
		}
	}
	return false
}

// recoverExtent derives a segment's extent when its sidecar supplies no trusted one: the recovery
// walk, continued past damage in the middle of the segment where the framing can be proven (see
// damage.go), with the damaged records stored as the segment's damage set. Where it cannot be proven
// the walk stops at the damage, and what lies past it is counted (RecordsPastDamage) rather than
// loaded.
func (c *Collection) recoverExtent(seg *segment) int {
	used, recs, crcStop := recoveryExtent(seg.data)
	c.recWalked.Add(int64(recs))
	// A torn tail has nothing after it. Records that verify past the stop are damage in the middle.
	if !crcStop {
		return used
	}
	past := surveyPastDamage(seg.data, used)
	if past.Records == 0 && !past.Dictionary && !past.ColumnarPayload {
		return used
	}
	past.Path, past.Offset = seg.path, used
	if end, spans, ok := proveFramingPastDamage(seg.data, used, snapshotPath(seg), c.h); ok {
		dm := newDamageSet(spans)
		seg.damage.Store(dm)
		seg.damageUnsaved = true
		damagedRecordsSkipped.Add(int64(dm.len()))
		past.Recovered = true
		past.Loaded = past.Records
		past.Records = 0
		past.Damaged = len(spans)
		c.openIdxDiag.notePastDamage(past)
		return end
	}
	// Not loaded: say so, rather than come back short in silence.
	seg.stoppedAtDamage = true
	c.openIdxDiag.notePastDamage(past)
	recordsPastDamage.Add(int64(past.Records + past.Damaged))
	return used
}

// proveFramingPastDamage looks for a proof of a segment's framing past a recovery walk that stopped
// at stop: a verified columnar payload after the damage, or failing that the key index of the
// segment's sidecar (at sidecarPath) when one parses and describes the segment under hasher h. It
// returns the extent and the damaged spans. Fsck runs it too, to say what a normal open will do.
func proveFramingPastDamage(data []byte, stop int, sidecarPath string, h Hasher) (int, []damageSpan, bool) {
	if end, spans, ok := recoverPastDamage(data, stop); ok {
		return end, spans, true
	}
	ki := sidecarKeyIndexAt(sidecarPath)
	if ki == nil || int(ki.upto) <= stop {
		return 0, nil, false
	}
	upto := int(ki.upto)
	if !recExtentEndsCleanly(data, upto) {
		return 0, nil, false
	}
	spans, ok := keyIndexDamage(data, upto, ki, h, nil)
	if !ok || len(spans) == 0 {
		return 0, nil, false
	}
	return upto, spans, true
}

// sidecarKeyIndex reads a segment's key index from its sidecar file, or nil when there is none that
// parses. Copied out of the file, so it does not depend on a mapping.
func sidecarKeyIndex(seg *segment) *mmapKeyIndex { return sidecarKeyIndexAt(snapshotPath(seg)) }

// sidecarKeyIndexAt is sidecarKeyIndex for a sidecar path.
func sidecarKeyIndexAt(path string) *mmapKeyIndex {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	_, key, _, _, ok := splitSegmentSidecar(data)
	if !ok {
		return nil
	}
	ki, err := parseKeyIndex(key)
	if err != nil {
		return nil
	}
	return ki
}

// recoveryExtent walks a segment from the start the way recovery does when no trusted extent is
// recorded, returning where the written data ends and how many records it had to examine to say so.
// The walk stops at an unwritten (zero) length, or at damage, which crcStop reports: a record that
// fails its checksum, or a length that cannot be one (not a multiple of 8, or running past the file).
func recoveryExtent(data []byte) (used, recs int, crcStop bool) {
	for used+recHeaderSize <= len(data) {
		total := int(recTotalLen(data, uint32(used)))
		if total == 0 {
			return used, recs, false // unwritten tail
		}
		if total%8 != 0 || used+total > len(data) {
			return used, recs, true // a damaged length
		}
		if !recVerifyCRC(data, uint32(used)) {
			return used, recs, true // torn/partial record: the durable data ends here
		}
		used += total
		recs++
	}
	return used, recs, false
}

// surveyPastDamage reports what a recovery walk that stopped at from leaves unloaded: the keyed
// records that verify after it, the keyed records that frame but fail their checksum, and whether
// the segment's dictionary or columnar payload is among them. It steps over records whose framing
// holds and resyncs past those whose framing does not, as Fsck does. Path and Offset are left to
// the caller. It runs only when the walk stopped on a checksum, so a clean open pays nothing for it.
func surveyPastDamage(data []byte, from int) OpenDamageStop {
	var out OpenDamageStop
	for off := from; off+recHeaderSize <= len(data); {
		total := int(recTotalLen(data, uint32(off)))
		framed := total > 0 && total%8 == 0 && off+total <= len(data)
		if framed && recVerifyCRC(data, uint32(off)) {
			switch {
			case recIsDict(data, uint32(off)):
				out.Dictionary = true
			case recIsCol(data, uint32(off)) && recIsMarker(data, uint32(off)):
				out.ColumnarPayload = true
			case !recIsMarker(data, uint32(off)):
				out.Records++
			}
			off += total
			continue
		}
		if framed {
			// Unverified, so its flags may be wrong; at worst a record is reported as the
			// dictionary or payload it is not, and its segment's reads fail instead of decoding.
			switch {
			case recMayBeDict(data, uint32(off)):
				out.Dictionary = true
			case recIsCol(data, uint32(off)) && recIsMarker(data, uint32(off)):
				out.ColumnarPayload = true
			case !recIsMarker(data, uint32(off)):
				out.Damaged++
			}
			off += total
			continue
		}
		next := repairResync(data, off)
		if next < 0 {
			break
		}
		off = next
	}
	return out
}

// recordsPastDamage counts records not loaded because recovery stopped at damage at or before them.
var recordsPastDamage atomic.Int64

// RecordsPastDamage reports how many keyed records, since the process started, were not loaded
// because Open's recovery walk stopped at damage in the middle of their segment: the records that
// verify after the stop, plus the damaged ones that still frame (the record the walk stopped at
// among them). It counts only stops with something readable after them. Recovery walks a
// segment whose sidecar does not supply a trusted extent and stops at the first record that fails
// its checksum; that is right for a torn tail and drops everything after damage in the middle.
// OpenIndexDiag.DamageStops names the segments. Fsck reports the damage, and Repair recovers the
// records of a segment it may rewrite.
func RecordsPastDamage() int64 { return recordsPastDamage.Load() }

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
//
// A damaged first record's seq is not believed; the first record past its damage is used instead.
func segFirstSeq(seg *segment) (uint64, bool) {
	if seg == nil || seg.used < recHeaderSize {
		return 0, false
	}
	off := uint32(0)
	if end, ok := seg.damaged().skip(0); ok {
		if int(end)+recHeaderSize > seg.used {
			return 0, false
		}
		off = end
	}
	return recSeq(seg.data, off), true
}
