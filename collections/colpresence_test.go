package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// `attr is undefined` over the columnar accelerator. The risk is not speed, it is that "present" and
// "defined" are different things, so every shape that separates them is exercised here against the
// row path's answer.

// presenceFixture builds a store whose ProcId column exercises every classification:
//
//	most records   ProcId is a small int          -> in the fixed slot, not escaped
//	i%97 == 0      ProcId missing entirely        -> escaped, absent from the cold tail  -> UNDEFINED
//	i%101 == 0     ProcId is a string             -> escaped on TYPE, tail has a literal -> defined
//	i%103 == 0     ProcId out of the fitted width -> escaped on WIDTH, literal           -> defined
//	i%107 == 0     ProcId is literally undefined  -> escaped, tail holds LitUndef        -> UNDEFINED
//
// The exceptional rates are ~1% each on purpose. buildAdSchema only makes an attribute a schema
// field when its dominant storable kind covers >= 90% of the sample, so a fixture that makes ProcId
// undefined in 14% of records has no ProcId FIELD at all -- the accelerator then declines for a
// reason that has nothing to do with presence, and the test passes vacuously or skips.
func presenceFixture(t *testing.T, n int) *Collection {
	t.Helper()
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	for i := 0; i < n; i++ {
		var src string
		switch {
		case i%97 == 0:
			src = fmt.Sprintf("ClusterId = %d\nOwner = \"u%d\"", i/10, i%32) // no ProcId
		case i%101 == 0:
			src = fmt.Sprintf("ClusterId = %d\nProcId = \"notanint\"\nOwner = \"u%d\"", i/10, i%32)
		case i%103 == 0:
			src = fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"u%d\"", i/10, 1<<40+i, i%32)
		case i%107 == 0:
			src = fmt.Sprintf("ClusterId = %d\nProcId = undefined\nOwner = \"u%d\"", i/10, i%32)
		default:
			src = fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"u%d\"", i/10, i%10, i%32)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	q, err := vm.Parse("ProcId >= 0")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		for range c.Query(q) {
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 4) {
		t.Skip("no sealed segments")
	}
	return c
}

// rowTruth counts a constraint the ordinary way, which is the reference every case is checked against.
func rowTruth(t *testing.T, c *Collection, expr string) int {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range c.Query(q) {
		n++
	}
	return n
}

// TestPresenceCountMatchesRowPath is the equivalence test, over data where present and defined come
// apart in four different ways.
func TestPresenceCountMatchesRowPath(t *testing.T) {
	c := presenceFixture(t, 4000)
	defer c.Close()

	for _, expr := range []string{"ProcId is undefined", "ProcId isnt undefined"} {
		want := rowTruth(t, c, expr)
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		got, served := c.CountQuery(q)
		if !served {
			t.Fatalf("%s: columnar path declined; it should serve a lone presence probe", expr)
		}
		if got != want {
			t.Errorf("%s: columnar %d != row %d", expr, got, want)
		}
		t.Logf("%-22s columnar=%d row=%d", expr, got, want)
	}
	// The fixture must actually contain undefined records, or the equality above is vacuous.
	if n := rowTruth(t, c, "ProcId is undefined"); n == 0 {
		t.Error("fixture has no undefined ProcId; the test proves nothing")
	}
}

// TestPresenceCountDeclinesOnExpression is the safety property. An expression's value depends on the
// rest of the ad, so it cannot be classified from its node -- the columnar path must DECLINE and let
// the caller evaluate, rather than guess that "present" means "defined".
func TestPresenceCountDeclinesOnExpression(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	defer c.Close()
	for i := 0; i < 3000; i++ {
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"u%d\"", i/10, i%10, i%32)
		if i == 1500 {
			// An expression that evaluates to UNDEFINED (NoSuchAttr is absent), so a
			// presence-only reading would get the answer WRONG rather than merely imprecise.
			src = fmt.Sprintf("ClusterId = %d\nProcId = NoSuchAttr + 1\nOwner = \"u%d\"", i/10, i%32)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 4) {
		t.Skip("no sealed segments")
	}
	q, err := vm.Parse("ProcId is undefined")
	if err != nil {
		t.Fatal(err)
	}
	got, served := c.CountQuery(q)
	if served {
		t.Errorf("columnar path served a query over an expression-valued attribute (returned %d); "+
			"only evaluation can decide whether that record is undefined", got)
	}
	// And the ordinary path still answers correctly: the expression evaluates to undefined.
	if want := rowTruth(t, c, "ProcId is undefined"); want != 1 {
		t.Errorf("row path counted %d undefined, want 1 (the expression evaluates to undefined)", want)
	}
}

// TestPresenceCountMVCC checks visibility: a superseded version must not be counted, in either
// direction, or an update would shift the answer.
func TestPresenceCountMVCC(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	defer c.Close()
	const n = 2000
	const undef = 100 // ~5%: enough to be countable, few enough that ProcId stays a schema field
	for i := 0; i < n; i++ {
		// Every record starts WITH a ProcId.
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"u%d\"", i/10, i%10, i%32))); err != nil {
			t.Fatal(err)
		}
	}
	// Supersede a few with versions that LACK ProcId. Counting a superseded (defined) version
	// instead of its live (undefined) replacement would undercount.
	for i := 0; i < undef; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("ClusterId = %d\nOwner = \"u%d\"", i/10, i%32))); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 4) {
		t.Skip("no sealed segments")
	}
	q, err := vm.Parse("ProcId is undefined")
	if err != nil {
		t.Fatal(err)
	}
	got, served := c.CountQuery(q)
	if !served {
		t.Fatal("columnar path declined; ProcId should be a schema field in this fixture")
	}
	want := rowTruth(t, c, "ProcId is undefined")
	if want != undef {
		t.Fatalf("row truth is %d, want %d: the fixture is not shaped as intended", want, undef)
	}
	if got != want {
		t.Errorf("columnar %d != row %d; superseded versions are being counted", got, want)
	}
}

// TestPresenceCountUnknownAttrDeclines pins the routing: an attribute the schema does not carry is
// not columnar-servable, so the caller must fall back rather than get a wrong answer from a bitmap
// that has no bit for it.
func TestPresenceCountUnknownAttrDeclines(t *testing.T) {
	c := presenceFixture(t, 2000)
	defer c.Close()
	q, err := vm.Parse("NotAnAttribute is undefined")
	if err != nil {
		t.Fatal(err)
	}
	if got, served := c.CountQuery(q); served {
		t.Errorf("served a presence query on an attribute with no schema field (returned %d)", got)
	}
}
