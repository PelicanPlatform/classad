package collections

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// realOSPoolAds loads the committed OSPool slot corpus (condor_status -l), skipping when absent.
func realOSPoolAds(tb testing.TB, max int) []*classad.ClassAd {
	tb.Helper()
	data, err := os.ReadFile("testdata/ospool_slots.ldif")
	if err != nil {
		tb.Skip("no OSPool testdata: " + err.Error())
	}
	var ads []*classad.ClassAd
	for _, blk := range strings.Split(string(data), "\n\n") {
		if strings.TrimSpace(blk) == "" {
			continue
		}
		ad, err := classad.ParseOld(blk)
		if err != nil {
			continue
		}
		ads = append(ads, ad)
		if len(ads) >= max {
			break
		}
	}
	if len(ads) == 0 {
		tb.Skip("corpus parsed to zero ads")
	}
	return ads
}

// TestDictStrategiesRealOSPool repeats the dictionary comparison on REAL OSPool ads rather than a
// synthetic fixture whose numeric columns were regular enough to flatter a region-trained
// dictionary. These ads carry ~568 attributes apiece, so all three regions are populated naturally.
func TestDictStrategiesRealOSPool(t *testing.T) {
	ads := realOSPoolAds(t, 20000)
	t.Logf("corpus: %d real OSPool ads, %d attributes in the first",
		len(ads), len(ads[0].AST().Attributes))

	c := New(Options{Shards: 1, SegmentSize: 1 << 18})
	defer c.Close()
	for i, ad := range ads {
		if err := c.Put([]byte(fmt.Sprintf("slot%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	// Drive demand on a couple of numeric attributes real queries use, so the hot tier is realistic
	// and the rest of the numeric fields land in the cold column groups.
	for _, e := range []string{"Memory >= 0", "Cpus >= 0"} {
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			for range c.Query(q) {
			}
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to sample")
	}
	info := c.SchemaScanInfo()
	t.Logf("schema: %d fields, hot %v, %d/%d segments covered",
		info.SchemaFields, info.HotFields, info.CoveredSegments, info.SealedSegments)

	adSamples := c.CollectSamples(2000)
	regionSamples := c.CollectRegionSamples(DefaultDictSize, 0)
	if len(adSamples) == 0 || len(regionSamples) == 0 {
		t.Skip("need both sample kinds")
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
		if raw == 0 {
			return
		}
		t.Logf("%-14s raw=%9d  no-dict=%8d  ad-dict=%8d (%+.1f%%)  region-dict=%8d (%+.1f%%)",
			label, raw, n, a, 100*float64(a-n)/float64(n), r, 100*float64(r-n)/float64(n))
	}

	for _, kv := range []struct {
		kind streamKind
		name string
	}{{kindColdNum, "cold-numeric"}, {kindStr, "strings"}, {kindCold, "cold-tail"}} {
		var payloads [][]byte
		for _, b := range c.sampleableBlocks(0) {
			if raw, err := b.regionRaw(kv.kind); err == nil && len(raw) > 0 {
				payloads = append(payloads, raw)
			}
		}
		compare(kv.name, payloads)
	}
	recs := adSamples
	if len(recs) > 500 {
		recs = recs[:500]
	}
	compare("records", recs)
}
