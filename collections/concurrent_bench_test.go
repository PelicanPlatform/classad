package collections

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkConcurrentPut measures aggregate single-ad Update throughput under
// GOMAXPROCS concurrent writers — the daemon pattern (many senders, one ad at a
// time). shardsFor lets us contrast the realistic spread-across-shards case with
// the worst case where every writer contends on one shard.
func benchmarkConcurrentPut(b *testing.B, shards int, codec Codec) {
	c := New(Options{Shards: shards, Codec: codec})
	ad := mustAd(b, `[MyType="Machine"; Cpus=8; Memory=16384; Arch="X86_64"; State="Unclaimed"; Rank=Cpus*10]`)
	_ = ad.AST() // sort once up front: Put's AST() mutates (sorts) the ad, and this
	// benchmark shares one ad across goroutines (a real caller passes distinct ads).
	var w int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		id := atomic.AddInt64(&w, 1)
		prefix := "w" + strconv.FormatInt(id, 10) + "-"
		k := 0
		for pb.Next() {
			_ = c.Put([]byte(prefix+strconv.Itoa(k&1023)), ad)
			k++
		}
	})
}

func BenchmarkConcurrentPut16Identity(b *testing.B) { benchmarkConcurrentPut(b, 16, identityCodec{}) }
func BenchmarkConcurrentPut1Identity(b *testing.B)  { benchmarkConcurrentPut(b, 1, identityCodec{}) }
func BenchmarkConcurrentPut16Zstd(b *testing.B) {
	z, _ := NewZSTDCodec(nil)
	benchmarkConcurrentPut(b, 16, z)
}

// BenchmarkGroupCommitAmortized simulates the future durable case: a per-batch
// sync (~30µs, like an fsync) that group commit amortizes across coalesced
// writers. At -cpu=1 there is nothing to coalesce (ads/sync ~= 1); at higher
// -cpu, writers arriving during one sync are committed together, so ads/sync
// climbs and per-ad latency drops far below the sync cost. Reported ads/sync is
// the coalescing factor.
func BenchmarkGroupCommitAmortized(b *testing.B) {
	var syncs int64
	// One shard so all writers coalesce on the same commit stream, making the
	// amortization visible; a real store's shards each coalesce independently.
	c := New(Options{Shards: 1, CommitSync: func() {
		atomic.AddInt64(&syncs, 1)
		time.Sleep(200 * time.Microsecond) // stand-in for an fsync/serialize
	}})
	ad := mustAd(b, `[MyType="Machine"; Cpus=8; Memory=16384; State="Unclaimed"]`)
	_ = ad.AST() // sort once up front (shared ad; see benchmarkConcurrentPut)
	var w int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		id := atomic.AddInt64(&w, 1)
		prefix := "w" + strconv.FormatInt(id, 10) + "-"
		k := 0
		for pb.Next() {
			_ = c.Put([]byte(prefix+strconv.Itoa(k&1023)), ad)
			k++
		}
	})
	b.StopTimer()
	b.ReportMetric(float64(b.N)/float64(atomic.LoadInt64(&syncs)), "ads/sync")
}
