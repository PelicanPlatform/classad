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

// TestSchemaScanInfoSchema covers the derived schema itself, which is the part an operator
// actually needs to see: the accelerator can be "on" over a schema that recovered the wrong
// shape, and the counts alone cannot show that.
func TestSchemaScanInfoSchema(t *testing.T) {
	store := New(Options{Shards: 1, SegmentSize: 1 << 12})
	for i := 0; i < 2000; i++ {
		// A mix of kinds and int widths: JobStatus fits in one byte, QDate does not; Owner is
		// a string; Held is a bool.
		ad := fmt.Sprintf("Cpus=%d\nMemory=%d\nJobStatus=%d\nQDate=%d\nOwner=\"u%d\"\nHeld=%t",
			1+i%8, 1024+(i%64)*256, 1+i%5, 1700000000+i, i%5, i%2 == 0)
		if err := store.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, ad)); err != nil {
			t.Fatal(err)
		}
	}
	mc, _ := vm.Parse("Memory >= 0")
	for i := 0; i < 30; i++ {
		for range store.Query(mc) {
		}
	}
	if !store.BuildAndEnableSchemaScan(2000, 4) {
		t.Fatal("BuildAndEnableSchemaScan false")
	}

	info := store.SchemaScanInfo()
	if len(info.Schema) != info.SchemaFields {
		t.Fatalf("Schema has %d fields but SchemaFields says %d -- the two must agree",
			len(info.Schema), info.SchemaFields)
	}
	if len(info.Schema) == 0 {
		t.Fatal("no schema reported")
	}
	byName := map[string]SchemaScanField{}
	for _, f := range info.Schema {
		if f.Name == "" || f.Name == "?" {
			t.Errorf("field with unresolved name: %+v", f)
		}
		byName[f.Name] = f
	}
	// The kinds the sampler should have recovered.
	for name, wantKind := range map[string]string{
		"Cpus": "int", "Memory": "int", "JobStatus": "int", "QDate": "int",
		"Owner": "string", "Held": "bool",
	} {
		f, ok := byName[name]
		if !ok {
			t.Errorf("%s missing from the derived schema (fields: %v)", name, info.Schema)
			continue
		}
		if f.Kind != wantKind {
			t.Errorf("%s kind = %q, want %q", name, f.Kind, wantKind)
		}
	}
	// Width is the point of the exercise: a narrow int must not be given eight bytes.
	if js, ok := byName["JobStatus"]; ok {
		if js.Width == 0 || js.Width > 2 {
			t.Errorf("JobStatus width = %d, want the narrowest that fits 1..5", js.Width)
		}
	}
	if qd, ok := byName["QDate"]; ok {
		if qd.Width <= 2 {
			t.Errorf("QDate width = %d, too narrow for a unix timestamp", qd.Width)
		}
	}
	if h, ok := byName["Held"]; ok && h.Width != 0 {
		t.Errorf("Held width = %d, want 0 (bools are bit-packed)", h.Width)
	}
	// The Hot flag must agree with HotFields -- two views of one decision.
	hotFromList := map[string]bool{}
	for _, n := range info.HotFields {
		hotFromList[n] = true
	}
	for _, f := range info.Schema {
		if f.Hot != hotFromList[f.Name] {
			t.Errorf("%s: Schema says hot=%v but HotFields says %v", f.Name, f.Hot, hotFromList[f.Name])
		}
	}
	if m, ok := byName["Memory"]; !ok || !m.Hot {
		t.Errorf("Memory was the queried attribute; it should be marked hot: %+v", m)
	}
}
