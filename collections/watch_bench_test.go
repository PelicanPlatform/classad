package collections

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// benchmarkWatchedWrites measures concurrent write throughput on a watch-enabled
// collection -- alternating upsert and delete of each writer's keys, through Put/Delete
// or a one-write Txn -- with zero or one live watcher draining the stream. It is the
// commit path's watch bookkeeping (journal, publish ordering) under contention.
func benchmarkWatchedWrites(b *testing.B, shards int, watch, txn bool) {
	c := New(Options{Shards: shards, WatchHistory: 4096, WatchBuffer: 1 << 16, Codec: identityCodec{}})
	ad := mustAd(b, `[MyType="Machine"; Cpus=8; Memory=16384; State="Unclaimed"]`)
	_ = ad.AST() // sort once up front (shared ad; see benchmarkConcurrentPut)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var resyncs atomic.Int64
	if watch {
		ready := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			first := true
			for ctx.Err() == nil {
				// On a Resync, rejoin at the head rather than replaying from the last
				// cursor: this measures the write path, not catch-up.
				cur, _ := c.WatchCursor()
				seq, _ := c.Watch(ctx, cur)
				for ev := range seq {
					if first && ev.Kind == WatchSynced {
						first = false
						close(ready)
					}
					if ev.Kind == WatchResync {
						resyncs.Add(1)
						break
					}
				}
			}
		}()
		<-ready
	}

	var w int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		id := atomic.AddInt64(&w, 1)
		prefix := "w" + strconv.FormatInt(id, 10) + "-"
		k := 0
		for pb.Next() {
			key := []byte(prefix + strconv.Itoa((k>>1)&1023))
			del := k&1 == 1
			switch {
			case txn:
				tx := c.Begin()
				if del {
					tx.Delete(key)
				} else {
					tx.Put(key, ad)
				}
				tx.Commit()
			case del:
				c.Delete(key)
			default:
				_ = c.Put(key, ad)
			}
			k++
		}
	})
	b.StopTimer()
	cancel()
	wg.Wait()
	if watch {
		b.ReportMetric(float64(resyncs.Load()), "resyncs")
	}
}

func BenchmarkWatchedWrites16NoWatcher(b *testing.B)    { benchmarkWatchedWrites(b, 16, false, false) }
func BenchmarkWatchedWrites16Watcher(b *testing.B)      { benchmarkWatchedWrites(b, 16, true, false) }
func BenchmarkWatchedTxnWrites16NoWatcher(b *testing.B) { benchmarkWatchedWrites(b, 16, false, true) }
func BenchmarkWatchedTxnWrites16Watcher(b *testing.B)   { benchmarkWatchedWrites(b, 16, true, true) }
