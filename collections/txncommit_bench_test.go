package collections

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// Commit throughput on the DEFAULT (optimistic, partially-applying) path. These exist to answer
// one question: does the all-or-nothing commit mode cost the default path anything? They use
// only the public default API, so the same file measures a build with the mode and a build
// without it.
//
// In-memory collections: the durability sync is a no-op there, so what is timed is the commit
// machinery itself (encode, shard grouping, the apply under the write lock) rather than an
// msync. A 1-key commit is the most sensitive case -- per-commit fixed overhead is not amortized
// over anything -- which is why it leads.

// benchAd is a small, realistic job-shaped ad.
func benchAd(b *testing.B, i int) *classad.ClassAd {
	b.Helper()
	ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; ProcId=%d; Owner="u%d"; JobStatus=%d; Cmd="/bin/sleep" ]`, i, i%10, i%7, i%5))
	if err != nil {
		b.Fatal(err)
	}
	return ad
}

func benchAds(b *testing.B, n int) []*classad.ClassAd {
	b.Helper()
	out := make([]*classad.ClassAd, n)
	for i := range out {
		out[i] = benchAd(b, i)
	}
	return out
}

func benchKeys(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = []byte(fmt.Sprintf("k%d", i))
	}
	return out
}

// BenchmarkTxnCommitSingleWriter is the single-writer fast path: one goroutine, so no other
// committer has touched a shard since the transaction's snapshot and the commit performs zero
// per-key conflict checks. This is the schedd's common case.
func BenchmarkTxnCommitSingleWriter(b *testing.B) {
	cases := []struct {
		name           string
		shards, perTxn int
		keys           int
	}{
		{"1shard/1key", 1, 1, 4096},
		{"1shard/8keys", 1, 8, 4096},
		{"16shards/1key", 16, 1, 4096},
		{"16shards/8keys", 16, 8, 4096},
		{"16shards/64keys", 16, 64, 4096},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			c := New(Options{Shards: tc.shards})
			keys := benchKeys(tc.keys)
			ads := benchAds(b, tc.keys)
			for i, k := range keys { // pre-populate so every timed commit is an overwrite
				_ = c.Put(k, ads[i])
			}
			b.ReportAllocs()
			b.ResetTimer()
			n := 0
			for range b.N {
				tx := c.Begin()
				for range tc.perTxn {
					tx.Put(keys[n%tc.keys], ads[(n+1)%tc.keys])
					n++
				}
				tx.Commit()
			}
		})
	}
}

// BenchmarkTxnCommitContended is the multi-writer case: several goroutines committing to the
// same collection, so the fast path does NOT hold and every write pays a per-key conflict check.
func BenchmarkTxnCommitContended(b *testing.B) {
	for _, shards := range []int{1, 16} {
		for _, perTxn := range []int{1, 8} {
			b.Run(fmt.Sprintf("%dshards/%dkeys", shards, perTxn), func(b *testing.B) {
				const keys = 4096
				c := New(Options{Shards: shards})
				ks := benchKeys(keys)
				ads := benchAds(b, keys)
				for i, k := range ks {
					_ = c.Put(k, ads[i])
				}
				var seq atomic.Int64
				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						n := int(seq.Add(int64(perTxn)))
						tx := c.Begin()
						for j := range perTxn {
							tx.Put(ks[(n+j)%keys], ads[(n+j+1)%keys])
						}
						tx.Commit()
					}
				})
			})
		}
	}
}

// BenchmarkTxnCommitDelete times a delete batch on the default path, the other mutation shape
// the commit's apply loop handles.
func BenchmarkTxnCommitDelete(b *testing.B) {
	const keys = 8192
	c := New(Options{Shards: 16})
	ks := benchKeys(keys)
	ads := benchAds(b, keys)
	for i, k := range ks {
		_ = c.Put(k, ads[i])
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		k := ks[i%keys]
		if i >= keys {
			b.StopTimer()
			_ = c.Put(k, ads[i%keys])
			b.StartTimer()
		}
		tx := c.Begin()
		tx.Delete(k)
		tx.Commit()
	}
}
