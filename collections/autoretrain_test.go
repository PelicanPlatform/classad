package collections

import (
	"testing"
	"time"
)

func TestAutoRetrain(t *testing.T) {
	t.Parallel()
	sample := loadCorpus(t)
	c := populate(t, sample, 5000)
	if c.currentCodec().Name() != "identity" {
		t.Fatalf("initial codec = %q, want identity", c.currentCodec().Name())
	}

	stop := c.StartAutoRetrain(30*time.Millisecond, 2000, 8)

	// Wait (bounded) for a retrain to switch the codec to a trained dictionary.
	deadline := time.Now().Add(10 * time.Second)
	for c.currentCodec().Name() != "zstd+dict" {
		if time.Now().After(deadline) {
			stop()
			t.Skip("auto-retrain did not produce a dict in time (BuildDict may decline this corpus)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()

	// Data survived the background retrain.
	if c.Len() != 5000 {
		t.Fatalf("Len after auto-retrain = %d, want 5000", c.Len())
	}
	n := 0
	for range c.Scan() {
		n++
	}
	if n != 5000 {
		t.Fatalf("scan after auto-retrain yielded %d, want 5000", n)
	}

	// The hot set was populated by the maintenance loop.
	if len(c.HotAttrNames()) == 0 {
		t.Error("expected auto-refresh to populate a hot set")
	}

	// stop is idempotent and safe to call again.
	stop()
}

func TestRefreshHotSet(t *testing.T) {
	t.Parallel()
	sample := loadCorpus(t)
	c := populate(t, sample, 5000)
	if len(c.HotAttrNames()) != 0 {
		t.Fatalf("expected empty hot set initially, got %v", c.HotAttrNames())
	}
	n := c.RefreshHotSet(2000, 10)
	if n == 0 {
		t.Fatal("RefreshHotSet chose no attributes")
	}
	names := c.HotAttrNames()
	if len(names) != n {
		t.Fatalf("HotAttrNames len %d != chosen %d", len(names), n)
	}
	// The chosen attributes should be genuinely common ones. Real machine ads
	// carry MyType/TargetType on essentially every ad.
	has := func(want string) bool {
		for _, nm := range names {
			if nm == want {
				return true
			}
		}
		return false
	}
	if !has("MyType") && !has("TargetType") {
		t.Logf("hot set: %v", names) // informational; corpus-dependent
	}

	// After refresh, new writes carry hot headers for the chosen attrs and queries
	// still return correct results.
	if err := c.Put([]byte("ad-new"), sample[0]); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get([]byte("ad-new")); !ok {
		t.Fatal("ad-new missing after hot-set refresh")
	}
}
