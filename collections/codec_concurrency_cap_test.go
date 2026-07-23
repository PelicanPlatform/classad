package collections

import (
	"runtime"
	"testing"
)

// TestEncoderConcurrencyOnlyCapsDown pins the invariant that motivated the cap: the
// effective encoder concurrency is min(GOMAXPROCS, maxEncoderConcurrency), so on a
// many-core host it LOWERS the library default (bounding resident encoder memory) but on a
// small host it never RAISES it above GOMAXPROCS (a hardcoded 4 would have increased both
// concurrency and memory on a 1-2 core node).
func TestEncoderConcurrencyOnlyCapsDown(t *testing.T) {
	old := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(old)

	runtime.GOMAXPROCS(2)
	if got := encoderConcurrency(); got != 2 {
		t.Errorf("GOMAXPROCS=2: encoderConcurrency=%d, want 2 (must not raise above GOMAXPROCS)", got)
	}
	runtime.GOMAXPROCS(1)
	if got := encoderConcurrency(); got != 1 {
		t.Errorf("GOMAXPROCS=1: encoderConcurrency=%d, want 1", got)
	}
	runtime.GOMAXPROCS(16)
	if got := encoderConcurrency(); got != maxEncoderConcurrency {
		t.Errorf("GOMAXPROCS=16: encoderConcurrency=%d, want %d (must cap down)", got, maxEncoderConcurrency)
	}
}
