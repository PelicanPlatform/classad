package collections

import "github.com/dgraph-io/ristretto/v2"

// decodedStreams holds a columnar block's decompressed cold streams (cold-numeric column,
// string column, cold tail) -- what a full-ad reconstruct or an escaped/cold-field lookup
// needs. Immutable once built.
type decodedStreams struct {
	coldNum, str, cold []byte
}

func (d *decodedStreams) cost() int64 { return int64(len(d.coldNum) + len(d.str) + len(d.cold)) }

// blockCache caches columnar blocks' decompressed cold streams so repeated full-ad reads and
// escaped-record lookups within a row-group decompress once, not per record. Backed by
// ristretto: byte-cost-bounded (each entry's cost is its decompressed size) and concurrent, a
// good fit for large, variable-size, hot items under parallel scans. A nil *blockCache is
// valid and simply decompresses every time (used by tests and unconfigured collections).
type blockCache struct {
	c *ristretto.Cache[uint64, *decodedStreams]
}

// newBlockCache creates a cache bounded to about maxBytes of decompressed block streams.
func newBlockCache(maxBytes int64) (*blockCache, error) {
	c, err := ristretto.NewCache(&ristretto.Config[uint64, *decodedStreams]{
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
func (bc *blockCache) streams(b *columnarBlock) (*decodedStreams, error) {
	if bc != nil {
		if ds, ok := bc.c.Get(b.id); ok {
			return ds, nil
		}
	}
	coldNum, err := b.codec.Decompress(nil, b.coldNumComp)
	if err != nil {
		return nil, err
	}
	str, err := b.codec.Decompress(nil, b.strComp)
	if err != nil {
		return nil, err
	}
	cold, err := b.codec.Decompress(nil, b.coldComp)
	if err != nil {
		return nil, err
	}
	ds := &decodedStreams{coldNum: coldNum, str: str, cold: cold}
	if bc != nil {
		bc.c.Set(b.id, ds, ds.cost())
	}
	return ds, nil
}

// close releases the cache's resources.
func (bc *blockCache) close() {
	if bc != nil && bc.c != nil {
		bc.c.Close()
	}
}
