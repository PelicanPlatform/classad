package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// BenchmarkCommitPaths times the commit paths crash recovery's record stream touches, one op per
// iteration, on a one-shard persistent store: durable and nondurable overwrites, durable and
// nondurable deletes (each delete re-inserts its key first, untimed), and deferred appends to an
// archive. seg=8KiB rolls a segment every few dozen commits; seg=8MiB is the default size.
func BenchmarkCommitPaths(b *testing.B) {
	for _, segSize := range []int{1 << 13, 8 << 20} {
		b.Run(fmt.Sprintf("seg=%dKiB", segSize>>10), func(b *testing.B) { benchmarkCommitPaths(b, segSize) })
	}
}

func benchmarkCommitPaths(b *testing.B, segSize int) {
	mkAd := func(i int) *classad.ClassAd {
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; Owner="u%d"; JobStatus=%d; Pad="%0200d" ]`, i, i%7, i%5, i))
		if err != nil {
			b.Fatal(err)
		}
		return ad
	}
	const keys = 2000
	ads := make([]*classad.ClassAd, keys)
	for i := range ads {
		ads[i] = mkAd(i)
	}
	open := func(b *testing.B) *Collection {
		c, err := Open(Options{Dir: b.TempDir(), Shards: 1, SegmentSize: segSize})
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { c.Close() })
		for i := range keys {
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ads[i]); err != nil {
				b.Fatal(err)
			}
		}
		return c
	}
	for _, durable := range []bool{true, false} {
		name := "durable"
		if !durable {
			name = "nondurable"
		}
		b.Run("overwrite/"+name, func(b *testing.B) {
			c := open(b)
			b.ResetTimer()
			for i := range b.N {
				tx := c.Begin()
				tx.Put([]byte(fmt.Sprintf("k%d", i%keys)), ads[(i+1)%keys])
				tx.SetDurable(durable)
				tx.Commit()
			}
		})
		b.Run("delete/"+name, func(b *testing.B) {
			c := open(b)
			b.ResetTimer()
			for i := range b.N {
				k := []byte(fmt.Sprintf("k%d", i%keys))
				b.StopTimer()
				if i >= keys { // the key was deleted last pass: put it back
					if err := c.Put(k, ads[i%keys]); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
				tx := c.Begin()
				tx.Delete(k)
				tx.SetDurable(durable)
				tx.Commit()
			}
		})
	}
	b.Run("archive-append-deferred", func(b *testing.B) {
		a, err := OpenArchive(ArchiveOptions{Dir: b.TempDir(), SegmentSize: segSize})
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { a.Close() })
		b.ResetTimer()
		for i := range b.N {
			if err := a.AppendDeferred(ads[i%keys]); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		if err := a.Flush(); err != nil {
			b.Fatal(err)
		}
	})
}

// BenchmarkReopenMutable times a clean reopen of a mutable store of about 500 segments.
func BenchmarkReopenMutable(b *testing.B) {
	dir := b.TempDir()
	opts := Options{Dir: dir, Shards: 4, SegmentSize: 1 << 14}
	c, err := Open(opts)
	if err != nil {
		b.Fatal(err)
	}
	for gen := range 3 {
		for i := range 8000 {
			ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; Gen=%d; Pad="%0200d" ]`, i, gen, i))
			if err != nil {
				b.Fatal(err)
			}
			tx := c.Begin()
			tx.Put([]byte(fmt.Sprintf("k%d", i)), ad)
			tx.SetDurable(false)
			tx.Commit()
		}
	}
	segs := c.Stats().Segments
	if err := c.Close(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		c, err := Open(opts)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		c.Close()
		b.StartTimer()
	}
	b.ReportMetric(float64(segs), "segments")
}
