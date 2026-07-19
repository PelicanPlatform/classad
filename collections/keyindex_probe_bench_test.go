package collections

import (
	"fmt"
	"math/rand"
	"testing"
)

// These benchmarks quantify the pageable primary index's central cost trade (phase 3): a
// key evicted from the resident directory is served by the Bloom-gated sealed-segment probe
// instead of an O(1) map hit. The design doc flags probe read/write amplification as its one
// open question; these measure it.
//
// openPersistProbe writes n counters into a persistent collection. With reopen=true it is
// closed and reopened, so every sealed segment's keys are evicted from the directory and
// lookups go through the probe; with reopen=false the keys stay directory-resident. Both
// variants read the identical mmap sealed segments, so a resident-vs-evicted delta is purely
// directory-hit vs probe -- not RAM vs disk.
func openPersistProbe(b *testing.B, n, segSize int, reopen bool) (*Collection, [][]byte) {
	b.Helper()
	dir := b.TempDir()
	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 8, SegmentSize: segSize})
		if err != nil {
			b.Fatal(err)
		}
		return c
	}
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("k%08d", i))
	}
	ad := mustAd(b, `[ N = 0 ]`)
	c := open()
	for i := 0; i < n; i++ {
		if err := c.Put(keys[i], ad); err != nil {
			b.Fatal(err)
		}
	}
	if reopen {
		if err := c.Close(); err != nil {
			b.Fatal(err)
		}
		c = open()
	}
	return c, keys
}

// BenchmarkProbeGet compares Get latency for directory-resident keys against keys evicted to
// the sealed probe, at a fixed store size. The delta is the read amplification a Get pays for
// a key that no longer has a resident directory entry.
func BenchmarkProbeGet(b *testing.B) {
	const n = 200_000
	const segSize = 256 << 10
	for _, tc := range []struct {
		name   string
		reopen bool
	}{
		{"resident", false},
		{"evicted", true},
	} {
		c, keys := openPersistProbe(b, n, segSize, tc.reopen)
		b.Run(tc.name, func(b *testing.B) {
			rng := rand.New(rand.NewSource(1))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := c.Get(keys[rng.Intn(n)]); !ok {
					b.Fatal("unexpected miss")
				}
			}
		})
		c.Close()
	}
}

// BenchmarkProbeGetFanout measures evicted-key Get as the sealed-segment count grows (smaller
// segments -> more segments -> more per-segment Bloom filters to consult). The per-segment
// Bloom gates the probe to the ~one segment that actually holds the key, so cost should stay
// roughly flat rather than scale with segment count -- the claim that read fan-out is bounded.
func BenchmarkProbeGetFanout(b *testing.B) {
	const n = 200_000
	for _, segSize := range []int{64 << 10, 256 << 10, 1 << 20} {
		c, keys := openPersistProbe(b, n, segSize, true)
		b.Run(fmt.Sprintf("nseg=%d", liveSegments(c)), func(b *testing.B) {
			rng := rand.New(rand.NewSource(1))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := c.Get(keys[rng.Intn(n)]); !ok {
					b.Fatal("unexpected miss")
				}
			}
		})
		c.Close()
	}
}

// BenchmarkProbeUpdateResident updates directory-resident keys: findCurrent hits the dir
// chain, the old record is superseded in place, and the new version is appended. Write-path
// baseline for the evicted comparison below.
func BenchmarkProbeUpdateResident(b *testing.B) {
	const n = 50_000
	c, keys := openPersistProbe(b, n, 256<<10, false)
	defer c.Close()
	ad := mustAd(b, `[ N = 1 ]`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Put(keys[i%n], ad); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProbeUpdateEvicted updates keys whose current version lives in a sealed segment:
// the directory misses, so put locates the old version through the probe and supersedes it
// there (an extra msync of the sealed region) before appending the new version. This is the
// cold-key write amplification the pageable design trades for the RAM win. Sized so nKeys ==
// b.N and each iteration touches a distinct still-evicted key -- a second update to a key
// would find it directory-resident (its new version is in the active segment).
func BenchmarkProbeUpdateEvicted(b *testing.B) {
	c, keys := openPersistProbe(b, b.N, 256<<10, true)
	defer c.Close()
	ad := mustAd(b, `[ N = 1 ]`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Put(keys[i], ad); err != nil {
			b.Fatal(err)
		}
	}
}
