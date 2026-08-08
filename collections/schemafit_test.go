package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// fitStore seeds a store whose ads all fit a narrow schema, drives read demand on Memory so it
// lands in the hot tier, and enables the accelerator.
func fitStore(t *testing.T) *Collection {
	t.Helper()
	c := New(Options{Shards: 1, SegmentSize: 1 << 12})
	for i := 0; i < 2000; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, fmt.Sprintf(
			"Cpus=%d\nMemory=%d\nJobStatus=%d", 1+i%8, 1024+(i%64)*256, 1+i%5))); err != nil {
			t.Fatal(err)
		}
	}
	mc, _ := vm.Parse("Memory >= 0")
	for i := 0; i < 30; i++ {
		for range c.Query(mc) {
		}
	}
	if !c.BuildAndEnableSchemaScan(2000, 4) {
		t.Fatal("BuildAndEnableSchemaScan false")
	}
	return c
}

// fitOf measures fit and indexes it by attribute name, dropping the sample count.
func fitOf(c *Collection, sampleMax int) map[string]SchemaFieldFit {
	fit, _ := c.SchemaFit(sampleMax)
	return fitByName(fit)
}

// fitByName indexes a fit report by attribute name.
func fitByName(fit []SchemaFieldFit) map[string]SchemaFieldFit {
	m := map[string]SchemaFieldFit{}
	for _, f := range fit {
		m[f.Name] = f
	}
	return m
}

// TestSchemaFitOnMatchingData is the baseline: over the data the schema was built from, nothing
// should escape. If this drifts, the numbers below mean nothing.
func TestSchemaFitOnMatchingData(t *testing.T) {
	c := fitStore(t)
	defer c.Close()

	fit, sampled := c.SchemaFit(2000)
	if len(fit) == 0 || sampled == 0 {
		t.Fatalf("no fit reported (fields=%d sampled=%d)", len(fit), sampled)
	}
	for _, f := range fitByName(fit) {
		if f.Escaped != 0 || f.Missing != 0 {
			t.Errorf("%s escapes on the data its schema was built from: escaped=%.3f missing=%.3f",
				f.Name, f.Escaped, f.Missing)
		}
	}
}

// TestSchemaFitDetectsDrift is the point of the measurement. The schema pins Memory to the
// narrowest int width covering the sampled values; write values far outside that and they can no
// longer live in the fixed slot. The accelerator stays "enabled" and the coverage counts stay
// full, so this rate is the only thing that shows it.
func TestSchemaFitDetectsDrift(t *testing.T) {
	c := fitStore(t)
	defer c.Close()

	before := fitOf(c, 2000)
	if m, ok := before["Memory"]; !ok || m.Escaped != 0 {
		t.Fatalf("Memory should start fitting: %+v", m)
	}

	// Half the table gains a Memory far beyond the chosen width, and a brand-new attribute the
	// schema has never seen.
	for i := 0; i < 2000; i += 2 {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, fmt.Sprintf(
			"Cpus=%d\nMemory=%d\nJobStatus=%d\nGPUs=%d",
			1+i%8, int64(1)<<50+int64(i), 1+i%5, i%4))); err != nil {
			t.Fatal(err)
		}
	}

	after := fitOf(c, 4000)
	m, ok := after["Memory"]
	if !ok {
		t.Fatal("Memory vanished from the schema")
	}
	if m.Escaped == 0 {
		t.Errorf("Memory escape rate still 0 after half the values outgrew the slot: %+v", m)
	}
	if m.Missing != 0 {
		t.Errorf("Memory is present in every record; missing should be 0, got %.3f", m.Missing)
	}
	// The counts an operator would otherwise rely on are unmoved -- which is why the rate exists.
	if info := c.SchemaScanInfo(); !info.Enabled {
		t.Error("accelerator reports disabled; the drift should not disable it")
	}
}

// TestSchemaFitMissingIsDistinct separates the two escape causes: an absent attribute is an
// escape no width could fix, so it must not read as a bad width choice.
func TestSchemaFitMissingIsDistinct(t *testing.T) {
	c := fitStore(t)
	defer c.Close()

	// Rewrite a quarter of the records without JobStatus at all.
	for i := 0; i < 2000; i += 4 {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, fmt.Sprintf(
			"Cpus=%d\nMemory=%d", 1+i%8, 1024+(i%64)*256))); err != nil {
			t.Fatal(err)
		}
	}
	fit := fitOf(c, 4000)
	js, ok := fit["JobStatus"]
	if !ok {
		t.Fatal("JobStatus vanished from the schema")
	}
	if js.Missing == 0 {
		t.Errorf("JobStatus is absent from a quarter of records; missing should be > 0: %+v", js)
	}
	if js.Escaped < js.Missing {
		t.Errorf("escaped (%.3f) must include missing (%.3f)", js.Escaped, js.Missing)
	}
}

// TestReschemaScanRecovers is the fix for the drift: a rebuild re-derives the schema from the
// data as it now is, so the field that had outgrown its slot fits again.
func TestReschemaScanRecovers(t *testing.T) {
	c := fitStore(t)
	defer c.Close()

	for i := 0; i < 2000; i += 2 {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, fmt.Sprintf(
			"Cpus=%d\nMemory=%d\nJobStatus=%d", 1+i%8, int64(1)<<50+int64(i), 1+i%5))); err != nil {
			t.Fatal(err)
		}
	}
	mc, _ := vm.Parse("Memory >= 0")
	for i := 0; i < 30; i++ {
		for range c.Query(mc) { // keep Memory hot across the rebuild
		}
	}
	if drifted := fitOf(c, 4000)["Memory"]; drifted.Escaped == 0 {
		t.Fatal("expected Memory to be escaping before the rebuild")
	}

	if !c.ReschemaScan(2000, 4) {
		t.Fatal("ReschemaScan returned false")
	}
	info := c.SchemaScanInfo()
	if !info.Enabled {
		t.Fatal("accelerator disabled after a rebuild")
	}
	if info.SealedSegments > 0 && info.CoveredSegments != info.SealedSegments {
		t.Errorf("coverage %d/%d after a rebuild: every sealed segment should carry a new block",
			info.CoveredSegments, info.SealedSegments)
	}
	after := fitOf(c, 4000)["Memory"]
	if after.Escaped > 0.02 {
		t.Errorf("Memory still escaping %.3f after the rebuild; the new schema should fit it",
			after.Escaped)
	}
	if after.Width <= 4 {
		t.Errorf("Memory width = %d after values needing 2^50; the rebuild should have widened it",
			after.Width)
	}
}

// TestReschemaScanKeepsCountsCorrect is the correctness guard: the rebuild rewrites every
// segment's block, so a COUNT that routes through the accelerator must still agree with the
// truth afterwards.
func TestReschemaScanKeepsCountsCorrect(t *testing.T) {
	c := fitStore(t)
	defer c.Close()

	exprs := []string{"Memory >= 4096", "Cpus == 3", "JobStatus == 2", "Memory < 2048 && Cpus > 4"}
	truth := func(e string) int {
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for range c.Query(q) {
			n++
		}
		return n
	}
	want := map[string]int{}
	for _, e := range exprs {
		want[e] = truth(e)
	}
	if !c.ReschemaScan(2000, 4) {
		t.Fatal("ReschemaScan returned false")
	}
	for _, e := range exprs {
		if got, ok := c.CountConstraint(e); ok && got != want[e] {
			t.Errorf("%s: count %d after rebuild, want %d", e, got, want[e])
		}
		if got := truth(e); got != want[e] {
			t.Errorf("%s: scan %d after rebuild, want %d", e, got, want[e])
		}
	}
}

// TestSchemaFitDisabled reports nothing rather than inventing zeroes when there is no schema.
func TestSchemaFitDisabled(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	if fit, sampled := c.SchemaFit(100); fit != nil || sampled != 0 {
		t.Errorf("fit on a store with no accelerator = %v/%d, want nil/0", fit, sampled)
	}
}
