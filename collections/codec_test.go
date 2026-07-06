package collections

import (
	"fmt"
	"testing"
)

func TestZSTDCodecRoundTrip(t *testing.T) {
	codecs := map[string]func() Codec{
		"identity": func() Codec { return identityCodec{} },
		"zstd": func() Codec {
			z, err := NewZSTDCodec(nil)
			if err != nil {
				t.Fatal(err)
			}
			return z
		},
	}
	for name, mk := range codecs {
		t.Run(name, func(t *testing.T) {
			c := New(Options{Shards: 2, Codec: mk()})
			for i := 0; i < 200; i++ {
				if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
					mustAd(t, fmt.Sprintf(`[Id=%d; Owner="user%d"; Cpus=%d]`, i, i%10, i%8))); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 200; i++ {
				ad, ok := c.Get([]byte(fmt.Sprintf("k%d", i)))
				if !ok {
					t.Fatalf("%s: k%d missing", name, i)
				}
				if got, _ := ad.EvaluateAttrInt("Id"); got != int64(i) {
					t.Fatalf("%s: k%d Id=%d", name, i, got)
				}
			}
		})
	}
}

// TestCodecDensityReport compares stored density across identity, ZSTD, and
// ZSTD+trained-dictionary on the real-ad corpus.
func TestCodecDensityReport(t *testing.T) {
	sample := loadCorpus(t)
	const n = 20000

	report := func(name string, codec Codec) int {
		c := New(Options{Shards: 8, Codec: codec})
		const batch = 512
		b := make([]AdUpdate, 0, batch)
		for i := 0; i < n; i++ {
			b = append(b, AdUpdate{Key: []byte("ad-" + fmt.Sprint(i)), Ad: sample[i%len(sample)]})
			if len(b) == batch {
				if err := c.Update(b); err != nil {
					t.Fatal(err)
				}
				b = b[:0]
			}
		}
		if len(b) > 0 {
			if err := c.Update(b); err != nil {
				t.Fatal(err)
			}
		}
		lb := liveBytes(c)
		t.Logf("%-12s %d bytes/ad", name, lb/c.Len())
		return lb
	}

	idBytes := report("identity", identityCodec{})

	zc, err := NewZSTDCodec(nil)
	if err != nil {
		t.Fatal(err)
	}
	zBytes := report("zstd", zc)

	t.Logf("compression vs identity: zstd %.1fx", float64(idBytes)/float64(zBytes))

	// Train a dictionary from the sample, then measure ZSTD+dict. The pure-Go
	// zstd.BuildDict is less robust than C's ZDICT and can return an unusable
	// (empty) dictionary on a small/homogeneous corpus; treat that as a
	// best-effort measurement rather than a failure.
	train := New(Options{Shards: 4})
	for i := 0; i < 3000; i++ {
		if err := train.Put([]byte("t"+fmt.Sprint(i)), sample[i%len(sample)]); err != nil {
			t.Fatal(err)
		}
	}
	dict, err := TrainDict(train.CollectSamples(3000))
	if err != nil {
		t.Logf("zstd+dict: TrainDict unavailable on this corpus (%v) — known pure-Go BuildDict limitation", err)
		return
	}
	zdc, err := NewZSTDCodec(dict)
	if err != nil {
		t.Fatal(err)
	}
	zdBytes := report("zstd+dict", zdc)
	t.Logf("compression vs identity: zstd+dict %.1fx (dict=%d bytes)",
		float64(idBytes)/float64(zdBytes), len(dict))
}
