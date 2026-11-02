package collections

import (
	"testing"
)

// What the data-key cipher costs per sealed value, on the two paths that pay it once per record: an
// archive append sealing Cmd and Args, and a privileged full-ad read opening them. The "nokey" arm is
// the same work with nothing sealed, the floor the sealed arm is measured against.
//
//	go test -run '^$' -bench BenchmarkSealedArchive -benchmem .

func sealedBenchOpts(dir string, sealed bool) ArchiveOptions {
	o := ArchiveOptions{Dir: dir, ZoneAttrs: []string{"CompletionDate"}, SegmentSize: 1 << 22}
	if sealed {
		o.DataKey = sealedRefKey()
		o.EncryptedAttrs = []string{"Cmd", "Args"}
	}
	return o
}

// BenchmarkSealedArchiveAppend is BenchmarkArchiveAppend's deferred shape (AppendDeferred, a Flush
// every 1000) per arm; one op is one append.
func BenchmarkSealedArchiveAppend(b *testing.B) {
	ads := historyBenchAds(b, 2000)
	for _, arm := range []struct {
		name   string
		sealed bool
	}{{"nokey", false}, {"sealed", true}} {
		b.Run(arm.name, func(b *testing.B) {
			a, err := OpenArchive(sealedBenchOpts(b.TempDir(), arm.sealed))
			if err != nil {
				b.Fatal(err)
			}
			defer a.Close()
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				if err := a.AppendDeferred(ads[i%len(ads)]); err != nil {
					b.Fatal(err)
				}
				i++
				if i%1000 == 0 {
					if err := a.Flush(); err != nil {
						b.Fatal(err)
					}
				}
			}
			if err := a.Flush(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// BenchmarkSealedArchiveFullAdRead reads every record of a 5000-record archive as a whole ad through
// the entitled path, which opens each sealed value; one op is the whole scan.
func BenchmarkSealedArchiveFullAdRead(b *testing.B) {
	const n = 5000
	ads := historyBenchAds(b, n)
	q := mustQuery(b, "true")
	for _, arm := range []struct {
		name   string
		sealed bool
	}{{"nokey", false}, {"sealed", true}} {
		b.Run(arm.name, func(b *testing.B) {
			a, err := OpenArchive(sealedBenchOpts(b.TempDir(), arm.sealed))
			if err != nil {
				b.Fatal(err)
			}
			defer a.Close()
			for _, ad := range ads {
				if err := a.AppendDeferred(ad); err != nil {
					b.Fatal(err)
				}
			}
			if err := a.Flush(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if got := countSeq(a.QueryLimit(q, 0)); got != n {
					b.Fatalf("read %d records, want %d", got, n)
				}
			}
		})
	}
}
