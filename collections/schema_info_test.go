package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestSchemaScanInfo checks the columnar accelerator's diagnostics: disabled before enable, and
// after enable it reports the hot columns, a nonzero schema, and full segment coverage.
func TestSchemaScanInfo(t *testing.T) {
	store := New(Options{Shards: 1, SegmentSize: 1 << 12}) // small ⇒ sealed segments
	for i := 0; i < 2000; i++ {
		if err := store.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("Cpus=%d\nMemory=%d", 1+i%8, 1024+(i%64)*256))); err != nil {
			t.Fatal(err)
		}
	}
	if info := store.SchemaScanInfo(); info.Enabled || info.CoveredSegments != 0 {
		t.Fatalf("before enable: %+v (want disabled, 0 covered)", info)
	}
	mc, _ := vm.Parse("Memory >= 0") // drive Memory demand -> hot
	for i := 0; i < 30; i++ {
		for range store.Query(mc) {
		}
	}
	if !store.BuildAndEnableSchemaScan(2000, 4) {
		t.Fatal("BuildAndEnableSchemaScan false")
	}
	info := store.SchemaScanInfo()
	if !info.Enabled {
		t.Fatal("not enabled after BuildAndEnableSchemaScan")
	}
	if info.SchemaFields == 0 || info.SealedSegments == 0 {
		t.Fatalf("schema/segments empty: %+v", info)
	}
	if info.CoveredSegments != info.SealedSegments {
		t.Fatalf("coverage %d/%d, want all sealed covered", info.CoveredSegments, info.SealedSegments)
	}
	found := false
	for _, f := range info.HotFields {
		if f == "Memory" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Memory (queried) not in hot fields %v", info.HotFields)
	}
}
