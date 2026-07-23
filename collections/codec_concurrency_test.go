package collections

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

// TestZSTDCodecConcurrentEncodeAll verifies the shared codec's EncodeAll stays correct
// and concurrency-safe under the encoder-concurrency cap of 1 (added to bound the
// encoder's resident history buffers): many goroutines compress/decompress through the
// one codec at once, each round-tripping its own distinct payload. With concurrency 1 the
// calls serialize internally rather than corrupting a shared state, so every payload must
// survive intact.
func TestZSTDCodecConcurrentEncodeAll(t *testing.T) {
	t.Parallel()
	dict := TrainDictOrSkip(t)
	c, err := NewZSTDCodec(dict)
	if err != nil {
		t.Fatal(err)
	}
	const workers, iters = 16, 200
	var wg sync.WaitGroup
	var bad int64
	var mu sync.Mutex
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				src := []byte(fmt.Sprintf("Name = \"slot%d_%d@host.example\"\nState = \"Claimed\"\nMemory = %d\n", w, i, 1024+i))
				comp := c.Compress(nil, src)
				out, err := c.Decompress(nil, comp)
				if err != nil || !bytes.Equal(out, src) {
					mu.Lock()
					bad++
					mu.Unlock()
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if bad != 0 {
		t.Fatalf("%d worker(s) saw corruption/mismatch under concurrent EncodeAll", bad)
	}
}

// TrainDictOrSkip trains a small dictionary from the test corpus, skipping the test if the
// pure-Go BuildDict cannot (a known limitation on some corpora) so the concurrency check
// still runs dictionary-less elsewhere.
func TrainDictOrSkip(t *testing.T) []byte {
	t.Helper()
	sample := loadCorpus(t)
	texts := make([][]byte, 0, len(sample))
	for _, ad := range sample {
		texts = append(texts, []byte(ad.String()))
	}
	dict, err := TrainDict(texts)
	if err != nil {
		return nil // dictionary-less codec still exercises the concurrency cap
	}
	return dict
}
