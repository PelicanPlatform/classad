package collections

import (
	"sync"

	"github.com/dgraph-io/ristretto/v2"
)

// decodedStreams holds a columnar block's decompressed cold streams (cold-numeric column,
// string column, cold tail) -- what a full-ad reconstruct or an escaped/cold-field lookup
// needs. Immutable once built.
type decodedStreams struct {
	coldNum, str, cold []byte
}

func (d *decodedStreams) cost() int64 { return int64(len(d.coldNum) + len(d.str) + len(d.cold)) }

// streamKind names one of a block's three independently-compressed regions. They are cached and
// decompressed SEPARATELY because most readers want exactly one of them:
//
//	kindColdNum  the cold numeric column groups -- a cold column scan
//	kindStr      the string region              -- only a full-ad read
//	kindCold     the cold tail (row-wise)       -- an escaped value's node
//	kindStrDict  per-field string dictionaries -- a string predicate or read
//	kindStrCode  per-field string code columns -- a string predicate or read
//
// Decompressing all three together meant reading one escaped integer also inflated the string
// region, which on wide ads (a history record carries dozens of string attributes) is the largest
// of the three by far. It also spent the cache's byte budget on regions nobody read, evicting the
// ones they did.
type streamKind uint8

const (
	kindColdNum streamKind = iota
	kindStr
	kindCold
	kindStrDict
	kindStrCode
	numStreamKinds
)

// streamKey derives a per-stream cache key. Block ids come from a monotonic counter
// (colBlockSeq), so id*3+kind is unique per (block, stream).
func streamKey(id uint64, k streamKind) uint64 { return id*uint64(numStreamKinds) + uint64(k) }

// blockCache caches columnar blocks' decompressed regions, one entry PER REGION, so repeated
// full-ad reads and escaped-record lookups within a row-group decompress once, not per record.
// Per-region entries mean a reader pays only for what it reads, and the byte budget is not spent
// holding regions nobody touched. Backed by
// ristretto: byte-cost-bounded (each entry's cost is its decompressed size) and concurrent, a
// good fit for large, variable-size, hot items under parallel scans. A nil *blockCache is
// valid and simply decompresses every time (used by tests and unconfigured collections).
type blockCache struct {
	c *ristretto.Cache[uint64, []byte]
}

// newBlockCache creates a cache bounded to about maxBytes of decompressed block streams.
func newBlockCache(maxBytes int64) (*blockCache, error) {
	// NumCounters sizes ristretto's TinyLFU admission sketch + bloom, which is allocated once for
	// the cache's whole life regardless of how much is actually cached. Size it to the BUDGET
	// (~10x the number of items a full cache would hold, assuming small-ish regions) rather than a
	// fixed per-collection constant: one shared cache per kind means one right-sized sketch, not
	// one oversized sketch per collection.
	numCounters := maxBytes / 4096
	if numCounters < 1<<14 {
		numCounters = 1 << 14
	}
	c, err := ristretto.NewCache(&ristretto.Config[uint64, []byte]{
		NumCounters: numCounters,
		MaxCost:     maxBytes,
		BufferItems: 64,
	})
	if err != nil {
		return nil, err
	}
	return &blockCache{c: c}, nil
}

// The block cache is now SHARED PROCESS-WIDE, one instance per workload KIND, instead of one fixed
// 256 MB cache per collection. htcondordb opens many columnarized tables (jobs, clusters, users,
// jobsets, header, clusterprivate, logmeta, plus the history/epoch_history archives), so a
// per-collection budget made the block-cache footprint 256 MB × table_count (~1 GB on a busy host)
// with no runtime knob. Two kinds get SEPARATE global budgets because their working sets differ:
//
//	mutatingCacheKind  sharded, key-superseding tables (the job queue's mutable state)
//	archiveCacheKind   append-only history logs (condor_history / epoch archives)
//
// A shared instance per kind is preferred over per-collection caches drawing from a shared cost
// counter because it also collapses ristretto's fixed admission metadata (sized to NumCounters
// regardless of occupancy) to ONE allocation per kind -- the very overhead that drove the
// per-segment reopen heap leak, now bounded process-wide rather than growing with table/segment
// count.
//
// No key namespacing is required for the cross-collection share: block ids come from colBlockSeq, a
// PROCESS-global monotonic counter every block build increments (colblock.go / colpersist.go), so
// streamKey(b.id, k) is already unique across collections and cannot collide in a shared cache.
type blockCacheKind int

const (
	mutatingCacheKind blockCacheKind = iota
	archiveCacheKind
	numBlockCacheKinds
)

// Default per-kind global budgets: whole-process ceilings shared by every collection of the kind.
// 512 MiB each keeps the two kinds' combined ceiling near ~1 GiB independent of how many tables are
// open -- a modest bump over the old single-collection 256 MB, but a large cut from the old
// 256 MB × table_count total, and now overridable via config.
const (
	defaultMutatingCacheBytes = 512 << 20
	defaultArchiveCacheBytes  = 512 << 20
)

// sharedBlockCacheSlot lazily holds one kind's global cache and its configured budget. The budget
// may be set (via Set{Mutating,Archive}BlockCacheBudget) before OR after the cache is created: a
// later change resizes the live cache through ristretto's UpdateMaxCost, so config-vs-first-use
// ordering does not matter.
type sharedBlockCacheSlot struct {
	mu     sync.Mutex
	budget int64       // 0 ⇒ use the kind default when the cache is first created
	bc     *blockCache // created lazily on first use
	made   bool        // creation attempted; a failure is remembered as bc==nil, not retried
}

func (s *sharedBlockCacheSlot) setBudget(bytes int64) {
	if bytes <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.budget = bytes
	if s.made && s.bc != nil {
		s.bc.c.UpdateMaxCost(bytes) // resize the live shared cache in place
	}
}

func (s *sharedBlockCacheSlot) get(def int64) *blockCache {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.made {
		b := s.budget
		if b <= 0 {
			b = def
		}
		if bc, err := newBlockCache(b); err == nil {
			s.bc = bc
		}
		s.made = true
	}
	return s.bc
}

var globalBlockCaches [numBlockCacheKinds]sharedBlockCacheSlot

// SetMutatingBlockCacheBudget and SetArchiveBlockCacheBudget set the process-global shared
// block-cache byte budget for each workload kind. They are the config knob behind Options'
// {Mutating,Archive}BlockCacheBytes; db/ (and above it htcondordb) should call them once at startup
// from configuration. Safe to call at any time and concurrently: if the kind's cache already
// exists it is resized in place. A value ≤ 0 is ignored (keeps the current or default budget).
func SetMutatingBlockCacheBudget(bytes int64) {
	globalBlockCaches[mutatingCacheKind].setBudget(bytes)
}
func SetArchiveBlockCacheBudget(bytes int64) {
	globalBlockCaches[archiveCacheKind].setBudget(bytes)
}

// sharedColCache returns the PROCESS-GLOBAL block cache for this collection's KIND -- the archive
// cache for an append-only collection (a history log), the mutating cache otherwise. The chosen
// cache is memoized on the collection so the hot path skips the slot lock. A nil result (creation
// failed) is valid: blockCache's methods then decompress every time -- correct, just slower -- so a
// columnarized segment never becomes unreadable over a cache failure.
func (c *Collection) sharedColCache() *blockCache {
	c.colCacheOnce.Do(func() {
		if c.appendOnly() {
			c.colCache = globalBlockCaches[archiveCacheKind].get(defaultArchiveCacheBytes)
		} else {
			c.colCache = globalBlockCaches[mutatingCacheKind].get(defaultMutatingCacheBytes)
		}
	})
	return c.colCache
}

// streams returns b's decompressed cold streams, from the cache when present. On a miss it
// decompresses all three and inserts them (cost = decompressed bytes). Safe for concurrent use;
// ristretto's Set is asynchronous, so a get-immediately-after-set may miss -- harmless here (an
// occasional redundant decompress, never a wrong result).
// stream returns one of b's decompressed regions, from the cache when present. A reader that needs
// a single region pays for that region only -- see streamKind.
func (bc *blockCache) stream(b *columnarBlock, k streamKind) ([]byte, error) {
	key := streamKey(b.id, k)
	if bc != nil {
		if raw, ok := bc.c.Get(key); ok {
			return raw, nil
		}
	}
	var comp []byte
	switch k {
	case kindColdNum:
		comp = b.coldNumComp
	case kindStr:
		comp = b.strComp
	case kindCold:
		comp = b.coldComp
	case kindStrDict:
		comp = b.strDictComp
	case kindStrCode:
		comp = b.strCodeComp
	}
	raw, err := b.codec.Decompress(nil, comp)
	if err != nil {
		return nil, err
	}
	if bc != nil {
		bc.c.Set(key, raw, int64(len(raw)))
	}
	return raw, nil
}

// streams returns all three regions, for a reader that genuinely needs them (a full-ad
// reconstruction). Each is cached separately, so a later single-stream read hits.
func (bc *blockCache) streams(b *columnarBlock) (*decodedStreams, error) {
	coldNum, err := bc.stream(b, kindColdNum)
	if err != nil {
		return nil, err
	}
	str, err := bc.stream(b, kindStr)
	if err != nil {
		return nil, err
	}
	cold, err := bc.stream(b, kindCold)
	if err != nil {
		return nil, err
	}
	return &decodedStreams{coldNum: coldNum, str: str, cold: cold}, nil
}

// close releases the cache's resources.
func (bc *blockCache) close() {
	if bc != nil && bc.c != nil {
		bc.c.Close()
	}
}
