package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// benchAdText is a job ad of the width a writer actually inserts, in old-ClassAd form --
// what arrives over dbrpc's opNewAd/opNewAdBatch and over a CEDAR socket.
func benchAdText(i int) string {
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

const ingestBatch = 200

// These two are the BATCHED comparison -- one Update/UpdateOld call for the whole batch --
// which is the shape a transactional commit and the RPC server's batch insert actually
// take. BenchmarkIngestOldDirect/ParseOldThenPut above compare the same two encoders one
// Put at a time, where per-ad commit overhead dominates and the encoder difference does not
// stand out.
//
// BenchmarkIngestParseThenUpdate is what the RPC server did: parse each ad's text into a
// ClassAd, then hand the ClassAd to the store, which encodes it to wire.
func BenchmarkIngestParseThenUpdate(b *testing.B) {
	texts := make([]string, ingestBatch)
	keys := make([][]byte, ingestBatch)
	for i := range texts {
		texts[i] = benchAdText(i)
		keys[i] = []byte(fmt.Sprintf("%d.0", i))
	}
	c := New(Options{})
	defer c.Close()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		batch := make([]AdUpdate, len(texts))
		for j, t := range texts {
			ad, err := classad.ParseOld(t)
			if err != nil {
				b.Fatal(err)
			}
			batch[j] = AdUpdate{Key: keys[j], Ad: ad}
		}
		if err := c.Update(batch); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*ingestBatch), "ns/ad")
}

// BenchmarkIngestUpdateOld is the wire-native ingest: the same text encoded straight to the
// stored wire form, attribute by attribute, with no intermediate ast.ClassAd.
func BenchmarkIngestUpdateOld(b *testing.B) {
	batch := make([]OldAdUpdate, ingestBatch)
	for i := range batch {
		batch[i] = OldAdUpdate{Key: []byte(fmt.Sprintf("%d.0", i)), Text: benchAdText(i)}
	}
	c := New(Options{})
	defer c.Close()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := c.UpdateOld(batch); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*ingestBatch), "ns/ad")
}
