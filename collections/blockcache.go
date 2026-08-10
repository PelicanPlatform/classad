package collections

import "github.com/dgraph-io/ristretto/v2"

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
)

// streamKey derives a per-stream cache key. Block ids come from a monotonic counter
// (colBlockSeq), so id*3+kind is unique per (block, stream).
func streamKey(id uint64, k streamKind) uint64 { return id*3 + uint64(k) }

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
	c, err := ristretto.NewCache(&ristretto.Config[uint64, []byte]{
		NumCounters: 1 << 20, // ~10x the expected item count, for TinyLFU admission
		MaxCost:     maxBytes,
		BufferItems: 64,
	})
	if err != nil {
		return nil, err
	}
	return &blockCache{c: c}, nil
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
