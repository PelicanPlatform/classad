package collections

import (
	"context"
	"fmt"
	"testing"
)

// benchmarkChainedWatchFanout times a live watch of an encrypted chained collection: each op changes a
// public parent attribute, which fans out to every child, and waits until the watcher has delivered
// them all. It covers the per-watcher parent decode, the fan-out diff and the child decodes.
func benchmarkChainedWatchFanout(b *testing.B, redact bool) {
	if !mmapSupported {
		b.Skip("persistence is unix-only")
	}
	const children = 64
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	c, err := Open(Options{
		Shards: 1, Dir: b.TempDir(), SegmentSize: 1 << 20, WatchHistory: 1 << 16,
		ParentKeyFor: jobParentKey, IsStructural: jobStructural,
		DataKey: key, EncryptedAttrs: []string{"Secret"},
	})
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	parent := func(v int) {
		if perr := c.Put([]byte("1.-1"), mustAd(b, fmt.Sprintf(`[DAGManJobId=%d; Owner="alice"; Secret="p"]`, v))); perr != nil {
			b.Fatal(perr)
		}
	}
	parent(0)
	for i := 0; i < children; i++ {
		if perr := c.Put([]byte(fmt.Sprintf("1.%d", i)), mustAd(b, fmt.Sprintf(`[ProcId=%d; Cpus=1; Secret="c%d"]`, i, i))); perr != nil {
			b.Fatal(perr)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := c.Watch
	if redact {
		watch = c.WatchRedacted
	}
	seq, err := watch(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	upserts := make(chan struct{}, 4*children)
	synced := make(chan struct{})
	go func() {
		live := false
		for ev := range seq {
			switch {
			case ev.Kind == WatchSynced:
				live = true
				close(synced)
			case live && ev.Kind == WatchUpsert:
				upserts <- struct{}{}
			case ev.Kind == WatchResync:
				panic("watch fell behind")
			}
		}
	}()
	<-synced
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parent(i + 1)
		for j := 0; j < children; j++ {
			<-upserts
		}
	}
}

func BenchmarkChainedWatchFanoutEntitled(b *testing.B) { benchmarkChainedWatchFanout(b, false) }
func BenchmarkChainedWatchFanoutKeyless(b *testing.B)  { benchmarkChainedWatchFanout(b, true) }
