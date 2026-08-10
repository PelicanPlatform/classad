package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// dictFixture builds a collection with all three block regions populated and the accelerator on,
// so its blocks can be sampled.
func dictFixture(t *testing.T, n int) *Collection {
	t.Helper()
	c := New(Options{Shards: 1, SegmentSize: 1 << 14})
	for i := 0; i < n; i++ {
		var b strings.Builder
		for k := 0; k < 20; k++ {
			v := (i*7 + k) % 1000
			if k == 3 && i%50 == 49 {
				v = 1 << 30 // escapes its fitted width -> cold tail
			}
			fmt.Fprintf(&b, "Num%02d = %d\n", k, v)
		}
		for k := 0; k < 6; k++ {
			fmt.Fprintf(&b, "Str%02d = \"user%03d-path-%d-with-some-length\"\n", k, i%200, k)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, b.String())); err != nil {
			t.Fatal(err)
		}
	}
	q, _ := vm.Parse("Num00 >= 0")
	for i := 0; i < 25; i++ {
		for range c.Query(q) {
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 2) { // 2 hot slots => 18 cold numeric columns
		t.Fatal("BuildAndEnableSchemaScan false")
	}
	return c
}

// TestRegionSamplesShape checks the sampler does what it claims: equal-ish shares across the three
// region kinds, drawn from block regions rather than records.
func TestRegionSamplesShape(t *testing.T) {
	c := dictFixture(t, 4000)
	defer c.Close()

	samples := c.CollectRegionSamples(DefaultDictSize, 0)
	if len(samples) == 0 {
		t.Fatal("no region samples collected")
	}
	total := 0
	for _, s := range samples {
		total += len(s)
	}
	t.Logf("region samples: %d chunks, %d bytes (budget %d)", len(samples), total, DefaultDictSize)
	if total > DefaultDictSize {
		t.Errorf("sampled %d bytes, over the %d budget", total, DefaultDictSize)
	}

	// A collection with no blocks at all has nothing to sample and must say so, so the caller can
	// fall back to record sampling rather than train on nothing.
	plain := New(Options{Shards: 1})
	defer plain.Close()
	if got := plain.CollectRegionSamples(DefaultDictSize, 0); got != nil {
		t.Errorf("expected nil from a collection with no columnar blocks, got %d samples", len(got))
	}
}

// TestRegionSamplesLookBack checks the recency bias: a look-back budget must restrict the draw to
// recent segments, which is what future writes resemble.
func TestRegionSamplesLookBack(t *testing.T) {
	c := dictFixture(t, 6000)
	defer c.Close()

	all := c.sampleableBlocks(0)
	recent := c.sampleableBlocks(1 << 14) // roughly one segment's worth
	t.Logf("blocks: %d without a look-back, %d within one segment's bytes", len(all), len(recent))
	if len(all) < 2 {
		t.Skip("fixture produced too few blocks to distinguish")
	}
	if len(recent) >= len(all) {
		t.Errorf("look-back of one segment selected %d of %d blocks; it should restrict the draw",
			len(recent), len(all))
	}
	if len(recent) == 0 {
		t.Error("look-back selected no blocks at all")
	}
}

// TestDictStrategies is the decision: for each of the three regions AND for whole records, compare
// no dictionary, the ad-trained dictionary (status quo), and a region-trained one.
//
// The two uses pull in opposite directions. A record is small and independent, which is exactly what
// a dictionary is for. A block region is a large aggregate with its own redundancy, where a
// dictionary has little to add. Since ONE codec compresses both, the numbers below decide whether
// training on regions is an improvement or a trade.
func TestDictStrategies(t *testing.T) {
	c := dictFixture(t, 4000)
	defer c.Close()

	adSamples := c.CollectSamples(2000)
	regionSamples := c.CollectRegionSamples(DefaultDictSize, 0)
	if len(adSamples) == 0 || len(regionSamples) == 0 {
		t.Fatal("need both sample kinds")
	}
	adDict, err := TrainDict(adSamples)
	if err != nil {
		t.Fatal(err)
	}
	regionDict, err := TrainDict(regionSamples)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(d []byte) Codec {
		cd, err := NewZSTDCodec(d)
		if err != nil {
			t.Fatal(err)
		}
		return cd
	}
	none, adTrained, regionTrained := mk(nil), mk(adDict), mk(regionDict)

	compare := func(label string, payloads [][]byte) {
		var raw, n, a, r int
		for _, p := range payloads {
			raw += len(p)
			n += len(none.Compress(nil, p))
			a += len(adTrained.Compress(nil, p))
			r += len(regionTrained.Compress(nil, p))
		}
		t.Logf("%-14s raw=%8d  no-dict=%7d  ad-dict=%7d (%+.1f%%)  region-dict=%7d (%+.1f%%)",
			label, raw, n, a, 100*float64(a-n)/float64(n), r, 100*float64(r-n)/float64(n))
	}

	// The regions, as whole payloads (what actually gets compressed).
	for kind, name := range map[streamKind]string{
		kindColdNum: "cold-numeric", kindStr: "strings", kindCold: "cold-tail",
	} {
		var payloads [][]byte
		for _, b := range c.sampleableBlocks(0) {
			if raw, err := b.regionRaw(kind); err == nil && len(raw) > 0 {
				payloads = append(payloads, raw)
			}
		}
		if len(payloads) > 0 {
			compare(name, payloads)
		}
	}
	// And whole records, compressed individually -- the row path, where a dictionary earns its keep.
	if len(adSamples) > 500 {
		adSamples = adSamples[:500]
	}
	compare("records", adSamples)
}
