package collections

import (
	"fmt"
	"testing"
)

// TestCodecStats: a fresh collection reports the identity codec at ~1.0x (no
// compression, the "DB size ≈ ad size" symptom); after RetrainDict it reports
// zstd+dict with a >1 ratio, a dictionary size, and a set last-retrain time.
func TestCodecStats(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4})
	for i := 0; i < 2000; i++ {
		c.Put([]byte(fmt.Sprintf("m%d", i)), mustAd(t, fmt.Sprintf(
			`[ Id=%d; Owner="user%d"; Arch="X86_64"; OpSys="LINUX"; Memory=%d;
			   Requirements = (TARGET.RequestMemory <= Memory) && (Arch == TARGET.Arch) ]`,
			i, i%50, (i%8+1)*1024)))
	}

	cs := c.CodecStats(1000)
	if cs.Codec != "identity" {
		t.Errorf("fresh codec = %q, want identity", cs.Codec)
	}
	if cs.Ratio < 0.99 || cs.Ratio > 1.01 {
		t.Errorf("identity ratio = %.3f, want ~1.0", cs.Ratio)
	}
	if !cs.LastRetrain.IsZero() {
		t.Errorf("LastRetrain should be zero before any retrain")
	}
	if cs.SampleRecords == 0 || cs.CompressedBytes == 0 {
		t.Errorf("expected a non-empty sample, got %+v", cs)
	}

	if _, err := c.RetrainDict(1000); err != nil {
		t.Fatalf("retrain: %v", err)
	}
	cs = c.CodecStats(1000)
	if cs.Codec != "zstd+dict" {
		t.Errorf("after retrain codec = %q, want zstd+dict", cs.Codec)
	}
	if cs.Ratio <= 1.1 {
		t.Errorf("after retrain ratio = %.2f, want a real compression gain", cs.Ratio)
	}
	if cs.DictBytes <= 0 || cs.LastRetrain.IsZero() {
		t.Errorf("expected dict bytes + last-retrain set, got %+v", cs)
	}
}
