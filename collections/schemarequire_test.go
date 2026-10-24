package collections

import (
	"fmt"
	"slices"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// requireCorpus builds a sample where Cpus is in every ad, Rss is in only a tenth of them (far
// below any usable presence floor), and Usage is present everywhere but as an EXPRESSION rather
// than a literal -- the shape a job ad has when the shadow passes an expression through.
//
// Rss values are deliberately small so Fit picks a 1-byte column: the point of requiring an
// attribute is a real column, not a widest-possible one.
func requireCorpus(t *testing.T, n int) (*Collection, [][]byte) {
	t.Helper()
	c := New(Options{Shards: 1})
	var wires [][]byte
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("Cpus = %d\nUsage = (Cpus * 2)\n", 1+i%8)
		if i%10 == 0 { // 10% presence: well under the 0.90 floor
			src += fmt.Sprintf("Rss = %d\n", 100+i%50)
		}
		ad, err := classad.ParseOld(src)
		if err != nil {
			t.Fatal(err)
		}
		wires = append(wires, c.encodeAd(ad.AST()))
	}
	return c, wires
}

func fieldByName(t *testing.T, c *Collection, s *adSchema, name string) (adField, bool) {
	t.Helper()
	for _, f := range s.fields {
		if n, ok := c.schemaFieldName(f.id); ok && n == name {
			return f, true
		}
	}
	return adField{}, false
}

// TestRequireAdmitsBelowPresenceFloor is the whole point of the knob: an attribute no presence
// floor would admit becomes a field when required, and its int width is still chosen by Fit from
// the sampled values rather than defaulting to the widest slot.
func TestRequireAdmitsBelowPresenceFloor(t *testing.T) {
	c, wires := requireCorpus(t, 200)

	base := buildAdSchema(wires, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	if _, ok := fieldByName(t, c, base, "Rss"); ok {
		t.Fatal("Rss is in the schema without being required; the corpus no longer tests the floor")
	}

	rssID := c.intern.Intern("Rss")
	s, skipped := buildAdSchemaReport(wires, adSchemaOpts{
		Presence: 0.90, Fit: 0.95, Strings: true,
		Require: map[uint32]struct{}{rssID: {}},
	})
	f, ok := fieldByName(t, c, s, "Rss")
	if !ok {
		t.Fatal("required attribute Rss did not become a schema field")
	}
	if f.kind != akInt {
		t.Errorf("Rss kind = %v, want int", f.kind)
	}
	// Values are 100..149: one unsigned byte. A width of 8 would mean the requirement path
	// skipped Fit and handed out a default slot.
	if f.width != 1 || !f.unsigned {
		t.Errorf("Rss width = (%d, unsigned=%v), want (1, true) -- Fit must still choose the width",
			f.width, f.unsigned)
	}
	for _, sk := range skipped {
		if sk.ID == rssID {
			t.Errorf("Rss was admitted but also reported skipped: %s", sk.Reason)
		}
	}
}

// TestRequireRefusesExpressionValued covers the attribute that must NOT get a column. Usage is in
// every ad, so presence alone would admit it, but it is never a literal: a slot would escape in
// every record, so the value is read from the record either way while every record pays for the
// slot -- and a numeric column of escaped expressions makes the vectorized count hand the query
// back per record, turning a block scan into a row scan.
func TestRequireRefusesExpressionValued(t *testing.T) {
	c, wires := requireCorpus(t, 200)
	usageID := c.intern.Intern("Usage")

	s, skipped := buildAdSchemaReport(wires, adSchemaOpts{
		Presence: 0.90, Fit: 0.95, Strings: true,
		Require: map[uint32]struct{}{usageID: {}},
	})
	if _, ok := fieldByName(t, c, s, "Usage"); ok {
		t.Fatal("expression-valued Usage became a schema field; every record would escape")
	}
	idx := slices.IndexFunc(skipped, func(sk requireSkip) bool { return sk.ID == usageID })
	if idx < 0 {
		t.Fatal("expression-valued Usage was refused with no diagnostic; a required attribute " +
			"that silently does nothing is the failure mode the report exists for")
	}
	if skipped[idx].Reason != requireSkipNoLiteral {
		t.Errorf("Usage skip reason = %q, want %q", skipped[idx].Reason, requireSkipNoLiteral)
	}
}

// TestRequireAbsentIsReported covers the zero-occurrence edge: an attribute nothing in the sample
// carries has no kind and no width, so no slot can be laid out. It is skipped -- and reported,
// because a misspelled name is otherwise indistinguishable from a working one.
func TestRequireAbsentIsReported(t *testing.T) {
	c, wires := requireCorpus(t, 50)
	ghostID := c.intern.Intern("NoSuchAttribute")

	s, skipped := buildAdSchemaReport(wires, adSchemaOpts{
		Presence: 0.90, Fit: 0.95, Strings: true,
		Require: map[uint32]struct{}{ghostID: {}},
	})
	if _, ok := fieldByName(t, c, s, "NoSuchAttribute"); ok {
		t.Fatal("an attribute absent from the sample became a schema field")
	}
	idx := slices.IndexFunc(skipped, func(sk requireSkip) bool { return sk.ID == ghostID })
	if idx < 0 {
		t.Fatal("an absent required attribute was skipped with no diagnostic")
	}
	if skipped[idx].Reason != requireSkipAbsent {
		t.Errorf("skip reason = %q, want %q", skipped[idx].Reason, requireSkipAbsent)
	}
}

// TestSchemaRequireSetTiers checks the configured/runtime split: re-asserting the configured set
// (what an application does from its own config on every open) does not erase a runtime addition,
// and a drop removes from both.
func TestSchemaRequireSetTiers(t *testing.T) {
	c := New(Options{Shards: 1, SchemaRequireAttrs: []string{"Rss", "ImageSize"}})
	if got, want := c.SchemaRequireAttrs(), []string{"imagesize", "rss"}; !slices.Equal(got, want) {
		t.Fatalf("initial set = %v, want %v", got, want)
	}
	if !c.AddSchemaRequireAttrs("DiskUsage") {
		t.Fatal("AddSchemaRequireAttrs reported no change")
	}
	if got, want := c.SchemaRequireRuntimeAttrs(), []string{"diskusage"}; !slices.Equal(got, want) {
		t.Fatalf("runtime tier = %v, want %v", got, want)
	}
	// Re-asserting a different configured set must leave the runtime addition alone.
	if !c.SetSchemaRequireAttrs([]string{"Rss"}) {
		t.Fatal("SetSchemaRequireAttrs reported no change")
	}
	if got, want := c.SchemaRequireAttrs(), []string{"diskusage", "rss"}; !slices.Equal(got, want) {
		t.Fatalf("after re-assert = %v, want %v", got, want)
	}
	if c.SetSchemaRequireAttrs([]string{"rss"}) {
		t.Error("re-asserting the same set reported a change")
	}
	if !c.DropSchemaRequireAttrs("DiskUsage", "Rss") {
		t.Fatal("DropSchemaRequireAttrs reported no change")
	}
	if got := c.SchemaRequireAttrs(); len(got) != 0 {
		t.Fatalf("after drop = %v, want empty", got)
	}
}

// TestSchemaRequireDrivesDerivation runs the knob end to end on a live collection: a required
// attribute below the floor shows up in the derived schema, is marked Required in the
// diagnostics, and the refusals are reported there too.
func TestSchemaRequireDrivesDerivation(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	for i := 0; i < 400; i++ {
		src := fmt.Sprintf("Cpus = %d\nUsage = (Cpus * 2)\n", 1+i%8)
		if i%10 == 0 {
			src += fmt.Sprintf("Rss = %d\n", 100+i%50)
		}
		ad, err := classad.ParseOld(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.SetSchemaRequireAttrs([]string{"Rss", "Usage", "NoSuchAttribute"})
	if !c.BuildAndEnableSchemaScan(500, 4) {
		t.Fatal("BuildAndEnableSchemaScan did not run")
	}
	info := c.SchemaScanInfo()
	var rss *SchemaScanField
	for i := range info.Schema {
		if info.Schema[i].Name == "Rss" {
			rss = &info.Schema[i]
		}
		if info.Schema[i].Name == "Usage" {
			t.Error("expression-valued Usage became a schema field")
		}
	}
	if rss == nil {
		t.Fatal("required Rss is not in the derived schema")
	}
	if !rss.Required {
		t.Error("Rss is in the schema but not marked Required; the diagnostics cannot " +
			"distinguish a field that was asked for from one that cleared the floor")
	}
	reasons := map[string]string{}
	for _, sk := range info.RequireSkipped {
		reasons[sk.Name] = sk.Reason
	}
	if reasons["usage"] != requireSkipNoLiteral {
		t.Errorf("Usage skip = %q, want %q", reasons["usage"], requireSkipNoLiteral)
	}
	if reasons["nosuchattribute"] != requireSkipAbsent {
		t.Errorf("NoSuchAttribute skip = %q, want %q", reasons["nosuchattribute"], requireSkipAbsent)
	}
	if got, want := info.Require, []string{"nosuchattribute", "rss", "usage"}; !slices.Equal(got, want) {
		t.Errorf("info.Require = %v, want %v", got, want)
	}
}
