package collections

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// Migrating a store written before private attributes were sealed.
//
// Sealing is applied at ENCODE time, so turning it on protects new writes and leaves everything already
// on disk exactly as it was: a ClaimId written last week is still a plaintext string literal in its
// segment. Nothing reports that -- reads work, queries work -- so without this pass a store would sit
// half-protected indefinitely, and the operator would have no way to tell.
//
// The pass rewrites each segment that still holds an unsealed value through the ordinary reseal path,
// which re-encodes every record under the current codec and hot set, and therefore under the current
// sealer.
//
// Three properties, in the order they matter:
//
//   - IDEMPOTENT, because the candidate test is the data itself rather than bookkeeping. A segment is a
//     candidate only if it actually contains a value that should be sealed and is not, so a re-run after
//     a crash re-examines everything and rewrites only what is left. There is no state to disagree with
//     the store.
//   - CRASH-SAFE, because each segment is replaced whole: the reseal writes a new file and the swap
//     installs it under the shard lock, so an interrupted run leaves every segment either fully migrated
//     or untouched. A partly-written replacement is never installed.
//   - RESUMABLE follows from the first two. The marker below is only an optimization -- deleting it
//     costs a scan, not correctness.

// sealMigrationMarker records that a full pass found nothing left to migrate. Its absence costs one scan
// of the sealed segments at open; its presence skips that. It is written only after a clean pass, so a
// crash mid-pass simply leaves it absent.
const sealMigrationMarker = "sealed-attrs.done"

// MigrateSealedAttrs rewrites every sealed segment that still holds a value this collection would seal,
// so a store written before sealing was turned on stops carrying private attributes in the clear.
// Returns the number of segments rewritten.
//
// Safe to call on a store that needs nothing: the scan finds no candidate and it returns 0. Safe to call
// twice, and safe to interrupt (see the file comment). A collection with no sealer, or no persistence,
// has nothing to migrate.
//
// workers bounds the concurrent rewrites; <= 0 takes a default derived from GOMAXPROCS. Rewriting is
// decompress + re-encode + compress per record, so it is CPU-bound and worth parallelizing on a store
// with many segments -- but each rewrite also holds a whole new segment in memory, which is why it is
// bounded rather than one goroutine per segment.
func (c *Collection) MigrateSealedAttrs(workers int) int {
	if c.sealer == nil || c.dir == "" {
		return 0 // nothing to seal with, or nothing durable to rewrite
	}
	if c.sealMigrationDone() {
		return 0
	}
	c.maintMu.Lock()
	defer c.maintMu.Unlock()

	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
		if workers > 4 {
			workers = 4 // each worker holds a segment's worth of rewritten records
		}
	}
	codec := c.currentCodec()
	migrated := 0
	for _, sh := range c.shards {
		sh.mu.Lock()
		// Retire the active segment from the write path so it is migrated too, exactly as the dict
		// reseal does. On reopen the last segment becomes the write target again, and its records are
		// the LEGACY ones -- skipping it left a store that reported a successful migration with
		// plaintext still in it, which is the failure this pass exists to prevent. writeRecord
		// allocates a fresh segment when act is nil, so the next write is unaffected.
		sh.act = nil
		var srcs []*segment
		for _, s := range sh.segs {
			if s != nil && s.used > 0 {
				srcs = append(srcs, s)
			}
		}
		sh.mu.Unlock()

		// Scan and rewrite off the shard lock; only the swap takes it.
		type result struct {
			src, dst *segment
		}
		var (
			wg      sync.WaitGroup
			sem     = make(chan struct{}, workers)
			mu      sync.Mutex
			results []result
		)
		for _, src := range srcs {
			src := src
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if !c.segmentHasUnsealedValue(src) {
					return
				}
				if dst := c.resealOneSegment(sh, src, codec); dst != nil {
					mu.Lock()
					results = append(results, result{src: src, dst: dst})
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		var toReap []*segment
		for _, r := range results {
			sh.mu.Lock()
			// Swap only if the source still occupies its slot and has not become the write target again
			// -- the same defence the dict transcode takes against a concurrent reseal or rotate.
			if int(r.src.id) < len(sh.segs) && sh.segs[r.src.id] == r.src && sh.act != r.src {
				sh.segs[r.src.id] = r.dst
				migrated++
				if r.src.retire() {
					toReap = append(toReap, r.src)
				}
			} else {
				r.dst.retire()
				r.dst.reapAndHook() // slot moved under us: drop the fresh file rather than leak it
			}
			sh.mu.Unlock()
		}
		for _, seg := range toReap {
			seg.reapAndHook() // munmap + unlink the superseded file, off-lock
		}
	}
	if migrated > 0 {
		// EVERY STRUCTURE THAT NAMES A (segment, offset) IS NOW STALE. A reseal re-encodes each record
		// and sizes the new segment exactly, so the same key sits at a different offset -- and the old
		// offset may be past the end of the new, smaller file.
		//
		// Leaving them stale did not read as corruption. A SCAN walks segments directly and was
		// perfectly fine, which is what the first version of this pass verified. A GET goes through the
		// directory and the bucket chain, so it read a header where there was none: usually a key
		// mismatch that ends the chain and reports the key MISSING (1998 of 2000, in the test below),
		// occasionally an offset past the mapping, which is a panic in a running daemon:
		//
		//	panic: runtime error: slice bounds out of range [:67269717] with capacity 148816
		//
		// Reindex rebuilds each segment's key sidecar from its actual contents and evicts that
		// segment's directory entries, so both lookup paths point at the new layout. This is the same
		// reconciliation the other two swap-a-rewritten-segment paths do (compaction, InternSealed);
		// this pass simply did not, which is the whole defect.
		c.reindexAfterCompaction()
		c.pruneDicts()
	}
	c.markSealMigrationDone()
	return migrated
}

// segmentHasUnsealedValue reports whether seg holds any attribute this collection would seal, stored as
// a plain literal. A sealed value is not a literal, so this is exactly "was this record written before
// sealing was on".
//
// It reads the segment's records directly and stops at the first hit: a store that needs migrating pays
// almost nothing to discover it, and a store that does not pays one pass.
func (c *Collection) segmentHasUnsealedValue(seg *segment) bool {
	var buf []byte
	found := false
	for off := 0; off < seg.used && !found; {
		o := uint32(off)
		total := recTotalLen(seg.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(seg.data, o) {
			w, err := seg.codec.Decompress(buf[:0], recAd(seg.data, o))
			if err == nil {
				buf = w
				found = c.recordHasUnsealedValue(seg.dict.Load(), w)
			}
		}
		off += int(total)
	}
	return found
}

// recordHasUnsealedValue reports whether a record's STORED form holds a value this collection would seal
// as a plain literal.
//
// It reads the raw wire deliberately. Converting the record first -- wireToInline, say -- would decode
// with the key and RE-SEAL on the way out, so a legacy plaintext value would come back sealed and this
// would report every segment clean: the check would mask exactly what it exists to find.
//
// Name resolution has to follow the record's own encoding: a segment-local dictionary resolves its ids,
// an inline record carries its names, and anything else resolves against the collection's global table.
// Getting that wrong is not a wrong answer, it is a nil dereference -- which is how the first version
// announced itself.
func (c *Collection) recordHasUnsealedValue(dict *segDictHandle, w []byte) bool {
	sealedAlready := func(name string, node []byte) bool {
		if !c.shouldEncrypt(name) {
			return true // not ours to seal
		}
		_, isLiteral := wire.LiteralValue(node)
		return !isLiteral // a sealed value is not a literal
	}
	found := false
	switch {
	case dict != nil:
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			if nm := dict.name(id); nm != nil && !sealedAlready(string(nm), node) {
				found = true
				return false
			}
			return true
		})
	case c.inline:
		wire.Ad(w).ForEachNamed(nil, func(name string, node []byte) bool {
			if !sealedAlready(name, node) {
				found = true
				return false
			}
			return true
		})
	default:
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			if name, ok := c.intern.Name(id); ok && !sealedAlready(name, node) {
				found = true
				return false
			}
			return true
		})
	}
	return found
}

func (c *Collection) sealMigrationPath() string { return filepath.Join(c.dir, sealMigrationMarker) }

func (c *Collection) sealMigrationDone() bool {
	if c.dir == "" {
		return false
	}
	_, err := os.Stat(c.sealMigrationPath())
	return err == nil
}

// markSealMigrationDone records a clean pass. A failure to write it costs a rescan next time, which is
// why it is not reported: the store is correct either way.
func (c *Collection) markSealMigrationDone() {
	if c.dir == "" {
		return
	}
	path := c.sealMigrationPath()
	if _, err := os.Stat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("sealed\n"), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
