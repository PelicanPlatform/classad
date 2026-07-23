package collections

import (
	"sync"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// wireRowBatch is one worker's assembled wire-form rows: row i occupies
// buf[offs[i]:offs[i+1]] (offs always starts with 0). Rows build DIRECTLY into
// the arena -- the subset assembler appends to it -- so a row is copied exactly
// once between the decompressed record and the consumer.
type wireRowBatch struct {
	buf  []byte
	offs []int
}

// wireBatchArena is a worker's target arena size before handing rows to the
// yielding goroutine -- large enough to amortize channel handoffs, small enough
// that W workers' in-flight arenas stay a few hundred KB total.
const wireBatchArena = 64 << 10

// wirePrefixBytes is how much of a compressed record a projected scan
// decompresses first: the hot region is encoded as a physical prefix (hot
// header + hot entries, typically ~1-2KB for a monitoring hot set), so this
// covers it with margin. A subset that the prefix cannot satisfy -- offsets
// beyond it, a projected attribute that is cold -- falls back to a full
// decompress of that record.
const wirePrefixBytes = 3 << 10

// runParallelWireScan fans the wire-row subset scan across the collection's
// segment windows, using the same worker budget, work-size gate and
// work-stealing pattern as runParallelQuery: per-record ZSTD decompression is
// the scan's dominant cost and is embarrassingly parallel across segments.
// Rows arrive at yield in arbitrary order (queries do not order rows).
//
// Returns false without consuming the yield when parallelism is not engaged --
// too little work, no budget, or an at-rest-encrypted collection (the Sealer
// contract is single-goroutine per pass, so encrypted stores stay serial) --
// and the caller runs the serial path.
func (c *Collection) runParallelWireScan(sel *wireSubsetSelector, needed int, yield func([]byte) bool) bool {
	if c.sealer != nil {
		return false
	}
	tasks, totalBytes, release := c.gatherTasks()
	W := 0
	if c.qsem != nil && len(tasks) >= 2 && totalBytes >= c.parallelMinBytes {
		want := c.queryPar
		if want > len(tasks) {
			want = len(tasks)
		}
		W = tryAcquire(c.qsem, want)
	}
	if W < 2 {
		for i := 0; i < W; i++ {
			<-c.qsem
		}
		release()
		return false
	}
	defer release()
	defer func() {
		for i := 0; i < W; i++ {
			<-c.qsem
		}
	}()

	results := make(chan wireRowBatch, W)
	// Arena free-list: the consumer recycles drained batches back to the workers,
	// so a steady scan reuses ~2W arenas total instead of allocating one per 64KB
	// of result (a whole-pool unprojected scan would otherwise churn tens of MB of
	// garbage per query). getBatch falls back to allocation only until the pool
	// warms.
	recycle := make(chan wireRowBatch, 2*W+2)
	getBatch := func() wireRowBatch {
		select {
		case b := <-recycle:
			b.buf = b.buf[:0]
			b.offs = append(b.offs[:0], 0)
			return b
		default:
			return wireRowBatch{buf: make([]byte, 0, wireBatchArena), offs: []int{0}}
		}
	}
	var next int64
	var stopped atomic.Bool
	var wg sync.WaitGroup
	for i := 0; i < W; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var dbuf []byte
			var sc wire.SubsetScratch
			// Adaptive prefix gate: a stretch of records that cannot be served from
			// their prefix (not yet re-encoded hot-first, or the projection is not
			// hot-covered) would pay a wasted prefix decompress each -- so after
			// eight consecutive fallbacks the worker stops trying for the rest of
			// its scan. Any success re-arms it.
			usePrefix := true
			consecFail := 0
			batch := getBatch()
			flush := func() {
				if len(batch.offs) > 1 {
					results <- batch
					batch = getBatch()
				}
			}
			for {
				if stopped.Load() {
					return
				}
				idx := int(atomic.AddInt64(&next, 1)) - 1
				if idx >= len(tasks) {
					flush()
					return
				}
				forEachVisibleWindowKeyed(tasks[idx].s0, tasks[idx].win, func(key, ad []byte, codec Codec) bool {
					if stopped.Load() {
						return false
					}
					if isSystemKeyBytes(key) {
						return true
					}
					start := len(batch.buf)
					// Projected reads try a PREFIX decompress first: the hot region is
					// a physical prefix of the record, so a hot-covered projection
					// never decompresses the rest. A truncated parse (or an unmet
					// selection) falls back to the full record.
					if pd, canPrefix := codec.(PrefixDecompressor); canPrefix && usePrefix && needed > 0 {
						w, perr := pd.DecompressPrefix(dbuf[:0], ad, wirePrefixBytes)
						dbuf = w
						if perr == nil {
							if out, ok := wire.AppendAdSubsetInlineHotFirst(batch.buf, wire.Ad(w), sel.keep, needed, nil, &sc); ok {
								consecFail = 0
								batch.buf = out
								batch.offs = append(batch.offs, len(out))
								if len(batch.buf) >= wireBatchArena {
									flush()
								}
								return true
							} else {
								batch.buf = out[:start]
							}
						}
						if consecFail++; consecFail >= 8 {
							usePrefix = false
						}
					}
					w, err := codec.Decompress(dbuf[:0], ad)
					dbuf = w
					if err != nil {
						return true // undecodable record: skip, matching the serial scan
					}
					out, ok := wire.AppendAdSubsetInlineHotFirst(batch.buf, wire.Ad(w), sel.keep, needed, nil, &sc)
					if !ok {
						batch.buf = out[:start] // discard the partial append
						return true
					}
					batch.buf = out
					batch.offs = append(batch.offs, len(out))
					if len(batch.buf) >= wireBatchArena {
						flush()
					}
					return true
				})
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	for b := range results {
		stop := false
		for i := 0; i+1 < len(b.offs); i++ {
			if !yield(b.buf[b.offs[i]:b.offs[i+1]]) {
				stop = true
				break
			}
		}
		select {
		case recycle <- b:
		default: // free-list full; let the arena be collected
		}
		if stop {
			stopped.Store(true)
			for range results {
				// drain so the workers and closer finish
			}
			return true
		}
	}
	return true
}
