package collections

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

// errNoMmap is returned when a persistent collection is requested on a platform
// without mmap support (persistence is currently unix-only).
var errNoMmap = errors.New("collections: persistent collections are unix-only")

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
	// Give each shard an mmap-segment factory writing to its own subdirectory.
	// Files are named by a per-shard monotonic counter, independent of the logical
	// segment id (which is the array index and is reassigned at compaction/recovery),
	// so a segment's id can change with no file rename.
	for i, sh := range c.shards {
		shardDir := filepath.Join(opts.Dir, fmt.Sprintf("%d", i))
		if err := os.MkdirAll(shardDir, 0o755); err != nil {
			return nil, err
		}
		var counter uint64
		sh.alloc = func(id uint32, size int, codec Codec) (*segment, error) {
			n := atomic.AddUint64(&counter, 1)
			path := filepath.Join(shardDir, fmt.Sprintf("seg-%d.dat", n))
			return newMmapSegment(id, size, codec, path)
		}
	}
	return c, nil
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
