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
		// pinned segment to its last unpin. reapAndHook (not bare reap) so a sealed
		// segment's mmap sidecar index is unmapped with its data (the onReap hook) --
		// otherwise the sidecar mapping leaks. Every other drop path (compact/retention/
		// retrain) already routes through reapAndHook; Truncate must too.
		for _, seg := range toReap {
			_ = seg.reapAndHook()
		}
	}
	for _, oi := range c.ordered {
		oi.clear()
	}
}

// ForEachAd calls fn with every stored (non-system) ad and its key, including structural
// (parent-only) ads -- a client-facing image, unlike Scan/Keys which hide structural ads.
// It is the driver for db.DeleteWhere's key matching, so it must NOT expose internal system
// records (they are neither client-visible nor client-deletable); system records are
// enumerated separately via ForEachSystemAd. Each ad is fully decoded (decompressed and,
// for encrypted attributes, decrypted). Iteration stops early if fn returns false. Per-shard
// consistent snapshot, like Scan.
func (c *Collection) ForEachAd(fn func(key string, ad *classad.ClassAd) bool) {
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		stop := false
		forEachVisibleKeyed(s0, wins, func(key, ad []byte, codec Codec, dict *segDictHandle) bool {
			if isSystemKeyBytes(key) {
				return true // internal system record: hidden from client iteration
			}
			decoded, err := c.decodeAdDict(dict, ad, codec)
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

// ForEachAdAt is ForEachAd over the versions visible at a caller-chosen snapshot
// sequence per shard, instead of each shard's current commit sequence.
//
// snapOf is called once per shard, with the shard's index, and returns the sequence to
// read that shard at. It is a callback rather than a map so a transaction can capture a
// shard's sequence lazily on first touch -- and, having captured it, stay pinned to it
// for every later read of that shard.
//
// This is what lets a transaction scan the committed store at its own snapshot: the same
// versions its point lookups see, rather than whatever has committed since.
func (c *Collection) ForEachAdAt(snapOf func(shardIdx int) uint64, fn func(key string, ad *classad.ClassAd) bool) {
	for i, sh := range c.shards {
		s0 := snapOf(i)
		wins := sh.snapshotAt(s0)
		stop := false
		forEachVisibleKeyed(s0, wins, func(key, ad []byte, codec Codec, dict *segDictHandle) bool {
			if isSystemKeyBytes(key) {
				return true // internal system record: hidden from client iteration
			}
			decoded, err := c.decodeAdDict(dict, ad, codec)
			if err != nil {
				return true // skip an undecodable record, as ForEachAd does
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

// ForEachSystemAd calls fn with every internal system-keyed ad and its key -- the mirror
// image of ForEachAd, which hides them. It is the enumeration path a TTL reaper uses to
// find and expire durable bookkeeping records. Each ad is fully decoded; iteration stops
// early if fn returns false. Per-shard consistent snapshot, like Scan.
func (c *Collection) ForEachSystemAd(fn func(key string, ad *classad.ClassAd) bool) {
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		stop := false
		forEachVisibleKeyed(s0, wins, func(key, ad []byte, codec Codec, dict *segDictHandle) bool {
			if !isSystemKeyBytes(key) {
				return true // only system records
			}
			decoded, err := c.decodeAdDict(dict, ad, codec)
			if err != nil {
				return true // skip an undecodable record rather than abort the sweep
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
