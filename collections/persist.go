package collections

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	r.mu.Lock()
	id := r.next
	r.next++
	r.byID[id] = codec
	r.idOf[codec] = id
	dir := r.dir
	r.mu.Unlock()
	if dir == "" {
		return id, nil // in-memory: nothing to persist
	}
	if err := writeDictFile(filepath.Join(dir, fmt.Sprintf("%d.zst", id)), dict); err != nil {
		return id, err
	}
	return id, nil
}

// writeDictFile writes a dictionary's bytes to path and fsyncs it (so a codec that
// segments already reference cannot be lost across a crash).
func writeDictFile(path string, dict []byte) error {
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
		sh.alloc = func(id uint32, size int, codec Codec) (*segment, error) {
			n := atomic.AddUint64(&counter, 1)
			dictID, ok := c.dicts.idFor(codec)
			if !ok {
				// Every codec new writes/compaction use comes from currentCodec or
				// RetrainDict, both registered; fall back to the base codec's id.
				dictID = 0
			}
			path := filepath.Join(shardDir, fmt.Sprintf("seg-%d.d%d.dat", n, dictID))
			return newMmapSegment(id, size, codec, path)
		}
	}
	// Indexes are derived state, not persisted: build them over the recovered
	// segments so a reopened collection's queries are immediately selective.
	if c.spec.Load().any() {
		c.Reindex()
	}
	// The maintained ordered indexes are likewise derived: rebuild them from the
	// recovered ads so a reopened collection's Ordered() is immediately correct.
	c.rebuildOrdered()
	return c, nil
}

// loadShard mmaps the existing segment files under shardDir (in file-number order,
// which is commit order), scans each for its written extent, and rebuilds the
// shard's directory. Returns the highest segment file number seen.
func (c *Collection) loadShard(sh *shard, shardDir string) (uint64, error) {
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
		seg, err := openMmapSegment(uint32(len(sh.segs)), codec, f, int(st.Size()))
		if err != nil {
			f.Close()
			return 0, err
		}
		// The write extent is the run of records from offset 0 until an unwritten
		// (zero totalLen) or out-of-bounds record — the file is zero-initialized.
		used := 0
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
		seg.used = used
		seg.synced = used
		sh.segs = append(sh.segs, seg)
	}
	c.rebuildDir(sh) // sets sh.act to the last (active) segment
	// Map each sealed segment's existing sidecar directly (skip the active append target,
	// which stays in-RAM): the reopen restores the index by mmapping it instead of
	// re-indexing every record. A missing/invalid/stale sidecar leaves msidx nil so the
	// Reindex that follows Open rebuilds and re-seals that segment.
	if spec := c.spec.Load(); spec != nil && spec.any() {
		for _, seg := range sh.segs {
			if seg != nil && seg != sh.act {
				c.loadSealedIndex(seg, spec)
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
			if recSuperseded(seg.data, o) == seqMax {
				b := byKey[string(recKey(seg.data, o))]
				if b.loc.seg != seg.id || b.loc.off != o {
					// A stale still-current duplicate/older version: retire it at its
					// own seq so seq <= S0 < sup is false for every scan.
					setRecSuperseded(seg.data, o, recSeq(seg.data, o))
					seg.dead += int64(total)
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

// Close flushes all committed data to disk and unmaps the collection's segment
// files. It is a no-op for an in-memory collection. The collection must not be used
// after Close.
func (c *Collection) Close() error {
	if c.dir == "" {
		return nil
	}
	var firstErr error
	for _, sh := range c.shards {
		sh.mu.Lock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if err := seg.unmap(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		sh.mu.Unlock()
	}
	return firstErr
}
