package collections

import (
	"github.com/PelicanPlatform/classad/classad"
)

// Truncate removes every ad from the collection, leaving it empty (an in-place reset
// used by a DB restore before it reloads a snapshot). It resets each shard's directory
// and count and retires its segments -- RAM segments are dropped for the GC; persistent
// segments are munmap'd + unlinked once no in-flight scan references them (the compaction
// pin/reap protocol). A concurrent scan that already captured its window reads the old
// data safely until it finishes; scans and writes that start after Truncate see an empty
// collection. Ordered indexes are cleared in place. Callers needing atomicity against
// concurrent writers must serialize Truncate with them (the db layer holds its DB-wide
// lock); at the shard level Truncate is itself consistent.
func (c *Collection) Truncate() {
	for _, sh := range c.shards {
		var toReap []*segment
		sh.mu.Lock()
		for _, seg := range sh.segs {
			if seg != nil && seg.retire() {
				toReap = append(toReap, seg)
			}
		}
		sh.segs = nil
		sh.act = nil
		sh.dir = make(map[uint64]loc)
		sh.dirty = nil
		sh.dirtySup = nil
		sh.count = 0
		// Bump the commit sequence and floor so an older transaction snapshot cannot be
		// applied over the truncated state (it conflicts), and new scans see the reset.
		sh.commitSeq++
		sh.gcFloor = sh.commitSeq
		if sh.childCount != nil {
			sh.childCount = make(map[uint64]int)
		}
		sh.writeErr = nil
		sh.mu.Unlock()
		// Reap (munmap + unlink) outside the lock; retire() already deferred any still-
		// pinned segment to its last unpin.
		for _, seg := range toReap {
			_ = seg.reap()
		}
	}
	for _, oi := range c.ordered {
		oi.clear()
	}
}

// ForEachAd calls fn with every stored ad and its key, including structural (parent-only)
// ads -- a complete image for a backup, unlike Scan/Keys which hide structural ads. Each
// ad is fully decoded (decompressed and, for encrypted attributes, decrypted). Iteration
// stops early if fn returns false. Per-shard consistent snapshot, like Scan.
func (c *Collection) ForEachAd(fn func(key string, ad *classad.ClassAd) bool) {
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		stop := false
		forEachVisibleKeyed(s0, wins, func(key, ad []byte, codec Codec) bool {
			decoded, err := c.decodeAd(ad, codec)
			if err != nil {
				return true // skip an undecodable record rather than abort the backup
			}
			if !fn(string(key), decoded) {
				stop = true
				return false
			}
			return true
		})
		releaseWindows(wins)
		if stop {
			return
		}
	}
}
