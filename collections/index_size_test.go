package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestIndexSizesAndProvenance: IndexSizes reports non-zero bytes for built indexes,
// tags human (Options) vs auto (addIndexAuto) provenance, and computes a data fraction.
func TestIndexSizesAndProvenance(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"Memory"}})
	c.addIndexAuto([]string{"Host"}, nil) // an auto index
	for i := 0; i < 3000; i++ {
		c.Put([]byte(fmt.Sprintf("m%d", i)),
			mustAd(t, fmt.Sprintf(`[ Id=%d; Owner="user%d"; Host="host%d.example.org"; Memory=%d ]`,
				i, i%50, i, (i%8+1)*1024)))
	}
	c.Reindex()

	sz := c.IndexSizes()
	if sz.TotalBytes <= 0 || sz.DataBytes <= 0 {
		t.Fatalf("expected non-zero index and data bytes, got total=%d data=%d", sz.TotalBytes, sz.DataBytes)
	}
	byAttr := map[string]IndexSize{}
	for _, s := range sz.PerIndex {
		byAttr[s.Attr] = s
	}
	if s, ok := byAttr["Owner"]; !ok || s.Auto || s.Bytes <= 0 {
		t.Errorf("Owner should be a human index with bytes; got %+v", s)
	}
	if s, ok := byAttr["Host"]; !ok || !s.Auto || s.Bytes <= 0 {
		t.Errorf("Host should be an auto index with bytes; got %+v", s)
	}
	if s := byAttr["Memory"]; s.Auto {
		t.Errorf("Memory (Options) should be human, got auto")
	}

	// Sketch accounting: categorical indexes carry a bloom + HLL, so their SketchBytes
	// is positive and reported apart from the posting Bytes; the value index carries only
	// an HLL (no bloom). TotalSketchBytes sums them and is not folded into TotalBytes.
	if s := byAttr["Owner"]; s.SketchBytes <= 0 {
		t.Errorf("Owner (categorical) should report sketch bytes (bloom+HLL); got %+v", s)
	}
	if s := byAttr["Memory"]; s.SketchBytes <= 0 {
		t.Errorf("Memory (value) should report sketch bytes (HLL); got %+v", s)
	}
	var sumSketch int64
	for _, s := range sz.PerIndex {
		sumSketch += s.SketchBytes
	}
	if sz.TotalSketchBytes != sumSketch || sz.TotalSketchBytes <= 0 {
		t.Errorf("TotalSketchBytes=%d should equal the per-index sum=%d and be > 0", sz.TotalSketchBytes, sumSketch)
	}
}

// TestBudgetTrimNeverDropsHuman: with a tight memory budget, AutoTune trims auto indexes
// until under the low watermark but never drops a human-created index.
func TestBudgetTrimNeverDropsHuman(t *testing.T) {
	t.Parallel()
	// Owner is human; Host/GLIDEIN_Site are auto and high-cardinality (big indexes).
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Owner"}})
	c.addIndexAuto([]string{"Host", "GLIDEIN_Site"}, nil)
	for i := 0; i < 4000; i++ {
		c.Put([]byte(fmt.Sprintf("m%d", i)),
			mustAd(t, fmt.Sprintf(`[ Id=%d; Owner="user%d"; Host="host%d.example.org"; GLIDEIN_Site="site%d" ]`,
				i, i%20, i, i)))
	}
	c.Reindex()
	// Record some demand so Owner looks used (belt-and-suspenders; it's human anyway).
	c.demand.record([]vm.Probe{{Attr: "Owner", Op: "=="}})

	before := c.IndexSizes()
	if before.Frac == 0 {
		t.Fatal("expected non-zero index fraction")
	}
	// Set the high watermark below the current fraction so trimming triggers. Disable the
	// absolute slack (this small test DB is well under the default 10 MiB leeway).
	high := before.Frac * 0.5
	low := before.Frac * 0.3
	res := c.AutoTune(AutoTuneOptions{BudgetHighFrac: high, BudgetLowFrac: low, BudgetSlackBytes: -1, Reindex: true})
	if !res.Changed {
		t.Fatalf("expected AutoTune to trim over-budget indexes (frac %.4f > high %.4f)", before.Frac, high)
	}

	cat, _ := c.IndexedAttrs()
	hasOwner := false
	for _, a := range cat {
		if a == "Owner" {
			hasOwner = true
		}
	}
	if !hasOwner {
		t.Errorf("human index Owner must never be auto-dropped; remaining categorical=%v", cat)
	}
	after := c.IndexSizes()
	if after.Frac >= before.Frac {
		t.Errorf("expected index fraction to shrink after trim: before %.4f after %.4f", before.Frac, after.Frac)
	}
}

// TestBudgetSlackSuppressesTrim: an overage smaller than the absolute slack does not
// trigger a trim, even though the fraction exceeds the high watermark -- preventing
// churn on small databases where the percentage is large but the bytes are trivial.
func TestBudgetSlackSuppressesTrim(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4})
	c.addIndexAuto([]string{"Host", "GLIDEIN_Site"}, nil)
	for i := 0; i < 2000; i++ {
		c.Put([]byte(fmt.Sprintf("m%d", i)),
			mustAd(t, fmt.Sprintf(`[ Id=%d; Host="host%d.example.org"; GLIDEIN_Site="site%d" ]`, i, i, i)))
	}
	c.Reindex()
	before := c.IndexSizes()
	high := before.Frac * 0.5 // fraction is over budget...
	// ...but the default 10 MiB slack dwarfs this KB-scale index, so no trim fires.
	res := c.AutoTune(AutoTuneOptions{BudgetHighFrac: high, Reindex: true})
	for _, ch := range res.Changes {
		if ch.Action == "drop" {
			t.Errorf("slack should suppress trimming a %d-byte index over a %.1f%% budget; dropped %s",
				before.TotalBytes, high*100, ch.Attr)
		}
	}
}
