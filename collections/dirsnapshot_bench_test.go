package collections

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkDirSnapshotReopen times reopening a mutable store of about 500 segments with puts,
// patches (delta records), rewrites and deletes behind it. "snapshot" reopens after a clean Close;
// "walk" removes the directory snapshots first, which is the open a refused snapshot costs. Each
// iteration is one Open; the Close before it is untimed. dir-ns/op is the open's directory phase
// alone (snapshot restore or record walk); restored/op the fraction of shards restored from a
// snapshot.
func BenchmarkDirSnapshotReopen(b *testing.B) {
	opts := Options{Dir: b.TempDir(), Shards: 4, SegmentSize: 1 << 14, DeltaMax: 8, GroupSchemaCount: -1}
	c, err := Open(opts)
	if err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	now := time.Now().Unix()
	next := 0
	for ; next < 24000; next++ {
		if err := c.Put([]byte(fmt.Sprintf("%d.0", next)), seqFixtureAd(next, now, rng)); err != nil {
			b.Fatal(err)
		}
	}
	for range 16000 {
		seqWriter(b, c, rng, &next, now)
	}
	segs := c.Stats().Segments
	if err := c.Close(); err != nil {
		b.Fatal(err)
	}
	for _, walk := range []bool{false, true} {
		name := "snapshot"
		if walk {
			name = "walk"
		}
		b.Run(name, func(b *testing.B) {
			var restored, total int
			onDirSnapLoad = func(ok bool) {
				total++
				if ok {
					restored++
				}
			}
			var dirTime time.Duration
			OpenIndexDiagHook = func(d OpenIndexDiag) { dirTime += d.Timing.DirRestore }
			defer func() { onDirSnapLoad, OpenIndexDiagHook = nil, nil }()
			b.ResetTimer()
			for range b.N {
				if walk {
					b.StopTimer()
					for i := range opts.Shards {
						os.Remove(filepath.Join(opts.Dir, fmt.Sprint(i), dirSnapName))
					}
					b.StartTimer()
				}
				c, err := Open(opts)
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if err := c.Close(); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
			b.ReportMetric(float64(dirTime.Nanoseconds())/float64(b.N), "dir-ns/op")
			b.ReportMetric(float64(segs), "segments")
			b.ReportMetric(float64(restored)/float64(total), "restored/op")
		})
	}
}
