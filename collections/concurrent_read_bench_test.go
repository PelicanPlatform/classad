package collections

import (
	"strconv"
	"testing"
)

// Read-lock contention. Every point read takes the shard's RWMutex read lock, and an RLock is an atomic
// add on a shared counter -- the cache line holding it bounces between cores even with no writer present,
// so read throughput scales with the NUMBER OF LOCKS, not with cores.
//
// Sharding is what makes that scale: keys hash to shards, so N shards means N independent locks. These
// benchmarks quantify it by holding the work constant and varying only the shard count, and by pinning
// every reader to ONE shard to show the worst case a badly-distributed key space would hit.
//
// ShardGet isolates the lock: sh.get is the read lock plus a directory lookup plus a byte copy, with no
// decode. Get is the realistic figure, where decode cost sits on top and dilutes the lock's share.

const readBenchKeys = 4096

func readBenchFixture(b *testing.B, shards int) *Collection {
	b.Helper()
	c := New(Options{Shards: shards, Codec: identityCodec{}})
	ad := mustAd(b, `[MyType="Machine"; Cpus=8; Memory=16384; Arch="X86_64"; State="Unclaimed"]`)
	_ = ad.AST() // Put's AST() sorts in place; do it once rather than per iteration
	for i := 0; i < readBenchKeys; i++ {
		if err := c.Put([]byte("k"+strconv.Itoa(i)), ad); err != nil {
			b.Fatal(err)
		}
	}
	return c
}

// benchmarkConcurrentShardGet measures the lock path alone, keys spread across all shards.
func benchmarkConcurrentShardGet(b *testing.B, shards int) {
	c := readBenchFixture(b, shards)
	defer c.Close()
	keys := make([][]byte, readBenchKeys)
	hashes := make([]uint64, readBenchKeys)
	shardOf := make([]int, readBenchKeys)
	for i := range keys {
		keys[i] = []byte("k" + strconv.Itoa(i))
		hashes[i] = c.h.Hash(keys[i])
		shardOf[i] = c.shardOf(keys[i], hashes[i])
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			j := i & (readBenchKeys - 1)
			c.shards[shardOf[j]].get(c, hashes[j], keys[j])
			i++
		}
	})
}

// benchmarkConcurrentShardGetOneShard is the worst case: every reader hammers a single shard's lock, so
// the read-lock cache line is contended by every core at once.
func benchmarkConcurrentShardGetOneShard(b *testing.B, shards int) {
	c := readBenchFixture(b, shards)
	defer c.Close()
	// Collect keys that all live in shard 0, so the spread of the key space is irrelevant.
	var keys [][]byte
	var hashes []uint64
	for i := 0; i < readBenchKeys && len(keys) < 256; i++ {
		k := []byte("k" + strconv.Itoa(i))
		h := c.h.Hash(k)
		if c.shardOf(k, h) == 0 {
			keys = append(keys, k)
			hashes = append(hashes, h)
		}
	}
	if len(keys) == 0 {
		b.Skip("no keys landed in shard 0")
	}
	sh := c.shards[0]
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			j := i % len(keys)
			sh.get(c, hashes[j], keys[j])
			i++
		}
	})
}

func BenchmarkConcurrentShardGet1(b *testing.B)  { benchmarkConcurrentShardGet(b, 1) }
func BenchmarkConcurrentShardGet4(b *testing.B)  { benchmarkConcurrentShardGet(b, 4) }
func BenchmarkConcurrentShardGet16(b *testing.B) { benchmarkConcurrentShardGet(b, 16) }
func BenchmarkConcurrentShardGet64(b *testing.B) { benchmarkConcurrentShardGet(b, 64) }

// One shard's lock under full fan-in, whatever the collection's shard count.
func BenchmarkConcurrentShardGetHotShard(b *testing.B) { benchmarkConcurrentShardGetOneShard(b, 16) }

// benchmarkConcurrentGet is the realistic path: lock + lookup + copy + decode.
func benchmarkConcurrentGet(b *testing.B, shards int) {
	c := readBenchFixture(b, shards)
	defer c.Close()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Get([]byte("k" + strconv.Itoa(i&(readBenchKeys-1))))
			i++
		}
	})
}

func BenchmarkConcurrentGet1(b *testing.B)  { benchmarkConcurrentGet(b, 1) }
func BenchmarkConcurrentGet16(b *testing.B) { benchmarkConcurrentGet(b, 16) }

// benchmarkRLockOnly is the lock and nothing else: no lookup, no copy, no decode. This is the figure that
// answers "how much do our read locks bounce" -- an RLock is an atomic add on the mutex's reader counter,
// so the cache line holding it moves between cores on every acquisition, whether or not a writer exists.
//
// fanIn=1 puts every reader on ONE lock (the worst case, and what a hot key range or a single-shard
// collection produces); fanIn=N spreads them over N locks the way hashed keys do.
func benchmarkRLockOnly(b *testing.B, fanIn int) {
	c := New(Options{Shards: fanIn, Codec: identityCodec{}})
	defer c.Close()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			sh := c.shards[i%fanIn]
			sh.mu.RLock()
			sh.mu.RUnlock()
			i++
		}
	})
}

func BenchmarkRLockOnly1(b *testing.B)  { benchmarkRLockOnly(b, 1) }
func BenchmarkRLockOnly4(b *testing.B)  { benchmarkRLockOnly(b, 4) }
func BenchmarkRLockOnly16(b *testing.B) { benchmarkRLockOnly(b, 16) }
func BenchmarkRLockOnly64(b *testing.B) { benchmarkRLockOnly(b, 64) }
