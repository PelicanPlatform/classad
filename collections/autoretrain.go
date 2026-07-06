package collections

import (
	"sync"
	"time"
)

// DefaultRetrainInterval is the suggested cadence for background dictionary
// retraining. A dictionary trained on a representative sample stays effective as
// long as the ad population's shape is stable, so retraining need not be frequent.
const DefaultRetrainInterval = 15 * time.Minute

// StartAutoRetrain runs periodic maintenance in a background goroutine every
// interval, until the returned stop function is called (stop blocks until the
// goroutine exits). Each tick:
//   - retrains the ZSTD dictionary from a sample of up to sampleMax ads
//     (RetrainDict), and
//   - if hotTopN > 0, refreshes the hot-attribute set to the topN most common
//     attributes (RefreshHotSet), so the store self-tunes which attributes it
//     front-loads for fast queries.
//
// Retrain errors (e.g. the pure-Go BuildDict declining a small/homogeneous
// corpus) are ignored — the previous codec keeps working.
//
// Cost note: retraining recompacts every shard. Compaction is concurrent (the
// recompression runs without the shard lock), so writers and scanners are not
// blocked for its duration; only two brief per-shard critical sections take the
// lock. The 15-minute default still keeps the work rare.
func (c *Collection) StartAutoRetrain(interval time.Duration, sampleMax, hotTopN int) (stop func()) {
	if interval <= 0 {
		interval = DefaultRetrainInterval
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if hotTopN > 0 {
					c.RefreshHotSet(sampleMax, hotTopN)
				}
				_, _ = c.RetrainDict(sampleMax)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		wg.Wait()
	}
}
