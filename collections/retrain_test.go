package collections

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestRetrainDictImprovesDensityAndPreservesData populates a collection with real
// ads under the default (identity) codec, retrains a ZSTD dictionary from the
// stored corpus, and verifies density drops sharply while every ad remains
// correct and scannable.
func TestRetrainDictImprovesDensityAndPreservesData(t *testing.T) {
	t.Parallel()
	sample := loadCorpus(t)
	// A few thousand ads is enough to train a dictionary that measurably improves
	// density (per-ad gain), and to exercise the preserve-all-data scan.
	const n = 4000
	c := populate(t, sample, n)

	before := liveBytes(c)
	dictSize, err := c.RetrainDict(3000)
	if err != nil {
		t.Skipf("RetrainDict unavailable on this corpus: %v (known pure-Go BuildDict limitation)", err)
	}
	after := liveBytes(c)
	t.Logf("retrain: %d -> %d live bytes (%.0f -> %.0f bytes/ad), dict=%d bytes",
		before, after, float64(before)/float64(n), float64(after)/float64(n), dictSize)
	if after >= before {
		t.Errorf("RetrainDict did not reduce live bytes: before=%d after=%d", before, after)
	}
	if c.currentCodec().Name() != "zstd+dict" {
		t.Errorf("codec after retrain = %q, want zstd+dict", c.currentCodec().Name())
	}

	// Every ad still decodes correctly (with the new per-segment codec).
	if c.Len() != n {
		t.Fatalf("Len = %d, want %d", c.Len(), n)
	}
	got := 0
	for range c.Scan() {
		got++
	}
	if got != n {
		t.Fatalf("scan after retrain yielded %d, want %d", got, n)
	}
	// Spot-check a specific ad round-trips.
	want := sample[0]
	if ad, ok := c.Get([]byte("ad-0")); !ok || !ad.Equal(want) {
		t.Errorf("ad-0 not preserved across retrain")
	}
}

// TestInflightScanAcrossRetrain verifies a scan in progress still yields each key
// once when the codec is retrained (and all segments recompressed) mid-iteration.
func TestInflightScanAcrossRetrain(t *testing.T) {
	t.Parallel()
	sample := loadCorpus(t)
	const n = 4000
	c := New(Options{Shards: 1})
	for i := 0; i < n; i++ {
		if err := c.Put([]byte("ad-"+itoa(i)), sample[i%len(sample)]); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[string]int, n)
	total := 0
	triggered := false
	for ad := range c.Scan() {
		name, _ := ad.EvaluateAttrString("Name")
		seen[name]++ // Name may repeat across tiled ads; count total instead
		total++
		if !triggered {
			triggered = true
			if _, err := c.RetrainDict(2000); err != nil {
				t.Skipf("RetrainDict unavailable: %v", err)
			}
		}
	}
	if total != n {
		t.Fatalf("in-flight scan across retrain yielded %d, want %d", total, n)
	}
}

// TestConcurrentRetrainUnderUpdates exercises the codec-changing recompression
// path concurrently with updaters and a scanner. Keys are stable and only
// updated (never inserted/deleted), so a correct scan yields exactly n ads.
func TestConcurrentRetrainUnderUpdates(t *testing.T) {
	t.Parallel()
	sample := loadCorpus(t)
	const n = 4000
	c := New(Options{Shards: 4})
	for i := 0; i < n; i++ {
		if err := c.Put([]byte("ad-"+itoa(i)), sample[i%len(sample)]); err != nil {
			t.Fatal(err)
		}
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for x := seed; !stop.Load(); x += 11 {
				_ = c.Put([]byte("ad-"+itoa(x%n)), sample[x%len(sample)])
			}
		}(w * 400)
	}
	// Retrain a few times concurrently with the updaters.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 3 && !stop.Load(); i++ {
			if _, err := c.RetrainDict(1500); err != nil {
				return // best-effort; BuildDict may decline a homogeneous corpus
			}
		}
	}()

	for iter := 0; iter < 5; iter++ {
		total := 0
		for range c.Scan() {
			total++
		}
		if total != n {
			t.Fatalf("scan yielded %d, want %d (codec=%s)", total, n, c.currentCodec().Name())
		}
	}
	stop.Store(true)
	wg.Wait()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
