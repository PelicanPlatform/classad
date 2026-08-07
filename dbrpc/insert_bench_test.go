package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// insertAdText is a job ad of the width a writer actually inserts, in the old-ClassAd form
// opNewAd/opNewAdBatch carry.
func insertAdText(i int) string {
	return fmt.Sprintf(`ClusterId = %d
ProcId = 0
Owner = "u%d"
JobStatus = %d
QDate = %d
RequestMemory = %d
RequestCpus = %d
RemoteHost = "slot1@node%d.example.edu"
Cmd = "/home/u%d/run.sh"
Iwd = "/home/u%d/work"
JobUniverse = 5
RemoteWallClockTime = 1234.5
NumJobStarts = 1
ExitCode = 0`, i, i%5, (i%5)+1, 1700000000+i, ((i%16)+1)*512, (i%8)+1, i%64, i%5, i%5)
}

// BenchmarkInsertBatch measures the whole write path a client sees: one transaction, one
// batched insert of `batch` ads, one commit -- server-side parse (or wire-native ingest)
// included.
func BenchmarkInsertBatch(b *testing.B) {
	const batch = 200
	items := make([]AdKV, batch)
	for i := range items {
		items[i] = AdKV{Key: fmt.Sprintf("%d.0", i), Ad: insertAdText(i)}
	}
	// A persistent db: the wire-native ingest is the path a deployment runs.
	d, err := db.Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()
	ctx := context.Background()

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tx, berr := c.Begin(ctx)
		if berr != nil {
			b.Fatal(berr)
		}
		rej, err := tx.NewClassAdBatch(ctx, items)
		if err != nil {
			b.Fatal(err)
		}
		if len(rej) != 0 {
			b.Fatalf("%d ads rejected", len(rej))
		}
		if err := tx.Commit(ctx); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*batch), "ns/ad")
}
