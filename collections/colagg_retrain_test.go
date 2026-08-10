package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// A columnar block's three regions are stored COMPRESSED, and marshalColSegment writes those bytes
// verbatim -- so a dictionary retrain, which swaps the codec and recompacts, must not leave a block
// whose streams were compressed under the old dictionary reachable through a codec that no longer
// has it. These check that a retrain (and a rewrite, which also recompacts) leaves the columnar
// path both working and correct.

// retrainStore seeds a store with enough similar ads for dictionary training to be meaningful, and
// enables the columnar accelerator.
func retrainStore(t *testing.T) *Collection {
	t.Helper()
	c := New(Options{Shards: 2, SegmentSize: 1 << 12})
	for i := 0; i < 3000; i++ {
		mem := 1024 + (i%64)*256
		if i%500 == 499 {
			mem = 1 << 40 // escapees, so the cold tail is exercised too
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, fmt.Sprintf(
			"Cpus=%d\nMemory=%d\nOwner=\"user%03d\"\nCmd=\"/home/user%03d/run.sh\"\nWallClock=%d.5",
			1+i%8, mem, i%200, i%200, i))); err != nil {
			t.Fatal(err)
		}
	}
	mc, _ := vm.Parse("Memory >= 0")
	for i := 0; i < 25; i++ {
		for range c.Query(mc) {
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		t.Fatal("BuildAndEnableSchemaScan false")
	}
	return c
}

// aggAll reads the aggregate via the columnar path, insisting it is actually served there.
func aggAll(t *testing.T, c *Collection, attr string) NumStats {
	t.Helper()
	ns, ok := c.NumStatsQuery(nil, attr)
	if !ok {
		t.Fatalf("columnar aggregate declined for %s", attr)
	}
	return ns
}

// TestColumnarSurvivesRetrain is the load-bearing one: a retrain trains a NEW dictionary from
// record samples and recompacts every segment under it. If a preserved block's streams were still
// compressed under the previous dictionary, reading them through the new codec would fail or return
// nonsense -- so the aggregate has to keep returning the same answers.
func TestColumnarSurvivesRetrain(t *testing.T) {
	c := retrainStore(t)
	defer c.Close()

	before := aggAll(t, c, "Memory")
	beforeCpus := aggAll(t, c, "Cpus")

	if _, err := c.RetrainDict(2000); err != nil {
		t.Fatalf("RetrainDict: %v", err)
	}

	after := aggAll(t, c, "Memory")
	if after.N != before.N || after.Min != before.Min || after.Max != before.Max || after.Sum != before.Sum {
		t.Errorf("Memory aggregate changed across a retrain:\n before %+v\n after  %+v", before, after)
	}
	if got := aggAll(t, c, "Cpus"); got.N != beforeCpus.N || got.Max != beforeCpus.Max {
		t.Errorf("Cpus aggregate changed across a retrain:\n before %+v\n after  %+v", beforeCpus, got)
	}
	// And the row path must agree with the columnar one afterwards.
	q, _ := vm.Parse("Memory =!= undefined")
	rows := 0
	rowMax := 0.0
	for ad := range c.Query(q) {
		if v, ok := ad.EvaluateAttrNumber("Memory"); ok {
			rows++
			if v > rowMax {
				rowMax = v
			}
		}
	}
	if rows != after.N || rowMax != after.Max {
		t.Errorf("after retrain: columnar N/Max = %d/%v, row scan = %d/%v", after.N, after.Max, rows, rowMax)
	}
}

// TestColumnarSurvivesRewrite covers the other recompacting path, which an operator reaches as
// `.rewrite`.
func TestColumnarSurvivesRewrite(t *testing.T) {
	c := retrainStore(t)
	defer c.Close()

	before := aggAll(t, c, "Memory")
	c.Rewrite()
	after := aggAll(t, c, "Memory")
	if after.N != before.N || after.Min != before.Min || after.Max != before.Max {
		t.Errorf("Memory aggregate changed across a rewrite:\n before %+v\n after  %+v", before, after)
	}
}
